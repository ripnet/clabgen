package bgp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// VPN families: VPNv4/VPNv6 (RFC 4364, 4659), EVPN (RFC 7432: Ethernet
// A-D, MAC/IP, inclusive multicast, Ethernet segment; RFC 9136 IP prefix)
// and L2VPN (BGP VPLS, RFC 4761; BGP VPWS, RFC 6624). Their routes aren't
// keyed by a prefix alone, so they live apart from the unicast table:
// each is an NLRI in wire form with its family and the attributes it was
// advertised with.

var (
	famVPNv4 = family{gb.RF_IPv4_VPN, "ipv4-vpn", false}
	famVPNv6 = family{gb.RF_IPv6_VPN, "ipv6-vpn", true}
	famEVPN  = family{gb.RF_EVPN, "evpn", false}
	famL2VPN = family{gb.RF_VPLS, "l2vpn", false}

	xfamilies = []family{famVPNv4, famVPNv6, famEVPN, famL2VPN, famSRPv4, famSRPv6}
)

// xroute is one advertised route of a VPN family.
type xroute struct {
	f    family
	key  string // family/NLRI text without labels
	nlri []byte
	text string
	t    *tmpl
}

// EVPNRoute is an EVPN route (or count MAC/IP or IP prefix routes).
type EVPNRoute struct {
	Type         string   `json:"type" jsonschema:"required,enum=mac-ip,enum=inclusive-multicast,enum=ethernet-ad,enum=ethernet-segment,enum=ip-prefix" jsonschema_description:"Route type 2, 3, 1, 4 or 5"`
	RD           string   `json:"rd" jsonschema:"required" jsonschema_description:"Route distinguisher, e.g. 65100:600 or 192.0.2.50:600"`
	RouteTargets []string `json:"route_targets,omitempty" jsonschema_description:"Route targets, e.g. 65100:600"`
	ESI          string   `json:"esi,omitempty" jsonschema_description:"Ethernet segment identifier (10 bytes, e.g. 01:00:00:00:00:00:00:00:00:01). Default 0 (single-homed)" jsonschema_extras:"x-show-if=type!=inclusive-multicast"`
	EthernetTag  uint32   `json:"ethernet_tag,omitempty" jsonschema_extras:"x-advanced=true"`
	MAC          string   `json:"mac,omitempty" jsonschema:"description=mac-ip: the first MAC" jsonschema_extras:"x-show-if=type=mac-ip"`
	IP           string   `json:"ip,omitempty" jsonschema:"description=mac-ip: the first host address (optional; counts up with the MAC)" jsonschema_extras:"x-show-if=type=mac-ip"`
	Count        int      `json:"count,omitempty" jsonschema:"description=mac-ip / ip-prefix: consecutive MACs (and IPs) or prefixes. Default 1" jsonschema_extras:"x-show-if=type=mac-ip|ip-prefix"`
	Prefix       string   `json:"prefix,omitempty" jsonschema:"description=ip-prefix: the (first) prefix" jsonschema_extras:"x-show-if=type=ip-prefix"`
	Gateway      string   `json:"gateway,omitempty" jsonschema:"description=ip-prefix: gateway address. Default 0" jsonschema_extras:"x-show-if=type=ip-prefix"`
	Label        uint32   `json:"label,omitempty" jsonschema:"description=MPLS label (or VXLAN VNI) of the route; inclusive-multicast: in the PMSI tunnel attribute. Default 0" jsonschema_extras:"x-show-if=type!=ethernet-segment"`
	Label2       uint32   `json:"label2,omitempty" jsonschema:"description=mac-ip: second (L3) label" jsonschema_extras:"x-show-if=type=mac-ip"`
	Originator   string   `json:"originator,omitempty" jsonschema:"description=inclusive-multicast / ethernet-segment: originating router address. Default: our router ID" jsonschema_extras:"x-show-if=type=inclusive-multicast|ethernet-segment"`
	Encap        string   `json:"encapsulation,omitempty" jsonschema:"enum=mpls,enum=vxlan,description=Default mpls (labels in the high 20 bits); vxlan adds the encapsulation community and carries VNIs" jsonschema_extras:"x-advanced=true"`
	SingleActive bool     `json:"single_active,omitempty" jsonschema:"description=ethernet-ad: the ESI label community's single-active flag" jsonschema_extras:"x-show-if=type=ethernet-ad"`
	ESILabel     *uint32  `json:"esi_label,omitempty" jsonschema:"description=ethernet-ad: add the ESI label community with this label" jsonschema_extras:"x-show-if=type=ethernet-ad"`
	MACMobility  *uint32  `json:"mac_mobility,omitempty" jsonschema:"description=mac-ip: MAC mobility sequence number" jsonschema_extras:"x-show-if=type=mac-ip"`
	RouterMAC    string   `json:"router_mac,omitempty" jsonschema:"description=Router's MAC community (EVPN IRB / type 5)" jsonschema_extras:"x-advanced=true,x-show-if=type=mac-ip|ip-prefix"`
	Attributes
}

