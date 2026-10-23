package isis

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Config is one emulated IS-IS router with one interface (its device).
type Config struct {
	SystemID string   `json:"system_id,omitempty" jsonschema:"description=Default: derived from the device MAC (0200.xxxx.xxxx style)" jsonschema_extras:"x-group=Router"`
	Hostname string   `json:"hostname,omitempty" jsonschema:"description=Dynamic hostname (TLV 137). Default: the instance name" jsonschema_extras:"x-group=Router"`
	Areas    []string `json:"areas" jsonschema:"description=Area addresses like 49.0001" jsonschema_extras:"x-group=Router"`
	Level    string   `json:"level" jsonschema:"enum=L1,enum=L2,enum=L1L2,description=Levels this router runs" jsonschema_extras:"x-group=Router"`
	Circuit  string   `json:"circuit" jsonschema:"enum=point-to-point,enum=broadcast,description=Interface type" jsonschema_extras:"x-group=First interface"`
	Priority uint8    `json:"priority" jsonschema:"minimum=0,maximum=127,description=DIS election priority (broadcast circuits)" jsonschema_extras:"x-group=First interface"`
	Passive  bool     `json:"passive,omitempty" jsonschema:"description=Advertise this interface's subnets without hellos (no adjacency)" jsonschema_extras:"x-group=First interface"`

	// Interfaces are further devices of this router (the instance's own
	// device is its first interface, with the settings above).
	Interfaces []Interface `json:"interfaces,omitempty" jsonschema:"description=Further interfaces: devices of this router besides the instance's own" jsonschema_extras:"x-summary={device}[ {circuit}][ metric {metric}][ {passive}]"`
	Metric     uint32      `json:"metric" jsonschema:"minimum=1,maximum=16777214,description=Wide metric toward the neighbor" jsonschema_extras:"x-group=First interface"`
	// TE attributes of the first interface (for flex-algo).
	TEMetric    uint32 `json:"te_metric,omitempty" jsonschema:"maximum=16777215,description=TE metric of the first interface (flex-algo metric type te)" jsonschema_extras:"x-group=TE attributes,x-advanced=true"`
	DelayUS     uint32 `json:"delay_us,omitempty" jsonschema:"maximum=16777215,description=Link delay in µs of the first interface (flex-algo metric type delay)" jsonschema_extras:"x-group=TE attributes,x-advanced=true"`
	AdminGroups []int  `json:"admin_groups,omitempty" jsonschema:"description=Admin groups (0-31) of the first interface (flex-algo include/exclude)" jsonschema_extras:"x-group=TE attributes,x-advanced=true"`

	Topologies []string `json:"topologies,omitempty" jsonschema:"description=Multi-topology (RFC 5120) besides IPv4 unicast: ipv6-unicast / ipv4-multicast / ipv6-multicast. Empty: single topology" jsonschema_extras:"x-group=Router"`

	HelloInterval   int  `json:"hello_interval" jsonschema:"minimum=1,maximum=20000,description=Seconds" jsonschema_extras:"x-group=Timers and LSPs"`
	HelloMultiplier int  `json:"hello_multiplier" jsonschema:"minimum=2,maximum=100,description=Hold time = interval × multiplier" jsonschema_extras:"x-group=Timers and LSPs"`
	HelloPadding    bool `json:"hello_padding,omitempty" jsonschema:"description=Pad hellos to the port MTU (detects MTU mismatches)" jsonschema_extras:"x-group=Timers and LSPs,x-advanced=true"`
	CSNPInterval    int  `json:"csnp_interval" jsonschema:"minimum=1,maximum=65535,description=Seconds between periodic CSNPs on the point-to-point link" jsonschema_extras:"x-group=Timers and LSPs"`
	LSPLifetime     int  `json:"lsp_lifetime" jsonschema:"minimum=350,maximum=65535,description=Seconds" jsonschema_extras:"x-group=Timers and LSPs"`
	LSPRefresh      int  `json:"lsp_refresh" jsonschema:"minimum=1,maximum=65535,description=Seconds between refreshes of our LSPs (below the lifetime)" jsonschema_extras:"x-group=Timers and LSPs"`
	LSPMTU          int  `json:"lsp_mtu" jsonschema:"minimum=512,maximum=9190,description=Largest LSP we originate; more prefixes go in further fragments" jsonschema_extras:"x-group=Timers and LSPs"`

	HelloAuth Auth `json:"hello_auth" jsonschema:"description=Authentication of hellos on the interface"`
	LSPAuth   Auth `json:"lsp_auth" jsonschema:"description=Area (L1) / domain (L2) authentication of LSPs (and CSNPs/PSNPs with snp_auth)"`
	SNPAuth   bool `json:"snp_auth" jsonschema:"description=Also authenticate CSNPs and PSNPs with the LSP key"`

	Overload bool `json:"overload,omitempty" jsonschema:"description=Set the overload bit" jsonschema_extras:"x-group=Router"`
	// OverloadOnStartup sets the overload bit for a while after starting.
	OverloadOnStartup int `json:"overload_on_startup_s,omitempty" jsonschema:"minimum=0,maximum=86400,description=Seconds to keep the overload bit set after starting (0: off)" jsonschema_extras:"x-group=Router"`

	// L1/L2 routers (level L1L2).
	AttachedBit string    `json:"attached_bit,omitempty" jsonschema:"enum=auto,enum=always,enum=never,description=L1L2: the attached bit in our level-1 LSPs. auto: set while level 2 reaches another area. Default auto" jsonschema_extras:"x-group=L1/L2,x-show-if=level=L1L2"`
	Summaries   []Summary `json:"summaries,omitempty" jsonschema:"description=L1L2: summary addresses for level-1 prefixes advertised into level 2 (the prefixes they cover are replaced by them)" jsonschema_extras:"x-summary={prefix}[ metric {metric}]"`
	LeakToL1    []string  `json:"leak_to_l1,omitempty" jsonschema:"description=L1L2: level-2 prefixes within these ranges (e.g. 192.0.2.0/24; 0.0.0.0/0 for all IPv4) are leaked into level 1 with the up/down bit" jsonschema_extras:"x-group=L1/L2,x-show-if=level=L1L2"`

	BFD             BFDConfig       `json:"bfd" jsonschema:"description=BFD (RFC 5880/5881) with every neighbor: a failed session drops the adjacency at once"`
	GracefulRestart GracefulRestart `json:"graceful_restart" jsonschema_description:"Graceful restart (RFC 8706, restart TLV 211)"`
	// Interface subnets are advertised automatically.
	Prefixes []Prefix `json:"prefixes,omitempty" jsonschema:"description=Extra prefixes to advertise (route injection)" jsonschema_extras:"x-summary={prefix}[ ×{count}][ metric {metric}]"`
	// Loopbacks are the router's own addresses, answered on every interface.
	Loopbacks []string `json:"loopbacks,omitempty" jsonschema_description:"Router loopback addresses (e.g. 192.0.2.50/32): advertised at metric 0 and answered (ping, flows) on every interface; the first IPv4 one is the router ID" jsonschema_extras:"x-group=Router"`

	SegmentRouting SegmentRouting `json:"segment_routing" jsonschema_description:"Segment routing (RFC 8667; MPLS): SRGB, node SIDs, adjacency SIDs"`
	FlexAlgos      []FlexAlgo     `json:"flex_algos,omitempty" jsonschema_description:"Flexible algorithms (RFC 9350) we take part in, optionally with their definitions" jsonschema_extras:"x-summary={algorithm}[ {metric_type}][ {participate_only}]"`
	SRv6           SRv6           `json:"srv6" jsonschema_description:"SRv6 (RFC 9352): locators with End SIDs, End.X SIDs on adjacencies"`

	Topology Topology `json:"topology" jsonschema:"description=Simulated routers behind this one; their LSPs are originated here"`
}

