package ospf

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
)

// Config is one emulated OSPFv2 router.
type Config struct {
	RouterID string `json:"router_id,omitempty" jsonschema:"description=Default: the first IPv4 loopback, else the device's first IPv4 address"`
	Area     AreaID `json:"area" jsonschema:"description=Area ID (dotted, like 0.0.0.0, or a number) of the first interface, the loopbacks and internal prefixes"`
	// Areas sets area types and ranges; interfaces name their area.
	Areas    []AreaConfig `json:"areas,omitempty" jsonschema:"description=Area settings (type, ranges). An area needs an entry only to change the defaults (normal, no ranges)"`
	Network  string       `json:"network" jsonschema:"enum=broadcast,enum=point-to-point,enum=non-broadcast,enum=point-to-multipoint,description=Interface type (non-broadcast: NBMA, hellos to neighbors)"`
	Cost     uint16       `json:"cost" jsonschema:"minimum=1,maximum=65535,description=Interface cost"`
	Priority uint8        `json:"priority" jsonschema:"description=DR election priority (broadcast; 0: never DR/BDR)"`
	Passive  bool         `json:"passive,omitempty" jsonschema:"description=Advertise the interface's subnet without hellos (no adjacency)"`
	// Neighbors of the first interface (non-broadcast networks).
	Neighbors []string `json:"neighbors,omitempty" jsonschema:"description=Neighbor addresses for non-broadcast networks (NBMA, or point-to-multipoint without multicast): hellos go to each"`

	// Interfaces are further devices of this router (the instance's own
	// device is its first interface, with the settings above).
	Interfaces []Interface `json:"interfaces,omitempty" jsonschema:"description=Further interfaces: devices of this router besides the instance's own"`

	HelloInterval      int  `json:"hello_interval" jsonschema:"minimum=1,maximum=65535,description=Seconds"`
	DeadInterval       int  `json:"dead_interval" jsonschema:"minimum=2,description=Seconds (must match the neighbor's)"`
	RetransmitInterval int  `json:"retransmit_interval" jsonschema:"minimum=1,maximum=3600,description=Seconds between retransmissions of DBDs, requests and LSAs"`
	MTU                int  `json:"mtu" jsonschema:"minimum=576,maximum=9600,description=IP MTU sent in database descriptions and used to pack packets"`
	MTUIgnore          bool `json:"mtu_ignore,omitempty" jsonschema:"description=Accept neighbors with a larger MTU"`
	LSARefresh         int  `json:"lsa_refresh" jsonschema:"minimum=10,maximum=3599,description=Seconds between refreshes of our LSAs"`

	Auth Auth `json:"auth" jsonschema:"description=Packet authentication on the interfaces"`

	MaxMetric bool `json:"max_metric,omitempty" jsonschema:"description=Stub router (RFC 6987): advertise transit links at 65535"`
	// Interface subnets are advertised automatically.
	Prefixes []Prefix `json:"prefixes,omitempty" jsonschema:"description=Extra prefixes to advertise (route injection)"`
	// Loopbacks are the router's own addresses, answered on every interface.
	Loopbacks []string `json:"loopbacks,omitempty" jsonschema:"description=Router loopback addresses (e.g. 192.0.2.60/32): stub links at cost 0, answered (ping, flows) on every interface; the first is the default router ID"`

	Topology Topology `json:"topology" jsonschema:"description=Simulated routers behind this one; their LSAs are originated here"`

	SegmentRouting     SegmentRouting  `json:"segment_routing" jsonschema:"description=Segment routing (RFC 8665; MPLS): SRGB, node SIDs, adjacency SIDs in opaque LSAs"`
	FlexAlgos          []FlexAlgo      `json:"flex_algos,omitempty" jsonschema:"description=Flexible algorithms (RFC 9350) we take part in, optionally with their definitions"`
	TrafficEngineering bool            `json:"traffic_engineering,omitempty" jsonschema:"description=Originate TE LSAs (RFC 3630): router address and a link TLV per adjacency"`
	GracefulRestart    GracefulRestart `json:"graceful_restart" jsonschema:"description=Graceful restart (RFC 3623 / 5187): helper mode and our grace period"`
	VirtualLinks       []VirtualLink   `json:"virtual_links,omitempty" jsonschema:"description=Virtual links to other area border routers across transit areas (backbone connectivity)"`
	BFD                BFDConfig       `json:"bfd" jsonschema:"description=BFD (RFC 5880/5881) with every neighbor: a failed session drops the neighbor at once"`
	LinkAttrs                          // of the first interface
}

