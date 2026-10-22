package ospf

import (
	"encoding/binary"
	"net/netip"
	"sort"
	"time"
)

// Origination outside an area's own LSAs: summaries into each area (as a
// border router: the other areas' intra-area routes, aggregated by their
// ranges, the backbone's inter-area routes into non-backbone areas, ASBRs,
// defaults into stub and NSSA areas; and injected inter-area prefixes),
// AS-external LSAs, type-7 LSAs in NSSAs and their translation (RFC 3101).

var (
	default4 = netip.MustParsePrefix("0.0.0.0/0")
	default6 = netip.MustParsePrefix("::/0")
)

func (s *session) defaultRoute() netip.Prefix {
	if s.cd.v3 {
		return default6
	}
	return default4
}

// put adds an LSA to the desired set in area a (nil: AS scope). OSPFv2
// headers get the area's options unless they carry their own (type 7).
func (s *session) put(out map[LSAKey]want, a *area, h LSAHeader, body []byte) {
	k := h.Key()
	if a != nil {
		k.Area = a.key()
		if !s.cd.v3 && h.Type != LSANSSA {
			h.Options = s.lsaOptions(a)
		}
	} else if !s.cd.v3 {
		h.Options = OptE
	}
	out[k] = want{h, body}
}

// routeSet is the injected routes by kind, sorted.
type routeSet struct {
	external, inter []route
	sids            map[netip.Prefix][]prefixSID // ours and the simulated routers' (OSPFv2)
}

func (s *session) routeSetLocked() routeSet {
	rs := routeSet{sids: s.sidTableLocked()}
	for _, r := range s.routes {
		switch r.kind {
		case "internal":
		case "inter-area":
			rs.inter = append(rs.inter, r)
		default:
			rs.external = append(rs.external, r)
		}
	}
	for _, l := range [][]route{rs.external, rs.inter} {
		sort.Slice(l, func(i, j int) bool { return prefixLess(l[i].prefix, l[j].prefix) })
	}
	return rs
}

// routerFlagsLocked are our router LSA's flags in area a.
func (s *session) routerFlagsLocked(a *area, abr bool, rs routeSet) uint8 {
	var f uint8
	if abr || len(rs.inter) > 0 {
		f |= RouterB
	}
	translating := false
	for _, t := range s.translating {
		translating = translating || t
	}
	if len(rs.external) > 0 && a.kind != areaStub || translating && a.kind == areaNormal {
		f |= RouterE
	}
	if s.virtualThrough(a) {
		f |= RouterV
	}
	if a.kind == areaNSSA && abr && a.translate == "always" {
		f |= RouterNt
	}
	return f
}

// lsid2 is an OSPFv2 summary or external LSA's link state ID: the network
// address, or with the host bits set when another prefix of the same
// network took it (RFC 2328 appendix E).
func lsid2(out map[LSAKey]want, a *area, typ uint16, adv netip.Addr, p netip.Prefix) netip.Addr {
	id := p.Addr()
	k := LSAKey{Type: typ, ID: id, AdvRtr: adv}
	if a != nil {
		k.Area = a.key()
	}
	if _, taken := out[k]; taken {
		v := binary.BigEndian.Uint32(id.AsSlice()) | ^binary.BigEndian.Uint32(MaskOf(p.Bits()).AsSlice())
		id = id4(v)
	}
	return id
}

// lsid3 is a stable OSPFv3 link state ID for a prefix.
func (s *session) lsid3(p netip.Prefix) netip.Addr {
	id, ok := s.extIDs[p]
	if !ok {
		s.nextExt++
		id = s.nextExt
		s.extIDs[p] = id
	}
	return id4(id)
}

// desiredInterLocked lists summaries and externals. Border routers first
// compute every area's routes.
func (s *session) desiredInterLocked(areas []*area, abr bool, rs routeSet, now time.Time) map[LSAKey]want {
	out := map[LSAKey]want{}
	for _, a := range s.areas {
		a.spf = nil
		if abr || len(s.cfg.VirtualLinks) > 0 {
			a.spf = s.spfLocked(a, now)
		}
	}
	s.updateVirtualLinksLocked(now)
	for _, b := range areas {
		s.summariesLocked(out, b, areas, abr, rs)
	}
	s.externalsLocked(out, areas, abr, rs)
	return out
}

