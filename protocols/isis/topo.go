package isis

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Emulated topologies: routers that exist only as LSPs we originate. Each
// has its own system ID, hostname, links (to other simulated routers or
// to us) and prefixes, so the real routers' SPF sees a whole network
// behind this one. Links and nodes can be taken down at runtime.

const maxSimNodes = 10000

// us is the name links use for this router.
const usName = "us"

type simNode struct {
	name      string
	sys       SysID
	loopback  netip.Prefix
	loopback6 netip.Prefix
	prefixes  []IPReach
	links     map[string]uint32 // neighbor name → metric
	down      bool
	overload  bool
}

type topology struct {
	kind   string
	nodes  []*simNode
	byName map[string]*simNode
	bySys  map[SysID]*simNode
	// Links are keyed by their two names in sorted order.
	linkDown   map[[2]string]bool
	linkMetric map[[2]string]uint32
}

func linkKey(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

func addSys(s SysID, n uint64) SysID {
	var b [8]byte
	copy(b[2:], s[:])
	v := binary.BigEndian.Uint64(b[:]) + n
	binary.BigEndian.PutUint64(b[:], v)
	return SysID(b[2:])
}

// buildTopology expands a topology config. If us is set, simulated system
// IDs must differ from it.
func buildTopology(c Topology, us *SysID) (*topology, error) {
	t := &topology{kind: c.Kind, byName: map[string]*simNode{}, bySys: map[SysID]*simNode{},
		linkDown: map[[2]string]bool{}, linkMetric: map[[2]string]uint32{}}
	metric := c.Metric
	if metric == 0 {
		metric = 10
	}
	if metric > 0xfffffe {
		return nil, errors.New("metric must be 1-16777214")
	}
	base := SysID{0, 0, 0x10, 0, 0, 1}
	if c.SystemIDBase != "" {
		var err error
		if base, err = ParseSysID(c.SystemIDBase); err != nil {
			return nil, err
		}
	}
	add := func(name string, sys SysID) (*simNode, error) {
		if name == "" || name == usName || strings.ContainsAny(name, " /") {
			return nil, fmt.Errorf("invalid node name %q", name)
		}
		if t.byName[name] != nil {
			return nil, fmt.Errorf("node %s defined twice", name)
		}
		if t.bySys[sys] != nil || (us != nil && sys == *us) {
			return nil, fmt.Errorf("node %s: system ID %s is already used", name, sys)
		}
		n := &simNode{name: name, sys: sys, links: map[string]uint32{}}
		t.nodes = append(t.nodes, n)
		t.byName[name], t.bySys[sys] = n, n
		return n, nil
	}
	link := func(a, b string, m uint32) error {
		if a == b {
			return fmt.Errorf("link %s-%s loops", a, b)
		}
		for _, x := range []string{a, b} {
			if x != usName && t.byName[x] == nil {
				return fmt.Errorf("link to unknown node %q", x)
			}
		}
		if m == 0 {
			m = metric
		}
		if n := t.byName[a]; n != nil {
			n.links[b] = m
		}
		if n := t.byName[b]; n != nil {
			n.links[a] = m
		}
		return nil
	}
	var err error
	switch c.Kind {
	case "grid":
		if c.Rows < 1 || c.Cols < 1 {
			return nil, errors.New("grid needs rows and cols")
		}
		if c.Rows*c.Cols > maxSimNodes {
			return nil, fmt.Errorf("at most %d nodes", maxSimNodes)
		}
		name := func(r, k int) string { return fmt.Sprintf("r%d-%d", r, k) }
		for r := 1; r <= c.Rows; r++ {
			for k := 1; k <= c.Cols; k++ {
				if _, err = add(name(r, k), addSys(base, uint64(len(t.nodes)))); err != nil {
					return nil, err
				}
			}
		}
		for r := 1; r <= c.Rows; r++ {
			for k := 1; k <= c.Cols; k++ {
				if k < c.Cols {
					link(name(r, k), name(r, k+1), 0)
				}
				if r < c.Rows {
					link(name(r, k), name(r+1, k), 0)
				}
			}
		}
	case "ring", "line", "full":
		if c.Nodes < 1 || c.Nodes > maxSimNodes {
			return nil, fmt.Errorf("nodes must be 1-%d", maxSimNodes)
		}
		if c.Kind == "full" && c.Nodes > 200 {
			return nil, errors.New("a full mesh is limited to 200 nodes")
		}
		for i := 1; i <= c.Nodes; i++ {
			if _, err = add(fmt.Sprintf("r%d", i), addSys(base, uint64(i-1))); err != nil {
				return nil, err
			}
		}
		for i := 1; i < c.Nodes; i++ {
			if c.Kind == "full" {
				for j := i + 1; j <= c.Nodes; j++ {
					link(fmt.Sprintf("r%d", i), fmt.Sprintf("r%d", j), 0)
				}
			} else {
				link(fmt.Sprintf("r%d", i), fmt.Sprintf("r%d", i+1), 0)
			}
		}
		if c.Kind == "ring" && c.Nodes > 2 {
			link(fmt.Sprintf("r%d", c.Nodes), "r1", 0)
		}
	case "custom":
		if len(c.Custom) == 0 || len(c.Custom) > maxSimNodes {
			return nil, fmt.Errorf("custom needs 1-%d nodes", maxSimNodes)
		}
		for i, cn := range c.Custom {
			sys := addSys(base, uint64(i))
			if cn.SystemID != "" {
				if sys, err = ParseSysID(cn.SystemID); err != nil {
					return nil, fmt.Errorf("node %s: %w", cn.Name, err)
				}
			}
			if _, err = add(cn.Name, sys); err != nil {
				return nil, err
			}
		}
		for _, cn := range c.Custom {
			for _, l := range cn.Links {
				if err := link(cn.Name, l.To, l.Metric); err != nil {
					return nil, fmt.Errorf("node %s: %w", cn.Name, err)
				}
			}
			for _, p := range cn.Prefixes {
				ps, err := p.expand()
				if err != nil {
					return nil, fmt.Errorf("node %s: %w", cn.Name, err)
				}
				t.byName[cn.Name].prefixes = append(t.byName[cn.Name].prefixes, ps...)
			}
		}
	default:
		return nil, fmt.Errorf("invalid kind %q", c.Kind)
	}
	// Attachment to us.
	attach := c.Attach
	if len(attach) == 0 && c.Kind != "custom" {
		attach = []string{t.nodes[0].name}
	}
	for _, a := range attach {
		if err := link(usName, a, 0); err != nil {
			return nil, err
		}
	}
	// Generated addressing.
	if c.LoopbackBase != "" {
		lb, err := netip.ParsePrefix(c.LoopbackBase)
		if err != nil {
			return nil, fmt.Errorf("loopback_base: %w", err)
		}
		for i, n := range t.nodes {
			if i > 0 {
				var ok bool
				if lb, ok = nextPrefix(lb); !ok {
					return nil, errors.New("loopback_base runs out of addresses")
				}
			}
			n.loopback = lb
		}
	}
	if c.Loopback6Base != "" {
		lb, err := netip.ParsePrefix(c.Loopback6Base)
		if err != nil || !lb.Addr().Is6() {
			return nil, fmt.Errorf("loopback6_base: want an IPv6 prefix like 2001:db8:ff::1/128")
		}
		for i, n := range t.nodes {
			if i > 0 {
				var ok bool
				if lb, ok = nextPrefix(lb); !ok {
					return nil, errors.New("loopback6_base runs out of addresses")
				}
			}
			n.loopback6 = lb
		}
	}
	if c.PrefixesPerNode > 0 {
		if c.PrefixBase == "" && c.Prefix6Base == "" {
			return nil, errors.New("prefixes_per_node needs prefix_base or prefix6_base")
		}
		if c.PrefixesPerNode*len(t.nodes) > 200000 {
			return nil, errors.New("at most 200000 simulated prefixes per family")
		}
		for _, base := range []string{c.PrefixBase, c.Prefix6Base} {
			if base == "" {
				continue
			}
			ps, err := Prefix{Prefix: base, Count: c.PrefixesPerNode * len(t.nodes)}.expand()
			if err != nil {
				return nil, fmt.Errorf("prefix base %s: %w", base, err)
			}
			for i, n := range t.nodes {
				n.prefixes = append(n.prefixes, ps[i*c.PrefixesPerNode:(i+1)*c.PrefixesPerNode]...)
			}
		}
	}
	return t, nil
}

// up reports whether the a–b link is usable (both ends up, link up).
func (t *topology) up(a, b string) bool {
	if t.linkDown[linkKey(a, b)] {
		return false
	}
	for _, x := range []string{a, b} {
		if n := t.byName[x]; n != nil && n.down {
			return false
		}
	}
	return true
}

func (t *topology) metric(a, b string, def uint32) uint32 {
	if m, ok := t.linkMetric[linkKey(a, b)]; ok {
		return m
	}
	return def
}

// attachedLocked is our IS reachability toward the simulated routers.
func (t *topology) attachedLocked(level int) []ISReach {
	var out []ISReach
	for _, n := range t.nodes {
		m, ok := n.links[usName]
		if !ok || !t.up(usName, n.name) {
			continue
		}
		var id NodeID
		copy(id[:], n.sys[:])
		out = append(out, ISReach{id, t.metric(usName, n.name, m)})
	}
	return out
}

// originateLocked (re)builds the simulated routers' LSPs at level l.
func (t *topology) originateLocked(s *session, l *level, isType byte, now time.Time) {
	for _, n := range t.nodes {
		if n.down {
			s.withdrawSys(l, n.sys, isType, now)
			continue
		}
		body := lspBody{Areas: s.areas, Hostname: s.simHost(n), Overload: n.overload}
		has4, has6 := false, false
		var pfx []IPReach
		for _, lb := range []netip.Prefix{n.loopback, n.loopback6} {
			if !lb.IsValid() {
				continue
			}
			pfx = append(pfx, IPReach{Prefix: lb.Masked(), Metric: 0})
			if lb.Addr().Is4() {
				body.RouterID = lb.Addr()
				body.V4Addrs = []netip.Addr{lb.Addr()}
			} else {
				body.V6Addrs = []netip.Addr{lb.Addr()}
			}
		}
		pfx = append(pfx, n.prefixes...)
		for _, p := range pfx {
			if p.Prefix.Addr().Is4() {
				has4 = true
			} else {
				has6 = true
			}
		}
		if has4 {
			body.Protocols = append(body.Protocols, NLPIDIPv4)
		}
		if has6 {
			body.Protocols = append(body.Protocols, NLPIDIPv6)
		}
		names := make([]string, 0, len(n.links))
		for x := range n.links {
			names = append(names, x)
		}
		sort.Strings(names)
		for _, x := range names {
			if !t.up(n.name, x) {
				continue
			}
			var id NodeID
			if x == usName {
				copy(id[:], s.sys[:])
			} else {
				copy(id[:], t.byName[x].sys[:])
			}
			body.Neighbors = append(body.Neighbors, ISReach{id, t.metric(n.name, x, n.links[x])})
		}
		pfx = s.mtify(&body, pfx)
		s.originateSys(l, n.sys, body, pfx, isType, now)
	}
}

func (s *session) simHost(n *simNode) string {
	p := s.cfg.Topology.HostnamePrefix
	if p == "" {
		p = "sim-"
	}
	return p + n.name
}

// ---- actions ----

type TopoLinkArgs struct {
	A      string `json:"a" jsonschema:"description=Node name (us for this router)"`
	B      string `json:"b" jsonschema:"description=Node name (us for this router)"`
	Up     bool   `json:"up"`
	Metric uint32 `json:"metric,omitempty" jsonschema:"description=Set a new metric (0: keep)"`
}

type TopoNodeArgs struct {
	Node     string `json:"node"`
	Up       bool   `json:"up"`
	Overload *bool  `json:"overload,omitempty"`
}

func (s *session) topoLinkLocked(a *TopoLinkArgs) (any, error) {
	t := s.topo
	if t == nil {
		return nil, errors.New("no topology configured")
	}
	// Find a simulated end and the name of the other end.
	n, other := t.byName[a.A], a.B
	if n == nil {
		n, other = t.byName[a.B], a.A
	}
	if n == nil {
		return nil, fmt.Errorf("unknown nodes %s-%s", a.A, a.B)
	}
	def, ok := n.links[other]
	if !ok {
		return nil, fmt.Errorf("there is no %s-%s link", a.A, a.B)
	}
	if a.Metric > 0xfffffe {
		return nil, errors.New("metric must be 1-16777214")
	}
	k := linkKey(a.A, a.B)
	t.linkDown[k] = !a.Up
	if a.Metric > 0 {
		t.linkMetric[k] = a.Metric
	}
	for _, l := range s.lv {
		l.dirty = true
	}
	return map[string]any{"link": a.A + "-" + a.B, "up": a.Up, "metric": t.metric(a.A, a.B, def)}, nil
}

func (s *session) topoNodeLocked(a *TopoNodeArgs) (any, error) {
	if s.topo == nil {
		return nil, errors.New("no topology configured")
	}
	n := s.topo.byName[a.Node]
	if n == nil {
		return nil, fmt.Errorf("unknown node %q", a.Node)
	}
	n.down = !a.Up
	if a.Overload != nil {
		n.overload = *a.Overload
	}
	for _, l := range s.lv {
		l.dirty = true
	}
	return map[string]any{"node": n.name, "up": !n.down, "overload": n.overload}, nil
}

// ---- status ----

type TopoStatus struct {
	Kind       string   `json:"kind"`
	Nodes      int      `json:"nodes"`
	Links      int      `json:"links"`
	Prefixes   int      `json:"prefixes"`
	DownNodes  []string `json:"down_nodes,omitempty"`
	DownLinks  []string `json:"down_links,omitempty"`
	Overloaded []string `json:"overloaded,omitempty"`
	Attached   []string `json:"attached"`
}

func (t *topology) status() *TopoStatus {
	st := &TopoStatus{Kind: t.kind, Nodes: len(t.nodes), Attached: []string{}}
	links := map[[2]string]bool{}
	for _, n := range t.nodes {
		st.Prefixes += len(n.prefixes)
		if n.loopback.IsValid() {
			st.Prefixes++
		}
		if n.loopback6.IsValid() {
			st.Prefixes++
		}
		if n.down {
			st.DownNodes = append(st.DownNodes, n.name)
		}
		if n.overload {
			st.Overloaded = append(st.Overloaded, n.name)
		}
		for x := range n.links {
			links[linkKey(n.name, x)] = true
			if x == usName {
				st.Attached = append(st.Attached, n.name)
			}
		}
	}
	st.Links = len(links)
	for k, d := range t.linkDown {
		if d {
			st.DownLinks = append(st.DownLinks, k[0]+"-"+k[1])
		}
	}
	sort.Strings(st.DownLinks)
	return st
}