// Interface is one more circuit of the router. Empty fields take the
// router-wide value.
type Interface struct {
	Device    string `json:"device" jsonschema:"required,description=Device name"`
	BFD       *bool  `json:"bfd,omitempty" jsonschema:"description=BFD on this interface. Default: the router's bfd.enabled"`
	Circuit   string `json:"circuit,omitempty" jsonschema:"enum=point-to-point,enum=broadcast"`
	Metric    uint32 `json:"metric,omitempty" jsonschema:"minimum=1,maximum=16777214"`
	Priority  *uint8 `json:"priority,omitempty" jsonschema:"minimum=0,maximum=127"`
	Passive   bool   `json:"passive,omitempty" jsonschema:"description=Advertise the subnets without hellos"`
	HelloAuth *Auth  `json:"hello_auth,omitempty" jsonschema:"description=Default: the router's hello_auth"`

	TEMetric    uint32 `json:"te_metric,omitempty" jsonschema:"maximum=16777215" jsonschema_extras:"x-group=TE attributes,x-advanced=true"`
	DelayUS     uint32 `json:"delay_us,omitempty" jsonschema:"maximum=16777215,description=Link delay in µs" jsonschema_extras:"x-group=TE attributes,x-advanced=true"`
	AdminGroups []int  `json:"admin_groups,omitempty" jsonschema:"description=Admin groups 0-31" jsonschema_extras:"x-group=TE attributes,x-advanced=true"`
}