// summariesLocked adds our summaries into area b.
func (s *session) summariesLocked(out map[LSAKey]want, b *area, areas []*area, abr bool, rs routeSet) {
	pfx := map[netip.Prefix]uint32{}
	asbr := map[netip.Addr]uint32{}
	if abr {
		for _, a := range areas {
			if a == b || a.spf == nil {
				continue
			}
			agg := map[*areaRange]uint32{}
			for p, c := range a.spf.intra {
				if r := a.rangeFor(p); r != nil {
					if c > agg[r] || agg[r] == 0 {
						agg[r] = c
					}
					continue
				}
				setMin(pfx, p, c)
			}
			for r, c := range agg {
				if r.noAdvertise {
					continue
				}
				if r.cost > 0 {
					c = r.cost
				}
				setMin(pfx, r.prefix, c)
			}
			if a.kind == areaNormal {
				for rid, c := range a.spf.asbrs {
					setMin(asbr, rid, c)
				}
			}
			if a.backbone() && !b.backbone() {
				for p, c := range a.spf.inter {
					setMin(pfx, p, c)
				}
				for rid, c := range a.spf.interASBR {
					setMin(asbr, rid, c)
				}
			}
		}
		if b.kind != areaNormal {
			if b.noSummary {
				pfx = map[netip.Prefix]uint32{}
			}
			if b.kind == areaStub || b.noSummary {
				pfx[s.defaultRoute()] = b.defCost
			}
			asbr = nil // no type 4 into stub areas or NSSAs
		}
		// Nothing about b's own routers goes back into b.
		if b.spf != nil {
			for rid := range asbr {
				if _, in := b.spf.dist[rid]; in {
					delete(asbr, rid)
				}
			}
		}
	}
	for _, r := range rs.inter {
		setMin(pfx, r.prefix, r.metric)
	}
	ps := make([]netip.Prefix, 0, len(pfx))
	for p, c := range pfx {
		if c < LSInfinity {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool { return prefixLess(ps[i], ps[j]) })
	if s.cfg.SegmentRouting.Enabled {
		set := map[netip.Prefix]bool{}
		for _, p := range ps {
			set[p] = true
		}
		if s.cd.v3 {
			s.interSIDs3(out, b, pfx, rs.sids)
		} else {
			s.interSIDsLocked(out, b, set, rs.sids)
		}
	}
	for _, p := range ps {
		if s.cd.v3 {
			h := LSAHeader{Type: LSA3InterPrefix, ID: s.lsid3(p), AdvRtr: s.rid}
			s.put(out, b, h, InterPrefixLSA{Metric: pfx[p], Prefix: Prefix6{Prefix: p}}.Encode())
		} else {
			h := LSAHeader{Type: LSASummary, ID: lsid2(out, b, LSASummary, s.rid, p), AdvRtr: s.rid}
			s.put(out, b, h, SummaryLSA{Mask: MaskOf(p.Bits()), Metric: pfx[p]}.Encode())
		}
	}
	for rid, c := range asbr {
		if c >= LSInfinity || rid == s.rid {
			continue
		}
		if s.cd.v3 {
			h := LSAHeader{Type: LSA3InterRouter, ID: rid, AdvRtr: s.rid}
			s.put(out, b, h, InterRouterLSA{Options: options3, Metric: c, Router: rid}.Encode())
		} else {
			h := LSAHeader{Type: LSAASBRSummary, ID: rid, AdvRtr: s.rid}
			s.put(out, b, h, SummaryLSA{Mask: zero4, Metric: c}.Encode())
		}
	}
}

// externalsLocked adds AS-external LSAs (when we are in a normal area),
// type-7 LSAs in each NSSA, an NSSA default, and translations.
func (s *session) externalsLocked(out map[LSAKey]want, areas []*area, abr bool, rs routeSet) {
	normal := false
	for _, a := range areas {
		normal = normal || a.externals()
	}
	if normal {
		for _, r := range rs.external {
			s.putExternal(out, nil, r, false)
		}
	}
	translating := map[*area]bool{}
	for _, a := range areas {
		if a.kind != areaNSSA {
			continue
		}
		// An NSSA border router's own type-7 LSAs aren't translated (it
		// originates AS-externals itself); an internal router's are, so it
		// gives a forwarding address.
		prop := !abr
		for _, r := range rs.external {
			if prop && !r.forward.IsValid() || prop && r.forward.IsUnspecified() {
				r.forward = s.nssaForwardLocked(a)
			}
			s.putExternal(out, a, r, prop)
		}
		if abr && a.nssaDefault {
			s.putExternal(out, a, route{prefix: s.defaultRoute(), metric: a.defCost, kind: "external-2", forward: s.unspecified()}, false)
		}
		if abr && normal && s.translatorLocked(a) {
			translating[a] = true
			s.translateLocked(out, a)
		}
	}
	changed := len(translating) != len(s.translating)
	for a := range translating {
		changed = changed || !s.translating[a]
	}
	if changed {
		s.translating = translating
		s.dirty = true // router LSA flags (E) follow
	}
}

func (s *session) unspecified() netip.Addr {
	if s.cd.v3 {
		return netip.Addr{}
	}
	return zero4
}

// putExternal adds a type-5 (a nil) or type-7 LSA for a route.
func (s *session) putExternal(out map[LSAKey]want, a *area, r route, prop bool) {
	if s.cd.v3 {
		e := External3LSA{E2: r.kind == "external-2", Metric: r.metric, Prefix: Prefix6{Prefix: r.prefix}, Tag: r.tag}
		if prop {
			e.Prefix.Options |= PrefixP
		}
		if r.forward.IsValid() && !r.forward.IsUnspecified() {
			e.Forward = r.forward
		}
		t := uint16(LSA3External)
		if a != nil {
			t = LSA3NSSA
		}
		s.put(out, a, LSAHeader{Type: t, ID: s.lsid3(r.prefix), AdvRtr: s.rid}, e.Encode())
		return
	}
	t := uint16(LSAExternal)
	h := LSAHeader{Options: OptE, AdvRtr: s.rid}
	if a != nil {
		t, h.Options = LSANSSA, 0
		if prop {
			h.Options = OptNP
		}
	}
	h.Type = t
	h.ID = lsid2(out, a, t, s.rid, r.prefix)
	fwd := r.forward
	if !fwd.IsValid() {
		fwd = zero4
	}
	s.put(out, a, h, ExternalLSA{Mask: MaskOf(r.prefix.Bits()), E2: r.kind == "external-2", Metric: r.metric, Forward: fwd, Tag: r.tag}.Encode())
}

// nssaForwardLocked is the forwarding address of our type-7 LSAs in an
// NSSA: an address of an interface in it, else a loopback.
func (s *session) nssaForwardLocked(a *area) netip.Addr {
	for _, f := range s.ifs {
		if f.area != a || f.state == ifDown {
			continue
		}
		if s.cd.v3 {
			if ps := s.ifPrefixes6(f); len(ps) > 0 {
				return ps[0].Addr()
			}
		} else if p, ok := s.ifAddrLocked(f); ok {
			return p.Addr()
		}
	}
	if len(s.loopbacks) > 0 {
		return s.loopbacks[0].Addr()
	}
	return s.unspecified()
}

// translatorLocked: we translate the NSSA's type-7 LSAs (RFC 3101 3.1):
// always, never, or as the candidate with the highest router ID among the
// area's reachable border routers.
func (s *session) translatorLocked(a *area) bool {
	switch a.translate {
	case "always":
		return true
	case "never":
		return false
	}
	if a.spf == nil {
		return false
	}
	for rid, f := range a.spf.flags {
		if rid == s.rid || f&RouterB == 0 {
			continue
		}
		if f&RouterNt != 0 || s.rid.Less(rid) {
			return false
		}
	}
	return true
}

// translateLocked originates AS-external LSAs for the NSSA's type-7 LSAs
// with the P bit, from reachable routers, that give a forwarding address.
func (s *session) translateLocked(out map[LSAKey]want, a *area) {
	type cand struct {
		p   netip.Prefix
		r   route
		adv netip.Addr
	}
	var cs []cand
	for k, l := range s.db {
		if k.Area != a.key() || k.Type != s.cd.nssaType() || l.own || s.isSelfLocked(l.hdr) || l.ageAt(time.Now()) >= MaxAge {
			continue
		}
		if _, ok := a.spf.dist[k.AdvRtr]; !ok {
			continue
		}
		if s.cd.v3 {
			e, err := DecodeExternal3LSA(l.body())
			if err != nil || e.Prefix.Options&PrefixP == 0 || !e.Forward.IsValid() || e.Metric >= LSInfinity {
				continue
			}
			kind := "external-1"
			if e.E2 {
				kind = "external-2"
			}
			cs = append(cs, cand{e.Prefix.Prefix, route{prefix: e.Prefix.Prefix, metric: e.Metric, kind: kind, tag: e.Tag, forward: e.Forward}, k.AdvRtr})
			continue
		}
		e, err := DecodeExternalLSA(l.body())
		if err != nil || l.hdr.Options&OptNP == 0 || e.Forward == zero4 || BitsOf(e.Mask) < 0 || e.Metric >= LSInfinity {
			continue
		}
		p := netip.PrefixFrom(k.ID, BitsOf(e.Mask)).Masked()
		kind := "external-1"
		if e.E2 {
			kind = "external-2"
		}
		cs = append(cs, cand{p, route{prefix: p, metric: e.Metric, kind: kind, tag: e.Tag, forward: e.Forward}, k.AdvRtr})
	}
	// Our own routes win; between type-7 LSAs for one prefix the lowest
	// advertising router ID.
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].p != cs[j].p {
			return prefixLess(cs[i].p, cs[j].p)
		}
		return cs[i].adv.Less(cs[j].adv)
	})
	have := map[netip.Prefix]bool{}
	for _, r := range s.routes {
		have[r.prefix] = true
	}
	for _, c := range cs {
		if have[c.p] {
			continue
		}
		have[c.p] = true
		s.putExternal(out, nil, c.r, false)
	}
}