// LinkAttrs are an interface's TE attributes: in TE LSAs and, for
// flex-algo, in application-specific link attributes (RFC 8920).
type LinkAttrs struct {
	TEMetric      uint32 `json:"te_metric,omitempty" jsonschema:"description=TE metric (flex-algo metric type te). Default: none (TE LSAs use the cost)"`
	DelayUS       uint32 `json:"delay_us,omitempty" jsonschema:"maximum=16777215,description=Link delay in µs (flex-algo metric type delay)"`
	AdminGroups   []int  `json:"admin_groups,omitempty" jsonschema:"description=Admin groups 0-31 (flex-algo include/exclude)"`
	BandwidthMbps uint32 `json:"bandwidth_mbps,omitempty" jsonschema:"description=Maximum bandwidth in TE LSAs. Default 1000"`
}

// SegmentRouting configures SR-MPLS advertisements.
type SegmentRouting struct {
	Enabled    bool      `json:"enabled"`
	SRGBBase   uint32    `json:"srgb_base" jsonschema:"minimum=16,maximum=1048575,description=First label of the SR global block"`
	SRGBSize   uint32    `json:"srgb_size" jsonschema:"minimum=1,maximum=1048560"`
	NodeSIDs   []NodeSID `json:"node_sids,omitempty" jsonschema:"description=Prefix-SIDs on our prefixes (loopbacks, internal prefixes or interface subnets); /32s get the node flag"`
	AdjSIDBase uint32    `json:"adj_sid_base" jsonschema:"description=Local label for our first adjacency SID (the others count up); 0: no adjacency SIDs"`
	SRLBBase   uint32    `json:"srlb_base,omitempty" jsonschema:"description=First label of the SR local block (advertised; 0: none)"`
	SRLBSize   uint32    `json:"srlb_size,omitempty"`
}

