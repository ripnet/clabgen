package isis

import (
	"bytes"
	"container/heap"
	"net/netip"
	"sort"
	"time"
)

// SPF over a level's LSDB, for what an L1L2 router derives from it
// (clabgen computes no RIB): the prefixes level 1 reaches, which it
// advertises into level 2 (RFC 1195 3.2, summarised as configured); the
// level-2 prefixes it leaks into level 1 (RFC 5302, with the up/down bit);
// and whether level 2 reaches another area (the attached bit, ISO 10589
// 7.2.9.2). Level-1 routers note the attached L1L2 routers they reach.

// maxPathMetric is the largest usable wide metric (RFC 5305 4).
const maxPathMetric = 0xfe000000

// spfHold is the least time between SPF runs of a level.
const spfHold = time.Second

// lspNode is one node's LSP fragments, decoded.
type lspNode struct {
	frags []*LSPInfo
	zero  *LSPInfo // fragment 0
}

// route is a prefix reached through SPF.
type route struct {
	metric uint32
	down   bool
	sids   []PrefixSID
	from   SysID
}

// reach is what one SPF run found.
type reach struct {
	routes   map[netip.Prefix]route
	attached bool    // L2: another area is reachable
	attRtrs  []SysID // L1: reachable L1L2 routers setting the attached bit
}

// nodesLocked groups a level's live LSPs by node.
func nodesLocked(l *level, now time.Time) map[NodeID]*lspNode {
	out := map[NodeID]*lspNode{}
	for id, e := range l.db {
		if !e.purgeAt.IsZero() || e.remaining(now) == 0 {
			continue
		}
		n := out[id.Node()]
		if n == nil {
			n = &lspNode{}
			out[id.Node()] = n
		}
		in := e.infoLocked()
		n.frags = append(n.frags, in)
		if id[7] == 0 {
			n.zero = in
		}
	}
	return out
}

// edges are a node's IS neighbors in topology mt, lowest metric per neighbor.
func (n *lspNode) edges(mt uint16) map[NodeID]uint32 {
	out := map[NodeID]uint32{}
	for _, f := range n.frags {
		for _, x := range f.Neighbors {
			if x.MT != mt {
				continue
			}
			if m, ok := out[x.id]; !ok || x.Metric < m {
				out[x.id] = x.Metric
			}
		}
	}
	return out
}

type spfItem struct {
	id   NodeID
	dist uint32
}
type spfHeap []spfItem

func (h spfHeap) Len() int { return len(h) }
func (h spfHeap) Less(i, j int) bool {
	if h[i].dist != h[j].dist {
		return h[i].dist < h[j].dist
	}
	return bytes.Compare(h[i].id[:], h[j].id[:]) < 0
}
func (h spfHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *spfHeap) Push(x any)   { *h = append(*h, x.(spfItem)) }
func (h *spfHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// spf returns the distance to every node reachable from root in topology
// mt. A link counts only when both ends list each other (two-way check);
// overloaded routers aren't transited.
func spf(nodes map[NodeID]*lspNode, root NodeID, mt uint16) map[NodeID]uint32 {
	dist := map[NodeID]uint32{}
	if nodes[root] == nil || nodes[root].zero == nil {
		return dist
	}
	edges := map[NodeID]map[NodeID]uint32{}
	edgesOf := func(id NodeID) map[NodeID]uint32 {
		if e, ok := edges[id]; ok {
			return e
		}
		var e map[NodeID]uint32
		if n := nodes[id]; n != nil && n.zero != nil {
			e = n.edges(mt)
		}
		edges[id] = e
		return e
	}
	h := &spfHeap{{root, 0}}
	for h.Len() > 0 {
		it := heap.Pop(h).(spfItem)
		if _, done := dist[it.id]; done {
			continue
		}
		dist[it.id] = it.dist
		if it.id != root && nodes[it.id].zero.Overload && it.id[6] == 0 {
			continue // reachable, but no transit
		}
		for nb, m := range edgesOf(it.id) {
			if _, done := dist[nb]; done {
				continue
			}
			back := edgesOf(nb)
			if _, ok := back[it.id]; !ok {
				continue
			}
			d := uint64(it.dist) + uint64(m)
			if d > maxPathMetric {
				continue
			}
			heap.Push(h, spfItem{nb, uint32(d)})
		}
	}
	return dist
}

// reachLocked runs SPF on level l: the prefixes of systems we don't
// originate, the attached state (L2) and the attached routers (L1).
func (s *session) reachLocked(l *level, now time.Time) *reach {
	nodes := nodesLocked(l, now)
	r := &reach{routes: map[netip.Prefix]route{}}
	root := s.nodeID()
	topos := []uint16{MTIPv4Unicast}
	v6mt := false
	for _, m := range s.mts {
		if m == MTIPv6Unicast {
			topos, v6mt = append(topos, MTIPv6Unicast), true
		}
	}
	ours := map[string]bool{}
	for _, a := range s.cfg.Areas {
		b, _ := ParseArea(a)
		ours[AreaString(b)] = true
	}
	for _, mt := range topos {
		dist := spf(nodes, root, mt)
		ids := make([]NodeID, 0, len(dist))
		for id := range dist {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
		for _, id := range ids {
			d := dist[id]
			if id[6] != 0 || s.originates(id.Sys()) {
				continue
			}
			n := nodes[id]
			if mt == MTIPv4Unicast && l.n == 2 && !r.attached {
				for _, a := range n.zero.Areas {
					if !ours[a] {
						r.attached = true
					}
				}
			}
			if mt == MTIPv4Unicast && l.n == 1 && n.zero.Attached {
				r.attRtrs = append(r.attRtrs, id.Sys())
			}
			for _, f := range n.frags {
				for _, p := range f.Prefixes {
					if !p.pfx.IsValid() {
						continue
					}
					// MT 0 carries IPv4 and, without MT 2, IPv6; MT 2 IPv6.
					if mt == MTIPv4Unicast && (p.MT != 0 || v6mt && p.pfx.Addr().Is6()) || mt == MTIPv6Unicast && p.MT != MTIPv6Unicast {
						continue
					}
					c := uint64(d) + uint64(p.Metric)
					if c > maxPathMetric {
						continue
					}
					old, ok := r.routes[p.pfx]
					// Lower metric wins; at equal metric an up route beats a down one.
					if ok && (old.metric < uint32(c) || old.metric == uint32(c) && (!old.down || p.Down)) {
						continue
					}
					r.routes[p.pfx] = route{metric: uint32(c), down: p.Down, sids: p.sids, from: id.Sys()}
				}
			}
		}
	}
	return r
}

// spfTickLocked reruns SPF on levels whose LSDB changed (at most once per
// spfHold) and marks the other level for re-origination when what it
// derives changed.
func (s *session) spfTickLocked(now time.Time) {
	if s.cfg.Level != "L1L2" && s.cfg.Level != "L1" {
		return
	}
	for _, n := range s.sortedLevels() {
		l := s.lv[n]
		if l.reach != nil && l.spfGen == l.gen || now.Before(l.nextSPF) {
			continue
		}
		prev := l.reach
		l.reach, l.spfGen, l.nextSPF = s.reachLocked(l, now), l.gen, now.Add(spfHold)
		s.count("spf_runs")
		if s.cfg.Level != "L1L2" {
			continue
		}
		other := s.lv[3-n]
		if prev == nil || !sameReach(prev, l.reach) {
			other.dirty = true
		}
	}
}

func sameReach(a, b *reach) bool {
	if a.attached != b.attached || len(a.routes) != len(b.routes) {
		return false
	}
	for p, x := range a.routes {
		y, ok := b.routes[p]
		if !ok || x.metric != y.metric || x.down != y.down || len(x.sids) != len(y.sids) {
			return false
		}
		for i := range x.sids {
			if x.sids[i] != y.sids[i] {
				return false
			}
		}
	}
	return true
}

// interLevelLocked returns what an L1L2 router adds to its level-n LSP from
// the other level: level-1 prefixes (summarised) into level 2, leaked
// level-2 prefixes (down bit) into level 1, with their Prefix-SIDs
// re-advertised (R and P set, RFC 8667 2.1.1). have are the prefixes the
// LSP already carries.
func (s *session) interLevelLocked(n int, have map[netip.Prefix]bool) ([]IPReach, map[netip.Prefix][]PrefixSID) {
	if s.cfg.Level != "L1L2" {
		return nil, nil
	}
	src := s.lv[3-n].reach
	if src == nil {
		return nil, nil
	}
	sids := map[netip.Prefix][]PrefixSID{}
	var out []IPReach
	add := func(p netip.Prefix, r route, down bool) {
		out = append(out, IPReach{Prefix: p, Metric: r.metric, Down: down})
		for _, x := range r.sids {
			x.Flags |= PSIDReadvertised | PSIDNoPHP
			sids[p] = append(sids[p], x)
		}
	}
	keys := make([]netip.Prefix, 0, len(src.routes))
	for p := range src.routes {
		if !have[p] {
			keys = append(keys, p)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Addr() != keys[j].Addr() {
			return keys[i].Addr().Less(keys[j].Addr())
		}
		return keys[i].Bits() < keys[j].Bits()
	})
	if n == 2 {
		// Level 1 into level 2: everything level 1 reaches (but what was
		// leaked into it), summaries in place of what they cover.
		type sum struct {
			p      netip.Prefix
			metric uint32
			fixed  bool
			used   bool
		}
		var sums []*sum
		for _, x := range s.cfg.Summaries {
			p := netip.MustParsePrefix(x.Prefix).Masked()
			sums = append(sums, &sum{p: p, metric: x.Metric, fixed: x.Metric != 0})
		}
		for _, p := range keys {
			r := src.routes[p]
			if r.down {
				continue
			}
			covered := false
			for _, sm := range sums {
				if sm.p.Bits() <= p.Bits() && sm.p.Contains(p.Addr()) {
					covered = true
					if !sm.fixed && (!sm.used || r.metric < sm.metric) {
						sm.metric = r.metric
					}
					sm.used = true
					break
				}
			}
			if !covered {
				add(p, r, false)
			}
		}
		for _, sm := range sums {
			if sm.used && !have[sm.p] {
				out = append(out, IPReach{Prefix: sm.p, Metric: sm.metric})
			}
		}
		return out, sids
	}
	// Level 2 into level 1: what the leak ranges cover, unless level 1
	// reaches it itself.
	l1 := s.lv[1].reach
	for _, p := range keys {
		if l1 != nil {
			if _, ok := l1.routes[p]; ok {
				continue
			}
		}
		for _, x := range s.cfg.LeakToL1 {
			rg := netip.MustParsePrefix(x).Masked()
			if rg.Addr().Is4() == p.Addr().Is4() && rg.Bits() <= p.Bits() && rg.Contains(p.Addr()) {
				add(p, src.routes[p], true)
				break
			}
		}
	}
	return out, sids
}

// attachedLocked reports whether our level-1 LSPs set the attached bit.
func (s *session) attachedLocked() bool {
	if s.cfg.Level != "L1L2" {
		return false
	}
	switch s.cfg.AttachedBit {
	case "always":
		return true
	case "never":
		return false
	}
	r := s.lv[2].reach
	return r != nil && r.attached
}
