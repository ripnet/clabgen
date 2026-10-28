package bgp

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// tmpl is a route's attributes, parsed once; advertised prefixes point at
// the template they were advertised with, so UPDATEs group them.
type tmpl struct {
	id        int
	origin    uint8
	asPath    []uint32
	med       *uint32
	localPref *uint32
	nextHop   netip.Addr
	comms     []uint32
	large     []*gb.LargeCommunity
	ext       []gb.ExtendedCommunityInterface
	atomic    bool
	extra     []gb.PathAttributeInterface // per-route-type attributes (EVPN PMSI tunnel)
	peers     map[netip.Addr]bool         // nil: every peer
	text      string                      // for status
	count     [2]int                      // prefixes advertised with it: IPv4, IPv6
}

var origins = map[string]uint8{"": gb.BGP_ORIGIN_ATTR_TYPE_IGP, "igp": gb.BGP_ORIGIN_ATTR_TYPE_IGP, "egp": gb.BGP_ORIGIN_ATTR_TYPE_EGP, "incomplete": gb.BGP_ORIGIN_ATTR_TYPE_INCOMPLETE}

// attrs parses a route's attributes.
func (r Route) attrs() (*tmpl, error) {
	t := &tmpl{asPath: r.ASPath, med: r.MED, localPref: r.LocalPref, atomic: r.AtomicAggregate}
	o, ok := origins[r.Origin]
	if !ok {
		return nil, fmt.Errorf("route %s: origin must be igp, egp or incomplete", r.Prefix)
	}
	t.origin = o
	if r.NextHop != "" {
		t.nextHop, _ = netip.ParseAddr(r.NextHop)
	}
	for _, c := range r.Communities {
		v, err := parseCommunity(c)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", r.Prefix, err)
		}
		t.comms = append(t.comms, v)
	}
	for _, c := range r.LargeCommunities {
		v, err := parseLarge(c)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", r.Prefix, err)
		}
		t.large = append(t.large, gb.NewLargeCommunity(v[0], v[1], v[2]))
	}
	for _, rt := range r.RouteTargets {
		r.ExtCommunities = append(r.ExtCommunities, "rt:"+rt)
	}
	for _, c := range r.ExtCommunities {
		kind, val, ok := strings.Cut(c, ":")
		if kind == "color" {
			// Color (RFC 9012 4.3), CO bits 00: steers into SR policies of that color.
			v, err := strconv.ParseUint(val, 10, 32)
			if err != nil || !ok {
				return nil, fmt.Errorf("route %s: extended community %q: color:N with N a 32-bit number", r.Prefix, c)
			}
			t.ext = append(t.ext, gb.NewColorExtended(uint32(v)))
			continue
		}
		sub := map[string]gb.ExtendedCommunityAttrSubType{"rt": gb.EC_SUBTYPE_ROUTE_TARGET, "soo": gb.EC_SUBTYPE_ROUTE_ORIGIN}[kind]
		if !ok || sub == 0 {
			return nil, fmt.Errorf("route %s: extended community %q: want rt:…, soo:… or color:…", r.Prefix, c)
		}
		e, err := gb.ParseExtendedCommunity(sub, val)
		if err != nil {
			return nil, fmt.Errorf("route %s: extended community %q: %v", r.Prefix, c, err)
		}
		t.ext = append(t.ext, e)
	}
	if len(r.Peers) > 0 {
		t.peers = map[netip.Addr]bool{}
		for _, p := range r.Peers {
			a, _ := netip.ParseAddr(p)
			t.peers[a.Unmap()] = true
		}
	}
	var parts []string
	if len(r.ASPath) > 0 {
		parts = append(parts, fmt.Sprint("path ", r.ASPath))
	}
	if r.NextHop != "" {
		parts = append(parts, "nh "+r.NextHop)
	}
	if r.MED != nil {
		parts = append(parts, fmt.Sprint("med ", *r.MED))
	}
	if r.LocalPref != nil {
		parts = append(parts, fmt.Sprint("lp ", *r.LocalPref))
	}
	for _, x := range [][]string{r.Communities, r.LargeCommunities, r.ExtCommunities} {
		if len(x) > 0 {
			parts = append(parts, strings.Join(x, " "))
		}
	}
	if r.RD != "" {
		parts = append([]string{"rd " + r.RD}, parts...)
	}
	t.text = strings.Join(parts, ", ")
	return t, nil
}

