package bgp

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// SR Policy (RFC 9830, SAFI 73): candidate paths of SR policies (RFC 9256)
// advertised to headends, which install them and steer traffic for routes
// of the policy's color (color extended community) into them. The NLRI and
// the Tunnel Encapsulation attribute (RFC 9012) with its SR Policy TLV are
// written here: one policy candidate path is one route.

var (
	famSRPv4 = family{gb.RF_SR_POLICY_IPv4, "ipv4-srpolicy", false}
	famSRPv6 = family{gb.RF_SR_POLICY_IPv6, "ipv6-srpolicy", true}
)

// SRPolicy is one candidate path of an SR policy.
type SRPolicy struct {
	Endpoint      string        `json:"endpoint" jsonschema:"required" jsonschema_description:"The policy's endpoint (IPv4: family ipv4-srpolicy, IPv6: ipv6-srpolicy); 0.0.0.0 or :: for a null endpoint" jsonschema_extras:"x-group=Policy"`
	Color         uint32        `json:"color" jsonschema:"required,minimum=1,description=Routes with this color (color:N extended community) are steered into the policy" jsonschema_extras:"x-group=Policy"`
	Headend       string        `json:"headend,omitempty" jsonschema_description:"BGP identifier of the router that should install it (route target IP:0). Default: none, and NO_ADVERTISE instead: for the peer itself" jsonschema_extras:"x-group=Policy"`
	Distinguisher uint32        `json:"distinguisher,omitempty" jsonschema:"description=Tells candidate paths of one policy apart. Default 1" jsonschema_extras:"x-group=Policy"`
	Preference    *uint32       `json:"preference,omitempty" jsonschema:"description=The highest valid candidate path is used. Default 100" jsonschema_extras:"x-group=Candidate path"`
	BindingSID    uint32        `json:"binding_sid,omitempty" jsonschema:"minimum=0,maximum=1048575,description=MPLS label that steers into the policy (0: none: the headend may pick one)" jsonschema_extras:"x-group=Candidate path"`
	SpecifiedBSID bool          `json:"specified_bsid_only,omitempty" jsonschema:"description=Use the candidate path only with this binding SID (S flag)" jsonschema_extras:"x-group=Candidate path,x-advanced=true"`
	DropInvalid   bool          `json:"drop_upon_invalid,omitempty" jsonschema:"description=Drop traffic for the binding SID while the policy is invalid (I flag)" jsonschema_extras:"x-group=Candidate path,x-advanced=true"`
	SegmentLists  []SegmentList `json:"segment_lists" jsonschema:"required,description=Paths (traffic is spread over them by weight)" jsonschema_extras:"x-group=Candidate path,x-summary=[weight {weight}: ]{labels}"`
	Priority      *uint8        `json:"priority,omitempty" jsonschema:"description=Recomputation order after a topology change (lower first)" jsonschema_extras:"x-group=Candidate path,x-advanced=true"`
	ENLP          string        `json:"explicit_null,omitempty" jsonschema:"enum=ipv4,enum=ipv6,enum=both,enum=none,description=Explicit NULL label policy. Default: not sent (the headend decides)" jsonschema_extras:"x-group=Candidate path,x-advanced=true"`
	Name          string        `json:"name,omitempty" jsonschema:"description=Policy name" jsonschema_extras:"x-group=Candidate path,x-advanced=true"`
	PathName      string        `json:"path_name,omitempty" jsonschema:"description=Candidate path name" jsonschema_extras:"x-group=Candidate path,x-advanced=true"`
	Attributes
}

// SegmentList is one path: SR-MPLS labels, outermost first.
type SegmentList struct {
	Labels []uint32 `json:"labels" jsonschema:"required,description=Segment labels (node or adjacency SIDs) outermost first"`
	Weight uint32   `json:"weight,omitempty" jsonschema:"description=Share of the traffic. Default: not sent (equal shares)"`
}

var enlpValues = map[string]byte{"ipv4": 1, "ipv6": 2, "both": 3, "none": 4}

const noAdvertise = 0xffffff02

func (p SRPolicy) endpoint() (netip.Addr, error) {
	a, err := netip.ParseAddr(p.Endpoint)
	if err != nil {
		return a, fmt.Errorf("sr policy endpoint %q: not an IP address", p.Endpoint)
	}
	return a.Unmap(), nil
}

func (p SRPolicy) name() string {
	return fmt.Sprintf("sr-policy color %d endpoint %s", p.Color, p.Endpoint)
}

// subTLV writes an RFC 9012 sub-TLV: types 128 and up have a 2-octet length.
func subTLV(b []byte, typ byte, v []byte) []byte {
	b = append(b, typ)
	if typ >= 128 {
		b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
	} else {
		b = append(b, byte(len(v)))
	}
	return append(b, v...)
}