type NodeSID struct {
	Prefix       string `json:"prefix" jsonschema:"description=e.g. 192.0.2.60/32"`
	Index        uint32 `json:"index" jsonschema:"description=Index into the SRGB"`
	Algorithm    byte   `json:"algorithm,omitempty" jsonschema:"description=0 (SPF) or a flex-algo (128-255) from flex_algos"`
	NoPHP        bool   `json:"no_php,omitempty" jsonschema:"description=Ask the penultimate hop not to pop (NP flag)"`
	ExplicitNull bool   `json:"explicit_null,omitempty" jsonschema:"description=Ask for explicit null (E flag)"`
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

// Interface is one more interface of the router. Empty fields take the
// router-wide value.
type Interface struct {
	Device   string `json:"device" jsonschema:"description=Device name"`
	Network  string `json:"network,omitempty" jsonschema:"enum=broadcast,enum=point-to-point,enum=non-broadcast,enum=point-to-multipoint"`
	Cost     uint16 `json:"cost,omitempty" jsonschema:"minimum=1,maximum=65535"`
	Priority *uint8 `json:"priority,omitempty"`
	Passive  bool   `json:"passive,omitempty"`
	Area     AreaID `json:"area,omitempty" jsonschema:"description=Default: the router's area. Interfaces in more than one area make this an area border router"`
	Auth     *Auth  `json:"auth,omitempty" jsonschema:"description=Default: the router's auth"`
	// Neighbors on a non-broadcast network.
	Neighbors     []string `json:"neighbors,omitempty" jsonschema:"description=Neighbor addresses (non-broadcast networks)"`
	BFD           *bool    `json:"bfd,omitempty" jsonschema:"description=BFD on this interface. Default: the router's bfd.enabled"`
	HelloInterval int      `json:"hello_interval,omitempty" jsonschema:"description=Seconds. Default: the router's"`
	DeadInterval  int      `json:"dead_interval,omitempty" jsonschema:"description=Seconds. Default: the router's"`
	LinkAttrs
}

// AreaConfig is one area's type and the ranges we summarise it with as an
// area border router.
type AreaConfig struct {
	ID          AreaID      `json:"id" jsonschema:"description=Area ID"`
	Type        string      `json:"type,omitempty" jsonschema:"enum=normal,enum=stub,enum=nssa,description=Default normal. stub: no AS-external LSAs; nssa: externals as type-7 LSAs (RFC 3101)"`
	NoSummary   bool        `json:"no_summary,omitempty" jsonschema:"description=stub / nssa: an area border router sends only a default summary into the area (totally stubby)"`
	DefaultCost uint32      `json:"default_cost,omitempty" jsonschema:"description=Cost of the default summary into a stub or NSSA area. Default 1"`
	NSSADefault bool        `json:"nssa_default,omitempty" jsonschema:"description=nssa: an area border router also originates a type-7 default into the area"`
	Translate   string      `json:"nssa_translate,omitempty" jsonschema:"enum=candidate,enum=always,enum=never,description=nssa: translating type-7 LSAs to AS-external ones as a border router. candidate (default): when ours is the highest router ID of the area's border routers"`
	Ranges      []AreaRange `json:"ranges,omitempty" jsonschema:"description=Address ranges summarising this area's prefixes into the other areas"`
}

type AreaRange struct {
	Prefix      string `json:"prefix"`
	NoAdvertise bool   `json:"no_advertise,omitempty" jsonschema:"description=Hide the range (and the prefixes in it) from the other areas"`
	Cost        uint32 `json:"cost,omitempty" jsonschema:"description=Cost of the summary. Default: the largest cost of the prefixes in it"`
}

type Auth struct {
	Type  string `json:"type" jsonschema:"enum=none,enum=simple,enum=md5,enum=hmac-sha1,enum=hmac-sha256,enum=hmac-sha384,enum=hmac-sha512,enum=keychain,description=none / simple (cleartext password) / md5 (keyed MD5) / hmac-sha* (RFC 5709) / keychain"`
	Key   string `json:"key,omitempty" jsonschema:"description=Password (simple: up to 8 bytes; md5: up to 16)"`
	KeyID uint8  `json:"key_id,omitempty" jsonschema:"description=Key ID (md5, hmac-sha*)"`
	SAID  uint16 `json:"-"` // OSPFv3
	// Keychain (type keychain): keys with send and accept windows, for
	// rollover without dropping adjacencies.
	Keys []AuthKey `json:"keys,omitempty" jsonschema:"description=For type keychain: keys (md5 / hmac-sha*) with send/accept lifetimes"`
}

// AuthKey is one keychain entry. Times are RFC 3339; empty means unbounded.
type AuthKey struct {
	ID          uint16 `json:"id" jsonschema:"description=Key ID (OSPFv2: 0-255; OSPFv3: the security association ID; on SR OS the keychain entry)"`
	Type        string `json:"type" jsonschema:"enum=md5,enum=hmac-sha1,enum=hmac-sha256,enum=hmac-sha384,enum=hmac-sha512"`
	Key         string `json:"key"`
	SendFrom    string `json:"send_from,omitempty" jsonschema:"description=Start sending with this key (RFC 3339); the valid key that started last is used"`
	SendUntil   string `json:"send_until,omitempty"`
	AcceptFrom  string `json:"accept_from,omitempty" jsonschema:"description=Start accepting this key (RFC 3339)"`
	AcceptUntil string `json:"accept_until,omitempty"`
}

// Auth3 is OSPFv3's authentication trailer (RFC 7166).
type Auth3 struct {
	Type string    `json:"type" jsonschema:"enum=none,enum=hmac-sha1,enum=hmac-sha256,enum=hmac-sha384,enum=hmac-sha512,enum=keychain"`
	Key  string    `json:"key,omitempty"`
	SAID uint16    `json:"sa_id,omitempty" jsonschema:"description=Security association ID (the peer's key ID)"`
	Keys []AuthKey `json:"keys,omitempty" jsonschema:"description=For type keychain: keys (hmac-sha*) with send/accept lifetimes; the ID is the SA ID"`
}

func (a Auth) Validate() error { return a.validate(false) }

func (a Auth) validate(v3 bool) error {
	switch a.Type {
	case "keychain":
		if len(a.Keys) == 0 {
			return errors.New("auth: keychain needs keys")
		}
		ids := map[uint16]bool{}
		for _, k := range a.Keys {
			if ids[k.ID] {
				return fmt.Errorf("auth: key ID %d twice", k.ID)
			}
			ids[k.ID] = true
			if !v3 && k.ID > 255 {
				return fmt.Errorf("auth: key ID %d: OSPFv2 key IDs are 0-255", k.ID)
			}
			if k.Type != "md5" && shaAlgos[k.Type].h == nil || v3 && k.Type == "md5" {
				return fmt.Errorf("auth: key %d: type must be %s", k.ID, map[bool]string{false: "md5 or hmac-sha*", true: "hmac-sha*"}[v3])
			}
			if err := k.single(v3).validate(v3); err != nil {
				return fmt.Errorf("key %d: %w", k.ID, err)
			}
			for _, t := range []string{k.SendFrom, k.SendUntil, k.AcceptFrom, k.AcceptUntil} {
				if _, err := parseWhen(t); err != nil {
					return fmt.Errorf("auth: key %d: time %q is not RFC 3339 (e.g. 2026-10-22T09:00:00Z)", k.ID, t)
				}
			}
		}
		return nil
	case "", "none":
	case "simple":
		if len(a.Key) > 8 {
			return errors.New("auth: a simple password is at most 8 bytes")
		}
	case "md5":
		if a.Key == "" || len(a.Key) > 16 {
			return errors.New("auth: an md5 key is 1-16 bytes")
		}
	default:
		if _, ok := shaAlgos[a.Type]; !ok {
			return fmt.Errorf("auth: unknown type %q", a.Type)
		}
		if a.Key == "" || len(a.Key) > 255 {
			return errors.New("auth: an hmac-sha key is 1-255 bytes")
		}
	}
	return nil
}

func (a Auth3) Validate() error {
	if a.Type == "keychain" {
		return a.common().validate(true)
	}
	if a.Type == "" || a.Type == "none" {
		return nil
	}
	if _, ok := shaAlgos[a.Type]; !ok {
		return fmt.Errorf("auth: unknown type %q (OSPFv3 takes hmac-sha1/256/384/512)", a.Type)
	}
	if a.Key == "" || len(a.Key) > 255 {
		return errors.New("auth: a key is 1-255 bytes")
	}
	return nil
}

// common is the session's form (one Auth for both versions).
func (a Auth3) common() Auth { return Auth{Type: a.Type, Key: a.Key, SAID: a.SAID, Keys: a.Keys} }

// Prefix is advertised as an AS-external LSA (we become an ASBR; type-7 in
// an NSSA), with type internal as a stub link in our router LSA (OSPFv3:
// our intra-area-prefix LSA), or with type inter-area as a summary LSA in
// every area (as an area border router would).
type Prefix struct {
	Prefix  string `json:"prefix" jsonschema:"description=e.g. 198.18.0.0/24"`
	Count   int    `json:"count,omitempty" jsonschema:"description=Consecutive prefixes of this length. Default 1"`
	Metric  uint32 `json:"metric,omitempty" jsonschema:"description=Default 20 (external) / 10 (internal)"`
	Type    string `json:"type,omitempty" jsonschema:"enum=external-2,enum=external-1,enum=internal,enum=inter-area,description=Default external-2. internal prefixes are stub links in our router LSA; inter-area ones summary LSAs"`
	Tag     uint32 `json:"tag,omitempty" jsonschema:"description=External route tag"`
	Forward string `json:"forwarding_address,omitempty" jsonschema:"description=External forwarding address. Default 0.0.0.0 (us)"`
}

// route is one expanded prefix.
type route struct {
	prefix  netip.Prefix
	metric  uint32
	kind    string // external-1, external-2, internal, inter-area
	tag     uint32
	forward netip.Addr
}

const maxPrefixes = 200000

// expand lists the prefixes an entry stands for (IPv6 for OSPFv3).
func (p Prefix) expand(v6 bool) ([]route, error) {
	pf, err := netip.ParsePrefix(p.Prefix)
	if err != nil || pf.Addr().Is4() == v6 {
		return nil, fmt.Errorf("prefix %q: want an %s prefix", p.Prefix, family(v6))
	}
	pf = pf.Masked()
	n := p.Count
	if n == 0 {
		n = 1
	}
	if n < 0 || n > maxPrefixes {
		return nil, fmt.Errorf("prefix %s: count must be 1-%d", p.Prefix, maxPrefixes)
	}
	r := route{metric: p.Metric, kind: p.Type, tag: p.Tag}
	if !v6 {
		r.forward = zero4
	}
	switch r.kind {
	case "":
		r.kind = "external-2"
	case "external-1", "external-2", "internal", "inter-area":
	default:
		return nil, fmt.Errorf("prefix %s: type must be external-1, external-2, internal or inter-area", p.Prefix)
	}
	if r.metric == 0 {
		r.metric = 20
		if r.kind == "internal" || r.kind == "inter-area" {
			r.metric = 10
		}
	}
	if r.metric >= LSInfinity || (r.kind == "internal" && r.metric > 0xffff) {
		return nil, fmt.Errorf("prefix %s: metric too large", p.Prefix)
	}
	if p.Forward != "" {
		a, err := netip.ParseAddr(p.Forward)
		if err != nil || a.Is4() == v6 {
			return nil, fmt.Errorf("prefix %s: bad forwarding address", p.Prefix)
		}
		r.forward = a
	}
	out := make([]route, 0, n)
	for i := 0; i < n; i++ {
		if i > 0 {
			next, ok := nextPrefix(pf)
			if !ok {
				return nil, fmt.Errorf("prefix %s: count runs past the address space", p.Prefix)
			}
			pf = next
		}
		r.prefix = pf
		out = append(out, r)
	}
	return out, nil
}

func family(v6 bool) string {
	if v6 {
		return "IPv6"
	}
	return "IPv4"
}

// nextPrefix is the following prefix of the same length (either family).
func nextPrefix(p netip.Prefix) (netip.Prefix, bool) {
	b := p.Addr().AsSlice()
	if p.Bits() == 0 {
		return p, false
	}
	// Add one at the last prefix bit, carrying towards the front.
	bit := p.Bits() - 1
	for i := bit / 8; i >= 0; i-- {
		inc := byte(1)
		if i == bit/8 {
			inc = 1 << (7 - bit%8)
		}
		old := b[i]
		b[i] += inc
		if b[i] > old {
			a, _ := netip.AddrFromSlice(b)
			return netip.PrefixFrom(a, p.Bits()), true
		}
		bit = i*8 - 1
	}
	return p, false
}

func defaultConfig() *Config {
	return &Config{Area: "0.0.0.0", Network: "broadcast", Cost: 10, Priority: 1, HelloInterval: 10, DeadInterval: 40,
		RetransmitInterval: 5, MTU: 1500, LSARefresh: 1800, Auth: Auth{Type: "none"},
		SegmentRouting: SegmentRouting{SRGBBase: 16000, SRGBSize: 8000, AdjSIDBase: 24000}, GracefulRestart: GracefulRestart{Helper: true, GracePeriod: 120}}
}

// ParseArea accepts dotted (0.0.0.1) or decimal (1) area IDs.
func ParseArea(s string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
		return a, nil
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("area %q: want dotted (0.0.0.0) or a number", s)
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	return netip.AddrFrom4(b), nil
}

func parseLoopback(s string, v6 bool) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		a, err2 := netip.ParseAddr(s)
		if err2 != nil {
			return netip.Prefix{}, fmt.Errorf("loopback %q: want an address or prefix", s)
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if p.Addr().Is4() == v6 {
		return netip.Prefix{}, fmt.Errorf("loopback %q: want an %s address", s, family(v6))
	}
	return p, nil
}

func (c *Config) Validate() error {
	if c.MTU < 576 || c.MTU > 9600 {
		return errors.New("mtu must be 576-9600")
	}
	if err := c.validateCommon(); err != nil {
		return err
	}
	if err := validateTopology(c.Topology, false); err != nil {
		return err
	}
	if err := c.validateSR(false); err != nil {
		return err
	}
	if err := validateVirtualLinks(c); err != nil {
		return err
	}
	return validatePrefixes(c.Prefixes, c.Loopbacks, false)
}

// validateCommon checks what both versions share.
func (c *Config) validateCommon() error {
	if c.RouterID != "" {
		if a, err := netip.ParseAddr(c.RouterID); err != nil || !a.Is4() || a == zero4 {
			return fmt.Errorf("router_id %q: want a non-zero IPv4 address", c.RouterID)
		}
	}
	if _, err := ParseArea(string(c.Area)); err != nil {
		return err
	}
	if err := validateAreas(c); err != nil {
		return err
	}
	nets := []string{c.Network}
	if err := validNeighbors(c.Network, c.Neighbors); err != nil {
		return err
	}
	for _, ic := range c.Interfaces {
		if err := validTimers(ic.HelloInterval, ic.DeadInterval, c); err != nil {
			return fmt.Errorf("interface %s: %w", ic.Device, err)
		}
		if err := validNeighbors(ic.Network, ic.Neighbors); err != nil {
			return fmt.Errorf("interface %s: %w", ic.Device, err)
		}
		if ic.Device == "" {
			return errors.New("interfaces: device is required")
		}
		if ic.Area != "" {
			if _, err := ParseArea(string(ic.Area)); err != nil {
				return fmt.Errorf("interface %s: %w", ic.Device, err)
			}
		}
		if ic.Network != "" {
			nets = append(nets, ic.Network)
		}
		if ic.Auth != nil {
			if err := ic.Auth.Validate(); err != nil {
				return fmt.Errorf("interface %s: %w", ic.Device, err)
			}
		}
	}
	for _, n := range nets {
		switch n {
		case "broadcast", "point-to-point", "non-broadcast", "point-to-multipoint":
		default:
			return fmt.Errorf("network must be broadcast, point-to-point, non-broadcast or point-to-multipoint, not %q", n)
		}
	}
	if c.Cost == 0 {
		return errors.New("cost must be 1-65535")
	}
	if c.HelloInterval < 1 || c.HelloInterval > 65535 {
		return errors.New("hello_interval must be 1-65535")
	}
	if c.DeadInterval <= c.HelloInterval {
		return errors.New("dead_interval must be longer than hello_interval")
	}
	if c.RetransmitInterval < 1 {
		return errors.New("retransmit_interval must be at least 1")
	}
	if c.LSARefresh < 10 || c.LSARefresh >= MaxAge {
		return errors.New("lsa_refresh must be 10-3599")
	}
	if err := c.BFD.validate(); err != nil {
		return err
	}
	if g := c.GracefulRestart.GracePeriod; g < 0 || g > 1800 {
		return errors.New("graceful_restart: grace_period must be 1-1800")
	}
	if err := c.Auth.Validate(); err != nil {
		return err
	}
	return nil
}

func validatePrefixes(prefixes []Prefix, loopbacks []string, v6 bool) error {
	seen := map[netip.Prefix]bool{}
	for _, p := range prefixes {
		rs, err := p.expand(v6)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if seen[r.prefix] {
				return fmt.Errorf("prefix %s is listed twice (check overlapping counts)", r.prefix)
			}
			seen[r.prefix] = true
		}
	}
	total := len(seen)
	if total > maxPrefixes {
		return fmt.Errorf("at most %d prefixes", maxPrefixes)
	}
	for _, l := range loopbacks {
		if _, err := parseLoopback(l, v6); err != nil {
			return err
		}
	}
	return nil
}