// L2VPNRoute is a BGP VPLS (RFC 4761) or VPWS (RFC 6624) label block.
type L2VPNRoute struct {
	Kind         string   `json:"kind" jsonschema:"enum=vpls,enum=vpws,description=Default vpls"`
	RD           string   `json:"rd" jsonschema:"required"`
	RouteTargets []string `json:"route_targets,omitempty"`
	VEID         uint16   `json:"ve_id" jsonschema:"required,description=VE ID (VPLS) / CE ID (VPWS)"`
	BlockOffset  uint16   `json:"block_offset,omitempty" jsonschema:"description=Label block offset. Default 1" jsonschema_extras:"x-group=Label block"`
	BlockSize    uint16   `json:"block_size,omitempty" jsonschema:"description=Label block size. Default 8" jsonschema_extras:"x-group=Label block"`
	LabelBase    uint32   `json:"label_base" jsonschema:"required,description=First label of the block"`
	MTU          uint16   `json:"mtu,omitempty" jsonschema:"description=Layer-2 MTU in the Layer2 Info community (0: don't check)" jsonschema_extras:"x-group=Layer2 info"`
	ControlWord  bool     `json:"control_word,omitempty" jsonschema:"description=C flag: control word required" jsonschema_extras:"x-group=Layer2 info"`
	Encap        string   `json:"encapsulation,omitempty" jsonschema:"enum=vpls,enum=ethernet,enum=vlan,description=Layer2 Info encapsulation. Default vpls (VPLS) / ethernet (VPWS)" jsonschema_extras:"x-group=Layer2 info"`
	Attributes
}

// Attributes are what non-unicast routes share with unicast ones.
type Attributes struct {
	NextHop          string   `json:"next_hop,omitempty" jsonschema:"description=Default: our address toward each peer" jsonschema_extras:"x-group=Path attributes,x-advanced=true"`
	Origin           string   `json:"origin,omitempty" jsonschema:"enum=igp,enum=egp,enum=incomplete" jsonschema_extras:"x-group=Path attributes,x-advanced=true"`
	ASPath           []uint32 `json:"as_path,omitempty" jsonschema_extras:"x-group=Path attributes,x-advanced=true"`
	MED              *uint32  `json:"med,omitempty" jsonschema_extras:"x-group=Path attributes,x-advanced=true"`
	LocalPref        *uint32  `json:"local_pref,omitempty" jsonschema_extras:"x-group=Path attributes,x-advanced=true"`
	Communities      []string `json:"communities,omitempty" jsonschema_extras:"x-group=Path attributes,x-advanced=true"`
	LargeCommunities []string `json:"large_communities,omitempty" jsonschema_extras:"x-group=Path attributes,x-advanced=true"`
	ExtCommunities   []string `json:"ext_communities,omitempty" jsonschema_extras:"x-group=Path attributes,x-advanced=true"`
	Peers            []string `json:"peers,omitempty" jsonschema_extras:"x-group=Path attributes,x-advanced=true"`
}