// tunnelEncap is the Tunnel Encapsulation attribute's value: one SR Policy
// TLV (tunnel type 15) with the candidate path's sub-TLVs.
func (p SRPolicy) tunnelEncap() ([]byte, error) {
	var sub []byte
	pref := uint32(100)
	if p.Preference != nil {
		pref = *p.Preference
	}
	sub = subTLV(sub, 12, binary.BigEndian.AppendUint32([]byte{0, 0}, pref)) // Preference
	bsid := []byte{0, 0}
	if p.SpecifiedBSID {
		bsid[0] |= 0x80
	}
	if p.DropInvalid {
		bsid[0] |= 0x40
	}
	if p.BindingSID != 0 {
		if p.BindingSID < 16 || p.BindingSID > 0xfffff {
			return nil, fmt.Errorf("%s: binding_sid must be 16-1048575", p.name())
		}
		bsid = binary.BigEndian.AppendUint32(bsid, p.BindingSID<<12)
	}
	sub = subTLV(sub, 13, bsid) // Binding SID
	if p.ENLP != "" {
		v, ok := enlpValues[p.ENLP]
		if !ok {
			return nil, fmt.Errorf("%s: explicit_null must be ipv4, ipv6, both or none", p.name())
		}
		sub = subTLV(sub, 14, []byte{0, 0, v})
	}
	if p.Priority != nil {
		sub = subTLV(sub, 15, []byte{*p.Priority, 0})
	}
	if p.PathName != "" {
		sub = subTLV(sub, 129, append([]byte{0}, p.PathName...))
	}
	if p.Name != "" {
		sub = subTLV(sub, 130, append([]byte{0}, p.Name...))
	}
	if len(p.SegmentLists) == 0 {
		return nil, fmt.Errorf("%s: at least one segment list", p.name())
	}
	for i, sl := range p.SegmentLists {
		if len(sl.Labels) == 0 || len(sl.Labels) > 64 {
			return nil, fmt.Errorf("%s: segment list %d: 1-64 labels", p.name(), i+1)
		}
		v := []byte{0} // reserved
		if sl.Weight != 0 {
			v = subTLV(v, 9, binary.BigEndian.AppendUint32([]byte{0, 0}, sl.Weight))
		}
		for _, l := range sl.Labels {
			if l > 0xfffff {
				return nil, fmt.Errorf("%s: segment list %d: label %d is above 1048575", p.name(), i+1, l)
			}
			// Type A: flags, reserved, label(20) TC(3) S(1) TTL(8); TC 0 and
			// TTL 255 leave both to the headend.
			v = subTLV(v, 1, binary.BigEndian.AppendUint32([]byte{0, 0}, l<<12|0xff))
		}
		sub = subTLV(sub, 128, v)
	}
	tlv := binary.BigEndian.AppendUint16(nil, 15) // tunnel type SR Policy
	tlv = binary.BigEndian.AppendUint16(tlv, uint16(len(sub)))
	return append(tlv, sub...), nil
}

// expand builds the candidate path's route.
func (p SRPolicy) expand() (*xroute, error) {
	ep, err := p.endpoint()
	if err != nil {
		return nil, err
	}
	if p.Color == 0 {
		return nil, fmt.Errorf("%s: color must not be 0", p.name())
	}
	f, bits := famSRPv4, byte(96)
	if ep.Is6() {
		f, bits = famSRPv6, 192
	}
	dist := p.Distinguisher
	if dist == 0 {
		dist = 1
	}
	var rts []string
	if p.Headend != "" {
		h, err := netip.ParseAddr(p.Headend)
		if err != nil || !h.Is4() {
			return nil, fmt.Errorf("%s: headend must be the router's BGP identifier (IPv4)", p.name())
		}
		rts = []string{h.String() + ":0"}
	}
	t, err := p.Attributes.route(p.name(), rts).attrs()
	if err != nil {
		return nil, err
	}
	if p.Headend == "" {
		t.comms = append(t.comms, noAdvertise)
	}
	enc, err := p.tunnelEncap()
	if err != nil {
		return nil, err
	}
	t.extra = append(t.extra, gb.NewPathAttributeUnknown(gb.BGP_ATTR_FLAG_OPTIONAL|gb.BGP_ATTR_FLAG_TRANSITIVE, gb.BGP_ATTR_TYPE_TUNNEL_ENCAP, enc))

	nlri := []byte{bits}
	nlri = binary.BigEndian.AppendUint32(nlri, dist)
	nlri = binary.BigEndian.AppendUint32(nlri, p.Color)
	nlri = append(nlri, ep.AsSlice()...)
	key := fmt.Sprintf("%d:%d:%s", dist, p.Color, ep)
	return &xroute{f: f, key: f.name + "/" + key, nlri: nlri, text: p.describe(dist, ep), t: t}, nil
}

func (p SRPolicy) describe(dist uint32, ep netip.Addr) string {
	pref := uint32(100)
	if p.Preference != nil {
		pref = *p.Preference
	}
	s := fmt.Sprintf("color %d endpoint %s (distinguisher %d) preference %d", p.Color, ep, dist, pref)
	if p.BindingSID != 0 {
		s += fmt.Sprintf(" bsid %d", p.BindingSID)
	}
	var ls []string
	for _, sl := range p.SegmentLists {
		l := strings.Trim(fmt.Sprint(sl.Labels), "[]")
		if sl.Weight != 0 {
			l = fmt.Sprintf("%s (weight %d)", l, sl.Weight)
		}
		ls = append(ls, "["+l+"]")
	}
	s += " segments " + strings.Join(ls, " ")
	if p.Headend != "" {
		s += " for " + p.Headend
	}
	return s
}

func (p SRPolicy) check() error {
	_, err := p.expand()
	return err
}
