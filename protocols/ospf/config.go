package ospf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
)

// Config is one emulated OSPFv2 router in one area.
type Config struct {
	RouterID string `json:"router_id,omitempty" jsonschema:"description=Default: the first IPv4 loopback, else the device's first IPv4 address"`
	Area     string `json:"area" jsonschema:"description=Area ID (dotted, like 0.0.0.0, or a number)"`
	Network  string `json:"network" jsonschema:"enum=broadcast,enum=point-to-point,description=Interface type"`
	Cost     uint16 `json:"cost" jsonschema:"minimum=1,maximum=65535,description=Interface cost"`
	Priority uint8  `json:"priority" jsonschema:"description=DR election priority (broadcast; 0: never DR/BDR)"`
	Passive  bool   `json:"passive,omitempty" jsonschema:"description=Advertise the interface's subnet without hellos (no adjacency)"`

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
}

// Interface is one more interface of the router. Empty fields take the
// router-wide value.
type Interface struct {
	Device   string `json:"device" jsonschema:"description=Device name"`
	Network  string `json:"network,omitempty" jsonschema:"enum=broadcast,enum=point-to-point"`
	Cost     uint16 `json:"cost,omitempty" jsonschema:"minimum=1,maximum=65535"`
	Priority *uint8 `json:"priority,omitempty"`
	Passive  bool   `json:"passive,omitempty"`
	Auth     *Auth  `json:"auth,omitempty" jsonschema:"description=Default: the router's auth"`
}

type Auth struct {
	Type  string `json:"type" jsonschema:"enum=none,enum=simple,enum=md5,enum=hmac-sha1,enum=hmac-sha256,enum=hmac-sha384,enum=hmac-sha512,description=none / simple (cleartext password) / md5 (keyed MD5) / hmac-sha* (RFC 5709)"`
	Key   string `json:"key,omitempty" jsonschema:"description=Password (simple: up to 8 bytes; md5: up to 16)"`
	KeyID uint8  `json:"key_id,omitempty" jsonschema:"description=Key ID (md5, hmac-sha*)"`
	SAID  uint16 `json:"-"` // OSPFv3
}

// Auth3 is OSPFv3's authentication trailer (RFC 7166).
type Auth3 struct {
	Type string `json:"type" jsonschema:"enum=none,enum=hmac-sha1,enum=hmac-sha256,enum=hmac-sha384,enum=hmac-sha512"`
	Key  string `json:"key,omitempty"`
	SAID uint16 `json:"sa_id,omitempty" jsonschema:"description=Security association ID (the peer's key ID)"`
}

