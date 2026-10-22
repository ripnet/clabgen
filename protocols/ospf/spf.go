package ospf

import (
	"container/heap"
	"net/netip"
	"time"
)

// Intra-area shortest paths (RFC 2328 16.1, RFC 5340 4.8.1), run by area
// border routers over each area's LSDB to know what to summarise into the
// other areas, which border routers and ASBRs are reachable, and the
// backbone's inter-area routes (16.2). No routes are installed.

type spfResult struct {
	dist  map[netip.Addr]uint32 // reachable routers
	flags map[netip.Addr]uint8  // their router LSA flags
	intra map[netip.Prefix]uint32
	asbrs map[netip.Addr]uint32 // reachable ASBRs (E bit)
	// From summaries of other reachable border routers (used from the
	// backbone only).
	inter     map[netip.Prefix]uint32
	interASBR map[netip.Addr]uint32
	// First hops: the neighbor router through which each router is reached
	// (for virtual links), and the address the router's interface toward
	// the path has (v2: from its router LSA; v3: unknown here).
	firstHop map[netip.Addr]netip.Addr
	addr     map[netip.Addr]netip.Addr
}

// vertex is a router (net false; id its router ID) or a transit network
// (v2: id is the DR's interface address; v3: id the DR's router ID and
// ifID its interface ID).
type vertex struct {
	net  bool
	id   netip.Addr
	ifID uint32
}

type pqItem struct {
	v    vertex
	d    uint32
	from vertex
}

type pq []pqItem

func (q pq) Len() int { return len(q) }
func (q pq) Less(i, j int) bool {
	return q[i].d < q[j].d || q[i].d == q[j].d && q[i].v.net && !q[j].v.net
}
func (q pq) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *pq) Push(x any)   { *q = append(*q, x.(pqItem)) }
func (q *pq) Pop() any     { o := *q; x := o[len(o)-1]; *q = o[:len(o)-1]; return x }
func setMin[K comparable](m map[K]uint32, k K, c uint32) {
	if old, ok := m[k]; !ok || c < old {
		m[k] = c
	}
}

func newSPF() *spfResult {
	return &spfResult{dist: map[netip.Addr]uint32{}, flags: map[netip.Addr]uint8{}, intra: map[netip.Prefix]uint32{},
		asbrs: map[netip.Addr]uint32{}, inter: map[netip.Prefix]uint32{}, interASBR: map[netip.Addr]uint32{},
		firstHop: map[netip.Addr]netip.Addr{}, addr: map[netip.Addr]netip.Addr{}}
}

// edge is a link of a vertex in the graph.
type edge struct {
	to   vertex
	cost uint32
}

// dijkstra runs over a graph given by its edge function (which also
// checks the links back) from us, filling dist and firstHop. It returns
// the distance and parent of every vertex reached.
func (s *session) dijkstra(r *spfResult, edges func(vertex) []edge) (map[vertex]uint32, map[vertex]vertex) {
	root := vertex{id: s.rid}
	done := map[vertex]uint32{}
	parent := map[vertex]vertex{}
	hop := map[vertex]netip.Addr{} // first-hop router of each vertex
	q := &pq{{v: root}}
	for q.Len() > 0 {
		it := heap.Pop(q).(pqItem)
		if _, ok := done[it.v]; ok {
			continue
		}
		done[it.v] = it.d
		parent[it.v] = it.from
		switch {
		case it.v == root:
		case it.from == root || it.from.net && hop[it.from] == netip.Addr{}:
			// Directly attached (or across a network we're on).
			if !it.v.net {
				hop[it.v] = it.v.id
			}
		default:
			hop[it.v] = hop[it.from]
		}
		if !it.v.net {
			r.dist[it.v.id] = it.d
			if h := hop[it.v]; h.IsValid() {
				r.firstHop[it.v.id] = h
			}
		}
		for _, e := range edges(it.v) {
			if _, ok := done[e.to]; !ok {
				heap.Push(q, pqItem{v: e.to, d: it.d + e.cost, from: it.v})
			}
		}
	}
	return done, parent
}

