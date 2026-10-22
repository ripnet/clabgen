package bgp

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Config is one BGP speaker on a device with its peers and the routes it
// injects.
type Config struct {
	LocalAS  uint32 `json:"local_as" jsonschema:"required,minimum=1,maximum=4294967295,description=Our AS (4-byte AS numbers are fine)"`
	RouterID string `json:"router_id,omitempty" jsonschema:"description=BGP identifier. Default: the device's first IPv4 address"`
	HoldTime int    `json:"hold_time" jsonschema:"minimum=0,maximum=65535,description=Seconds (0: no keepalives; else at least 3)"`
	// Peers we talk to; each is dialled (unless passive) and accepted.
	Peers []Peer `json:"peers,omitempty" jsonschema_extras:"x-summary={address} AS {remote_as}[ · {description}][ · {families}]"`
	// Routes we advertise to every peer of a matching family.
	Routes []Route      `json:"routes,omitempty" jsonschema:"description=Routes to advertise (route injection); with an rd: VPNv4 / VPNv6" jsonschema_extras:"x-summary={prefix}[ ×{count}][ rd {rd}][ label {label}][ nh {next_hop}]"`
	EVPN   []EVPNRoute  `json:"evpn,omitempty" jsonschema:"description=EVPN routes (family evpn)" jsonschema_extras:"x-summary={type} {rd}[ {mac}][ {prefix}][ ×{count}][ label {label}]"`
	L2VPN  []L2VPNRoute `json:"l2vpn,omitempty" jsonschema:"description=BGP VPLS / VPWS label blocks (family l2vpn)" jsonschema_extras:"x-summary=[{kind} ]{rd} ve {ve_id} labels from {label_base}"`
}

type Peer struct {
	Address      string   `json:"address" jsonschema:"required,description=Peer IP address" jsonschema_extras:"x-group=Session"`
	RemoteAS     uint32   `json:"remote_as" jsonschema:"required,minimum=1" jsonschema_extras:"x-group=Session"`
	LocalAddress string   `json:"local_address,omitempty" jsonschema:"description=Our source address. Default: the device address toward the peer" jsonschema_extras:"x-group=Session"`
	Passive      bool     `json:"passive,omitempty" jsonschema:"description=Only accept the peer's connection; don't dial" jsonschema_extras:"x-group=Transport,x-advanced=true"`
	Families     []string `json:"families,omitempty" jsonschema:"enum=ipv4-unicast,enum=ipv6-unicast,enum=ipv4-vpn,enum=ipv6-vpn,enum=evpn,enum=l2vpn,description=Address families. Default: the peer address's unicast family" jsonschema_extras:"x-group=Session"`
	HoldTime     int      `json:"hold_time,omitempty" jsonschema:"description=Default: the speaker's" jsonschema_extras:"x-group=Timers"`
	TTL          int      `json:"ttl,omitempty" jsonschema:"minimum=0,maximum=255,description=TTL of our packets (eBGP multihop / GTSM 255). Default 64 (eBGP 1)" jsonschema_extras:"x-group=Transport,x-advanced=true"`
	Description  string   `json:"description,omitempty" jsonschema_extras:"x-group=Session"`
	ConnectRetry int      `json:"connect_retry,omitempty" jsonschema:"description=Seconds between connection attempts. Default 5" jsonschema_extras:"x-group=Timers"`
}

