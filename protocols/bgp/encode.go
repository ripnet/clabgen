package bgp

import (
	"encoding/binary"
	"net/netip"
	"sort"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// UPDATE encoding for injected routes. Attributes are serialised once per
// batch by GoBGP's codec; the NLRI (and MP_REACH / MP_UNREACH around IPv6
// NLRI) are packed here, filling each message up to the size limit.

const maxMsg = 4096

type family struct {
	rf   gb.RouteFamily
	name string
	v6   bool
}

var (
	famIPv4 = family{gb.RF_IPv4_UC, "ipv4-unicast", false}
	famIPv6 = family{gb.RF_IPv6_UC, "ipv6-unicast", true}
)

func familyByName(n string) (family, bool) {
	switch n {
	case famIPv4.name:
		return famIPv4, true
	case famIPv6.name:
		return famIPv6, true
	}
	return family{}, false
}

func familyOf(rf gb.RouteFamily) (family, bool) {
	switch rf {
	case gb.RF_IPv4_UC:
		return famIPv4, true
	case gb.RF_IPv6_UC:
		return famIPv6, true
	}
	return family{}, false
}

func nlriLen(p netip.Prefix) int { return 1 + (p.Bits()+7)/8 }

func appendNLRI(b []byte, p netip.Prefix) []byte {
	b = append(b, byte(p.Bits()))
	return append(b, p.Addr().AsSlice()[:(p.Bits()+7)/8]...)
}

func header(b []byte, typ byte) []byte {
	for i := 0; i < 16; i++ {
		b = append(b, 0xff)
	}
	return append(b, 0, 0, typ) // length set by finish
}

func finish(b []byte) []byte {
	binary.BigEndian.PutUint16(b[16:], uint16(len(b)))
	return b
}

// serializeAttrs serialises attributes, split around where an MP
// attribute (type 14/15) goes to keep ascending type order.
func serializeAttrs(attrs []gb.PathAttributeInterface) (before, after []byte, err error) {
	for _, a := range attrs {
		b, err := a.Serialize()
		if err != nil {
			return nil, nil, err
		}
		if a.GetType() < gb.BGP_ATTR_TYPE_MP_REACH_NLRI {
			before = append(before, b...)
		} else {
			after = append(after, b...)
		}
	}
	return before, after, nil
}

// mpHeader is MP_REACH / MP_UNREACH's attribute header (extended length).
func mpHeader(b []byte, typ byte, l int) []byte {
	return append(b, 0x90, typ, byte(l>>8), byte(l))
}

// buildUpdates packs prefixes (one family, one attribute set) into UPDATE
// messages. nextHop is ours or the route's (IPv6: a global address, with
// linkLocal appended when valid).
func buildUpdates(f family, attrs []gb.PathAttributeInterface, nextHop, linkLocal netip.Addr, pfx []netip.Prefix) ([][]byte, error) {
	if !f.v6 {
		attrs = append(attrs, gb.NewPathAttributeNextHop(nextHop.String()))
	}
	sort.SliceStable(attrs, func(i, j int) bool { return attrs[i].GetType() < attrs[j].GetType() })
	ab1, ab2, err := serializeAttrs(attrs)
	if err != nil {
		return nil, err
	}
	ab := append(append([]byte(nil), ab1...), ab2...)
	var out [][]byte
	if !f.v6 {
		room := maxMsg - 19 - 4 - len(ab)
		for len(pfx) > 0 {
			b := header(make([]byte, 0, maxMsg), 2)
			b = append(b, 0, 0) // no withdrawn routes
			b = binary.BigEndian.AppendUint16(b, uint16(len(ab)))
			b = append(b, ab...)
			used := 0
			for len(pfx) > 0 && used+nlriLen(pfx[0]) <= room {
				b = appendNLRI(b, pfx[0])
				used += nlriLen(pfx[0])
				pfx = pfx[1:]
			}
			out = append(out, finish(b))
		}
		return out, nil
	}
	nh := nextHop.AsSlice()
	if linkLocal.IsValid() {
		nh = append(nh, linkLocal.AsSlice()...)
	}
	fixed := 2 + 1 + 1 + len(nh) + 1 // AFI, SAFI, NH length, NH, reserved
	room := maxMsg - 19 - 4 - len(ab) - 4 - fixed
	for len(pfx) > 0 {
		var nl []byte
		for len(pfx) > 0 && len(nl)+nlriLen(pfx[0]) <= room {
			nl = appendNLRI(nl, pfx[0])
			pfx = pfx[1:]
		}
		b := header(make([]byte, 0, maxMsg), 2)
		b = append(b, 0, 0)
		b = binary.BigEndian.AppendUint16(b, uint16(len(ab)+4+fixed+len(nl)))
		b = append(b, ab1...)
		b = mpHeader(b, byte(gb.BGP_ATTR_TYPE_MP_REACH_NLRI), fixed+len(nl))
		b = append(b, 0, 2, 1, byte(len(nh)))
		b = append(b, nh...)
		b = append(b, 0)
		b = append(b, nl...)
		b = append(b, ab2...)
		out = append(out, finish(b))
	}
	return out, nil
}

// buildWithdraws packs withdrawn prefixes of one family.
func buildWithdraws(f family, pfx []netip.Prefix) [][]byte {
	var out [][]byte
	for len(pfx) > 0 {
		var nl []byte
		room := maxMsg - 19 - 4
		if f.v6 {
			room -= 4 + 3
		}
		for len(pfx) > 0 && len(nl)+nlriLen(pfx[0]) <= room {
			nl = appendNLRI(nl, pfx[0])
			pfx = pfx[1:]
		}
		b := header(make([]byte, 0, maxMsg), 2)
		if !f.v6 {
			b = binary.BigEndian.AppendUint16(b, uint16(len(nl)))
			b = append(b, nl...)
			b = append(b, 0, 0)
		} else {
			b = append(b, 0, 0)
			b = binary.BigEndian.AppendUint16(b, uint16(4+3+len(nl)))
			b = mpHeader(b, byte(gb.BGP_ATTR_TYPE_MP_UNREACH_NLRI), 3+len(nl))
			b = append(b, 0, 2, 1)
			b = append(b, nl...)
		}
		out = append(out, finish(b))
	}
	return out
}

// endOfRIB is the End-of-RIB marker (RFC 4724).
func endOfRIB(f family) []byte {
	b, _ := gb.NewEndOfRib(f.rf).Serialize()
	return b
}

func serialize(m *gb.BGPMessage) []byte {
	b, err := m.Serialize()
	if err != nil {
		panic("bgp: serialize: " + err.Error())
	}
	return b
}
