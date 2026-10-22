package ospf

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Segment routing for OSPFv3 (RFC 8666) in sparse mode (RFC 8362 6.2): the
// legacy LSAs stay the topology; SR travels in a Router Information LSA
// (RFC 7770: SRGB, algorithms), an E-Router-LSA (our links, with Adj-SIDs)
// and E-Intra-Area-Prefix-LSAs (Prefix-SIDs), for us and the simulated
// routers, and E-Inter-Area-Prefix-LSAs for the SIDs a border router
// carries between areas.

const (
	LSA3RI           = 0xa00c // Router Information, area scope
	LSA3ERouter      = 0xa021
	LSA3ENetwork     = 0xa022
	LSA3EInterPrefix = 0xa023
	LSA3EIntraPrefix = 0xa029

	e3RouterLink  = 1
	e3InterPrefix = 3
	e3IntraPrefix = 6

	prefixN = 0x20 // N: a host prefix identifying a router (RFC 8362 3.1)

	sub3PrefixSID = 4
	sub3AdjSID    = 5
	sub3LANAdjSID = 6
)

var lsa3ExtNames = map[uint16]string{LSA3RI: "router-information", LSA3ERouter: "e-router", LSA3ENetwork: "e-network",
	LSA3EInterPrefix: "e-inter-area-prefix", LSA3EIntraPrefix: "e-intra-area-prefix", 0xa024: "e-inter-area-router",
	0xc025: "e-external", 0xa027: "e-nssa", 0x8028: "e-link"}

// prefixTLV3 is an Intra- or Inter-Area-Prefix TLV with Prefix-SIDs.
func prefixTLV3(typ uint16, metric uint32, p Prefix6, sids []prefixSID) []byte {
	v := []byte{0, byte(metric >> 16), byte(metric >> 8), byte(metric), byte(p.Prefix.Bits()), p.Options, 0, 0}
	a := p.Prefix.Masked().Addr().As16()
	v = append(v, a[:(p.Prefix.Bits()+31)/32*4]...)
	for _, x := range sids {
		v = append(v, tlv(sub3PrefixSID, binary.BigEndian.AppendUint32([]byte{x.flags, x.algo, 0, 0}, x.index))...)
	}
	return tlv(typ, v)
}

func routerLinkTLV(l Router3Link, subs []byte) []byte {
	v := []byte{l.Type, 0, byte(l.Metric >> 8), byte(l.Metric)}
	v = binary.BigEndian.AppendUint32(v, l.IfID)
	v = binary.BigEndian.AppendUint32(v, l.NbrIfID)
	v = append(v, l.NbrRouter.AsSlice()...)
	return tlv(e3RouterLink, append(v, subs...))
}

// sr3Locked adds our SR LSAs in area a (OSPFv3), given our router LSA's
// links there.
func (s *session) sr3Locked(out map[LSAKey]want, a *area, flags uint8, links []Router3Link, sids map[netip.Prefix][]prefixSID) {
	sr := s.cfg.SegmentRouting
	if !sr.Enabled {
		return
	}
	now := time.Now()
	put := func(t uint16, id, adv netip.Addr, body []byte) {
		s.put(out, a, LSAHeader{Type: t, ID: id, AdvRtr: adv}, body)
	}
	put(LSA3RI, zero4, s.rid, s.riBody(false))
	b := binary.BigEndian.AppendUint32(nil, s.options(a))
	b[0] = flags
	for _, l := range links {
		var subs []byte
		if f := s.ifByID(l.IfID); f != nil && sr.AdjSIDBase != 0 {
			if l.Type == LinkP2P {
				subs = tlv(sub3AdjSID, append([]byte{adjV | adjL, 0, 0, 0}, label3(s.adjLabel(f, l.NbrRouter))...))
			} else {
				for _, n := range f.sortedNbrs() {
					if n.adjacent(now) {
						v := append([]byte{adjV | adjL, 0, 0, 0}, n.rid.AsSlice()...)
						subs = append(subs, tlv(sub3LANAdjSID, append(v, label3(s.adjLabel(f, n.rid))...))...)
					}
				}
			}
		}
		b = append(b, routerLinkTLV(l, subs)...)
	}
	put(LSA3ERouter, zero4, s.rid, b)
	s.intraSIDs3(put, a, s.rid, sids)
	if s.topo != nil && a == s.topoArea {
		for _, n := range s.topo.nodes {
			if n.v.Down {
				continue
			}
			put(LSA3RI, zero4, n.rid, s.riBody(false))
			s.intraSIDs3(put, a, n.rid, sids)
		}
	}
}

