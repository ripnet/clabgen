package isis

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Config is one emulated IS-IS router with one interface (its device).
type Config struct {
	SystemID string   `json:"system_id,omitempty" jsonschema:"description=Default: derived from the device MAC (0200.xxxx.xxxx style)"`
	Hostname string   `json:"hostname,omitempty" jsonschema:"description=Dynamic hostname (TLV 137). Default: the instance name"`
	Areas    []string `json:"areas" jsonschema:"description=Area addresses like 49.0001"`
	Level    string   `json:"level" jsonschema:"enum=L1,enum=L2,enum=L1L2,description=Levels this router runs"`
	Circuit  string   `json:"circuit" jsonschema:"enum=point-to-point,enum=broadcast,description=Interface type"`
	Priority uint8    `json:"priority" jsonschema:"minimum=0,maximum=127,description=DIS election priority (broadcast circuits)"`
	Metric   uint32   `json:"metric" jsonschema:"minimum=1,maximum=16777214,description=Wide metric toward the neighbor"`

	HelloInterval   int  `json:"hello_interval" jsonschema:"minimum=1,maximum=20000,description=Seconds"`
	HelloMultiplier int  `json:"hello_multiplier" jsonschema:"minimum=2,maximum=100,description=Hold time = interval × multiplier"`
	HelloPadding    bool `json:"hello_padding,omitempty" jsonschema:"description=Pad hellos to the port MTU (detects MTU mismatches)"`
	CSNPInterval    int  `json:"csnp_interval" jsonschema:"minimum=1,maximum=65535,description=Seconds between periodic CSNPs on the point-to-point link"`
	LSPLifetime     int  `json:"lsp_lifetime" jsonschema:"minimum=350,maximum=65535,description=Seconds"`
	LSPRefresh      int  `json:"lsp_refresh" jsonschema:"minimum=1,maximum=65535,description=Seconds between refreshes of our LSPs (below the lifetime)"`
	LSPMTU          int  `json:"lsp_mtu" jsonschema:"minimum=512,maximum=9190,description=Largest LSP we originate; more prefixes go in further fragments"`

	HelloAuth Auth `json:"hello_auth" jsonschema:"description=Authentication of hellos on the interface"`
	LSPAuth   Auth `json:"lsp_auth" jsonschema:"description=Area (L1) / domain (L2) authentication of LSPs (and CSNPs/PSNPs with snp_auth)"`
	SNPAuth   bool `json:"snp_auth" jsonschema:"description=Also authenticate CSNPs and PSNPs with the LSP key"`

	Overload bool `json:"overload,omitempty" jsonschema:"description=Set the overload bit"`
	// Interface subnets are advertised automatically.
	Prefixes []Prefix `json:"prefixes,omitempty" jsonschema:"description=Extra prefixes to advertise (route injection)"`

	Topology Topology `json:"topology" jsonschema:"description=Simulated routers behind this one; their LSPs are originated here"`
}

// Topology describes an emulated network of routers attached to us.
type Topology struct {
	Kind   string   `json:"kind" jsonschema:"enum=none,enum=grid,enum=ring,enum=line,enum=full,enum=custom,description=grid: rows × cols; ring/line/full (mesh): nodes; custom: nodes with links"`
	Rows   int      `json:"rows,omitempty" jsonschema:"minimum=1,description=grid rows"`
	Cols   int      `json:"cols,omitempty" jsonschema:"minimum=1,description=grid columns"`
	Nodes  int      `json:"nodes,omitempty" jsonschema:"minimum=1,description=Node count for ring / line / full"`
	Attach []string `json:"attach,omitempty" jsonschema:"description=Nodes linked to us (names like r1-1 for grids or r1). Default: the first node"`
	Metric uint32   `json:"metric,omitempty" jsonschema:"description=Metric of every link. Default 10"`

	SystemIDBase    string `json:"system_id_base,omitempty" jsonschema:"description=System ID of the first node; the others count up. Default 0000.1000.0001"`
	HostnamePrefix  string `json:"hostname_prefix,omitempty" jsonschema:"description=Default sim-"`
	LoopbackBase    string `json:"loopback_base,omitempty" jsonschema:"description=Loopback of the first node (e.g. 10.255.0.1/32); the others count up"`
	PrefixBase      string `json:"prefix_base,omitempty" jsonschema:"description=First prefix (e.g. 10.200.0.0/24); each node gets prefixes_per_node consecutive ones"`
	PrefixesPerNode int    `json:"prefixes_per_node,omitempty" jsonschema:"minimum=0"`

	Custom []SimNode `json:"custom,omitempty" jsonschema:"description=Nodes for kind custom"`
}

type SimNode struct {
	Name     string    `json:"name"`
	SystemID string    `json:"system_id,omitempty"`
	Links    []SimLink `json:"links,omitempty" jsonschema:"description=Links to other nodes (by name; us for this router). Links are two-way"`
	Prefixes []Prefix  `json:"prefixes,omitempty"`
}

type SimLink struct {
	To     string `json:"to"`
	Metric uint32 `json:"metric,omitempty" jsonschema:"description=Default: the topology metric"`
}