func (a Attributes) route(name string, rts []string) Route {
	r := Route{Prefix: name, NextHop: a.NextHop, Origin: a.Origin, ASPath: a.ASPath, MED: a.MED, LocalPref: a.LocalPref,
		Communities: a.Communities, LargeCommunities: a.LargeCommunities, ExtCommunities: a.ExtCommunities, Peers: a.Peers}
	for _, rt := range rts {
		r.ExtCommunities = append(r.ExtCommunities, "rt:"+rt)
	}
	return r
}

// ---- building routes ----

const maxXRoutes = 200000

func parseRD(s string) (gb.RouteDistinguisherInterface, error) {
	if s == "" {
		return nil, errors.New("rd is required")
	}
	rd, err := gb.ParseRouteDistinguisher(s)
	if err != nil {
		return nil, fmt.Errorf("rd %q: %v", s, err)
	}
	return rd, nil
}

func parseRTs(rts []string) error {
	for _, rt := range rts {
		if _, err := gb.ParseExtendedCommunity(gb.EC_SUBTYPE_ROUTE_TARGET, rt); err != nil {
			return fmt.Errorf("route target %q: %v", rt, err)
		}
	}
	return nil
}

// vpnRoutes expands a VPNv4/v6 route (Route with an RD).
func (r Route) vpnRoutes(t *tmpl) ([]*xroute, error) {
	rd, err := parseRD(r.RD)
	if err != nil {
		return nil, fmt.Errorf("route %s: %w", r.Prefix, err)
	}
	if err := parseRTs(r.RouteTargets); err != nil {
		return nil, fmt.Errorf("route %s: %w", r.Prefix, err)
	}
	label := r.Label
	if label == 0 {
		label = defaultVPNLabel
	}
	if label > 0xfffff {
		return nil, fmt.Errorf("route %s: label must be below 1048576", r.Prefix)
	}
	ps, err := r.expand()
	if err != nil {
		return nil, err
	}
	out := make([]*xroute, 0, len(ps))
	for _, p := range ps {
		var n gb.AddrPrefixInterface
		f := famVPNv4
		if p.Addr().Is4() {
			n = gb.NewLabeledVPNIPAddrPrefix(uint8(p.Bits()), p.Addr().String(), *gb.NewMPLSLabelStack(label), rd)
		} else {
			f = famVPNv6
			n = gb.NewLabeledVPNIPv6AddrPrefix(uint8(p.Bits()), p.Addr().String(), *gb.NewMPLSLabelStack(label), rd)
		}
		x, err := newXRoute(f, n, t)
		if err != nil {
			return nil, err
		}
		x.text += fmt.Sprintf(" label %d", label)
		out = append(out, x)
	}
	return out, nil
}

const defaultVPNLabel = 30000

func newXRoute(f family, n gb.AddrPrefixInterface, t *tmpl) (*xroute, error) {
	b, err := n.Serialize()
	if err != nil {
		return nil, err
	}
	s := n.String()
	return &xroute{f: f, key: f.name + "/" + s, nlri: b, text: s, t: t}, nil
}

// field is a label as an EVPN 3-byte field: an MPLS label in the high 20
// bits, a VXLAN VNI as is.
func (e EVPNRoute) field(v uint32) uint32 {
	if e.Encap == "vxlan" {
		return v & 0xffffff
	}
	return v << 4
}

func nextMAC(m net.HardwareAddr) net.HardwareAddr {
	n := append(net.HardwareAddr(nil), m...)
	for i := len(n) - 1; i >= 0; i-- {
		n[i]++
		if n[i] != 0 {
			break
		}
	}
	return n
}