// intraSIDs3 adds adv's E-Intra-Area-Prefix-LSA with its Prefix-SIDs in
// area a.
func (s *session) intraSIDs3(put func(uint16, netip.Addr, netip.Addr, []byte), a *area, adv netip.Addr, sids map[netip.Prefix][]prefixSID) {
	var ps []netip.Prefix
	for p, xs := range sids {
		for _, x := range xs {
			if x.adv == adv && x.area == a {
				ps = append(ps, p)
				break
			}
		}
	}
	if len(ps) == 0 {
		return
	}
	sort.Slice(ps, func(i, j int) bool { return prefixLess(ps[i], ps[j]) })
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[2:], LSA3ERouter)
	put4(b[8:], adv)
	for _, p := range ps {
		var xs []prefixSID
		for _, x := range sids[p] {
			if x.adv == adv && x.area == a {
				xs = append(xs, x)
			}
		}
		opts := uint8(0)
		if p.Bits() == 128 {
			opts = PrefixLA | prefixN
		}
		b = append(b, prefixTLV3(e3IntraPrefix, 0, Prefix6{Prefix: p, Options: opts}, xs)...)
	}
	put(LSA3EIntraPrefix, zero4, adv, b)
}

// interSIDs3 adds E-Inter-Area-Prefix-LSAs in area b for summarised
// prefixes with Prefix-SIDs in other areas (as for OSPFv2: NP set unless
// the prefix is ours).
func (s *session) interSIDs3(out map[LSAKey]want, b *area, summarised map[netip.Prefix]uint32, sids map[netip.Prefix][]prefixSID) {
	var ps []netip.Prefix
	for p := range summarised {
		if len(sids[p]) > 0 {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool { return prefixLess(ps[i], ps[j]) })
	for _, p := range ps {
		var xs []prefixSID
		for _, x := range sids[p] {
			if x.area == b {
				xs = nil
				break
			}
			if x.adv != s.rid {
				x.flags = x.flags&^psidE | psidNP
			}
			xs = append(xs, x)
		}
		if len(xs) == 0 {
			continue
		}
		opts := uint8(0)
		if p.Bits() == 128 {
			opts = prefixN // kept across areas
		}
		h := LSAHeader{Type: LSA3EInterPrefix, ID: s.lsid3(p), AdvRtr: s.rid}
		s.put(out, b, h, prefixTLV3(e3InterPrefix, summarised[p], Prefix6{Prefix: p, Options: opts}, xs))
	}
}

func (s *session) ifByID(id uint32) *iface {
	for _, f := range s.ifs {
		if f.ifID == id {
			return f
		}
	}
	return nil
}

// ---- decoding for status ----

// prefixTLV3Text reads an Intra/Inter-Area-Prefix TLV value.
func prefixTLV3Text(v []byte) string {
	if len(v) < 8 {
		return "?"
	}
	bits := int(v[4])
	n := (bits + 31) / 32 * 4
	if bits > 128 || len(v) < 8+n {
		return "?"
	}
	var a [16]byte
	copy(a[:], v[8:8+n])
	out := netip.PrefixFrom(netip.AddrFrom16(a), bits).String()
	if m := u24(v[1:]); m != 0 {
		out += fmt.Sprintf(" metric %d", m)
	}
	for _, sub := range readTLVs(v[8+n:]) {
		if sub.t == sub3PrefixSID && len(sub.v) >= 7 {
			out += fmt.Sprintf(" %s algo %d flags %#02x", sidValue(sub.v[4:]), sub.v[1], sub.v[0])
		}
	}
	return out
}

// ext3Summary describes an extended or Router Information LSA (OSPFv3).
func ext3Summary(t uint16, b []byte) string {
	var parts []string
	switch t {
	case LSA3RI:
		s, _ := opaqueDetail(opaqueID(opqRI, 0), b)
		return s
	case LSA3ERouter:
		if len(b) < 4 {
			return "?"
		}
		for _, x := range readTLVs(b[4:]) {
			if x.t != e3RouterLink || len(x.v) < 16 {
				continue
			}
			p := fmt.Sprintf("%s %s", linkTypeNames[x.v[0]], addr4(x.v[12:]))
			for _, sub := range readTLVs(x.v[16:]) {
				switch {
				case sub.t == sub3AdjSID && len(sub.v) >= 7:
					p += " adj " + sidValue(sub.v[4:])
				case sub.t == sub3LANAdjSID && len(sub.v) >= 11:
					p += fmt.Sprintf(" lan-adj %s %s", addr4(sub.v[4:]), sidValue(sub.v[8:]))
				}
			}
			parts = append(parts, p)
		}
	case LSA3EIntraPrefix:
		if len(b) < 12 {
			return "?"
		}
		for _, x := range readTLVs(b[12:]) {
			if x.t == e3IntraPrefix {
				parts = append(parts, prefixTLV3Text(x.v))
			}
		}
	case LSA3EInterPrefix:
		for _, x := range readTLVs(b) {
			if x.t == e3InterPrefix {
				parts = append(parts, prefixTLV3Text(x.v))
			}
		}
	default:
		return fmt.Sprintf("%d bytes", len(b))
	}
	return strings.Join(parts, " · ")
}
