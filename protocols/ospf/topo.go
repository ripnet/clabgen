package ospf

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"

	"github.com/ripnet/clabgen/internal/topogen"
)

// Emulated topologies: routers that exist only as the LSAs we originate
// for them (topogen builds the graph). Each has a router ID, a loopback
// and prefixes; links between them, and to us, are unnumbered
// point-to-point (v2: an interface index as link data; v3: interface IDs).
// Links and routers can be taken down at run time.

type Topology struct {
	Kind   string   `json:"kind" jsonschema:"enum=none,enum=grid,enum=ring,enum=line,enum=full,enum=custom,description=grid: rows × cols; ring/line/full (mesh): nodes; custom: nodes with links"`
	Rows   int      `json:"rows,omitempty" jsonschema:"minimum=1,description=grid rows"`
	Cols   int      `json:"cols,omitempty" jsonschema:"minimum=1,description=grid columns"`
	Nodes  int      `json:"nodes,omitempty" jsonschema:"minimum=1,description=Node count for ring / line / full"`
	Attach []string `json:"attach,omitempty" jsonschema:"description=Nodes linked to us (names like r1-1 for grids or r1). Default: the first node"`
	Metric uint16   `json:"metric,omitempty" jsonschema:"description=Cost of every link. Default 10"`
	Area   AreaID   `json:"area,omitempty" jsonschema:"description=Area of the simulated routers and our links to them. Default: the router's area; another one makes us an area border router"`

	RouterIDBase    string `json:"router_id_base,omitempty" jsonschema:"description=Router ID of the first node; the others count up. Default 10.254.0.1"`
	LoopbackBase    string `json:"loopback_base,omitempty" jsonschema_description:"Loopback of the first node (10.255.0.1/32, or 2001:db8:ff::1/128 for OSPFv3); the others count up"`
	PrefixBase      string `json:"prefix_base,omitempty" jsonschema:"description=First prefix (e.g. 10.200.0.0/24); each node gets prefixes_per_node consecutive ones"`
	PrefixesPerNode int    `json:"prefixes_per_node,omitempty" jsonschema:"minimum=0,maximum=100"`

	Custom []SimNode `json:"custom,omitempty" jsonschema:"description=Nodes for kind custom" jsonschema_extras:"x-summary={name}[ {router_id}]"`

	NodeSIDBase  *uint32       `json:"node_sid_base,omitempty" jsonschema:"description=With segment routing (OSPFv2): node SID index of the first node's loopback; the others count up"`
	FlexAlgoSIDs []AlgoSIDBase `json:"flex_algo_sids,omitempty" jsonschema:"description=Node SIDs per flex-algo for the simulated loopbacks"`
	LinkDelayUS  uint32        `json:"link_delay_us,omitempty" jsonschema:"description=Delay of every simulated link in µs (flex-algo delay); custom links can override"`
}

type AlgoSIDBase struct {
	Algorithm   byte    `json:"algorithm"`
	NodeSIDBase *uint32 `json:"node_sid_base,omitempty"`
}

type SimNode struct {
	Name     string    `json:"name" jsonschema:"required"`
	RouterID string    `json:"router_id,omitempty"`
	Links    []SimLink `json:"links,omitempty" jsonschema:"description=Links to other nodes (by name; us for this router). Links are two-way"`
	Prefixes []Prefix  `json:"prefixes,omitempty" jsonschema:"description=Prefixes of this node (stub links / intra-area prefixes)"`
}

type SimLink struct {
	To          string `json:"to"`
	Metric      uint16 `json:"metric,omitempty" jsonschema:"description=Default: the topology metric"`
	TEMetric    uint32 `json:"te_metric,omitempty" jsonschema:"description=Default: none"`
	DelayUS     uint32 `json:"delay_us,omitempty" jsonschema:"description=Default: link_delay_us"`
	AdminGroups []int  `json:"admin_groups,omitempty"`
}

const maxPerNode = 100 // a v2 router LSA has to fit one packet

type simRouter struct {
	v        *topogen.Vertex
	rid      netip.Addr
	loopback netip.Prefix
	prefixes []route
}

type topology struct {
	g      *topogen.Graph
	nodes  []*simRouter
	byName map[string]*simRouter
	byRID  map[netip.Addr]*simRouter
}