func parseESI(s string) (gb.EthernetSegmentIdentifier, error) {
	if s == "" || s == "0" {
		return gb.EthernetSegmentIdentifier{Type: gb.ESI_ARBITRARY, Value: make([]byte, 9)}, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) != 10 {
		return gb.EthernetSegmentIdentifier{}, fmt.Errorf("esi %q: want 10 bytes like 01:00:00:00:00:00:00:00:00:01", s)
	}
	var b [10]byte
	for i, p := range parts {
		if _, err := fmt.Sscanf(p, "%02x", &b[i]); err != nil {
			return gb.EthernetSegmentIdentifier{}, fmt.Errorf("esi %q: bad byte %q", s, p)
		}
	}
	return gb.EthernetSegmentIdentifier{Type: gb.ESIType(b[0]), Value: append([]byte(nil), b[1:]...)}, nil
}

// expand builds an EVPN entry's routes (its template carries the
// type-specific communities and the PMSI tunnel).
func (e EVPNRoute) expand(rid netip.Addr) ([]*xroute, *tmpl, error) {
	name := "evpn " + e.Type
	rd, err := parseRD(e.RD)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := parseRTs(e.RouteTargets); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	if e.Encap != "" && e.Encap != "mpls" && e.Encap != "vxlan" {
		return nil, nil, fmt.Errorf("%s: encapsulation must be mpls or vxlan", name)
	}
	if e.Encap != "vxlan" && (e.Label > 0xfffff || e.Label2 > 0xfffff) {
		return nil, nil, fmt.Errorf("%s: labels must be below 1048576", name)
	}
	esi, err := parseESI(e.ESI)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	t, err := e.Attributes.route(name, e.RouteTargets).attrs()
	if err != nil {
		return nil, nil, err
	}
	if e.Encap == "vxlan" {
		t.ext = append(t.ext, gb.NewEncapExtended(gb.TUNNEL_TYPE_VXLAN))
	}
	if e.RouterMAC != "" {
		if _, err := net.ParseMAC(e.RouterMAC); err != nil {
			return nil, nil, fmt.Errorf("%s: router_mac %q: %v", name, e.RouterMAC, err)
		}
		t.ext = append(t.ext, gb.NewRoutersMacExtended(e.RouterMAC))
	}
	orig := rid
	if e.Originator != "" {
		if orig, err = netip.ParseAddr(e.Originator); err != nil {
			return nil, nil, fmt.Errorf("%s: originator %q: not an address", name, e.Originator)
		}
	}
	n := max(e.Count, 1)
	if n > maxXRoutes {
		return nil, nil, fmt.Errorf("%s: count must be 1-%d", name, maxXRoutes)
	}
	var nlris []*gb.EVPNNLRI
	switch e.Type {
	case "mac-ip":
		mac, err := net.ParseMAC(e.MAC)
		if err != nil || len(mac) != 6 {
			return nil, nil, fmt.Errorf("%s: mac %q: want a MAC address", name, e.MAC)
		}
		var ip netip.Addr
		if e.IP != "" {
			if ip, err = netip.ParseAddr(e.IP); err != nil {
				return nil, nil, fmt.Errorf("%s: ip %q: not an address", name, e.IP)
			}
		}
		labels := []uint32{e.field(e.Label)}
		if e.Label2 != 0 {
			labels = append(labels, e.field(e.Label2))
		}
		if e.MACMobility != nil {
			t.ext = append(t.ext, gb.NewMacMobilityExtended(*e.MACMobility, false))
		}
		for i := 0; i < n; i++ {
			ips := ""
			if ip.IsValid() {
				ips = ip.String()
			}
			nlris = append(nlris, gb.NewEVPNMacIPAdvertisementRoute(rd, esi, e.EthernetTag, mac.String(), ips, labels))
			mac = nextMAC(mac)
			if ip.IsValid() {
				ip = ip.Next()
			}
		}
	case "inclusive-multicast":
		nlris = append(nlris, gb.NewEVPNMulticastEthernetTagRoute(rd, e.EthernetTag, orig.String()))
		t.extra = append(t.extra, gb.NewPathAttributePmsiTunnel(gb.PMSI_TUNNEL_TYPE_INGRESS_REPL, false, e.field(e.Label), gb.NewIngressReplTunnelID(orig.String())))
	case "ethernet-ad":
		nlris = append(nlris, gb.NewEVPNEthernetAutoDiscoveryRoute(rd, esi, e.EthernetTag, e.field(e.Label)))
		if e.ESILabel != nil || e.SingleActive {
			l := uint32(0)
			if e.ESILabel != nil {
				l = *e.ESILabel
			}
			t.ext = append(t.ext, gb.NewESILabelExtended(e.field(l), e.SingleActive))
		}
	case "ethernet-segment":
		nlris = append(nlris, gb.NewEVPNEthernetSegmentRoute(rd, esi, orig.String()))
	case "ip-prefix":
		p, err := netip.ParsePrefix(e.Prefix)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: prefix %q: %v", name, e.Prefix, err)
		}
		p = p.Masked()
		gw := "0.0.0.0"
		if p.Addr().Is6() {
			gw = "::"
		}
		if e.Gateway != "" {
			gw = e.Gateway
		}
		for i := 0; i < n; i++ {
			nlris = append(nlris, gb.NewEVPNIPPrefixRoute(rd, esi, e.EthernetTag, uint8(p.Bits()), p.Addr().String(), gw, e.field(e.Label)))
			if i < n-1 {
				var ok bool
				if p, ok = nextPrefix(p); !ok {
					return nil, nil, fmt.Errorf("%s: count runs past the address space", name)
				}
			}
		}
	default:
		return nil, nil, fmt.Errorf("evpn: type must be mac-ip, inclusive-multicast, ethernet-ad, ethernet-segment or ip-prefix, not %q", e.Type)
	}
	var out []*xroute
	for _, nl := range nlris {
		x, err := newXRoute(famEVPN, nl, t)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %v", name, err)
		}
		out = append(out, x)
	}
	return out, t, nil
}