// pathAttrs are the attributes toward one peer (without NLRI / MP_REACH).
func (t *tmpl) pathAttrs(localAS uint32, ebgp bool) []gb.PathAttributeInterface {
	path := t.asPath
	if ebgp {
		path = append([]uint32{localAS}, path...)
	}
	attrs := []gb.PathAttributeInterface{gb.NewPathAttributeOrigin(t.origin)}
	var segs []gb.AsPathParamInterface
	if len(path) > 0 {
		segs = []gb.AsPathParamInterface{gb.NewAs4PathParam(gb.BGP_ASPATH_ATTR_TYPE_SEQ, path)}
	}
	attrs = append(attrs, gb.NewPathAttributeAsPath(segs))
	if t.med != nil {
		attrs = append(attrs, gb.NewPathAttributeMultiExitDisc(*t.med))
	}
	if !ebgp {
		lp := uint32(100)
		if t.localPref != nil {
			lp = *t.localPref
		}
		attrs = append(attrs, gb.NewPathAttributeLocalPref(lp))
	}
	if t.atomic {
		attrs = append(attrs, gb.NewPathAttributeAtomicAggregate())
	}
	if len(t.comms) > 0 {
		attrs = append(attrs, gb.NewPathAttributeCommunities(t.comms))
	}
	if len(t.ext) > 0 {
		attrs = append(attrs, gb.NewPathAttributeExtendedCommunities(t.ext))
	}
	if len(t.large) > 0 {
		attrs = append(attrs, gb.NewPathAttributeLargeCommunities(t.large))
	}
	return append(attrs, t.extra...)
}

func (t *tmpl) forPeer(a netip.Addr) bool { return t.peers == nil || t.peers[a] }

// ---- received routes ----

// pathInfo summarises a received path's attributes; NLRIs of one UPDATE
// share it.
type pathInfo struct {
	NextHop          string   `json:"next_hop,omitempty"`
	ASPath           string   `json:"as_path"`
	Origin           string   `json:"origin,omitempty"`
	MED              *uint32  `json:"med,omitempty"`
	LocalPref        *uint32  `json:"local_pref,omitempty"`
	Communities      []string `json:"communities,omitempty"`
	LargeCommunities []string `json:"large_communities,omitempty"`
	ExtCommunities   []string `json:"ext_communities,omitempty"`
	AtomicAggregate  bool     `json:"atomic_aggregate,omitempty"`
	Other            []string `json:"other_attributes,omitempty"`
	asns             []uint32 // every AS in the path (loop detection)
}

func (p *pathInfo) hasAS(as uint32) bool {
	for _, a := range p.asns {
		if a == as {
			return true
		}
	}
	return false
}

var originNames = map[uint8]string{gb.BGP_ORIGIN_ATTR_TYPE_IGP: "igp", gb.BGP_ORIGIN_ATTR_TYPE_EGP: "egp", gb.BGP_ORIGIN_ATTR_TYPE_INCOMPLETE: "incomplete"}

func communityName(c uint32) string {
	for n, v := range wellKnown {
		if v == c {
			return n
		}
	}
	return fmt.Sprintf("%d:%d", c>>16, c&0xffff)
}

// summarize decodes an UPDATE's attributes. MP_REACH's next hop is taken
// by the caller (it is per family).
func summarize(attrs []gb.PathAttributeInterface) *pathInfo {
	p := &pathInfo{}
	for _, a := range attrs {
		switch x := a.(type) {
		case *gb.PathAttributeOrigin:
			p.Origin = originNames[x.Value]
		case *gb.PathAttributeAsPath:
			var as []string
			for _, seg := range x.Value {
				as = append(as, seg.String())
				switch s := seg.(type) {
				case *gb.As4PathParam:
					p.asns = append(p.asns, s.AS...)
				case *gb.AsPathParam:
					for _, a := range s.AS {
						p.asns = append(p.asns, uint32(a))
					}
				}
			}
			p.ASPath = strings.Join(as, " ")
		case *gb.PathAttributeNextHop:
			p.NextHop = x.Value.String()
		case *gb.PathAttributeMultiExitDisc:
			v := x.Value
			p.MED = &v
		case *gb.PathAttributeLocalPref:
			v := x.Value
			p.LocalPref = &v
		case *gb.PathAttributeAtomicAggregate:
			p.AtomicAggregate = true
		case *gb.PathAttributeCommunities:
			for _, c := range x.Value {
				p.Communities = append(p.Communities, communityName(c))
			}
		case *gb.PathAttributeLargeCommunities:
			for _, c := range x.Values {
				p.LargeCommunities = append(p.LargeCommunities, fmt.Sprintf("%d:%d:%d", c.ASN, c.LocalData1, c.LocalData2))
			}
		case *gb.PathAttributeExtendedCommunities:
			for _, c := range x.Value {
				p.ExtCommunities = append(p.ExtCommunities, extText(c))
			}
		case *gb.PathAttributeMpReachNLRI, *gb.PathAttributeMpUnreachNLRI:
		default:
			p.Other = append(p.Other, a.GetType().String())
		}
	}
	sort.Strings(p.Other)
	return p
}

// withNextHop is p with another next hop (MP_REACH), sharing the rest.
func (p *pathInfo) withNextHop(nh string) *pathInfo {
	c := *p
	c.NextHop = nh
	return &c
}

// extText names an extended community; Layer2 Info (any encapsulation, as
// VPWS uses) is decoded here.
func extText(c gb.ExtendedCommunityInterface) string {
	if t, st := c.GetTypes(); t == gb.EC_TYPE_GENERIC_TRANSITIVE_EXPERIMENTAL && st == gb.EC_SUBTYPE_L2_INFO {
		if b, err := c.Serialize(); err == nil && len(b) == 8 {
			return (&l2InfoExtended{encap: b[2], flags: b[3], mtu: uint16(b[4])<<8 | uint16(b[5])}).String()
		}
	}
	return c.String()
}
