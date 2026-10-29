package ospf

import (
	"encoding/json"
	"net/netip"
	"strings"
	"time"
)

// Config changes that only alter what we advertise apply in place: our
// LSAs (and the simulated routers') are regenerated, and the ones we no
// longer want are flushed, while adjacencies stay up. What adjacencies
// depend on (router ID, areas, interfaces, network types, timers,
// authentication, BFD, graceful restart, virtual links) restarts the
// instance.

func (s *session) Reconfigure(c any) bool {
	var nc Config
	switch x := c.(type) {
	case *Config:
		nc = *x
	case *ConfigV3:
		if x.InstanceID != s.instance {
			return false
		}
		nc = x.common()
	default:
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oc := s.cfg
	if adjacencyKey(oc) != adjacencyKey(nc) {
		return false
	}
	// The router ID can come from the first loopback (OSPFv2).
	if nc.RouterID == "" && !s.cd.v3 && firstLoopback(oc, false) != firstLoopback(nc, false) {
		return false
	}
	var notes []string
	changed := func(name string, a, b any) bool {
		if same(a, b) {
			return false
		}
		notes = append(notes, name)
		return true
	}

	if changed("simulated topology", oc.Topology, nc.Topology) {
		t, err := s.newTopology(nc)
		if err != nil {
			return false
		}
		s.topo = t // routers that are gone are flushed with the next pass
	}
	if changed("loopbacks", oc.Loopbacks, nc.Loopbacks) {
		s.loopbacks = nil
		for _, l := range nc.Loopbacks {
			p, _ := parseLoopback(l, s.cd.v3)
			s.loopbacks = append(s.loopbacks, p)
		}
		for _, f := range s.ifs {
			if f.vl == nil && f.attached() {
				s.releaseLocalsLocked(f)
				s.addLocalsLocked(f)
			}
		}
	}
	if changed("prefixes", oc.Prefixes, nc.Prefixes) {
		// Prefixes of entries that are gone go, unless a new entry has
		// them; prefixes added with the advertise action stay.
		want := map[netip.Prefix]route{}
		for _, p := range nc.Prefixes {
			rs, _ := p.expand(s.cd.v3)
			for _, r := range rs {
				want[r.prefix] = r
			}
		}
		for _, p := range oc.Prefixes {
			rs, _ := p.expand(s.cd.v3)
			for _, r := range rs {
				if _, ok := want[r.prefix]; !ok {
					delete(s.routes, r.prefix)
				}
			}
		}
		for k, r := range want {
			s.routes[k] = r
		}
	}
	if changed("max-metric", oc.MaxMetric, nc.MaxMetric) {
		s.maxMetric = nc.MaxMetric
	}
	// Costs per interface (an interface without one takes the router's);
	// TE attributes are read from the config when LSAs are built.
	costs := func(c Config) []uint16 {
		out := []uint16{c.Cost}
		for _, ic := range c.Interfaces {
			out = append(out, ic.Cost)
		}
		return out
	}
	if changed("costs", costs(oc), costs(nc)) {
		cs := costs(nc)
		for i, f := range s.ifs {
			if f.vl != nil || i >= len(cs) {
				continue // virtual links take their cost from the transit area
			}
			f.cost = cs[i]
			if f.cost == 0 {
				f.cost = nc.Cost
			}
		}
	}
	te := func(c Config) []LinkAttrs {
		out := []LinkAttrs{c.LinkAttrs}
		for _, ic := range c.Interfaces {
			out = append(out, ic.LinkAttrs)
		}
		return out
	}
	changed("TE attributes", te(oc), te(nc))
	if changed("segment routing", []any{oc.SegmentRouting, oc.FlexAlgos, oc.TrafficEngineering}, []any{nc.SegmentRouting, nc.FlexAlgos, nc.TrafficEngineering}) {
		s.adjLabels = map[string]uint32{} // allocated again from the new base
	}
	if changed("LSA refresh", oc.LSARefresh, nc.LSARefresh) {
		s.refresh = time.Duration(nc.LSARefresh) * time.Second
	}

	// The fields one by one: other goroutines read the rest unlocked.
	s.cfg.Topology, s.cfg.Loopbacks, s.cfg.Prefixes, s.cfg.MaxMetric = nc.Topology, nc.Loopbacks, nc.Prefixes, nc.MaxMetric
	s.cfg.Cost, s.cfg.LinkAttrs, s.cfg.Interfaces = nc.Cost, nc.LinkAttrs, nc.Interfaces
	s.cfg.SegmentRouting, s.cfg.FlexAlgos, s.cfg.TrafficEngineering, s.cfg.LSARefresh = nc.SegmentRouting, nc.FlexAlgos, nc.TrafficEngineering, nc.LSARefresh
	if len(notes) > 0 {
		s.dirty = true
		s.env.Emit("config-changed", "OSPF config applied, adjacencies kept: "+strings.Join(notes, ", "), nil)
	}
	return true
}

// adjacencyKey is the config without what Reconfigure applies in place:
// two configs with the same key differ only in what we advertise. The
// simulated topology's area stays in it (it is one of our areas).
func adjacencyKey(c Config) string {
	area := c.Topology.Area
	c.Topology = Topology{Area: area}
	c.Loopbacks, c.Prefixes, c.MaxMetric = nil, nil, false
	c.Cost, c.LinkAttrs = 0, LinkAttrs{}
	c.SegmentRouting, c.FlexAlgos, c.TrafficEngineering, c.LSARefresh = SegmentRouting{}, nil, false, 0
	ifs := make([]Interface, len(c.Interfaces))
	for i, ic := range c.Interfaces {
		ic.Cost, ic.LinkAttrs = 0, LinkAttrs{}
		ifs[i] = ic
	}
	c.Interfaces = ifs
	b, _ := json.Marshal(c)
	return string(b)
}

func firstLoopback(c Config, v6 bool) netip.Prefix {
	if len(c.Loopbacks) == 0 {
		return netip.Prefix{}
	}
	p, _ := parseLoopback(c.Loopbacks[0], v6)
	return p
}

func same(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