// l2InfoExtended is the Layer2 Info extended community (RFC 4761 3.2.4,
// RFC 6624 3) with any encapsulation type.
type l2InfoExtended struct {
	encap, flags uint8
	mtu          uint16
}

func (e *l2InfoExtended) Serialize() ([]byte, error) {
	b := []byte{byte(gb.EC_TYPE_GENERIC_TRANSITIVE_EXPERIMENTAL), byte(gb.EC_SUBTYPE_L2_INFO), e.encap, e.flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(b[4:], e.mtu)
	return b, nil
}
func (e *l2InfoExtended) String() string {
	return fmt.Sprintf("l2-info encap %d flags %#x mtu %d", e.encap, e.flags, e.mtu)
}
func (e *l2InfoExtended) GetTypes() (gb.ExtendedCommunityAttrType, gb.ExtendedCommunityAttrSubType) {
	return gb.EC_TYPE_GENERIC_TRANSITIVE_EXPERIMENTAL, gb.EC_SUBTYPE_L2_INFO
}
func (e *l2InfoExtended) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", e.String())), nil
}
func (e *l2InfoExtended) Flat() map[string]string { return map[string]string{} }

var l2Encaps = map[string]uint8{"vpls": 19, "ethernet": 5, "vlan": 4}

// expand builds the label block route. The NLRI is written here: GoBGP's
// leaves the bottom-of-stack bit of the label base clear.
func (l L2VPNRoute) expand() (*xroute, error) {
	kind := l.Kind
	if kind == "" {
		kind = "vpls"
	}
	name := fmt.Sprintf("l2vpn %s ve %d", kind, l.VEID)
	if kind != "vpls" && kind != "vpws" {
		return nil, fmt.Errorf("%s: kind must be vpls or vpws", name)
	}
	rd, err := parseRD(l.RD)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := parseRTs(l.RouteTargets); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if l.LabelBase < 16 || l.LabelBase > 0xfffff {
		return nil, fmt.Errorf("%s: label_base must be 16-1048575", name)
	}
	off, size := l.BlockOffset, l.BlockSize
	if off == 0 {
		off = 1
	}
	if size == 0 {
		size = 8
	}
	encap := l.Encap
	if encap == "" {
		encap = map[string]string{"vpls": "vpls", "vpws": "ethernet"}[kind]
	}
	code, ok := l2Encaps[encap]
	if !ok {
		return nil, fmt.Errorf("%s: encapsulation must be vpls, ethernet or vlan", name)
	}
	t, err := l.Attributes.route(name, l.RouteTargets).attrs()
	if err != nil {
		return nil, err
	}
	flags := uint8(0)
	if l.ControlWord {
		flags |= 0x02 // C
	}
	t.ext = append(t.ext, &l2InfoExtended{encap: code, flags: flags, mtu: l.MTU})
	rdb, err := rd.Serialize()
	if err != nil {
		return nil, err
	}
	b := []byte{0, 17}
	b = append(b, rdb[:8]...)
	b = binary.BigEndian.AppendUint16(b, l.VEID)
	b = binary.BigEndian.AppendUint16(b, off)
	b = binary.BigEndian.AppendUint16(b, size)
	lb := l.LabelBase<<4 | 1
	b = append(b, byte(lb>>16), byte(lb>>8), byte(lb))
	text := fmt.Sprintf("%s:%d:%d", rd, l.VEID, off)
	return &xroute{f: famL2VPN, key: famL2VPN.name + "/" + text, nlri: b,
		text: fmt.Sprintf("%s %s (block %d, labels %d-%d)", kind, text, size, l.LabelBase, l.LabelBase+uint32(size)-1), t: t}, nil
}