// Route is a route (or count consecutive prefixes) with its attributes.
type Route struct {
	Prefix  string `json:"prefix" jsonschema:"required,description=e.g. 198.51.100.0/24 or 2001:db8:1::/48" jsonschema_extras:"x-group=Prefixes"`
	Count   int    `json:"count,omitempty" jsonschema:"description=Consecutive prefixes of this length. Default 1" jsonschema_extras:"x-group=Prefixes"`
	NextHop string `json:"next_hop,omitempty" jsonschema:"description=Default: our address toward each peer (next-hop self)" jsonschema_extras:"x-group=Path attributes"`
	Origin  string `json:"origin,omitempty" jsonschema:"enum=igp,enum=egp,enum=incomplete,description=Default igp" jsonschema_extras:"x-group=Path attributes"`
	// AS path: after our AS on eBGP (as received on iBGP).
	ASPath           []uint32 `json:"as_path,omitempty" jsonschema:"description=AS numbers after ours (eBGP) / the whole path (iBGP)" jsonschema_extras:"x-group=Path attributes"`
	MED              *uint32  `json:"med,omitempty" jsonschema_extras:"x-group=Path attributes"`
	LocalPref        *uint32  `json:"local_pref,omitempty" jsonschema:"description=iBGP only. Default 100" jsonschema_extras:"x-group=Path attributes"`
	Communities      []string `json:"communities,omitempty" jsonschema:"description=Standard communities: 65000:100, no-export, no-advertise, no-export-subconfed, blackhole" jsonschema_extras:"x-group=Path attributes"`
	LargeCommunities []string `json:"large_communities,omitempty" jsonschema:"description=Large communities global:local1:local2" jsonschema_extras:"x-group=Path attributes"`
	ExtCommunities   []string `json:"ext_communities,omitempty" jsonschema:"description=Extended communities: rt:65000:100, soo:65000:1, rt:192.0.2.1:7" jsonschema_extras:"x-group=Path attributes"`
	AtomicAggregate  bool     `json:"atomic_aggregate,omitempty" jsonschema_extras:"x-group=Path attributes"`
	Peers            []string `json:"peers,omitempty" jsonschema:"description=Only to these peers (addresses). Default: all" jsonschema_extras:"x-group=Advanced,x-advanced=true"`
	// VPN (RFC 4364 / 4659): a route distinguisher makes the route VPNv4/v6.
	RD           string   `json:"rd,omitempty" jsonschema:"description=Route distinguisher (65100:500 or 192.0.2.50:500): advertise as VPNv4 / VPNv6" jsonschema_extras:"x-group=VPN"`
	Label        uint32   `json:"label,omitempty" jsonschema:"description=VPN label. Default 30000" jsonschema_extras:"x-group=VPN"`
	RouteTargets []string `json:"route_targets,omitempty" jsonschema:"description=Route targets, e.g. 65100:500" jsonschema_extras:"x-group=VPN"`
}

func defaultConfig() *Config {
	return &Config{LocalAS: 65000, HoldTime: 90}
}

var families = map[string]bool{"ipv4-unicast": true, "ipv6-unicast": true, "ipv4-vpn": true, "ipv6-vpn": true, "evpn": true, "l2vpn": true}

func (c *Config) Validate() error {
	if c.LocalAS == 0 {
		return errors.New("local_as is required")
	}
	if c.RouterID != "" {
		if a, err := netip.ParseAddr(c.RouterID); err != nil || !a.Is4() {
			return fmt.Errorf("router_id %q must be an IPv4 address", c.RouterID)
		}
	}
	if c.HoldTime != 0 && (c.HoldTime < 3 || c.HoldTime > 65535) {
		return errors.New("hold_time must be 0 or 3-65535")
	}
	seen := map[netip.Addr]bool{}
	for _, p := range c.Peers {
		a, err := netip.ParseAddr(p.Address)
		if err != nil {
			return fmt.Errorf("peer %q: not an IP address", p.Address)
		}
		if seen[a] {
			return fmt.Errorf("peer %s twice", a)
		}
		seen[a] = true
		if p.RemoteAS == 0 {
			return fmt.Errorf("peer %s: remote_as is required", a)
		}
		if p.LocalAddress != "" {
			if _, err := netip.ParseAddr(p.LocalAddress); err != nil {
				return fmt.Errorf("peer %s: local_address %q: not an IP address", a, p.LocalAddress)
			}
		}
		for _, f := range p.Families {
			if !families[f] {
				return fmt.Errorf("peer %s: unknown family %q (ipv4-unicast, ipv6-unicast, ipv4-vpn, ipv6-vpn, evpn, l2vpn)", a, f)
			}
		}
		if p.HoldTime != 0 && (p.HoldTime < 3 || p.HoldTime > 65535) {
			return fmt.Errorf("peer %s: hold_time must be 3-65535", a)
		}
		if p.TTL < 0 || p.TTL > 255 || p.ConnectRetry < 0 {
			return fmt.Errorf("peer %s: ttl 0-255, connect_retry ≥ 0", a)
		}
	}
	for _, r := range c.Routes {
		if err := r.validate(); err != nil {
			return err
		}
		for _, p := range r.Peers {
			if a, err := netip.ParseAddr(p); err != nil || !seen[a] {
				return fmt.Errorf("route %s: peer %q is not configured", r.Prefix, p)
			}
		}
	}
	for _, e := range c.EVPN {
		if _, _, err := e.expand(netip.MustParseAddr("192.0.2.1")); err != nil {
			return err
		}
	}
	for _, l := range c.L2VPN {
		if _, err := l.expand(); err != nil {
			return err
		}
	}
	return nil
}

func (r Route) validate() error {
	if _, err := r.expand(); err != nil {
		return err
	}
	t, err := r.attrs()
	if err != nil {
		return err
	}
	if r.RD != "" {
		if _, err := r.vpnRoutes(t); err != nil {
			return err
		}
	}
	return nil
}

// maxRoutes bounds what one instance injects.
const maxRoutes = 2000000