// ConfigV3 is one emulated OSPFv3 router (IPv6, RFC 5340) in one area.
type ConfigV3 struct {
	RouterID   string       `json:"router_id,omitempty" jsonschema:"description=Dotted 32-bit ID. Default: the device's first IPv4 address (required if it has none)"`
	Area       AreaID       `json:"area" jsonschema:"description=Area ID (dotted, like 0.0.0.0, or a number) of the first interface, the loopbacks and internal prefixes"`
	Areas      []AreaConfig `json:"areas,omitempty" jsonschema:"description=Area settings (type, ranges). An area needs an entry only to change the defaults (normal, no ranges)"`
	InstanceID uint8        `json:"instance_id,omitempty" jsonschema:"description=Instance ID in packets (0 for IPv6 unicast)"`
	Network    string       `json:"network" jsonschema:"enum=broadcast,enum=point-to-point,enum=non-broadcast,enum=point-to-multipoint,description=Interface type (non-broadcast: NBMA, hellos to neighbors)"`
	Cost       uint16       `json:"cost" jsonschema:"minimum=1,maximum=65535,description=Interface cost"`
	Priority   uint8        `json:"priority" jsonschema:"description=DR election priority (broadcast; 0: never DR/BDR)"`
	Passive    bool         `json:"passive,omitempty" jsonschema:"description=Advertise the interface's prefixes without hellos (no adjacency)"`
	Neighbors  []string     `json:"neighbors,omitempty" jsonschema:"description=Neighbors' link-local addresses for non-broadcast networks"`

	Interfaces []Interface3 `json:"interfaces,omitempty" jsonschema:"description=Further interfaces: devices of this router besides the instance's own"`

	HelloInterval      int  `json:"hello_interval" jsonschema:"minimum=1,maximum=65535,description=Seconds"`
	DeadInterval       int  `json:"dead_interval" jsonschema:"minimum=2,maximum=65535,description=Seconds (must match the neighbor's)"`
	RetransmitInterval int  `json:"retransmit_interval" jsonschema:"minimum=1,maximum=3600,description=Seconds between retransmissions of DBDs, requests and LSAs"`
	MTU                int  `json:"mtu" jsonschema:"minimum=1280,maximum=9600,description=IPv6 MTU sent in database descriptions and used to pack packets"`
	MTUIgnore          bool `json:"mtu_ignore,omitempty" jsonschema:"description=Accept neighbors with a larger MTU"`
	LSARefresh         int  `json:"lsa_refresh" jsonschema:"minimum=10,maximum=3599,description=Seconds between refreshes of our LSAs"`

	Auth Auth3 `json:"auth" jsonschema:"description=Authentication trailer (RFC 7166) on the interfaces"`

	MaxMetric bool     `json:"max_metric,omitempty" jsonschema:"description=Stub router (RFC 6987): advertise transit links at 65535"`
	Prefixes  []Prefix `json:"prefixes,omitempty" jsonschema:"description=Extra IPv6 prefixes to advertise: AS-external, or internal (in our intra-area-prefix LSA)"`
	Loopbacks []string `json:"loopbacks,omitempty" jsonschema:"description=Router loopback addresses (e.g. 2001:db8:ffff::60/128): advertised at cost 0 and answered on every interface"`

	Topology Topology `json:"topology" jsonschema:"description=Simulated routers behind this one; their LSAs are originated here"`

	GracefulRestart GracefulRestart `json:"graceful_restart" jsonschema:"description=Graceful restart (RFC 5187): helper mode and our grace period"`
	BFD             BFDConfig       `json:"bfd" jsonschema:"description=BFD (RFC 5880/5881) with every neighbor"`
	VirtualLinks    []VirtualLink   `json:"virtual_links,omitempty" jsonschema:"description=Virtual links to other area border routers across transit areas"`
	SegmentRouting  SegmentRouting  `json:"segment_routing" jsonschema:"description=Segment routing (RFC 8666; MPLS) in extended LSAs (RFC 8362 sparse mode): SRGB, IPv6 node SIDs, adjacency SIDs"`
}