// ---- encoding ----

// buildMPUpdates packs NLRIs of one family and attribute set into
// UPDATEs with MP_REACH.
func buildMPUpdates(f family, attrs []gb.PathAttributeInterface, nh []byte, nlris [][]byte) ([][]byte, error) {
	sort.SliceStable(attrs, func(i, j int) bool { return attrs[i].GetType() < attrs[j].GetType() })
	ab1, ab2, err := serializeAttrs(attrs)
	if err != nil {
		return nil, err
	}
	afi, safi := gb.RouteFamilyToAfiSafi(f.rf)
	fixed := 2 + 1 + 1 + len(nh) + 1
	room := maxMsg - 19 - 4 - len(ab1) - len(ab2) - 4 - fixed
	var out [][]byte
	for len(nlris) > 0 {
		var nl []byte
		for len(nlris) > 0 && (len(nl) == 0 || len(nl)+len(nlris[0]) <= room) {
			nl = append(nl, nlris[0]...)
			nlris = nlris[1:]
		}
		b := header(make([]byte, 0, maxMsg), 2)
		b = append(b, 0, 0)
		b = binary.BigEndian.AppendUint16(b, uint16(len(ab1)+len(ab2)+4+fixed+len(nl)))
		b = append(b, ab1...)
		b = mpHeader(b, byte(gb.BGP_ATTR_TYPE_MP_REACH_NLRI), fixed+len(nl))
		b = binary.BigEndian.AppendUint16(b, afi)
		b = append(b, safi, byte(len(nh)))
		b = append(b, nh...)
		b = append(b, 0)
		b = append(b, nl...)
		b = append(b, ab2...)
		out = append(out, finish(b))
	}
	return out, nil
}

func buildMPWithdraws(f family, nlris [][]byte) [][]byte {
	afi, safi := gb.RouteFamilyToAfiSafi(f.rf)
	var out [][]byte
	for len(nlris) > 0 {
		var nl []byte
		room := maxMsg - 19 - 4 - 4 - 3
		for len(nlris) > 0 && (len(nl) == 0 || len(nl)+len(nlris[0]) <= room) {
			nl = append(nl, nlris[0]...)
			nlris = nlris[1:]
		}
		b := header(make([]byte, 0, maxMsg), 2)
		b = append(b, 0, 0)
		b = binary.BigEndian.AppendUint16(b, uint16(4+3+len(nl)))
		b = mpHeader(b, byte(gb.BGP_ATTR_TYPE_MP_UNREACH_NLRI), 3+len(nl))
		b = binary.BigEndian.AppendUint16(b, afi)
		b = append(b, safi)
		b = append(b, nl...)
		out = append(out, finish(b))
	}
	return out
}