// expand returns the prefixes a Route stands for.
func (r Route) expand() ([]netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(r.Prefix))
	if err != nil {
		return nil, fmt.Errorf("route %q: %w", r.Prefix, err)
	}
	p = p.Masked()
	n := r.Count
	if n == 0 {
		n = 1
	}
	if n < 0 || n > maxRoutes {
		return nil, fmt.Errorf("route %s: count must be 1-%d", p, maxRoutes)
	}
	if r.NextHop != "" {
		nh, err := netip.ParseAddr(r.NextHop)
		if err != nil || nh.Is4() != p.Addr().Is4() {
			return nil, fmt.Errorf("route %s: next_hop %q must be an address of the same family", p, r.NextHop)
		}
	}
	out := make([]netip.Prefix, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, p)
		var ok bool
		if i < n-1 {
			if p, ok = nextPrefix(p); !ok {
				return nil, fmt.Errorf("route %s: count %d runs past the address space", r.Prefix, n)
			}
		}
	}
	return out, nil
}

// nextPrefix is the prefix of the same length right after p.
func nextPrefix(p netip.Prefix) (netip.Prefix, bool) {
	b := p.Addr().AsSlice()
	for i := p.Bits() - 1; i >= 0; i-- {
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

// Well-known communities (RFC 1997, 7999).
var wellKnown = map[string]uint32{"no-export": 0xffffff01, "no-advertise": 0xffffff02, "no-export-subconfed": 0xffffff03, "blackhole": 0xffff029a}

func parseCommunity(s string) (uint32, error) {
	if v, ok := wellKnown[s]; ok {
		return v, nil
	}
	a, b, ok := strings.Cut(s, ":")
	x, e1 := strconv.ParseUint(a, 10, 16)
	y, e2 := strconv.ParseUint(b, 10, 16)
	if !ok || e1 != nil || e2 != nil {
		return 0, fmt.Errorf("community %q: want AS:value (16-bit each) or a well-known name", s)
	}
	return uint32(x)<<16 | uint32(y), nil
}

func parseLarge(s string) ([3]uint32, error) {
	var out [3]uint32
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return out, fmt.Errorf("large community %q: want global:local1:local2", s)
	}
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return out, fmt.Errorf("large community %q: %v", s, err)
		}
		out[i] = uint32(v)
	}
	return out, nil
}

// Action args.
type RouteArgs struct {
	Routes []Route      `json:"routes,omitempty"`
	EVPN   []EVPNRoute  `json:"evpn,omitempty"`
	L2VPN  []L2VPNRoute `json:"l2vpn,omitempty"`
}

type PeerArgs struct {
	Peer string `json:"peer,omitempty" jsonschema:"description=Peer address; empty: every peer"`
}

type NotifyArgs struct {
	Peer    string `json:"peer,omitempty" jsonschema:"description=Peer address; empty: every peer"`
	Code    uint8  `json:"code" jsonschema:"description=NOTIFICATION error code (6: Cease)"`
	Subcode uint8  `json:"subcode" jsonschema:"description=e.g. Cease 2 administrative shutdown, 4 administrative reset"`
	Message string `json:"message,omitempty" jsonschema:"description=Shutdown communication (RFC 9003) for Cease 2/4"`
}

type FlapArgs struct {
	Peer    string `json:"peer,omitempty" jsonschema:"description=Peer address; empty: every peer"`
	Seconds int    `json:"seconds" jsonschema:"minimum=1,maximum=3600,description=How long the session stays down"`
	Mode    string `json:"mode" jsonschema:"enum=notify,enum=silent,description=notify: send a Cease NOTIFICATION; silent: stop sending (the peer's hold timer expires)"`
}

func (a *FlapArgs) Validate() error {
	if a.Seconds < 1 || a.Seconds > 3600 || (a.Mode != "notify" && a.Mode != "silent") {
		return errors.New("seconds 1-3600, mode notify or silent")
	}
	return nil
}

func (a *RouteArgs) Validate() error {
	if len(a.Routes)+len(a.EVPN)+len(a.L2VPN) == 0 {
		return errors.New("at least one route")
	}
	for _, r := range a.Routes {
		if err := r.validate(); err != nil {
			return err
		}
	}
	for _, e := range a.EVPN {
		if _, _, err := e.expand(netip.MustParseAddr("192.0.2.1")); err != nil {
			return err
		}
	}
	for _, l := range a.L2VPN {
		if _, err := l.expand(); err != nil {
			return err
		}
	}
	return nil
}

type RIBArgs struct {
	Peer   string `json:"peer" jsonschema:"description=Peer address"`
	Family string `json:"family,omitempty" jsonschema:"enum=ipv4-unicast,enum=ipv6-unicast,enum=ipv4-vpn,enum=ipv6-vpn,enum=evpn,enum=l2vpn,description=Default: all"`
	Prefix string `json:"prefix,omitempty" jsonschema:"description=Unicast: only prefixes within this one; other families: routes whose text contains it"`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=At most this many routes. Default 1000"`
}
