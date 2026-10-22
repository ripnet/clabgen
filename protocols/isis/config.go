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
	Passive  bool     `json:"passive,omitempty" jsonschema:"description=Advertise this interface's subnets without hellos (no adjacency)"`

	// Interfaces are further devices of this router (the instance's own
	// device is its first interface, with the settings above).
	Interfaces []Interface `json:"interfaces,omitempty" jsonschema:"description=Further interfaces: devices of this router besides the instance's own"`
	Metric     uint32      `json:"metric" jsonschema:"minimum=1,maximum=16777214,description=Wide metric toward the neighbor"`

	Topologies []string `json:"topologies,omitempty" jsonschema:"description=Multi-topology (RFC 5120) besides IPv4 unicast: ipv6-unicast / ipv4-multicast / ipv6-multicast. Empty: single topology"`

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

	SegmentRouting SegmentRouting `json:"segment_routing" jsonschema:"description=Segment routing (RFC 8667; MPLS): SRGB, node SIDs, adjacency SIDs"`

	Topology Topology `json:"topology" jsonschema:"description=Simulated routers behind this one; their LSPs are originated here"`
}

// Interface is one more circuit of the router. Empty fields take the
// router-wide value.
type Interface struct {
	Device    string `json:"device" jsonschema:"description=Device name"`
	Circuit   string `json:"circuit,omitempty" jsonschema:"enum=point-to-point,enum=broadcast"`
	Metric    uint32 `json:"metric,omitempty" jsonschema:"minimum=1,maximum=16777214"`
	Priority  *uint8 `json:"priority,omitempty" jsonschema:"minimum=0,maximum=127"`
	Passive   bool   `json:"passive,omitempty" jsonschema:"description=Advertise the subnets without hellos"`
	HelloAuth *Auth  `json:"hello_auth,omitempty" jsonschema:"description=Default: the router's hello_auth"`
}

// SegmentRouting configures SR-MPLS advertisements.
type SegmentRouting struct {
	Enabled    bool      `json:"enabled"`
	SRGBBase   uint32    `json:"srgb_base" jsonschema:"minimum=16,maximum=1048575,description=First label of the SR global block"`
	SRGBSize   uint32    `json:"srgb_size" jsonschema:"minimum=1,maximum=1048560"`
	NodeSIDs   []NodeSID `json:"node_sids,omitempty" jsonschema:"description=Prefix-SIDs on our prefixes (a missing prefix is advertised); /32 and /128 get the node flag"`
	AdjSIDBase uint32    `json:"adj_sid_base" jsonschema:"description=Local label for our first adjacency SID (the others count up); 0: no adjacency SIDs"`
}

type NodeSID struct {
	Prefix       string `json:"prefix" jsonschema:"description=e.g. 192.0.2.50/32"`
	Index        uint32 `json:"index" jsonschema:"description=Index into the SRGB"`
	NoPHP        bool   `json:"no_php,omitempty" jsonschema:"description=Ask the penultimate hop not to pop (P flag)"`
	ExplicitNull bool   `json:"explicit_null,omitempty" jsonschema:"description=Ask for explicit null (E flag)"`
}

func (s SegmentRouting) validate(t Topology) error {
	if !s.Enabled {
		return nil
	}
	if s.SRGBBase < 16 || s.SRGBSize == 0 || s.SRGBBase+s.SRGBSize > 1<<20 {
		return errors.New("segment_routing: SRGB must lie within labels 16-1048575")
	}
	if s.AdjSIDBase != 0 && (s.AdjSIDBase < 16 || s.AdjSIDBase+1000 > 1<<20) {
		return errors.New("segment_routing: adj_sid_base must leave room for labels below 1048576")
	}
	if s.AdjSIDBase != 0 && s.AdjSIDBase < s.SRGBBase+s.SRGBSize && s.AdjSIDBase+1000 > s.SRGBBase {
		return errors.New("segment_routing: adjacency labels overlap the SRGB")
	}
	seen := map[uint32]bool{}
	for _, n := range s.NodeSIDs {
		if _, err := netip.ParsePrefix(n.Prefix); err != nil {
			return fmt.Errorf("segment_routing: node SID prefix %q: %w", n.Prefix, err)
		}
		if n.Index >= s.SRGBSize {
			return fmt.Errorf("segment_routing: index %d outside the SRGB (size %d)", n.Index, s.SRGBSize)
		}
		if seen[n.Index] {
			return fmt.Errorf("segment_routing: index %d used twice", n.Index)
		}
		seen[n.Index] = true
	}
	if k := t.Kind; k != "" && k != "none" {
		nodes, err := buildTopology(t, nil)
		if err != nil {
			return nil // reported by the topology check
		}
		for _, base := range []*uint32{t.NodeSIDBase, t.NodeSID6Base} {
			if base == nil {
				continue
			}
			for i := range nodes.nodes {
				idx := *base + uint32(i)
				if idx >= s.SRGBSize {
					return fmt.Errorf("segment_routing: simulated node SID index %d outside the SRGB", idx)
				}
				if seen[idx] {
					return fmt.Errorf("segment_routing: simulated node SID index %d collides with another SID", idx)
				}
				seen[idx] = true
			}
		}
	}
	return nil
}