// SegmentRouting configures SR-MPLS advertisements.
type SegmentRouting struct {
	Enabled    bool      `json:"enabled"`
	SRGBBase   uint32    `json:"srgb_base" jsonschema:"minimum=16,maximum=1048575,description=First label of the SR global block"`
	SRGBSize   uint32    `json:"srgb_size" jsonschema:"minimum=1,maximum=1048560"`
	NodeSIDs   []NodeSID `json:"node_sids,omitempty" jsonschema:"description=Prefix-SIDs on our prefixes (a missing prefix is advertised); /32 and /128 get the node flag" jsonschema_extras:"x-summary={prefix} index {index}[ algo {algorithm}]"`
	AdjSIDBase uint32    `json:"adj_sid_base" jsonschema:"description=Local label for our first adjacency SID (the others count up); 0: no adjacency SIDs"`
	SRLBBase   uint32    `json:"srlb_base,omitempty" jsonschema:"description=First label of the SR local block (advertised; 0: none)"`
	SRLBSize   uint32    `json:"srlb_size,omitempty"`
	// Mapping server (RFC 8667 2.4): SIDs for prefixes of routers without SR.
	Mappings []SIDMapping `json:"mappings,omitempty" jsonschema:"description=Mapping server entries: SID indexes for ranges of prefixes (SID/Label Binding TLV)" jsonschema_extras:"x-summary={prefix}[ ×{range}] index {index}"`
}

// SIDMapping maps Range consecutive prefixes from Prefix to indexes from Index.
type SIDMapping struct {
	Prefix    string `json:"prefix" jsonschema_description:"First prefix, e.g. 10.255.1.1/32"`
	Range     uint16 `json:"range,omitempty" jsonschema:"minimum=1,description=Number of consecutive prefixes. Default 1"`
	Index     uint32 `json:"index" jsonschema:"description=SID index of the first prefix"`
	Algorithm byte   `json:"algorithm,omitempty"`
	Attached  bool   `json:"attached,omitempty" jsonschema:"description=A flag: the prefixes are directly attached to their advertisers"`
}

// FlexAlgo is a flexible algorithm we take part in.
type FlexAlgo struct {
	Algorithm       byte   `json:"algorithm" jsonschema:"minimum=128,maximum=255"`
	MetricType      string `json:"metric_type,omitempty" jsonschema:"enum=igp,enum=delay,enum=te,description=Default igp"`
	Priority        byte   `json:"priority,omitempty" jsonschema:"description=Definition priority (highest wins). Default 128"`
	ParticipateOnly bool   `json:"participate_only,omitempty" jsonschema:"description=Take part without advertising a definition (another router defines it)"`
	ExcludeAny      []int  `json:"exclude_any,omitempty" jsonschema:"description=Admin groups (0-31) to exclude"`
	IncludeAny      []int  `json:"include_any,omitempty"`
	IncludeAll      []int  `json:"include_all,omitempty"`
}