type Interface3 struct {
	Device        string   `json:"device" jsonschema:"description=Device name"`
	Network       string   `json:"network,omitempty" jsonschema:"enum=broadcast,enum=point-to-point,enum=non-broadcast,enum=point-to-multipoint"`
	Cost          uint16   `json:"cost,omitempty" jsonschema:"minimum=1,maximum=65535"`
	Priority      *uint8   `json:"priority,omitempty"`
	Passive       bool     `json:"passive,omitempty"`
	Area          AreaID   `json:"area,omitempty" jsonschema:"description=Default: the router's area"`
	Auth          *Auth3   `json:"auth,omitempty" jsonschema:"description=Default: the router's auth"`
	Neighbors     []string `json:"neighbors,omitempty" jsonschema:"description=Neighbors' link-local addresses (non-broadcast networks)"`
	BFD           *bool    `json:"bfd,omitempty"`
	HelloInterval int      `json:"hello_interval,omitempty"`
	DeadInterval  int      `json:"dead_interval,omitempty"`
}

func defaultConfigV3() *ConfigV3 {
	return &ConfigV3{Area: "0.0.0.0", Network: "broadcast", Cost: 10, Priority: 1, HelloInterval: 10, DeadInterval: 40,
		RetransmitInterval: 5, MTU: 1500, LSARefresh: 1800, Auth: Auth3{Type: "none"}, GracefulRestart: GracefulRestart{Helper: true, GracePeriod: 120},
		SegmentRouting: SegmentRouting{SRGBBase: 16000, SRGBSize: 8000, AdjSIDBase: 24000}}
}