func (a Auth) Validate() error {
	switch a.Type {
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
func (a Auth3) common() Auth { return Auth{Type: a.Type, Key: a.Key, SAID: a.SAID} }

// Prefix is advertised as an AS-external LSA (we become an ASBR) or, with
// type internal, as a stub link in our router LSA.
type Prefix struct {
	Prefix  string `json:"prefix" jsonschema:"description=e.g. 198.18.0.0/24"`
	Count   int    `json:"count,omitempty" jsonschema:"description=Consecutive prefixes of this length. Default 1"`
	Metric  uint32 `json:"metric,omitempty" jsonschema:"description=Default 20 (external) / 10 (internal)"`
	Type    string `json:"type,omitempty" jsonschema:"enum=external-2,enum=external-1,enum=internal,description=Default external-2. internal prefixes are stub links in our router LSA, which has to fit one packet"`
	Tag     uint32 `json:"tag,omitempty" jsonschema:"description=External route tag"`
	Forward string `json:"forwarding_address,omitempty" jsonschema:"description=External forwarding address. Default 0.0.0.0 (us)"`
}

// route is one expanded prefix.
type route struct {
	prefix  netip.Prefix
	metric  uint32
	kind    string // external-1, external-2, internal
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
	case "external-1", "external-2", "internal":
	default:
		return nil, fmt.Errorf("prefix %s: type must be external-1, external-2 or internal", p.Prefix)
	}
	if r.metric == 0 {
		r.metric = 20
		if r.kind == "internal" {
			r.metric = 10
		}
	}
	if r.metric > LSInfinity || (r.kind == "internal" && r.metric > 0xffff) {
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
		RetransmitInterval: 5, MTU: 1500, LSARefresh: 1800, Auth: Auth{Type: "none"}}
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
	return validatePrefixes(c.Prefixes, c.Loopbacks, false)
}

// validateCommon checks what both versions share.
func (c *Config) validateCommon() error {
	if c.RouterID != "" {
		if a, err := netip.ParseAddr(c.RouterID); err != nil || !a.Is4() || a == zero4 {
			return fmt.Errorf("router_id %q: want a non-zero IPv4 address", c.RouterID)
		}
	}
	if _, err := ParseArea(c.Area); err != nil {
		return err
	}
	nets := []string{c.Network}
	for _, ic := range c.Interfaces {
		if ic.Device == "" {
			return errors.New("interfaces: device is required")
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
		if n != "broadcast" && n != "point-to-point" {
			return fmt.Errorf("network must be broadcast or point-to-point, not %q", n)
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
	RouterID   string `json:"router_id,omitempty" jsonschema:"description=Dotted 32-bit ID. Default: the device's first IPv4 address (required if it has none)"`
	Area       string `json:"area" jsonschema:"description=Area ID (dotted, like 0.0.0.0, or a number)"`
	InstanceID uint8  `json:"instance_id,omitempty" jsonschema:"description=Instance ID in packets (0 for IPv6 unicast)"`
	Network    string `json:"network" jsonschema:"enum=broadcast,enum=point-to-point,description=Interface type"`
	Cost       uint16 `json:"cost" jsonschema:"minimum=1,maximum=65535,description=Interface cost"`
	Priority   uint8  `json:"priority" jsonschema:"description=DR election priority (broadcast; 0: never DR/BDR)"`
	Passive    bool   `json:"passive,omitempty" jsonschema:"description=Advertise the interface's prefixes without hellos (no adjacency)"`

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
}

type Interface3 struct {
	Device   string `json:"device" jsonschema:"description=Device name"`
	Network  string `json:"network,omitempty" jsonschema:"enum=broadcast,enum=point-to-point"`
	Cost     uint16 `json:"cost,omitempty" jsonschema:"minimum=1,maximum=65535"`
	Priority *uint8 `json:"priority,omitempty"`
	Passive  bool   `json:"passive,omitempty"`
	Auth     *Auth3 `json:"auth,omitempty" jsonschema:"description=Default: the router's auth"`
}

func defaultConfigV3() *ConfigV3 {
	return &ConfigV3{Area: "0.0.0.0", Network: "broadcast", Cost: 10, Priority: 1, HelloInterval: 10, DeadInterval: 40,
		RetransmitInterval: 5, MTU: 1500, LSARefresh: 1800, Auth: Auth3{Type: "none"}}
}

// common is the shared form the session runs on.
func (c *ConfigV3) common() Config {
	out := Config{RouterID: c.RouterID, Area: c.Area, Network: c.Network, Cost: c.Cost, Priority: c.Priority, Passive: c.Passive,
		HelloInterval: c.HelloInterval, DeadInterval: c.DeadInterval, RetransmitInterval: c.RetransmitInterval, MTU: c.MTU,
		MTUIgnore: c.MTUIgnore, LSARefresh: c.LSARefresh, Auth: c.Auth.common(), MaxMetric: c.MaxMetric,
		Prefixes: c.Prefixes, Loopbacks: c.Loopbacks, Topology: c.Topology}
	for _, ic := range c.Interfaces {
		i := Interface{Device: ic.Device, Network: ic.Network, Cost: ic.Cost, Priority: ic.Priority, Passive: ic.Passive}
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
