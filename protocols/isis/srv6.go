package isis

import (
	"fmt"
	"net/netip"
	"sort"
)

// Origination of the SR extensions: flex-algo participation and
// definitions, link attributes, mapping-server bindings and SRv6 locators
// with End / End.X SIDs, for us and for simulated routers.

// flexAlgo reports whether flex-algos are configured (link attributes
// are only advertised then).
func (s *session) flexAlgo() bool { return len(s.cfg.FlexAlgos) > 0 }

// usesTE reports whether a flex-algo uses TE metrics (links then default
// their TE metric to the IGP metric).
func (s *session) usesTE() bool {
	for _, f := range s.cfg.FlexAlgos {
		if f.MetricType == "te" {
			return true
		}
	}
	return false
}

// circuitAttrs are a circuit's TE attributes for its adjacency.
func (s *session) circuitAttrs(c *circuit) *LinkAttrs {
	if !s.flexAlgo() {
		return nil
	}
	a := &LinkAttrs{TEMetric: c.teMetric, DelayUS: c.delayUS, AdminGroups: c.groups}
	if a.TEMetric == 0 && s.usesTE() {
		a.TEMetric = c.metric
	}
	return a
}

// simAttrs are a simulated link's attributes (nil without flex-algo).
func (s *session) simAttrs(a, b string, def uint32) *LinkAttrs {
	if !s.flexAlgo() {
		return nil
	}
	la := s.topo.attrs(a, b, def)
	if !s.usesTE() && s.topo.linkAttr[linkKey(a, b)].te == 0 {
		la.TEMetric = 0
	}
	return la
}