// common is the shared form the session runs on.
func (c *ConfigV3) common() Config {
	out := Config{RouterID: c.RouterID, Area: c.Area, Areas: c.Areas, Network: c.Network, Neighbors: c.Neighbors, Cost: c.Cost, Priority: c.Priority, Passive: c.Passive,
		HelloInterval: c.HelloInterval, DeadInterval: c.DeadInterval, RetransmitInterval: c.RetransmitInterval, MTU: c.MTU,
		MTUIgnore: c.MTUIgnore, LSARefresh: c.LSARefresh, Auth: c.Auth.common(), MaxMetric: c.MaxMetric,
		Prefixes: c.Prefixes, Loopbacks: c.Loopbacks, Topology: c.Topology, GracefulRestart: c.GracefulRestart, BFD: c.BFD,
		SegmentRouting: c.SegmentRouting, VirtualLinks: c.VirtualLinks}
	for _, ic := range c.Interfaces {
		i := Interface{Device: ic.Device, Network: ic.Network, Cost: ic.Cost, Priority: ic.Priority, Passive: ic.Passive, Area: ic.Area, Neighbors: ic.Neighbors, BFD: ic.BFD,
			HelloInterval: ic.HelloInterval, DeadInterval: ic.DeadInterval}
		if ic.Auth != nil {
			a := ic.Auth.common()
			i.Auth = &a
		}
		out.Interfaces = append(out.Interfaces, i)
	}
	return out
}