// SRv6 configures SRv6 advertisements.
type SRv6 struct {
	Enabled  bool          `json:"enabled"`
	Locators []SRv6Locator `json:"locators,omitempty"`
	// End.X SIDs for adjacencies, allocated from the first algorithm-0 locator.
	EndX bool `json:"end_x,omitempty" jsonschema:"description=Advertise End.X SIDs on our adjacencies (from the first algorithm-0 locator)"`
	// Simulated routers' locators: the first node's, then the next ones.
	TopologyLocatorBase string              `json:"topology_locator_base,omitempty" jsonschema:"description=Locator of the first simulated router (e.g. fc00:0:100::/48); the others count up"`
	Behavior            string              `json:"behavior,omitempty" jsonschema:"enum=end,enum=end-psp,enum=end-usp,enum=end-psp-usp,enum=un,enum=un-psp,enum=un-usp,enum=un-psp-usp,description=End SID behavior (End.X follows the same flavor). Default end; un for uSID"`
	Structure           *SIDStructureConfig `json:"structure,omitempty" jsonschema_description:"SID structure (bits). Default: block = locator − 16, node 16, function 16"`
}

type SRv6Locator struct {
	Prefix    string `json:"prefix" jsonschema:"description=e.g. fc00:0:50::/48"`
	Algorithm byte   `json:"algorithm,omitempty" jsonschema:"description=0 or a flex-algo"`
	Metric    uint32 `json:"metric,omitempty"`
	EndSID    string `json:"end_sid,omitempty" jsonschema_description:"Default: the locator itself for uSID (un), else locator::1"`
}

type SIDStructureConfig struct {
	LB  byte `json:"block_bits"`
	LN  byte `json:"node_bits"`
	Fun byte `json:"function_bits"`
	Arg byte `json:"argument_bits,omitempty"`
}

type NodeSID struct {
	Prefix       string `json:"prefix" jsonschema:"required,description=e.g. 192.0.2.50/32"`
	Index        uint32 `json:"index" jsonschema:"required,description=Index into the SRGB"`
	Algorithm    byte   `json:"algorithm,omitempty" jsonschema:"description=0 (SPF) or a flex-algo (128-255) from flex_algos"`
	NoPHP        bool   `json:"no_php,omitempty" jsonschema:"description=Ask the penultimate hop not to pop (P flag)"`
	ExplicitNull bool   `json:"explicit_null,omitempty" jsonschema:"description=Ask for explicit null (E flag)"`
}