type Prefix struct {
	Prefix string `json:"prefix" jsonschema:"description=e.g. 198.51.100.0/24 or 2001:db8:1::/48"`
	Metric uint32 `json:"metric,omitempty" jsonschema:"description=Default 10"`
	Count  int    `json:"count,omitempty" jsonschema:"description=How many consecutive prefixes of this length (e.g. 10.0.0.0/24 count 3 = 10.0.0-2.0/24). Default 1"`
}

func defaultConfig() *Config {
	return &Config{Areas: []string{"49.0001"}, Level: "L2", Circuit: "point-to-point", Priority: 64, Metric: 10,
		HelloAuth: Auth{Type: "none"}, LSPAuth: Auth{Type: "none"}, SNPAuth: true,
		HelloInterval: 3, HelloMultiplier: 3, CSNPInterval: 10, LSPLifetime: 1200, LSPRefresh: 900, LSPMTU: 1492}
}

func (c *Config) Validate() error {
	if c.SystemID != "" {
		if _, err := ParseSysID(c.SystemID); err != nil {
			return err
		}
	}
	if len(c.Areas) == 0 || len(c.Areas) > 3 {
		return errors.New("1 to 3 areas")
	}
	for _, a := range c.Areas {
		if _, err := ParseArea(a); err != nil {
			return err
		}
	}
	if c.Level != "L1" && c.Level != "L2" && c.Level != "L1L2" {
		return fmt.Errorf("invalid level %q (L1, L2 or L1L2)", c.Level)
	}
	if c.Circuit != "point-to-point" && c.Circuit != "broadcast" {
		return fmt.Errorf("invalid circuit %q", c.Circuit)
	}
	if c.Priority > 127 {
		return errors.New("priority must be 0-127")
	}
	if c.Metric < 1 || c.Metric > 0xfffffe {
		return errors.New("metric must be 1-16777214")
	}
	if c.HelloInterval < 1 || c.HelloMultiplier < 2 || c.HelloInterval*c.HelloMultiplier > 65535 {
		return errors.New("hello_interval ≥ 1, hello_multiplier ≥ 2, and their product ≤ 65535")
	}
	if c.CSNPInterval < 1 {
		return errors.New("csnp_interval ≥ 1")
	}
	if c.LSPLifetime < 350 || c.LSPLifetime > 65535 || c.LSPRefresh < 1 || c.LSPRefresh >= c.LSPLifetime-60 {
		return errors.New("lsp_lifetime 350-65535 and lsp_refresh at least a minute below it")
	}
	if c.LSPMTU < 512 || c.LSPMTU > 9190 {
		return errors.New("lsp_mtu must be 512-9190")
	}
	if err := c.HelloAuth.validate("hello_auth"); err != nil {
		return err
	}
	if err := c.LSPAuth.validate("lsp_auth"); err != nil {
		return err
	}
	if len(c.Hostname) > 255 {
		return errors.New("hostname too long")
	}
	total := 0
	for _, p := range c.Prefixes {
		ps, err := p.expand()
		if err != nil {
			return err
		}
		total += len(ps)
	}
	if total > 100000 {
		return fmt.Errorf("%d prefixes: at most 100000", total)
	}
	if c.Topology.Kind != "" && c.Topology.Kind != "none" {
		if _, err := buildTopology(c.Topology, nil); err != nil {
			return fmt.Errorf("topology: %w", err)
		}
	}
	return nil
}

// expand returns the prefixes a Prefix entry stands for.
func (p Prefix) expand() ([]IPReach, error) {
	pf, err := netip.ParsePrefix(strings.TrimSpace(p.Prefix))
	if err != nil {
		return nil, fmt.Errorf("prefix %q: %w", p.Prefix, err)
	}
	pf = pf.Masked()
	m := p.Metric
	if m == 0 {
		m = 10
	}
	if m > 0xfe000000 {
		return nil, fmt.Errorf("prefix %s: metric too large", pf)
	}
	n := p.Count
	if n <= 0 {
		n = 1
	}
	out := make([]IPReach, 0, n)
	cur := pf
	for i := 0; i < n; i++ {
		out = append(out, IPReach{Prefix: cur, Metric: m})
		next, ok := nextPrefix(cur)
		if !ok && i < n-1 {
			return nil, fmt.Errorf("prefix %s count %d runs out of address space", pf, n)
		}
		cur = next
	}
	return out, nil
}

// nextPrefix returns the prefix of the same length right after p.
func nextPrefix(p netip.Prefix) (netip.Prefix, bool) {
	b := p.Addr().AsSlice()
	bit := p.Bits() - 1
	if bit < 0 {
		return p, false
	}
	for i := bit; i >= 0; i-- {
		byteI, mask := i/8, byte(0x80>>(i%8))
		if b[byteI]&mask == 0 {
			b[byteI] |= mask
			a, _ := netip.AddrFromSlice(b)
			return netip.PrefixFrom(a, p.Bits()), true
		}
		b[byteI] &^= mask
	}
	return p, false
}

func (c *Config) levels() []int {
	switch c.Level {
	case "L1":
		return []int{1}
	case "L2":
		return []int{2}
	}
	return []int{1, 2}
}

func (c *Config) circuitType() uint8 {
	switch c.Level {
	case "L1":
		return circuitL1
	case "L2":
		return circuitL2
	}
	return circuitL1L2
}