func (s *session) algorithms() []byte {
	var out []byte
	for _, f := range s.cfg.FlexAlgos {
		out = append(out, f.Algorithm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *session) fads() []FAD {
	var out []FAD
	for _, f := range s.cfg.FlexAlgos {
		if f.ParticipateOnly {
			continue
		}
		prio := f.Priority
		if prio == 0 {
			prio = 128
		}
		out = append(out, FAD{Algo: f.Algorithm, MetricType: fadMetricTypes[f.MetricType], Priority: prio,
			ExclAny: groupMask(f.ExcludeAny), InclAny: groupMask(f.IncludeAny), InclAll: groupMask(f.IncludeAll)})
	}
	return out
}

// bindings are our mapping-server entries.
func (s *session) bindings() []Binding {
	var out []Binding
	for _, m := range s.cfg.SegmentRouting.Mappings {
		p := netip.MustParsePrefix(m.Prefix).Masked()
		b := Binding{Prefix: p, Range: max(m.Range, 1), Index: m.Index, Algo: m.Algorithm, Attached: m.Attached}
		if p.Addr().Is6() && s.hasMT(MTIPv6Unicast) {
			b.MT = MTIPv6Unicast
		}
		out = append(out, b)
	}
	return out
}

func (s *session) hasMT(mt uint16) bool {
	for _, m := range s.mts {
		if m == mt {
			return true
		}
	}
	return false
}

// ---- SRv6 ----

func (s *session) srv6Behavior() uint16 {
	if b, ok := srv6Behaviors[s.cfg.SRv6.Behavior]; ok {
		return b
	}
	return srv6Behaviors["end"]
}

func (s *session) usid() bool { b := s.srv6Behavior(); return b >= 43 && b <= 46 }

func (s *session) sidStructure(loc netip.Prefix) SIDStructure {
	if st := s.cfg.SRv6.Structure; st != nil {
		return SIDStructure{LB: st.LB, LN: st.LN, Fun: st.Fun, Arg: st.Arg}
	}
	b := byte(loc.Bits())
	ln := byte(16)
	if b < 16 {
		ln = b
	}
	return SIDStructure{LB: b - ln, LN: ln, Fun: 16}
}

// sidAt is the SID with function fn right after the locator.
func sidAt(loc netip.Prefix, fn uint16) netip.Addr {
	a := loc.Masked().Addr().As16()
	off := loc.Bits()
	for i := 0; i < 16; i++ {
		if fn&(1<<(15-i)) != 0 {
			bit := off + i
			a[bit/8] |= 0x80 >> (bit % 8)
		}
	}
	return netip.AddrFrom16(a)
}

// endSID is a locator's End SID: the locator itself for uSID (uN), else
// function 1.
func (s *session) endSID(loc netip.Prefix, cfg string) netip.Addr {
	if cfg != "" {
		return netip.MustParseAddr(cfg)
	}
	if s.usid() {
		return loc.Masked().Addr()
	}
	return sidAt(loc, 1)
}

// locatorMT is the topology SRv6 locators go in.
func (s *session) locatorMT() uint16 {
	if s.hasMT(MTIPv6Unicast) {
		return MTIPv6Unicast
	}
	return 0
}

// ourLocators are our configured locators.
func (s *session) ourLocators() []Locator {
	if !s.cfg.SRv6.Enabled {
		return nil
	}
	var out []Locator
	for _, l := range s.cfg.SRv6.Locators {
		p := netip.MustParsePrefix(l.Prefix).Masked()
		out = append(out, Locator{Prefix: p, Metric: l.Metric, Algo: l.Algorithm, MT: s.locatorMT(),
			EndSIDs: []EndSID{{SID: s.endSID(p, l.EndSID), Behavior: s.srv6Behavior(), Struct: s.sidStructure(p)}}})
	}
	return out
}

// endXLocator is the locator End.X SIDs come from (the first algorithm-0
// one) for us (owner "") or a simulated router.
func (s *session) endXLocator(owner string) (netip.Prefix, bool) {
	if !s.cfg.SRv6.Enabled || !s.cfg.SRv6.EndX {
		return netip.Prefix{}, false
	}
	if owner != "" {
		p, ok := s.topo.locators[owner]
		return p, ok
	}
	for _, l := range s.cfg.SRv6.Locators {
		if l.Algorithm == 0 {
			return netip.MustParsePrefix(l.Prefix).Masked(), true
		}
	}
	return netip.Prefix{}, false
}

// endXFunc allocates a stable End.X function for an adjacency of owner.
func (s *session) endXFunc(owner, key string) uint16 {
	if s.endXFuncs == nil {
		s.endXFuncs = map[string]map[string]uint16{}
	}
	m := s.endXFuncs[owner]
	if m == nil {
		m = map[string]uint16{}
		s.endXFuncs[owner] = m
	}
	if f, ok := m[key]; ok {
		return f
	}
	f := uint16(0xe000 + len(m))
	m[key] = f
	return f
}

// endXDecorate adds End.X (LAN End.X on pseudonode entries) SIDs to the
// neighbor list that carries IPv6: MT 2 with multi-topology, else MT 0.
func (s *session) endXDecorate(owner string, body *lspBody, lanNbrs map[NodeID][]SysID, st *srState) {
	loc, ok := s.endXLocator(owner)
	if !ok {
		return
	}
	beh, str := endXFor(s.srv6Behavior()), s.sidStructure(loc)
	list := &body.Neighbors
	if mt := s.locatorMT(); mt != 0 {
		l := body.MTNeighbors[mt]
		list = &l
		defer func() { body.MTNeighbors[mt] = l }()
	}
	out := make([]ISReach, len(*list))
	dup := map[NodeID]int{}
	for i, n := range *list {
		n.EndX = nil
		key := n.Neighbor.String()
		if k := dup[n.Neighbor]; k > 0 {
			key += fmt.Sprintf("#%d", k)
		}
		dup[n.Neighbor]++
		if nbrs, ok := lanNbrs[n.Neighbor]; ok && n.Neighbor[6] != 0 {
			for _, nb := range nbrs {
				nb := nb
				sid := sidAt(loc, s.endXFunc(owner, "lan/"+key+"/"+nb.String()))
				n.EndX = append(n.EndX, EndXSID{SID: sid, Behavior: beh, Struct: str, Neighbor: &nb})
				if st != nil {
					st.SRv6SIDs = append(st.SRv6SIDs, SRv6SIDStatus{SID: sid.String(), Behavior: behaviorName(beh), Neighbor: nb.String(), LAN: true})
				}
			}
		} else {
			sid := sidAt(loc, s.endXFunc(owner, key))
			n.EndX = append(n.EndX, EndXSID{SID: sid, Behavior: beh, Struct: str})
			if st != nil {
				st.SRv6SIDs = append(st.SRv6SIDs, SRv6SIDStatus{SID: sid.String(), Behavior: behaviorName(beh), Neighbor: n.Neighbor.String()})
			}
		}
		out[i] = n
	}
	*list = out
}

// SRv6SIDStatus is one SID we advertise.
type SRv6SIDStatus struct {
	SID       string `json:"sid"`
	Behavior  string `json:"behavior"`
	Locator   string `json:"locator,omitempty"`   // End SIDs
	Algorithm byte   `json:"algorithm,omitempty"` // End SIDs
	Neighbor  string `json:"neighbor,omitempty"`  // End.X
	LAN       bool   `json:"lan,omitempty"`
}

// locatorPrefixes advertises algorithm-0 locators as IPv6 prefixes too
// (for routers without SRv6) and records our End SIDs.
func locatorPrefixes(locs []Locator, pfx []IPReach, st *srState) []IPReach {
	for _, l := range locs {
		if st != nil {
			for _, e := range l.EndSIDs {
				st.SRv6SIDs = append(st.SRv6SIDs, SRv6SIDStatus{SID: e.SID.String(), Behavior: behaviorName(e.Behavior), Locator: l.Prefix.String(), Algorithm: l.Algo})
			}
		}
		if l.Algo != 0 {
			continue
		}
		found := false
		for _, p := range pfx {
			if p.Prefix == l.Prefix {
				found = true
			}
		}
		if !found {
			pfx = append(pfx, IPReach{Prefix: l.Prefix, Metric: l.Metric})
		}
	}
	return pfx
}