func (c *ConfigV3) Validate() error {
	cc := c.common()
	if c.MTU < 1280 || c.MTU > 9600 {
		return errors.New("mtu must be 1280-9600")
	}
	if c.DeadInterval > 65535 {
		return errors.New("dead_interval must be at most 65535 (16 bits in OSPFv3)")
	}
	if err := c.Auth.Validate(); err != nil {
		return err
	}
	for _, ic := range c.Interfaces {
		if ic.Auth != nil {
			if err := ic.Auth.Validate(); err != nil {
				return fmt.Errorf("interface %s: %w", ic.Device, err)
			}
		}
	}
	if err := cc.validateCommon(); err != nil {
		return err
	}
	if err := validateTopology(c.Topology, true); err != nil {
		return err
	}
	if err := cc.validateSR(true); err != nil {
		return err
	}
	if err := validateVirtualLinks(&cc); err != nil {
		return err
	}
	return validatePrefixes(c.Prefixes, c.Loopbacks, true)
}

func validateTopology(t Topology, v6 bool) error {
	if t.Kind == "" || t.Kind == "none" {
		return nil
	}
	if _, err := buildTopology(t, zero4, v6); err != nil {
		return fmt.Errorf("topology: %w", err)
	}
	return nil
}

// AreaID is an area ID as configured: dotted (0.0.0.1) or a number, which
// may also come as a JSON number (YAML area: 1).
type AreaID string