// xNextHop is the MP_REACH next hop of a VPN family: VPNv4 with a zero RD,
// VPNv6 with a zero RD (an IPv4-mapped address over IPv4, as 6VPE),
// EVPN and L2VPN a plain address.
func xNextHop(f family, nh netip.Addr) []byte {
	switch f.rf {
	case gb.RF_IPv4_VPN:
		return append(make([]byte, 8), nh.Unmap().AsSlice()...)
	case gb.RF_IPv6_VPN:
		a := nh.As16() // As16 of an IPv4 address is the IPv4-mapped one
		return append(make([]byte, 8), a[:]...)
	}
	return nh.AsSlice()
}

// ---- received routes ----

var labelsRE = regexp.MustCompile(`\[labels?:[^\]]*\]`)

// xKey keys a received NLRI (without its labels, as withdrawals may
// carry others).
func xKey(n gb.AddrPrefixInterface) string { return labelsRE.ReplaceAllString(n.String(), "") }

// rxText describes a received NLRI with its labels.
func rxText(n gb.AddrPrefixInterface, vxlan bool) string {
	s := xKey(n)
	lab := func(v uint32) uint32 {
		if vxlan {
			return v
		}
		return v >> 4
	}
	switch x := n.(type) {
	case *gb.LabeledVPNIPAddrPrefix:
		s += fmt.Sprintf(" label %v", x.Labels.Labels)
	case *gb.LabeledVPNIPv6AddrPrefix:
		s += fmt.Sprintf(" label %v", x.Labels.Labels)
	case *gb.EVPNNLRI:
		switch r := x.RouteTypeData.(type) {
		case *gb.EVPNMacIPAdvertisementRoute:
			var ls []uint32
			for _, l := range r.Labels {
				ls = append(ls, lab(l))
			}
			s += fmt.Sprintf(" esi %s label %v", r.ESI.String(), ls)
		case *gb.EVPNEthernetAutoDiscoveryRoute:
			s += fmt.Sprintf(" esi %s label %d", r.ESI.String(), lab(r.Label))
		case *gb.EVPNIPPrefixRoute:
			s += fmt.Sprintf(" gw %s label %d", r.GWIPAddress, lab(r.Label))
		case *gb.EVPNEthernetSegmentRoute:
			s += " esi " + r.ESI.String()
		}
	case *gb.SRPolicyIPv4:
		s = fmt.Sprintf("color %d endpoint %s (distinguisher %d)", x.Color, netip.AddrFrom4([4]byte(x.Endpoint)), x.Distinguisher)
	case *gb.SRPolicyIPv6:
		s = fmt.Sprintf("color %d endpoint %s (distinguisher %d)", x.Color, netip.AddrFrom16([16]byte(x.Endpoint)), x.Distinguisher)
	case *gb.VPLSNLRI:
		s = fmt.Sprintf("%s:%d:%d (block %d, labels from %d)", x.RD(), x.VEID, x.VEBlockOffset, x.VEBlockSize, x.LabelBlockBase)
	}
	return s
}

// rxRoute is a received route of a VPN family.
type rxRoute struct {
	text string
	info *pathInfo
}

func isXFamily(rf gb.RouteFamily) bool {
	for _, f := range xfamilies {
		if f.rf == rf {
			return true
		}
	}
	return false
}

// ---- L2VPN receive ----