func buildTopology(c Topology, us netip.Addr, v6 bool) (*topology, error) {
	spec := topogen.Spec{Kind: c.Kind, Rows: c.Rows, Cols: c.Cols, Nodes: c.Nodes, Attach: c.Attach, Metric: uint32(c.Metric), Max: MaxLinkMetric}
	custom := map[string]SimNode{}
	for _, cn := range c.Custom {
		n := topogen.Node{Name: cn.Name}
		for _, l := range cn.Links {
			n.Links = append(n.Links, topogen.Link{To: l.To, Metric: uint32(l.Metric)})
		}
		spec.Custom = append(spec.Custom, n)
		custom[cn.Name] = cn
	}
	g, err := topogen.Build(spec)
	if err != nil {
		return nil, err
	}
	t := &topology{g: g, byName: map[string]*simRouter{}, byRID: map[netip.Addr]*simRouter{}}
	rid := netip.MustParseAddr("10.254.0.1")
	if c.RouterIDBase != "" {
		if rid, err = netip.ParseAddr(c.RouterIDBase); err != nil || !rid.Is4() {
			return nil, errors.New("router_id_base: want a dotted IPv4-style ID")
		}
	}
	var lb netip.Prefix
	if c.LoopbackBase != "" {
		if lb, err = netip.ParsePrefix(c.LoopbackBase); err != nil || lb.Addr().Is4() == v6 {
			return nil, fmt.Errorf("loopback_base: want an %s prefix like %s", family(v6), map[bool]string{false: "10.255.0.1/32", true: "2001:db8:ff::1/128"}[v6])
		}
	}
	var gen []route
	if c.PrefixesPerNode > 0 {
		if c.PrefixBase == "" {
			return nil, errors.New("prefixes_per_node needs prefix_base")
		}
		if c.PrefixesPerNode > maxPerNode {
			return nil, fmt.Errorf("prefixes_per_node is at most %d", maxPerNode)
		}
		if gen, err = (Prefix{Prefix: c.PrefixBase, Count: c.PrefixesPerNode * len(g.Nodes), Type: "internal"}).expand(v6); err != nil {
			return nil, fmt.Errorf("prefix_base: %w", err)
		}
	}
	for i, v := range g.Nodes {
		n := &simRouter{v: v, rid: id4(u32(rid) + uint32(i))}
		if cn := custom[v.Name]; cn.RouterID != "" {
			if n.rid, err = netip.ParseAddr(cn.RouterID); err != nil || !n.rid.Is4() {
				return nil, fmt.Errorf("node %s: bad router_id %q", v.Name, cn.RouterID)
			}
		}
		if n.rid == us || t.byRID[n.rid] != nil || n.rid == zero4 {
			return nil, fmt.Errorf("node %s: router ID %s is already used", v.Name, n.rid)
		}
		if lb.IsValid() {
			if i > 0 {
				var ok bool
				if lb, ok = nextPrefix(lb); !ok {
					return nil, errors.New("loopback_base runs out of addresses")
				}
			}
			n.loopback = lb
		}
		if gen != nil {
			n.prefixes = append(n.prefixes, gen[i*c.PrefixesPerNode:(i+1)*c.PrefixesPerNode]...)
		}
		for _, p := range custom[v.Name].Prefixes {
			p.Type = "internal"
			rs, err := p.expand(v6)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", v.Name, err)
			}
			n.prefixes = append(n.prefixes, rs...)
		}
		if len(n.prefixes) > maxPerNode {
			return nil, fmt.Errorf("node %s: at most %d prefixes", v.Name, maxPerNode)
		}
		t.nodes = append(t.nodes, n)
		t.byName[v.Name], t.byRID[n.rid] = n, n
	}
	return t, nil
}

// ifID numbers node a's interface toward b: 1 toward us, 2 + b's index
// otherwise. Our interfaces toward simulated routers are 0x10000 + their
// index, clear of our real interfaces.
func (t *topology) ifID(a, b string) uint32 {
	if a == topogen.Us {
		return 0x10000 + uint32(t.byName[b].v.Index)
	}
	if b == topogen.Us {
		return 1
	}
	return 2 + uint32(t.byName[b].v.Index)
}

func (t *topology) ridOf(s *session, name string) netip.Addr {
	if name == topogen.Us {
		return s.rid
	}
	return t.byName[name].rid
}

// linkMetric is a–b's cost from a's side: max-metric applies to our side,
// overload to a simulated router's.
func (t *topology) linkMetric(s *session, a, b string) uint16 {
	if a == topogen.Us && s.maxMetric || a != topogen.Us && t.byName[a].v.Overload {
		return MaxLinkMetric
	}
	var def uint32
	if a == topogen.Us {
		def = t.byName[b].v.Links[topogen.Us]
	} else {
		def = t.byName[a].v.Links[b]
	}
	return uint16(t.g.Metric(a, b, def))
}

// ourLinks2 / ourLinks3 are our router LSA's links to simulated routers.
func (t *topology) ourLinks2(s *session) []RouterLink {
	var out []RouterLink
	for _, x := range t.g.Neighbors(topogen.Us) {
		if t.g.Up(topogen.Us, x) {
			out = append(out, RouterLink{ID: t.byName[x].rid, Data: id4(t.ifID(topogen.Us, x)), Type: LinkP2P, Metric: t.linkMetric(s, topogen.Us, x)})
		}
	}
	return out
}

func (t *topology) ourLinks3(s *session) []Router3Link {
	var out []Router3Link
	for _, x := range t.g.Neighbors(topogen.Us) {
		if t.g.Up(topogen.Us, x) {
			out = append(out, Router3Link{Type: LinkP2P, Metric: t.linkMetric(s, topogen.Us, x), IfID: t.ifID(topogen.Us, x),
				NbrIfID: t.ifID(x, topogen.Us), NbrRouter: t.byName[x].rid})
		}
	}
	return out
}

