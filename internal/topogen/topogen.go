// Package topogen builds the graphs of emulated topologies: simulated
// routers and the links between them and the emulating router ("us"),
// from a generator (grid, ring, line, full mesh) or an explicit list.
// Protocols add their own identities and prefixes per node, and keep the
// run-time state (links and nodes taken down, changed metrics) here.
package topogen

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Us is the name links use for the emulating router.
const Us = "us"

// MaxNodes bounds a topology.
const MaxNodes = 10000

// Spec describes a topology (the protocols' config embeds these fields).
type Spec struct {
	Kind   string
	Rows   int
	Cols   int
	Nodes  int
	Attach []string // nodes linked to us (default: the first, except for custom)
	Metric uint32   // default link metric (0: 10)
	Max    uint32   // largest metric the protocol allows
	Custom []Node
}

// Node is a custom node: its name and links (two-way).
type Node struct {
	Name  string
	Links []Link
}

type Link struct {
	To     string
	Metric uint32 // 0: the topology's
}

type Vertex struct {
	Name     string
	Index    int               // position in the topology (for generated IDs and addresses)
	Links    map[string]uint32 // neighbor name → configured metric
	Down     bool
	Overload bool
}

type Graph struct {
	Kind   string
	Nodes  []*Vertex
	ByName map[string]*Vertex
	down   map[[2]string]bool
	metric map[[2]string]uint32
}

// Key names a link by its two ends in sorted order.
func Key(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

func Build(c Spec) (*Graph, error) {
	g := &Graph{Kind: c.Kind, ByName: map[string]*Vertex{}, down: map[[2]string]bool{}, metric: map[[2]string]uint32{}}
	metric := c.Metric
	if metric == 0 {
		metric = 10
	}
	if c.Max > 0 && metric > c.Max {
		return nil, fmt.Errorf("metric must be 1-%d", c.Max)
	}
	add := func(name string) error {
		if name == "" || name == Us || strings.ContainsAny(name, " /") {
			return fmt.Errorf("invalid node name %q", name)
		}
		if g.ByName[name] != nil {
			return fmt.Errorf("node %s defined twice", name)
		}
		v := &Vertex{Name: name, Index: len(g.Nodes), Links: map[string]uint32{}}
		g.Nodes = append(g.Nodes, v)
		g.ByName[name] = v
		return nil
	}
	link := func(a, b string, m uint32) error {
		if a == b {
			return fmt.Errorf("link %s-%s loops", a, b)
		}
		for _, x := range []string{a, b} {
			if x != Us && g.ByName[x] == nil {
				return fmt.Errorf("link to unknown node %q", x)
			}
		}
		if m == 0 {
			m = metric
		}
		if c.Max > 0 && m > c.Max {
			return fmt.Errorf("link %s-%s: metric must be 1-%d", a, b, c.Max)
		}
		if v := g.ByName[a]; v != nil {
			v.Links[b] = m
		}
		if v := g.ByName[b]; v != nil {
			v.Links[a] = m
		}
		return nil
	}
	switch c.Kind {
	case "grid":
		if c.Rows < 1 || c.Cols < 1 {
			return nil, errors.New("grid needs rows and cols")
		}
		if c.Rows*c.Cols > MaxNodes {
			return nil, fmt.Errorf("at most %d nodes", MaxNodes)
		}
		name := func(r, k int) string { return fmt.Sprintf("r%d-%d", r, k) }
		for r := 1; r <= c.Rows; r++ {
			for k := 1; k <= c.Cols; k++ {
				if err := add(name(r, k)); err != nil {
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
		if c.Nodes < 1 || c.Nodes > MaxNodes {
			return nil, fmt.Errorf("nodes must be 1-%d", MaxNodes)
		}
		if c.Kind == "full" && c.Nodes > 200 {
			return nil, errors.New("a full mesh is limited to 200 nodes")
		}
		for i := 1; i <= c.Nodes; i++ {
			if err := add(fmt.Sprintf("r%d", i)); err != nil {
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
		if len(c.Custom) == 0 || len(c.Custom) > MaxNodes {
			return nil, fmt.Errorf("custom needs 1-%d nodes", MaxNodes)
		}
		for _, n := range c.Custom {
			if err := add(n.Name); err != nil {
				return nil, err
			}
		}
		for _, n := range c.Custom {
			for _, l := range n.Links {
				if err := link(n.Name, l.To, l.Metric); err != nil {
					return nil, fmt.Errorf("node %s: %w", n.Name, err)
				}
			}
		}
	default:
		return nil, fmt.Errorf("invalid kind %q", c.Kind)
	}
	attach := c.Attach
	if len(attach) == 0 && c.Kind != "custom" {
		attach = []string{g.Nodes[0].Name}
	}
	for _, a := range attach {
		if err := link(Us, a, 0); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// Up reports whether the a–b link is usable (both ends up, link up).
func (g *Graph) Up(a, b string) bool {
	if g.down[Key(a, b)] {
		return false
	}
	for _, x := range []string{a, b} {
		if v := g.ByName[x]; v != nil && v.Down {
			return false
		}
	}
	return true
}

// Metric is the a–b link's metric: changed at run time, else def.
func (g *Graph) Metric(a, b string, def uint32) uint32 {
	if m, ok := g.metric[Key(a, b)]; ok {
		return m
	}
	return def
}

// Neighbors lists a node's (or us's) neighbors in name order.
func (g *Graph) Neighbors(name string) []string {
	var out []string
	if name == Us {
		for _, v := range g.Nodes {
			if _, ok := v.Links[Us]; ok {
				out = append(out, v.Name)
			}
		}
		return out
	}
	for x := range g.ByName[name].Links {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

// SetLink takes a link down or up and optionally changes its metric (0:
// keep). It returns the link's configured metric.
func (g *Graph) SetLink(a, b string, up bool, metric uint32) (uint32, error) {
	v, other := g.ByName[a], b
	if v == nil {
		v, other = g.ByName[b], a
	}
	if v == nil {
		return 0, fmt.Errorf("unknown nodes %s-%s", a, b)
	}
	def, ok := v.Links[other]
	if !ok {
		return 0, fmt.Errorf("there is no %s-%s link", a, b)
	}
	k := Key(a, b)
	g.down[k] = !up
	if metric > 0 {
		g.metric[k] = metric
	}
	return def, nil
}

type Status struct {
	Kind       string   `json:"kind"`
	Nodes      int      `json:"nodes"`
	Links      int      `json:"links"`
	Prefixes   int      `json:"prefixes"`
	DownNodes  []string `json:"down_nodes,omitempty"`
	DownLinks  []string `json:"down_links,omitempty"`
	Overloaded []string `json:"overloaded,omitempty"`
	Attached   []string `json:"attached"`
}

// Status summarises the graph; the caller adds the prefix count.
func (g *Graph) Status() Status {
	st := Status{Kind: g.Kind, Nodes: len(g.Nodes), Attached: []string{}}
	links := map[[2]string]bool{}
	for _, v := range g.Nodes {
		if v.Down {
			st.DownNodes = append(st.DownNodes, v.Name)
		}
		if v.Overload {
			st.Overloaded = append(st.Overloaded, v.Name)
		}
		for x := range v.Links {
			links[Key(v.Name, x)] = true
			if x == Us {
				st.Attached = append(st.Attached, v.Name)
			}
		}
	}
	st.Links = len(links)
	for k, d := range g.down {
		if d {
			st.DownLinks = append(st.DownLinks, k[0]+"-"+k[1])
		}
	}
	sort.Strings(st.DownLinks)
	return st
}