// Topology describes an emulated network of routers attached to us.
type Topology struct {
	Kind   string   `json:"kind" jsonschema:"enum=none,enum=grid,enum=ring,enum=line,enum=full,enum=custom,description=grid: rows × cols; ring/line/full (mesh): nodes; custom: nodes with links"`
	Rows   int      `json:"rows,omitempty" jsonschema:"minimum=1,description=grid rows"`
	Cols   int      `json:"cols,omitempty" jsonschema:"minimum=1,description=grid columns"`
	Nodes  int      `json:"nodes,omitempty" jsonschema:"minimum=1,description=Node count for ring / line / full"`
	Attach []string `json:"attach,omitempty" jsonschema:"description=Nodes linked to us (names like r1-1 for grids or r1). Default: the first node"`
	Metric uint32   `json:"metric,omitempty" jsonschema:"description=Metric of every link. Default 10"`

	SystemIDBase    string  `json:"system_id_base,omitempty" jsonschema:"description=System ID of the first node; the others count up. Default 0000.1000.0001"`
	HostnamePrefix  string  `json:"hostname_prefix,omitempty" jsonschema:"description=Default sim-"`
	LoopbackBase    string  `json:"loopback_base,omitempty" jsonschema:"description=Loopback of the first node (e.g. 10.255.0.1/32); the others count up"`
	Loopback6Base   string  `json:"loopback6_base,omitempty" jsonschema:"description=IPv6 loopback of the first node (e.g. 2001:db8:ff::1/128)"`
	PrefixBase      string  `json:"prefix_base,omitempty" jsonschema:"description=First prefix (e.g. 10.200.0.0/24); each node gets prefixes_per_node consecutive ones"`
	Prefix6Base     string  `json:"prefix6_base,omitempty" jsonschema:"description=First IPv6 prefix (e.g. 2001:db8:200::/64); prefixes_per_node per node too"`
	PrefixesPerNode int     `json:"prefixes_per_node,omitempty" jsonschema:"minimum=0"`
	NodeSIDBase     *uint32 `json:"node_sid_base,omitempty" jsonschema:"description=With segment routing: node SID index of the first node's IPv4 loopback; the others count up"`
	NodeSID6Base    *uint32 `json:"node_sid6_base,omitempty" jsonschema:"description=Same for IPv6 loopbacks"`

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
		HelloAuth: Auth{Type: "none"}, LSPAuth: Auth{Type: "none"}, SNPAuth: true, SegmentRouting: SegmentRouting{SRGBBase: 16000, SRGBSize: 8000, AdjSIDBase: 24000},
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
	seenMT := map[string]bool{}
	for _, t := range c.Topologies {
		if _, ok := mtIDs[t]; !ok || seenMT[t] {
			return fmt.Errorf("invalid or repeated topology %q (ipv6-unicast, ipv4-multicast, ipv6-multicast)", t)
		}
		seenMT[t] = true
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
	if len(c.Interfaces) > 254 {
		return fmt.Errorf("at most 254 further interfaces")
	}
	seenDev := map[string]bool{}
	for i, f := range c.Interfaces {
		scope := fmt.Sprintf("interfaces[%d]", i)
		if f.Device == "" || seenDev[f.Device] {
			return fmt.Errorf("%s: device missing or repeated", scope)
		}
		seenDev[f.Device] = true
		if f.Circuit != "" && f.Circuit != "point-to-point" && f.Circuit != "broadcast" {
			return fmt.Errorf("%s: invalid circuit %q", scope, f.Circuit)
		}
		if f.Metric > 0xfffffe {
			return fmt.Errorf("%s: metric must be 1-16777214", scope)
		}
		if f.Priority != nil && *f.Priority > 127 {
			return fmt.Errorf("%s: priority must be 0-127", scope)
		}
		if f.HelloAuth != nil {
			if err := f.HelloAuth.validate(scope + ".hello_auth"); err != nil {
				return err
			}
		}
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
	if err := c.SegmentRouting.validate(c.Topology); err != nil {
		return err
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