// desiredLocked adds the simulated routers' LSAs (in area a).
func (t *topology) desiredLocked(s *session, out map[LSAKey]want, a *area) {
	for _, n := range t.nodes {
		if n.v.Down {
			continue
		}
		if s.cd.v3 {
			var links []Router3Link
			for _, x := range t.g.Neighbors(n.v.Name) {
				if t.g.Up(n.v.Name, x) {
					links = append(links, Router3Link{Type: LinkP2P, Metric: t.linkMetric(s, n.v.Name, x), IfID: t.ifID(n.v.Name, x),
						NbrIfID: t.ifID(x, n.v.Name), NbrRouter: t.ridOf(s, x)})
				}
			}
			h := LSAHeader{Type: LSA3Router, ID: zero4, AdvRtr: n.rid}
			s.put(out, a, h, Router3LSA{Options: s.options(a), Links: links}.Encode())
			var pfx []Prefix6
			if n.loopback.IsValid() {
				x := Prefix6{Prefix: n.loopback.Masked()}
				if n.loopback.Bits() == 128 {
					x.Options = PrefixLA
				}
				pfx = append(pfx, x)
			}
			for _, r := range n.prefixes {
				pfx = append(pfx, Prefix6{Prefix: r.prefix, Metric: uint16(r.metric)})
			}
			s.addIntraPrefix(out, a, n.rid, pfx)
			continue
		}
		var links []RouterLink
		for _, x := range t.g.Neighbors(n.v.Name) {
			if t.g.Up(n.v.Name, x) {
				links = append(links, RouterLink{ID: t.ridOf(s, x), Data: id4(t.ifID(n.v.Name, x)), Type: LinkP2P, Metric: t.linkMetric(s, n.v.Name, x)})
			}
		}
		if n.loopback.IsValid() {
			links = append(links, RouterLink{ID: n.loopback.Masked().Addr(), Data: MaskOf(n.loopback.Bits()), Type: LinkStub})
		}
		for _, r := range n.prefixes {
			links = append(links, RouterLink{ID: r.prefix.Addr(), Data: MaskOf(r.prefix.Bits()), Type: LinkStub, Metric: uint16(r.metric)})
		}
		h := LSAHeader{Type: LSARouter, ID: n.rid, AdvRtr: n.rid}
		s.put(out, a, h, RouterLSA{Links: links}.Encode())
	}
}

// addIntraPrefix adds a router's intra-area-prefix LSAs (OSPFv3), in
// chunks that each fit a packet.
func (s *session) addIntraPrefix(out map[LSAKey]want, a *area, rid netip.Addr, pfx []Prefix6) {
	sort.Slice(pfx, func(i, j int) bool { return prefixLess(pfx[i].Prefix, pfx[j].Prefix) })
	for i := 0; i < len(pfx); i += intraChunk {
		id := uint32(0)
		if i > 0 {
			id = 0x10000 + uint32(i/intraChunk) // clear of the interface IDs used for networks
		}
		ip := IntraPrefixLSA{RefType: LSA3Router, RefID: zero4, RefAdvRtr: rid, Prefixes: pfx[i:min(i+intraChunk, len(pfx))]}
		h := LSAHeader{Type: LSA3IntraPrefix, ID: id4(id), AdvRtr: rid}
		s.put(out, a, h, ip.Encode())
	}
}

// ---- actions ----

type TopoLinkArgs struct {
	A      string `json:"a" jsonschema:"description=Node name (us for this router)"`
	B      string `json:"b" jsonschema:"description=Node name (us for this router)"`
	Up     bool   `json:"up"`
	Metric uint16 `json:"metric,omitempty" jsonschema:"description=Set a new cost (0: keep)"`
}

type TopoNodeArgs struct {
	Node     string `json:"node"`
	Up       bool   `json:"up"`
	Overload *bool  `json:"overload,omitempty" jsonschema:"description=Advertise its links at 65535 (max-metric)"`
}

func (s *session) topoLinkLocked(a *TopoLinkArgs) (any, error) {
	if s.topo == nil {
		return nil, errors.New("no topology configured")
	}
	def, err := s.topo.g.SetLink(a.A, a.B, a.Up, uint32(a.Metric))
	if err != nil {
		return nil, err
	}
	s.dirty = true
	return map[string]any{"link": a.A + "-" + a.B, "up": a.Up, "metric": s.topo.g.Metric(a.A, a.B, def)}, nil
}

func (s *session) topoNodeLocked(a *TopoNodeArgs) (any, error) {
	if s.topo == nil {
		return nil, errors.New("no topology configured")
	}
	n := s.topo.byName[a.Node]
	if n == nil {
		return nil, fmt.Errorf("unknown node %q", a.Node)
	}
	n.v.Down = !a.Up
	if a.Overload != nil {
		n.v.Overload = *a.Overload
	}
	s.dirty = true
	return map[string]any{"node": a.Node, "router_id": n.rid.String(), "up": !n.v.Down, "overload": n.v.Overload}, nil
}

func (t *topology) status() *topogen.Status {
	st := t.g.Status()
	for _, n := range t.nodes {
		st.Prefixes += len(n.prefixes)
		if n.loopback.IsValid() {
			st.Prefixes++
		}
	}
	return &st
}