func (s *session) spfLocked(a *area, now time.Time) *spfResult {
	if s.cd.v3 {
		return s.spf3Locked(a, now)
	}
	r := newSPF()
	routers := map[netip.Addr]*RouterLSA{}
	nets := map[netip.Addr]*NetworkLSA{}
	var sums []*lsa
	for k, l := range s.db {
		if k.Area != a.key() || l.ageAt(now) >= MaxAge {
			continue
		}
		switch k.Type {
		case LSARouter:
			if k.ID == k.AdvRtr {
				if x, err := DecodeRouterLSA(l.body()); err == nil {
					routers[k.AdvRtr] = x
				}
			}
		case LSANetwork:
			if x, err := DecodeNetworkLSA(l.body()); err == nil {
				nets[k.ID] = x
			}
		case LSASummary, LSAASBRSummary:
			sums = append(sums, l)
		}
	}
	linksBack := func(rid, to netip.Addr, typ uint8) bool {
		x := routers[rid]
		if x == nil {
			return false
		}
		for _, l := range x.Links {
			if l.Type == typ && l.ID == to {
				return true
			}
		}
		return false
	}
	attached := func(n *NetworkLSA, rid netip.Addr) bool {
		for _, x := range n.Routers {
			if x == rid {
				return true
			}
		}
		return false
	}
	done, parent := s.dijkstra(r, func(v vertex) []edge {
		var out []edge
		if v.net {
			for _, rid := range nets[v.id].Routers {
				if linksBack(rid, v.id, LinkTransit) {
					out = append(out, edge{vertex{id: rid}, 0})
				}
			}
			return out
		}
		x := routers[v.id]
		if x == nil {
			return nil
		}
		for _, l := range x.Links {
			switch l.Type {
			case LinkP2P, LinkVirtual:
				if linksBack(l.ID, v.id, l.Type) {
					out = append(out, edge{vertex{id: l.ID}, uint32(l.Metric)})
				}
			case LinkTransit:
				if n := nets[l.ID]; n != nil && attached(n, v.id) {
					out = append(out, edge{vertex{net: true, id: l.ID}, uint32(l.Metric)})
				}
			}
		}
		return out
	})
	for v, d := range done {
		if v.net {
			if n := nets[v.id]; n != nil && BitsOf(n.Mask) >= 0 {
				setMin(r.intra, netip.PrefixFrom(v.id, BitsOf(n.Mask)).Masked(), d)
			}
			continue
		}
		x := routers[v.id]
		if x == nil {
			continue
		}
		r.flags[v.id] = x.Flags
		if x.Flags&RouterE != 0 && v.id != s.rid {
			r.asbrs[v.id] = d
		}
		for _, l := range x.Links {
			if l.Type == LinkStub && BitsOf(l.Data) >= 0 {
				setMin(r.intra, netip.PrefixFrom(l.ID, BitsOf(l.Data)).Masked(), d+uint32(l.Metric))
			}
		}
	}
	// Each router's interface address toward the path from us: the link
	// data of its link to its parent (a virtual link's far end).
	for v, p := range parent {
		x := routers[v.id]
		if v.net || x == nil || v.id == s.rid {
			continue
		}
		for _, l := range x.Links {
			if l.ID == p.id && (p.net && l.Type == LinkTransit || !p.net && l.Type == LinkP2P) {
				r.addr[v.id] = l.Data
				break
			}
		}
	}
	for _, l := range sums {
		d, ok := r.dist[l.hdr.AdvRtr]
		if !ok || l.hdr.AdvRtr == s.rid || r.flags[l.hdr.AdvRtr]&RouterB == 0 {
			continue
		}
		x, err := DecodeSummaryLSA(l.body())
		if err != nil || x.Metric >= LSInfinity {
			continue
		}
		if l.hdr.Type == LSASummary {
			if b := BitsOf(x.Mask); b >= 0 {
				setMin(r.inter, netip.PrefixFrom(l.hdr.ID, b).Masked(), d+x.Metric)
			}
		} else {
			setMin(r.interASBR, l.hdr.ID, d+x.Metric)
		}
	}
	return r
}

