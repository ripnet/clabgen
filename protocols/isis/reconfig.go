package isis

import (
	"encoding/json"
	"net/netip"
	"strings"
	"time"
)

// Config changes that only alter what we advertise apply in place: our
// LSPs (and the simulated routers') are regenerated and flooded while the
// adjacencies stay up. What adjacencies depend on (system ID, areas,
// levels, circuits, hellos, authentication, BFD, graceful restart,
// topologies, the interface list) restarts the instance.

func (s *session) Reconfigure(c any) bool {
	nc := *c.(*Config)
	s.mu.Lock()
	defer s.mu.Unlock()
	oc := s.cfg
	if adjacencyKey(oc) != adjacencyKey(nc) {
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
	now := time.Now()

	if changed("simulated topology", []any{oc.Topology, oc.SRv6.TopologyLocatorBase}, []any{nc.Topology, nc.SRv6.TopologyLocatorBase}) {
		t, err := newTopology(nc, &s.sys)
		if err != nil {
			return false
		}
		// Routers that are gone withdraw their LSPs.
		keep := map[SysID]bool{}
		if t != nil {
			for _, n := range t.nodes {
				keep[n.sys] = true
			}
		}
		if s.topo != nil {
			for _, n := range s.topo.nodes {
				if !keep[n.sys] {
					for _, l := range s.lv {
						s.withdrawSys(l, n.sys, s.isType(l.n), now)
					}
				}
			}
		}
		s.topo = t
	}
	if changed("loopbacks", oc.Loopbacks, nc.Loopbacks) {
		s.loopbacks = nil
		for _, l := range nc.Loopbacks {
			p, _ := parseLoopback(l)
			s.loopbacks = append(s.loopbacks, p)
		}
		for _, c := range s.circs {
			if c.attached() {
				s.releaseLocalsLocked(c)
				s.addLocalsLocked(c)
			}
		}
	}
	if changed("prefixes", oc.Prefixes, nc.Prefixes) {
		// Prefixes of entries that are gone go, unless a new entry has
		// them; prefixes added with the advertise action stay.
		want := map[netip.Prefix]IPReach{}
		for _, p := range nc.Prefixes {
			ps, _ := p.expand()
			for _, r := range ps {
				want[r.Prefix] = r
			}
		}
		for _, p := range oc.Prefixes {
			ps, _ := p.expand()
			for _, r := range ps {
				if _, ok := want[r.Prefix]; !ok {
					delete(s.prefixes, r.Prefix)
				}
			}
		}
		for k, r := range want {
			s.prefixes[k] = r
		}
	}
	if changed("hostname", oc.Hostname, nc.Hostname) {
		s.host = nc.Hostname
		if s.host == "" {
			s.host = s.env.InstanceName()
		}
	}
	if changed("overload", oc.Overload, nc.Overload) {
		s.overload = nc.Overload
	}
	// Metrics and TE attributes, per circuit (an interface without a
	// metric takes the instance's).
	ifs := append([]Interface{{Metric: nc.Metric, TEMetric: nc.TEMetric, DelayUS: nc.DelayUS, AdminGroups: nc.AdminGroups}}, nc.Interfaces...)
	old := append([]Interface{{Metric: oc.Metric, TEMetric: oc.TEMetric, DelayUS: oc.DelayUS, AdminGroups: oc.AdminGroups}}, oc.Interfaces...)
	if changed("metrics", old, ifs) {
		for i, c := range s.circs {
			ic := ifs[i]
			c.metric, c.teMetric, c.delayUS, c.groups = ic.Metric, ic.TEMetric, ic.DelayUS, groupMask(ic.AdminGroups)
			if c.metric == 0 {
				c.metric = nc.Metric
			}
		}
	}
	if changed("segment routing", []any{oc.SegmentRouting, oc.FlexAlgos, oc.SRv6}, []any{nc.SegmentRouting, nc.FlexAlgos, nc.SRv6}) {
		// Labels and End.X functions are allocated again from the new bases.
		s.adjLabels, s.endXFuncs = nil, nil
	}
	changed("summaries and leaking", []any{oc.AttachedBit, oc.Summaries, oc.LeakToL1}, []any{nc.AttachedBit, nc.Summaries, nc.LeakToL1})
	changed("LSP timers", []any{oc.LSPRefresh, oc.LSPLifetime, oc.LSPMTU, oc.CSNPInterval}, []any{nc.LSPRefresh, nc.LSPLifetime, nc.LSPMTU, nc.CSNPInterval})

	// The fields one by one: other goroutines read the rest unlocked.
	s.cfg.Hostname, s.cfg.Overload, s.cfg.Metric, s.cfg.TEMetric, s.cfg.DelayUS, s.cfg.AdminGroups = nc.Hostname, nc.Overload, nc.Metric, nc.TEMetric, nc.DelayUS, nc.AdminGroups
	s.cfg.Interfaces, s.cfg.Prefixes, s.cfg.Loopbacks, s.cfg.Topology = nc.Interfaces, nc.Prefixes, nc.Loopbacks, nc.Topology
	s.cfg.SegmentRouting, s.cfg.FlexAlgos, s.cfg.SRv6 = nc.SegmentRouting, nc.FlexAlgos, nc.SRv6
	s.cfg.AttachedBit, s.cfg.Summaries, s.cfg.LeakToL1 = nc.AttachedBit, nc.Summaries, nc.LeakToL1
	s.cfg.LSPRefresh, s.cfg.LSPLifetime, s.cfg.LSPMTU, s.cfg.CSNPInterval = nc.LSPRefresh, nc.LSPLifetime, nc.LSPMTU, nc.CSNPInterval
	if len(notes) > 0 {
		s.dirtyLocked()
		s.event("config-changed", "IS-IS config applied, adjacencies kept: "+strings.Join(notes, ", "))
	}
	return true
}

// adjacencyKey is the config without what Reconfigure applies in place:
// two configs with the same key differ only in what we advertise.
func adjacencyKey(c Config) string {
	c.Hostname, c.Overload, c.Metric, c.TEMetric, c.DelayUS, c.AdminGroups = "", false, 0, 0, 0, nil
	c.Prefixes, c.Loopbacks, c.Topology = nil, nil, Topology{}
	c.SegmentRouting, c.FlexAlgos, c.SRv6 = SegmentRouting{}, nil, SRv6{}
	c.AttachedBit, c.Summaries, c.LeakToL1 = "", nil, nil
	c.LSPRefresh, c.LSPLifetime, c.LSPMTU, c.CSNPInterval = 0, 0, 0, 0
	ifs := make([]Interface, len(c.Interfaces))
	for i, ic := range c.Interfaces {
		ic.Metric, ic.TEMetric, ic.DelayUS, ic.AdminGroups = 0, 0, 0, nil
		ifs[i] = ic
	}
	c.Interfaces = ifs
	b, _ := json.Marshal(c)
	return string(b)
}

func same(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