func (a *AreaID) UnmarshalJSON(b []byte) error {
	var n uint32
	if err := json.Unmarshal(b, &n); err == nil {
		*a = AreaID(strconv.FormatUint(uint64(n), 10))
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("area: want a dotted ID or a number")
	}
	*a = AreaID(s)
	return nil
}

// validNeighbors checks configured neighbors: addresses, on a network type
// that uses them (empty: the router's).
func validNeighbors(network string, ns []string) error {
	for _, a := range ns {
		if _, err := netip.ParseAddr(a); err != nil {
			return fmt.Errorf("neighbor %q: want an address", a)
		}
	}
	if len(ns) > 0 && network != "" && network != "non-broadcast" && network != "point-to-multipoint" {
		return errors.New("neighbors are for non-broadcast and point-to-multipoint networks")
	}
	return nil
}

// validTimers checks an interface's timer overrides (0: the router's).
func validTimers(hello, dead int, c *Config) error {
	if hello < 0 || dead < 0 || hello > 65535 {
		return errors.New("bad hello_interval or dead_interval")
	}
	if hello == 0 {
		hello = c.HelloInterval
	}
	if dead == 0 {
		dead = c.DeadInterval
	}
	if dead <= hello {
		return errors.New("dead_interval must be longer than hello_interval")
	}
	return nil
}