// l2Update is what an UPDATE carried for L2VPN (VPLS / VPWS), decoded
// here: GoBGP's parser rejects some peers' L2VPN NLRI and fails the whole
// message.
type l2Update struct {
	nextHop string
	reach   []l2NLRI
	unreach []l2NLRI
}

type l2NLRI struct{ key, text string }

// takeL2VPN removes L2VPN MP_REACH / MP_UNREACH attributes from an UPDATE
// (a whole message) and decodes them. It returns the message unchanged
// when there are none (or it can't be walked; GoBGP then reports why).
func takeL2VPN(b []byte) ([]byte, *l2Update) {
	if len(b) < 23 || b[18] != 2 {
		return b, nil
	}
	wl := int(binary.BigEndian.Uint16(b[19:]))
	if 21+wl+2 > len(b) {
		return b, nil
	}
	apos := 21 + wl + 2
	al := int(binary.BigEndian.Uint16(b[21+wl:]))
	if apos+al > len(b) {
		return b, nil
	}
	var u *l2Update
	var kept []byte
	for i := apos; i < apos+al; {
		if i+3 > apos+al {
			return b, nil
		}
		flags, typ := b[i], b[i+1]
		hl, vl := 3, int(b[i+2])
		if flags&0x10 != 0 {
			if i+4 > apos+al {
				return b, nil
			}
			hl, vl = 4, int(binary.BigEndian.Uint16(b[i+2:]))
		}
		end := i + hl + vl
		if end > apos+al {
			return b, nil
		}
		v := b[i+hl : end]
		if (typ == 14 || typ == 15) && len(v) >= 3 && binary.BigEndian.Uint16(v) == gb.AFI_L2VPN && v[2] == gb.SAFI_VPLS {
			if u == nil {
				u = &l2Update{}
			}
			if typ == 14 {
				if len(v) < 4 || len(v) < 5+int(v[3]) {
					return b, nil
				}
				nh := v[4 : 4+int(v[3])]
				if a, ok := netip.AddrFromSlice(nh); ok {
					u.nextHop = a.Unmap().String()
				}
				u.reach = append(u.reach, readL2NLRI(v[5+int(v[3]):])...)
			} else {
				u.unreach = append(u.unreach, readL2NLRI(v[3:])...)
			}
		} else {
			kept = append(kept, b[i:end]...)
		}
		i = end
	}
	if u == nil {
		return b, nil
	}
	out := append([]byte(nil), b[:21+wl]...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(kept)))
	out = append(out, kept...)
	out = append(out, b[apos+al:]...)
	binary.BigEndian.PutUint16(out[16:], uint16(len(out)))
	return out, u
}

// readL2NLRI reads L2VPN NLRIs: a 2-byte length, then RD, VE ID, block
// offset, size and label base (RFC 4761 3.2.2); BGP-AD's 12-byte form
// (RFC 6074) is listed by its RD and VSI ID.
func readL2NLRI(b []byte) []l2NLRI {
	var out []l2NLRI
	for len(b) >= 2 {
		n := int(binary.BigEndian.Uint16(b))
		if 2+n > len(b) || n < 10 {
			break
		}
		v := b[2 : 2+n]
		rd := gb.GetRouteDistinguisher(v[:8])
		switch {
		case n >= 17:
			ve, off, size := binary.BigEndian.Uint16(v[8:]), binary.BigEndian.Uint16(v[10:]), binary.BigEndian.Uint16(v[12:])
			base := u24(v[14:]) >> 4
			k := fmt.Sprintf("%s:%d:%d", rd, ve, off)
			out = append(out, l2NLRI{key: famL2VPN.name + "/" + k, text: fmt.Sprintf("%s (block %d, labels from %d)", k, size, base)})
		case n == 12:
			k := fmt.Sprintf("%s:%s", rd, netip.AddrFrom4([4]byte(v[8:12])))
			out = append(out, l2NLRI{key: famL2VPN.name + "/ad/" + k, text: "bgp-ad " + k})
		}
		b = b[2+n:]
	}
	return out
}

func u24(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }
