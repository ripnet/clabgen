package ospf

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// OSPFv3 origination (RFC 5340 4.4.3): a router LSA with links only, a
// link LSA per interface (its link-local address and prefixes), a network
// LSA and an intra-area-prefix LSA for each link where we are DR, and an
// intra-area-prefix LSA (or several) for our own prefixes: interfaces that
// aren't transit, loopbacks and internal routes.

// intraChunk is how many prefixes go in one of our intra-area-prefix
// LSAs, so each fits a packet.
const intraChunk = 50

func (s *session) desired3Locked() map[LSAKey]want {
	out := map[LSAKey]want{}
	var links []Router3Link
	var own []Prefix6
	for _, f := range s.ifs {
		if f.state == ifDown {
			continue
		}
		ll, ok := s.ifSourceLocked(f)
		if !ok {
			continue
		}
		pfx := s.ifPrefixes6(f)
		cost, transit := f.cost, f.cost
		if s.maxMetric {
			transit = MaxLinkMetric
		}
		if !f.passive {
			var lp []Prefix6
			for _, p := range pfx {
				lp = append(lp, Prefix6{Prefix: p.Masked()})
			}
			h := LSAHeader{Type: LSA3Link, ID: id4(f.ifID), AdvRtr: s.rid}
			k := h.Key()
			k.Link = f.idx + 1
			out[k] = want{h, LinkLSA{Priority: f.prio, Options: options3, LinkLocal: ll, Prefixes: lp}.Encode()}
		}
		addOwn := func() {
			for _, p := range pfx {
				own = append(own, Prefix6{Prefix: p.Masked(), Metric: cost})
			}
		}
		if f.p2p || f.passive {
			if !f.passive {
				for _, n := range f.sortedNbrs() {
					if n.state == nFull {
						links = append(links, Router3Link{Type: LinkP2P, Metric: transit, IfID: f.ifID, NbrIfID: n.ifID, NbrRouter: n.rid})
					}
				}
			}
			addOwn()
			continue
		}
		full := 0
		var dr *nbr
		for _, n := range f.nbrs {
			if n.state == nFull {
				full++
				if n.id == f.dr {
					dr = n
				}
			}
		}
		if f.state == ifWaiting || !(f.state == ifDR && full > 0 || dr != nil) {
			addOwn() // not a transit network (yet)
			continue
		}
		drIf, drRID := f.ifID, s.rid
		if f.state != ifDR {
			drIf, drRID = dr.ifID, dr.rid
		}
		links = append(links, Router3Link{Type: LinkTransit, Metric: transit, IfID: f.ifID, NbrIfID: drIf, NbrRouter: drRID})
		if f.state != ifDR {
			continue
		}
		rs := []netip.Addr{s.rid}
		fullRID := map[netip.Addr]bool{}
		for _, n := range f.sortedNbrs() {
			if n.state == nFull {
				rs = append(rs, n.rid)
				fullRID[n.rid] = true
			}
		}
		h := LSAHeader{Type: LSA3Network, ID: id4(f.ifID), AdvRtr: s.rid}
		out[h.Key()] = want{h, Network3LSA{Options: options3, Routers: rs}.Encode()}
		// The link's prefixes: ours and those in the link LSAs of the
		// routers adjacent to us on it.
		lp := map[netip.Prefix]bool{}
		for _, p := range pfx {
			lp[p.Masked()] = true
		}
		for k, l := range s.db {
			if k.Link != f.idx+1 || k.Type != LSA3Link || !fullRID[k.AdvRtr] {
				continue
			}
			if x, err := DecodeLinkLSA(l.body()); err == nil {
				for _, p := range x.Prefixes {
					if p.Options&(PrefixNU|PrefixLA) == 0 {
						lp[p.Prefix] = true
					}
				}
			}
		}
		ip := IntraPrefixLSA{RefType: LSA3Network, RefID: id4(f.ifID), RefAdvRtr: s.rid}
		for _, p := range sortedPrefixes(lp) {
			ip.Prefixes = append(ip.Prefixes, Prefix6{Prefix: p})
		}
		h = LSAHeader{Type: LSA3IntraPrefix, ID: id4(f.ifID), AdvRtr: s.rid}
		out[h.Key()] = want{h, ip.Encode()}
	}
	for _, p := range s.loopbacks {
		x := Prefix6{Prefix: p.Masked()}
		if p.Bits() == 128 {
			x.Options = PrefixLA
		}
		own = append(own, x)
	}
	var ext []route
	for _, r := range s.routes {
		if r.kind == "internal" {
			own = append(own, Prefix6{Prefix: r.prefix, Metric: uint16(r.metric)})
		} else {
			ext = append(ext, r)
		}
	}
	addIntraPrefix(out, s.rid, own)
	if s.topo != nil {
		links = append(links, s.topo.ourLinks3(s)...)
		s.topo.desiredLocked(s, out)
	}
	flags := uint8(0)
	if len(ext) > 0 {
		flags |= RouterE
	}
	sort.Slice(ext, func(i, j int) bool { return prefixLess(ext[i].prefix, ext[j].prefix) })
	for _, r := range ext {
		id, ok := s.extIDs[r.prefix]
		if !ok {
			s.nextExt++
			id = s.nextExt
			s.extIDs[r.prefix] = id
		}
		e := External3LSA{E2: r.kind == "external-2", Metric: r.metric, Prefix: Prefix6{Prefix: r.prefix}, Tag: r.tag}
		if r.forward.IsValid() && !r.forward.IsUnspecified() {
			e.Forward = r.forward
		}
		h := LSAHeader{Type: LSA3External, ID: id4(id), AdvRtr: s.rid}
		out[h.Key()] = want{h, e.Encode()}
	}
	h := LSAHeader{Type: LSA3Router, ID: zero4, AdvRtr: s.rid}
	out[h.Key()] = want{h, Router3LSA{Flags: flags, Options: options3, Links: links}.Encode()}
	return out
}

func prefixLess(a, b netip.Prefix) bool {
	if a.Addr() != b.Addr() {
		return a.Addr().Less(b.Addr())
	}
	return a.Bits() < b.Bits()
}

func sortedPrefixes(m map[netip.Prefix]bool) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return prefixLess(out[i], out[j]) })
	return out
}

// ---- status ----

func prefixText(ps []Prefix6, metric bool) string {
	var parts []string
	for i, p := range ps {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(ps)-3))
			break
		}
		t := p.Prefix.String()
		if metric && p.Metric != 0 {
			t += fmt.Sprintf(" (%d)", p.Metric)
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, ", ")
}

func summary3(t uint16, b []byte) string {
	switch t {
	case LSA3Router:
		r, err := DecodeRouter3LSA(b)
		if err != nil {
			return err.Error()
		}
		c := map[uint8]int{}
		for _, x := range r.Links {
			c[x.Type]++
		}
		var parts []string
		for _, lt := range []uint8{LinkP2P, LinkTransit, LinkVirtual} {
			if c[lt] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", c[lt], linkTypeNames[lt]))
			}
		}
		if len(parts) == 0 {
			parts = append(parts, "no links")
		}
		if r.Flags&RouterE != 0 {
			parts = append(parts, "ASBR")
		}
		if r.Flags&RouterB != 0 {
			parts = append(parts, "ABR")
		}
		return strings.Join(parts, ", ")
	case LSA3Network:
		n, err := DecodeNetwork3LSA(b)
		if err != nil {
			return err.Error()
		}
		return fmt.Sprintf("%d routers", len(n.Routers))
	case LSA3Link:
		l, err := DecodeLinkLSA(b)
		if err != nil {
			return err.Error()
		}
		out := l.LinkLocal.String()
		if len(l.Prefixes) > 0 {
			out += " · " + prefixText(l.Prefixes, false)
		}
		return out
	case LSA3IntraPrefix:
		l, err := DecodeIntraPrefixLSA(b)
		if err != nil {
			return err.Error()
		}
		return fmt.Sprintf("%s → %s", lsa3TypeName(l.RefType), prefixText(l.Prefixes, true))
	case LSA3InterPrefix:
		x, err := DecodeInterPrefixLSA(b)
		if err != nil {
			return err.Error()
		}
		return fmt.Sprintf("%s metric %d", x.Prefix.Prefix, x.Metric)
	case LSA3External:
		e, err := DecodeExternal3LSA(b)
		if err != nil {
			return err.Error()
		}
		t := "E1"
		if e.E2 {
			t = "E2"
		}
		out := fmt.Sprintf("%s %s metric %d", e.Prefix.Prefix, t, e.Metric)
		if e.Tag != 0 {
			out += fmt.Sprintf(" tag %d", e.Tag)
		}
		if e.Forward.IsValid() {
			out += " fwd " + e.Forward.String()
		}
		return out
	}
	return ""
}

func prefixList(ps []Prefix6) []map[string]any {
	var out []map[string]any
	for _, p := range ps {
		m := map[string]any{"prefix": p.Prefix.String(), "metric": p.Metric}
		if p.Options != 0 {
			m["options"] = fmt.Sprintf("%#02x", p.Options)
		}
		out = append(out, m)
	}
	return out
}

func detail3(t uint16, b []byte, out map[string]any) {
	switch t {
	case LSA3Router:
		if r, err := DecodeRouter3LSA(b); err == nil {
			var links []map[string]any
			for _, x := range r.Links {
				links = append(links, map[string]any{"type": linkTypeNames[x.Type], "metric": x.Metric, "interface_id": x.IfID,
					"neighbor_interface_id": x.NbrIfID, "neighbor_router": x.NbrRouter.String()})
			}
			out["flags"] = map[string]bool{"asbr": r.Flags&RouterE != 0, "abr": r.Flags&RouterB != 0, "virtual": r.Flags&RouterV != 0}
			out["options"] = fmt.Sprintf("%#06x", r.Options)
			out["links"] = links
		}
	case LSA3Network:
		if n, err := DecodeNetwork3LSA(b); err == nil {
			var rs []string
			for _, r := range n.Routers {
				rs = append(rs, r.String())
			}
			out["attached_routers"] = rs
		}
	case LSA3Link:
		if l, err := DecodeLinkLSA(b); err == nil {
			out["priority"], out["link_local"], out["prefixes"] = l.Priority, l.LinkLocal.String(), prefixList(l.Prefixes)
		}
	case LSA3IntraPrefix:
		if l, err := DecodeIntraPrefixLSA(b); err == nil {
			out["references"] = fmt.Sprintf("%s %s %s", lsa3TypeName(l.RefType), l.RefID, l.RefAdvRtr)
			out["prefixes"] = prefixList(l.Prefixes)
		}
	case LSA3InterPrefix:
		if x, err := DecodeInterPrefixLSA(b); err == nil {
			out["prefix"], out["metric"] = x.Prefix.Prefix.String(), x.Metric
		}
	case LSA3External:
		if e, err := DecodeExternal3LSA(b); err == nil {
			out["prefix"], out["metric"], out["e2"], out["tag"] = e.Prefix.Prefix.String(), e.Metric, e.E2, e.Tag
			if e.Forward.IsValid() {
				out["forwarding_address"] = e.Forward.String()
			}
		}
	}
}