func (s SegmentRouting) validate(t Topology, algos map[byte]bool) error {
	if !s.Enabled {
		if len(s.Mappings) > 0 {
			return errors.New("segment_routing: mappings need segment routing enabled")
		}
		return nil
	}
	if s.SRLBBase != 0 || s.SRLBSize != 0 {
		if s.SRLBBase < 16 || s.SRLBSize == 0 || s.SRLBBase+s.SRLBSize > 1<<20 {
			return errors.New("segment_routing: SRLB must lie within labels 16-1048575")
		}
		if s.SRLBBase < s.SRGBBase+s.SRGBSize && s.SRLBBase+s.SRLBSize > s.SRGBBase {
			return errors.New("segment_routing: the SRLB overlaps the SRGB")
		}
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
		if n.Algorithm != 0 && !algos[n.Algorithm] {
			return fmt.Errorf("segment_routing: node SID %s: algorithm %d is not in flex_algos", n.Prefix, n.Algorithm)
		}
	}
	if len(s.Mappings) > 100 {
		return errors.New("segment_routing: at most 100 mappings")
	}
	for _, m := range s.Mappings {
		if _, err := netip.ParsePrefix(m.Prefix); err != nil {
			return fmt.Errorf("segment_routing: mapping %q: %w", m.Prefix, err)
		}
		r := uint32(max(m.Range, 1))
		if m.Index+r > s.SRGBSize {
			return fmt.Errorf("segment_routing: mapping %s: indexes %d-%d outside the SRGB (size %d)", m.Prefix, m.Index, m.Index+r-1, s.SRGBSize)
		}
		if m.Algorithm != 0 && !algos[m.Algorithm] {
			return fmt.Errorf("segment_routing: mapping %s: algorithm %d is not in flex_algos", m.Prefix, m.Algorithm)
		}
		for i := m.Index; i < m.Index+r; i++ {
			if seen[i] {
				return fmt.Errorf("segment_routing: mapping %s: index %d collides with another SID", m.Prefix, i)
			}
			seen[i] = true
		}
	}
	if k := t.Kind; k != "" && k != "none" {
		nodes, err := buildTopology(t, nil)
		if err != nil {
			return nil // reported by the topology check
		}
		bases := []*uint32{t.NodeSIDBase, t.NodeSID6Base}
		for _, f := range t.FlexAlgoSIDs {
			if !algos[f.Algorithm] {
				return fmt.Errorf("topology: flex_algo_sids: algorithm %d is not in flex_algos", f.Algorithm)
			}
			bases = append(bases, f.NodeSIDBase, f.NodeSID6Base)
		}
		for _, base := range bases {
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

	SystemIDBase    string        `json:"system_id_base,omitempty" jsonschema:"description=System ID of the first node; the others count up. Default 0000.1000.0001"`
	HostnamePrefix  string        `json:"hostname_prefix,omitempty" jsonschema:"description=Default sim-"`
	LoopbackBase    string        `json:"loopback_base,omitempty" jsonschema:"description=Loopback of the first node (e.g. 10.255.0.1/32); the others count up"`
	Loopback6Base   string        `json:"loopback6_base,omitempty" jsonschema:"description=IPv6 loopback of the first node (e.g. 2001:db8:ff::1/128)"`
	PrefixBase      string        `json:"prefix_base,omitempty" jsonschema:"description=First prefix (e.g. 10.200.0.0/24); each node gets prefixes_per_node consecutive ones"`
	Prefix6Base     string        `json:"prefix6_base,omitempty" jsonschema:"description=First IPv6 prefix (e.g. 2001:db8:200::/64); prefixes_per_node per node too"`
	PrefixesPerNode int           `json:"prefixes_per_node,omitempty" jsonschema:"minimum=0"`
	NodeSIDBase     *uint32       `json:"node_sid_base,omitempty" jsonschema:"description=With segment routing: node SID index of the first node's IPv4 loopback; the others count up"`
	NodeSID6Base    *uint32       `json:"node_sid6_base,omitempty" jsonschema:"description=Same for IPv6 loopbacks"`
	FlexAlgoSIDs    []AlgoSIDBase `json:"flex_algo_sids,omitempty" jsonschema:"description=Node SIDs per flex-algo for the simulated loopbacks"`
	LinkDelayUS     uint32        `json:"link_delay_us,omitempty" jsonschema:"description=Delay of every simulated link in µs (flex-algo delay); custom links can override"`

	Custom []SimNode `json:"custom,omitempty" jsonschema:"description=Nodes for kind custom" jsonschema_extras:"x-summary={name}[ {system_id}]"`
}

type SimNode struct {
	Name     string    `json:"name" jsonschema:"required"`
	SystemID string    `json:"system_id,omitempty"`
	Links    []SimLink `json:"links,omitempty" jsonschema:"description=Links to other nodes (by name; us for this router). Links are two-way"`
	Prefixes []Prefix  `json:"prefixes,omitempty"`
}

type SimLink struct {
	To          string `json:"to"`
	Metric      uint32 `json:"metric,omitempty" jsonschema:"description=Default: the topology metric"`
	TEMetric    uint32 `json:"te_metric,omitempty" jsonschema:"description=Default: the metric"`
	DelayUS     uint32 `json:"delay_us,omitempty" jsonschema:"description=Default: link_delay_us"`
	AdminGroups []int  `json:"admin_groups,omitempty"`
}

// AlgoSIDBase gives simulated loopbacks node SIDs in a flex-algo.
type AlgoSIDBase struct {
	Algorithm    byte    `json:"algorithm"`
	NodeSIDBase  *uint32 `json:"node_sid_base,omitempty"`
	NodeSID6Base *uint32 `json:"node_sid6_base,omitempty"`
}

// Summary is an L1→L2 summary address.
type Summary struct {
	Prefix string `json:"prefix" jsonschema:"required,description=e.g. 10.203.0.0/16"`
	Metric uint32 `json:"metric,omitempty" jsonschema:"maximum=4261412864,description=Default: the lowest metric among the prefixes it covers"`
}

// GracefulRestart configures RFC 8706 graceful restart.
type GracefulRestart struct {
	Helper bool `json:"helper" jsonschema_description:"Help restarting neighbors: keep their adjacencies, acknowledge (RA) and resend the LSDB"`
	// HoldS is how long a neighbor keeps our adjacency while we restart
	// (the remaining time we ask for), and our database sync limit.
	HoldS int `json:"restart_hold_s,omitempty" jsonschema:"minimum=1,maximum=65535,description=Our restart: how long neighbors keep our adjacencies and we wait for the LSDB to resynchronize. Default 60"`
}

type Prefix struct {
	Prefix string `json:"prefix" jsonschema:"required,description=e.g. 198.51.100.0/24 or 2001:db8:1::/48"`
	Metric uint32 `json:"metric,omitempty" jsonschema:"description=Default 10"`
	Count  int    `json:"count,omitempty" jsonschema:"description=How many consecutive prefixes of this length (e.g. 10.0.0.0/24 count 3 = 10.0.0-2.0/24). Default 1"`
}

func defaultConfig() *Config {
	return &Config{Areas: []string{"49.0001"}, Level: "L2", Circuit: "point-to-point", Priority: 64, Metric: 10,
		HelloAuth: Auth{Type: "none"}, LSPAuth: Auth{Type: "none"}, SNPAuth: true, SegmentRouting: SegmentRouting{SRGBBase: 16000, SRGBSize: 8000, AdjSIDBase: 24000},
		HelloInterval: 3, HelloMultiplier: 3, CSNPInterval: 10, LSPLifetime: 1200, LSPRefresh: 900, LSPMTU: 1492,
		GracefulRestart: GracefulRestart{Helper: true}, BFD: BFDConfig{TLV: true}}
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
	for _, l := range c.Loopbacks {
		if _, err := parseLoopback(l); err != nil {
			return err
		}
	}
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
	algos, err := c.validateFlexAlgos()
	if err != nil {
		return err
	}
	if err := c.SegmentRouting.validate(c.Topology, algos); err != nil {
		return err
	}
	if err := c.SRv6.validate(algos); err != nil {
		return err
	}
	return c.validateL1L2()
}

func (c *Config) validateL1L2() error {
	switch c.AttachedBit {
	case "", "auto", "always", "never":
	default:
		return fmt.Errorf("attached_bit must be auto, always or never")
	}
	if c.Level != "L1L2" && (c.AttachedBit != "" && c.AttachedBit != "auto" || len(c.Summaries) > 0 || len(c.LeakToL1) > 0) {
		return errors.New("attached_bit, summaries and leak_to_l1 need level L1L2")
	}
	if c.OverloadOnStartup < 0 || c.OverloadOnStartup > 86400 {
		return errors.New("overload_on_startup_s must be 0-86400")
	}
	for _, x := range c.Summaries {
		if _, err := netip.ParsePrefix(x.Prefix); err != nil {
			return fmt.Errorf("summaries: %q: %w", x.Prefix, err)
		}
		if x.Metric > maxPathMetric {
			return fmt.Errorf("summaries: %s: metric too large", x.Prefix)
		}
	}
	for _, x := range c.LeakToL1 {
		if _, err := netip.ParsePrefix(x); err != nil {
			return fmt.Errorf("leak_to_l1: %q: %w", x, err)
		}
	}
	if err := c.BFD.validate(); err != nil {
		return err
	}
	if h := c.GracefulRestart.HoldS; h < 0 || h > 65535 {
		return errors.New("graceful_restart: restart_hold_s must be 1-65535")
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

// parseLoopback takes an address with or without a prefix length (a bare
// address is a host prefix).
func parseLoopback(s string) (netip.Prefix, error) {
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return p, fmt.Errorf("loopback %q: not an address or prefix", s)
	}
	return p, nil
}

func validGroups(scope string, gs []int) error {
	for _, g := range gs {
		if g < 0 || g > 31 {
			return fmt.Errorf("%s: admin groups are 0-31", scope)
		}
	}
	return nil
}

// validateFlexAlgos checks flex_algos and link attributes; it returns the
// configured algorithms.
func (c *Config) validateFlexAlgos() (map[byte]bool, error) {
	algos := map[byte]bool{}
	for _, f := range c.FlexAlgos {
		if f.Algorithm < 128 {
			return nil, fmt.Errorf("flex_algos: algorithm %d: flex-algos are 128-255", f.Algorithm)
		}
		if algos[f.Algorithm] {
			return nil, fmt.Errorf("flex_algos: algorithm %d twice", f.Algorithm)
		}
		algos[f.Algorithm] = true
		if _, ok := fadMetricTypes[f.MetricType]; f.MetricType != "" && !ok {
			return nil, fmt.Errorf("flex_algos: algorithm %d: metric_type must be igp, delay or te", f.Algorithm)
		}
		for _, gs := range [][]int{f.ExcludeAny, f.IncludeAny, f.IncludeAll} {
			if err := validGroups(fmt.Sprintf("flex_algos %d", f.Algorithm), gs); err != nil {
				return nil, err
			}
		}
	}
	if err := validGroups("admin_groups", c.AdminGroups); err != nil {
		return nil, err
	}
	for _, f := range c.Interfaces {
		if err := validGroups("interface "+f.Device, f.AdminGroups); err != nil {
			return nil, err
		}
	}
	for _, n := range c.Topology.Custom {
		for _, l := range n.Links {
			if err := validGroups("topology link "+n.Name+"-"+l.To, l.AdminGroups); err != nil {
				return nil, err
			}
		}
	}
	return algos, nil
}

func (s SRv6) validate(algos map[byte]bool) error {
	if !s.Enabled {
		return nil
	}
	if len(s.Locators) == 0 {
		return errors.New("srv6: at least one locator")
	}
	if _, ok := srv6Behaviors[s.Behavior]; s.Behavior != "" && !ok {
		return fmt.Errorf("srv6: unknown behavior %q", s.Behavior)
	}
	for _, l := range s.Locators {
		p, err := netip.ParsePrefix(l.Prefix)
		if err != nil || !p.Addr().Is6() || p.Bits() > 112 {
			return fmt.Errorf("srv6: locator %q: want an IPv6 prefix of at most /112", l.Prefix)
		}
		if l.Algorithm != 0 && !algos[l.Algorithm] {
			return fmt.Errorf("srv6: locator %s: algorithm %d is not in flex_algos", l.Prefix, l.Algorithm)
		}
		if l.EndSID != "" {
			a, err := netip.ParseAddr(l.EndSID)
			if err != nil || !p.Contains(a) {
				return fmt.Errorf("srv6: end_sid %q is not inside locator %s", l.EndSID, l.Prefix)
			}
		}
	}
	if s.TopologyLocatorBase != "" {
		p, err := netip.ParsePrefix(s.TopologyLocatorBase)
		if err != nil || !p.Addr().Is6() || p.Bits() > 112 {
			return fmt.Errorf("srv6: topology_locator_base %q: want an IPv6 prefix of at most /112", s.TopologyLocatorBase)
		}
	}
	if st := s.Structure; st != nil && int(st.LB)+int(st.LN)+int(st.Fun)+int(st.Arg) > 128 {
		return errors.New("srv6: structure adds up to more than 128 bits")
	}
	return nil
}