func (s *session) spf3Locked(a *area, now time.Time) *spfResult {
	r := newSPF()
	type rtr struct {
		flags uint8
		links []Router3Link
		lsid  netip.Addr // lowest LSID seen (its flags count)
	}
	routers := map[netip.Addr]*rtr{}
	nets := map[vertex]*Network3LSA{}
	var prefixLSAs, sums []*lsa
	for k, l := range s.db {
		if k.Area != a.key() || l.ageAt(now) >= MaxAge {
			continue
		}
		switch k.Type {
		case LSA3Router:
			x, err := DecodeRouter3LSA(l.body())
			if err != nil {
				continue
			}
			rt := routers[k.AdvRtr]
			if rt == nil {
				rt = &rtr{lsid: k.ID, flags: x.Flags}
				routers[k.AdvRtr] = rt
			} else if k.ID.Less(rt.lsid) {
				rt.lsid, rt.flags = k.ID, x.Flags
			}
			rt.links = append(rt.links, x.Links...)
		case LSA3Network:
			if x, err := DecodeNetwork3LSA(l.body()); err == nil {
				nets[vertex{net: true, id: k.AdvRtr, ifID: u32(k.ID)}] = x
			}
		case LSA3IntraPrefix:
			prefixLSAs = append(prefixLSAs, l)
		case LSA3InterPrefix, LSA3InterRouter:
			sums = append(sums, l)
		}
	}
	linksBack := func(rid netip.Addr, ok func(Router3Link) bool) bool {
		x := routers[rid]
		if x == nil {
			return false
		}
		for _, l := range x.links {
			if ok(l) {
				return true
			}
		}
		return false
	}
	done, _ := s.dijkstra(r, func(v vertex) []edge {
		var out []edge
		if v.net {
			for _, rid := range nets[v].Routers {
				if linksBack(rid, func(l Router3Link) bool {
					return l.Type == LinkTransit && l.NbrRouter == v.id && l.NbrIfID == v.ifID
				}) {
					out = append(out, edge{vertex{id: rid}, 0})
				}
			}
			return out
		}
		x := routers[v.id]
		if x == nil {
			return nil
		}
		for _, l := range x.links {
			switch l.Type {
			case LinkP2P, LinkVirtual:
				if linksBack(l.NbrRouter, func(b Router3Link) bool { return b.Type == l.Type && b.NbrRouter == v.id }) {
					out = append(out, edge{vertex{id: l.NbrRouter}, uint32(l.Metric)})
				}
			case LinkTransit:
				nv := vertex{net: true, id: l.NbrRouter, ifID: l.NbrIfID}
				if n := nets[nv]; n != nil {
					for _, x := range n.Routers {
						if x == v.id {
							out = append(out, edge{nv, uint32(l.Metric)})
							break
						}
					}
				}
			}
		}
		return out
	})
	for v, d := range done {
		if !v.net {
			if x := routers[v.id]; x != nil {
				r.flags[v.id] = x.flags
				if x.flags&RouterE != 0 && v.id != s.rid {
					r.asbrs[v.id] = d
				}
			}
		}
	}
	for _, l := range prefixLSAs {
		x, err := DecodeIntraPrefixLSA(l.body())
		if err != nil {
			continue
		}
		v := vertex{id: x.RefAdvRtr}
		if x.RefType == LSA3Network {
			v = vertex{net: true, id: x.RefAdvRtr, ifID: u32(x.RefID)}
		} else if x.RefType != LSA3Router {
			continue
		}
		d, ok := done[v]
		if !ok {
			continue
		}
		for _, p := range x.Prefixes {
			if p.Options&PrefixNU == 0 {
				setMin(r.intra, p.Prefix, d+uint32(p.Metric))
			}
			// A router's own global address (a virtual link's far end).
			if !v.net && p.Options&PrefixLA != 0 && p.Prefix.Bits() == 128 && !r.addr[v.id].IsValid() {
				r.addr[v.id] = p.Prefix.Addr()
			}
		}
	}
	for _, l := range sums {
		d, ok := r.dist[l.hdr.AdvRtr]
		if !ok || l.hdr.AdvRtr == s.rid || r.flags[l.hdr.AdvRtr]&RouterB == 0 {
			continue
		}
		if l.hdr.Type == LSA3InterPrefix {
			if x, err := DecodeInterPrefixLSA(l.body()); err == nil && x.Metric < LSInfinity && x.Prefix.Options&PrefixNU == 0 {
				setMin(r.inter, x.Prefix.Prefix, d+x.Metric)
			}
		} else if x, err := DecodeInterRouterLSA(l.body()); err == nil && x.Metric < LSInfinity {
			setMin(r.interASBR, x.Router, d+x.Metric)
		}
	}
	return r
}
