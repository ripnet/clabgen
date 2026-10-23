package packet

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strconv"
	"strings"

	"github.com/ripnet/clabgen/internal/ipflow"
	"github.com/ripnet/clabgen/internal/pkt"
)

// Layer is one header (or the payload) of a packet: exactly one of its
// fields is set. In YAML: `- ipv4: {dst: 10.0.0.2}`.
type Layer struct {
	Ethernet *Ethernet `json:"ethernet,omitempty" jsonschema_description:"Ethernet header: first (the frame's own, sent from the device with its VLAN tags) or inner (after VXLAN, MPLS, …)"`
	VLAN     *VLAN     `json:"vlan,omitempty" jsonschema_description:"An 802.1Q/802.1ad tag inside the device's own tags"`
	MPLS     *MPLS     `json:"mpls,omitempty" jsonschema:"description=An MPLS label stack entry"`
	ARP      *ARP      `json:"arp,omitempty" jsonschema:"description=ARP (Ethernet/IPv4)"`
	IPv4     *IPv4     `json:"ipv4,omitempty"`
	IPv6     *IPv6     `json:"ipv6,omitempty"`
	UDP      *UDP      `json:"udp,omitempty"`
	TCP      *TCP      `json:"tcp,omitempty" jsonschema:"description=A TCP segment (sent as is; there is no connection)"`
	ICMP     *ICMP     `json:"icmp,omitempty" jsonschema:"description=ICMP for IPv4"`
	ICMPv6   *ICMP     `json:"icmpv6,omitempty" jsonschema:"description=ICMPv6"`
	VXLAN    *VXLAN    `json:"vxlan,omitempty"`
	Raw      *Raw      `json:"raw,omitempty" jsonschema:"description=Bytes given as hex (any header clabgen doesn't model)"`
	Payload  *Payload  `json:"payload,omitempty"`
}

type Ethernet struct {
	Dst       string  `json:"dst,omitempty" jsonschema_description:"Destination MAC. Default: first layer: the next hop of the first IP destination (ARP/ND), else broadcast; inner: broadcast"`
	Src       string  `json:"src,omitempty" jsonschema:"description=Source MAC. Default: the device's"`
	EtherType *uint16 `json:"ethertype,omitempty" jsonschema:"description=Default: from the next layer (0x88b5 when it is raw or payload)"`
}

type VLAN struct {
	TPID uint16 `json:"tpid,omitempty" jsonschema:"description=Tag protocol ID. Default 0x8100"`
	VID  uint16 `json:"vid" jsonschema:"minimum=0,maximum=4095"`
	PCP  uint8  `json:"pcp,omitempty" jsonschema:"minimum=0,maximum=7"`
	DEI  bool   `json:"dei,omitempty"`
}

type MPLS struct {
	Label uint32 `json:"label" jsonschema:"minimum=0,maximum=1048575"`
	TC    uint8  `json:"tc,omitempty" jsonschema:"minimum=0,maximum=7,description=Traffic class"`
	TTL   *uint8 `json:"ttl,omitempty" jsonschema:"description=Default 64"`
	BOS   *bool  `json:"bos,omitempty" jsonschema:"description=Bottom of stack. Default: set on the last MPLS layer"`
}

type ARP struct {
	Op        string `json:"op,omitempty" jsonschema:"description=request or reply or a number. Default request"`
	SenderMAC string `json:"sender_mac,omitempty" jsonschema:"description=Default: the device's MAC"`
	SenderIP  string `json:"sender_ip,omitempty" jsonschema:"description=Default: the device's first IPv4 address"`
	TargetMAC string `json:"target_mac,omitempty" jsonschema:"description=Default 00:00:00:00:00:00"`
	TargetIP  string `json:"target_ip" jsonschema:"required"`
}

type IPv4 struct {
	Src        string  `json:"src,omitempty" jsonschema:"description=Default: the device's address toward dst"`
	Dst        string  `json:"dst" jsonschema:"required"`
	DSCP       uint8   `json:"dscp,omitempty" jsonschema:"minimum=0,maximum=63"`
	ECN        uint8   `json:"ecn,omitempty" jsonschema:"minimum=0,maximum=3"`
	ID         *uint16 `json:"id,omitempty" jsonschema:"description=Identification. Default: counts up per packet"`
	DF         bool    `json:"df,omitempty" jsonschema:"description=Don't fragment"`
	MF         bool    `json:"mf,omitempty" jsonschema:"description=More fragments"`
	FragOffset uint16  `json:"frag_offset,omitempty" jsonschema:"minimum=0,maximum=8191,description=Fragment offset in 8-byte units"`
	TTL        *uint8  `json:"ttl,omitempty" jsonschema:"description=Default 64"`
	Protocol   *uint8  `json:"protocol,omitempty" jsonschema:"description=Default: from the next layer (253 when it is raw or payload)"`
	Length     *uint16 `json:"length,omitempty" jsonschema:"description=Total length. Default: the real one"`
	Checksum   string  `json:"checksum,omitempty" jsonschema:"enum=correct,enum=zero,enum=invalid,description=Default correct"`
	Options    string  `json:"options,omitempty" jsonschema:"description=Options as hex (padded to 4 bytes)"`
}

type IPv6 struct {
	Src          string  `json:"src,omitempty" jsonschema:"description=Default: the device's address toward dst"`
	Dst          string  `json:"dst" jsonschema:"required"`
	TrafficClass uint8   `json:"traffic_class,omitempty"`
	FlowLabel    uint32  `json:"flow_label,omitempty" jsonschema:"minimum=0,maximum=1048575"`
	HopLimit     *uint8  `json:"hop_limit,omitempty" jsonschema:"description=Default 64"`
	NextHeader   *uint8  `json:"next_header,omitempty" jsonschema:"description=Default: from the next layer (59 when there is none; 253 when it is raw or payload)"`
	Length       *uint16 `json:"length,omitempty" jsonschema:"description=Payload length. Default: the real one"`
}

type UDP struct {
	SrcPort  *uint16 `json:"src_port,omitempty" jsonschema:"description=Default: a port derived from the flow (49152-65535)"`
	DstPort  *uint16 `json:"dst_port,omitempty" jsonschema:"description=Default: 4789 before VXLAN; else 9 (discard)"`
	Length   *uint16 `json:"length,omitempty" jsonschema:"description=Default: the real one"`
	Checksum string  `json:"checksum,omitempty" jsonschema:"enum=correct,enum=zero,enum=invalid,description=Default correct (zero: none over IPv4)"`
}

type TCP struct {
	SrcPort  *uint16 `json:"src_port,omitempty" jsonschema:"description=Default: a port derived from the flow (49152-65535)"`
	DstPort  uint16  `json:"dst_port" jsonschema:"required"`
	Seq      uint32  `json:"seq,omitempty"`
	Ack      uint32  `json:"ack,omitempty"`
	Flags    string  `json:"flags,omitempty" jsonschema:"description=Letters FSRPAUEC (e.g. S or SA) or a number. Default S"`
	Window   *uint16 `json:"window,omitempty" jsonschema:"description=Default 65535"`
	Urgent   uint16  `json:"urgent,omitempty"`
	Checksum string  `json:"checksum,omitempty" jsonschema:"enum=correct,enum=zero,enum=invalid,description=Default correct"`
	Options  string  `json:"options,omitempty" jsonschema:"description=Options as hex (padded to 4 bytes)"`
}

type ICMP struct {
	Type     *uint8 `json:"type,omitempty" jsonschema:"description=Default: echo request (8 or 128)"`
	Code     uint8  `json:"code,omitempty"`
	ID       uint16 `json:"id,omitempty" jsonschema_description:"Identifier (echo); with seq, the 4 bytes after the checksum"`
	Seq      uint16 `json:"seq,omitempty"`
	Checksum string `json:"checksum,omitempty" jsonschema:"enum=correct,enum=zero,enum=invalid,description=Default correct"`
}

type VXLAN struct {
	VNI   uint32 `json:"vni" jsonschema:"minimum=0,maximum=16777215"`
	Flags *uint8 `json:"flags,omitempty" jsonschema:"description=Default 0x08 (VNI present)"`
}

type Raw struct {
	Hex string `json:"hex" jsonschema:"required"`
}

type Payload struct {
	Size      int    `json:"size" jsonschema:"minimum=0,maximum=9000,description=Bytes (the signature counts)"`
	Pattern   string `json:"pattern,omitempty" jsonschema:"enum=increment,enum=zeros,enum=random,enum=text,enum=hex,description=Default increment"`
	Data      string `json:"data,omitempty" jsonschema:"description=Text or hex digits for the text and hex patterns; repeated to fill the payload"`
	Signature *bool  `json:"signature,omitempty" jsonschema_description:"Start with the 24-byte signature (flow ID, sequence, send time), so clabgen receivers count the stream. Default on"`
}

// UnmarshalJSON decodes into a zeroed layer: encoding/json reuses the
// elements of a slice it decodes into (the defaults' layers), which would
// leave their headers set alongside the new ones.
func (l *Layer) UnmarshalJSON(b []byte) error {
	type plain Layer
	var p plain
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return err
	}
	*l = Layer(p)
	return nil
}

// kind is the layer's name ("ipv4", …); "" when none or several are set.
func (l Layer) kind() string {
	k := ""
	v := reflect.ValueOf(l)
	for i := 0; i < v.NumField(); i++ {
		if !v.Field(i).IsNil() {
			if k != "" {
				return ""
			}
			k = strings.Split(v.Type().Field(i).Tag.Get("json"), ",")[0]
		}
	}
	return k
}

var titles = map[string]string{"ethernet": "Ethernet", "vlan": "VLAN", "mpls": "MPLS", "arp": "ARP", "ipv4": "IPv4", "ipv6": "IPv6",
	"udp": "UDP", "tcp": "TCP", "icmp": "ICMP", "icmpv6": "ICMPv6", "vxlan": "VXLAN", "raw": "Raw", "payload": "Payload"}

// What a layer is to the one before it.
var etherTypes = map[string]uint16{"vlan": 0, "mpls": 0x8847, "arp": pkt.EtherTypeARP, "ipv4": pkt.EtherTypeIPv4, "ipv6": pkt.EtherTypeIPv6}
var ipProtos = map[string]uint8{"udp": 17, "tcp": 6, "icmp": 1, "icmpv6": 58, "ipv4": 4, "ipv6": 41, "mpls": 137}

// Inputs a build needs from the device.
type buildCtx struct {
	devMAC pkt.MAC
	dstMAC pkt.MAC // the first layer's destination when not configured (zero: broadcast)
	source func(dst netip.Addr) (netip.Addr, bool)
	port   uint16 // default source port
}

// A built layer, for previews and field lookup.
type region struct {
	Layer  int     `json:"layer"` // index in the config's layers (-1: the implicit Ethernet header)
	Kind   string  `json:"kind"`
	Title  string  `json:"title"`
	Off    int     `json:"offset"`
	Len    int     `json:"length"`
	Fields []field `json:"-"`
}

// A template is a packet ready to send: its bytes, the fields modifiers
// change, and the checksums to redo after they do.
type template struct {
	frame   []byte // outer dst, src, ethertype, then the rest; no device tags
	regions []region
	fields  map[string]field // "ipv4.src", "vlan[2].vid"
	fixups  []func(b []byte) // checksums, innermost first
	sigOff  int              // -1: no signature
	idField *field           // IPv4 ID to count up per packet
}

func parseHex(s string) ([]byte, error) {
	s = strings.NewReplacer(" ", "", ":", "", "\n", "", "\t", "").Replace(s)
	return hex.DecodeString(s)
}

func pad4(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func u8(p *uint8, d uint8) uint8 {
	if p != nil {
		return *p
	}
	return d
}

func u16(p *uint16, d uint16) uint16 {
	if p != nil {
		return *p
	}
	return d
}

func mac(s string, d pkt.MAC) (pkt.MAC, error) {
	if s == "" {
		return d, nil
	}
	return pkt.ParseMAC(s)
}

func parseOp(s string) (uint16, error) {
	switch s {
	case "", "request":
		return 1, nil
	case "reply":
		return 2, nil
	}
	v, err := strconv.ParseUint(s, 0, 16)
	if err != nil {
		return 0, fmt.Errorf("op %q: request, reply or a number", s)
	}
	return uint16(v), nil
}

func parseFlags(s string) (uint8, error) {
	if s == "" {
		return 0x02, nil
	}
	if v, err := strconv.ParseUint(s, 0, 8); err == nil {
		return uint8(v), nil
	}
	var f uint8
	for _, c := range strings.ToUpper(s) {
		i := strings.IndexRune("FSRPAUEC", c)
		if i < 0 {
			return 0, fmt.Errorf("flags %q: letters FSRPAUEC or a number", s)
		}
		f |= 1 << i
	}
	return f, nil
}

// checkLayers validates what can be checked without a device.
func checkLayers(ls []Layer) error {
	if len(ls) == 0 {
		return errors.New("layers: at least one")
	}
	ip := false
	for i, l := range ls {
		k := l.kind()
		if k == "" {
			return fmt.Errorf("layer %d: set exactly one of ethernet, vlan, mpls, arp, ipv4, ipv6, udp, tcp, icmp, icmpv6, vxlan, raw, payload", i+1)
		}
		switch k {
		case "udp", "tcp":
			if !ip {
				return fmt.Errorf("layer %d (%s): needs an IPv4 or IPv6 layer before it", i+1, k)
			}
		case "icmp", "icmpv6":
			want := map[string]string{"icmp": "ipv4", "icmpv6": "ipv6"}[k]
			if j := lastIP(ls[:i]); j < 0 || ls[j].kind() != want {
				return fmt.Errorf("layer %d (%s): needs an %s layer before it", i+1, k, titles[want])
			}
		case "ipv4", "ipv6":
			ip = true
		case "payload":
			if i != len(ls)-1 {
				return fmt.Errorf("layer %d: the payload must be the last layer", i+1)
			}
		}
	}
	return nil
}

func lastIP(ls []Layer) int {
	for i := len(ls) - 1; i >= 0; i-- {
		if k := ls[i].kind(); k == "ipv4" || k == "ipv6" {
			return i
		}
	}
	return -1
}

// firstIPDst is the destination the frame's next hop is resolved for.
func firstIPDst(ls []Layer) (netip.Addr, bool) {
	for _, l := range ls {
		switch {
		case l.IPv4 != nil:
			a, err := netip.ParseAddr(l.IPv4.Dst)
			return a, err == nil
		case l.IPv6 != nil:
			a, err := netip.ParseAddr(l.IPv6.Dst)
			return a, err == nil
		}
	}
	return netip.Addr{}, false
}

// build lays out the packet.
func build(ls []Layer, cx buildCtx) (*template, error) {
	if err := checkLayers(ls); err != nil {
		return nil, err
	}
	t := &template{fields: map[string]field{}, sigOff: -1}
	seen := map[string]int{}
	var b []byte
	add := func(r *region, name string, byteOff, bitOff, bits int, k kind) {
		r.Fields = append(r.Fields, field{Name: name, Pos: (r.Off+byteOff)*8 + bitOff, Bits: bits, Kind: k})
	}
	// Each IP layer's start, for pseudo-headers.
	ipAt := map[int]int{}

	// The outer Ethernet header is the first layer, written or implied.
	first := Ethernet{}
	start := 0
	if ls[0].Ethernet != nil {
		first, start = *ls[0].Ethernet, 1
	}
	layerIdx := -1
	if start == 1 {
		layerIdx = 0
	}

	ethernet := func(idx int, e Ethernet, next string, outer bool) (region, error) {
		r := region{Layer: idx, Kind: "ethernet", Title: "Ethernet", Off: len(b), Len: 14}
		dd := pkt.MAC{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		if outer && !cx.dstMAC.IsZero() {
			dd = cx.dstMAC
		}
		dst, err := mac(e.Dst, dd)
		if err != nil {
			return r, fmt.Errorf("ethernet dst: %w", err)
		}
		src, err := mac(e.Src, cx.devMAC)
		if err != nil {
			return r, fmt.Errorf("ethernet src: %w", err)
		}
		et := uint16(0x88b5)
		if e.EtherType != nil {
			et = *e.EtherType
		} else if next != "" {
			if v, ok := etherTypes[next]; ok {
				et = v
				if next == "vlan" {
					et = 0 // filled in by the tag
				}
			}
		}
		b = append(b, dst[:]...)
		b = append(b, src[:]...)
		b = binary.BigEndian.AppendUint16(b, et)
		add(&r, "dst", 0, 0, 48, kindMAC)
		add(&r, "src", 6, 0, 48, kindMAC)
		add(&r, "ethertype", 12, 0, 16, kindInt)
		return r, nil
	}

	r0, err := ethernet(layerIdx, first, kindAt(ls, start), true)
	if err != nil {
		return nil, err
	}
	t.regions = append(t.regions, r0)
	// The ethertype a VLAN layer sets is the field before it.
	prevET := func() int { return len(b) - 2 }

	for i := start; i < len(ls); i++ {
		l, k, next := ls[i], ls[i].kind(), kindAt(ls, i+1)
		r := region{Layer: i, Kind: k, Title: titles[k], Off: len(b)}
		switch k {
		case "ethernet":
			er, err := ethernet(i, *l.Ethernet, next, false)
			if err != nil {
				return nil, fmt.Errorf("layer %d: %w", i+1, err)
			}
			r = er
		case "vlan":
			v := l.VLAN
			if v.VID > 4095 || v.PCP > 7 {
				return nil, fmt.Errorf("layer %d: vid 0-4095, pcp 0-7", i+1)
			}
			tpid := v.TPID
			if tpid == 0 {
				tpid = pkt.TPIDDot1Q
			}
			if binary.BigEndian.Uint16(b[prevET():]) == 0 {
				binary.BigEndian.PutUint16(b[prevET():], tpid)
			}
			tci := uint16(v.PCP)<<13 | v.VID
			if v.DEI {
				tci |= 1 << 12
			}
			et := uint16(0x88b5)
			if x, ok := etherTypes[next]; ok {
				et = x
			}
			b = binary.BigEndian.AppendUint16(b, tci)
			b = binary.BigEndian.AppendUint16(b, et) // 0 before another tag: that tag fills it in
			add(&r, "pcp", 0, 0, 3, kindInt)
			add(&r, "dei", 0, 3, 1, kindInt)
			add(&r, "vid", 0, 4, 12, kindInt)
			add(&r, "ethertype", 2, 0, 16, kindInt)
		case "mpls":
			m := l.MPLS
			if m.Label > 1<<20-1 || m.TC > 7 {
				return nil, fmt.Errorf("layer %d: label 0-1048575, tc 0-7", i+1)
			}
			bos := next != "mpls"
			if m.BOS != nil {
				bos = *m.BOS
			}
			w := m.Label<<12 | uint32(m.TC)<<9 | uint32(u8(m.TTL, 64))
			if bos {
				w |= 1 << 8
			}
			b = binary.BigEndian.AppendUint32(b, w)
			add(&r, "label", 0, 0, 20, kindInt)
			add(&r, "tc", 2, 4, 3, kindInt)
			add(&r, "ttl", 3, 0, 8, kindInt)
		case "arp":
			a := l.ARP
			op, err := parseOp(a.Op)
			if err != nil {
				return nil, fmt.Errorf("layer %d: %w", i+1, err)
			}
			smac, err := mac(a.SenderMAC, cx.devMAC)
			if err != nil {
				return nil, fmt.Errorf("layer %d: sender_mac: %w", i+1, err)
			}
			tmac, err := mac(a.TargetMAC, pkt.MAC{})
			if err != nil {
				return nil, fmt.Errorf("layer %d: target_mac: %w", i+1, err)
			}
			tip, err := netip.ParseAddr(a.TargetIP)
			if err != nil || !tip.Is4() {
				return nil, fmt.Errorf("layer %d: target_ip: an IPv4 address", i+1)
			}
			sip := netip.Addr{}
			if a.SenderIP != "" {
				if sip, err = netip.ParseAddr(a.SenderIP); err != nil || !sip.Is4() {
					return nil, fmt.Errorf("layer %d: sender_ip: an IPv4 address", i+1)
				}
			} else if s, ok := cx.source(tip); ok && s.Is4() {
				sip = s
			} else {
				sip = netip.IPv4Unspecified()
			}
			b = append(b, pkt.ARP{Op: op, SHA: smac, SPA: sip, THA: tmac, TPA: tip}.Encode()...)
			add(&r, "op", 6, 0, 16, kindInt)
			add(&r, "sender_mac", 8, 0, 48, kindMAC)
			add(&r, "sender_ip", 14, 0, 32, kindIPv4)
			add(&r, "target_mac", 18, 0, 48, kindMAC)
			add(&r, "target_ip", 24, 0, 32, kindIPv4)
		case "ipv4":
			h := l.IPv4
			dst, err := netip.ParseAddr(h.Dst)
			if err != nil || !dst.Is4() {
				return nil, fmt.Errorf("layer %d: ipv4 dst: an IPv4 address", i+1)
			}
			src, err := ipSrc(h.Src, dst, cx, i)
			if err != nil {
				return nil, err
			}
			if h.DSCP > 63 || h.ECN > 3 || h.FragOffset > 8191 {
				return nil, fmt.Errorf("layer %d: dscp 0-63, ecn 0-3, frag_offset 0-8191", i+1)
			}
			opts, err := parseHex(h.Options)
			if err != nil {
				return nil, fmt.Errorf("layer %d: options: %w", i+1, err)
			}
			opts = pad4(opts)
			if len(opts) > 40 {
				return nil, fmt.Errorf("layer %d: at most 40 bytes of options", i+1)
			}
			proto := uint8(253)
			if h.Protocol != nil {
				proto = *h.Protocol
			} else if p, ok := ipProtos[next]; ok {
				proto = p
			}
			hl := 20 + len(opts)
			hdr := make([]byte, hl)
			hdr[0] = 0x40 | byte(hl/4)
			hdr[1] = h.DSCP<<2 | h.ECN
			binary.BigEndian.PutUint16(hdr[4:], u16(h.ID, 0))
			fo := h.FragOffset
			if h.DF {
				fo |= 0x4000
			}
			if h.MF {
				fo |= 0x2000
			}
			binary.BigEndian.PutUint16(hdr[6:], fo)
			hdr[8] = u8(h.TTL, 64)
			hdr[9] = proto
			copy(hdr[12:], src.AsSlice())
			copy(hdr[16:], dst.AsSlice())
			copy(hdr[20:], opts)
			b = append(b, hdr...)
			ipAt[i] = r.Off
			add(&r, "dscp", 1, 0, 6, kindInt)
			add(&r, "ecn", 1, 6, 2, kindInt)
			add(&r, "length", 2, 0, 16, kindInt)
			add(&r, "id", 4, 0, 16, kindInt)
			add(&r, "frag_offset", 6, 3, 13, kindInt)
			add(&r, "ttl", 8, 0, 8, kindInt)
			add(&r, "protocol", 9, 0, 8, kindInt)
			add(&r, "checksum", 10, 0, 16, kindInt)
			add(&r, "src", 12, 0, 32, kindIPv4)
			add(&r, "dst", 16, 0, 32, kindIPv4)
			if h.ID == nil {
				f := r.Fields[3]
				t.idField = &f
			}
		case "ipv6":
			h := l.IPv6
			dst, err := netip.ParseAddr(h.Dst)
			if err != nil || !dst.Is6() || dst.Is4In6() {
				return nil, fmt.Errorf("layer %d: ipv6 dst: an IPv6 address", i+1)
			}
			src, err := ipSrc(h.Src, dst, cx, i)
			if err != nil {
				return nil, err
			}
			if h.FlowLabel > 1<<20-1 {
				return nil, fmt.Errorf("layer %d: flow_label 0-1048575", i+1)
			}
			nh := uint8(59)
			if h.NextHeader != nil {
				nh = *h.NextHeader
			} else if p, ok := ipProtos[next]; ok {
				nh = p
			} else if next != "" {
				nh = 253
			}
			hdr := make([]byte, 40)
			binary.BigEndian.PutUint32(hdr, 6<<28|uint32(h.TrafficClass)<<20|h.FlowLabel)
			hdr[6] = nh
			hdr[7] = u8(h.HopLimit, 64)
			copy(hdr[8:], src.AsSlice())
			copy(hdr[24:], dst.AsSlice())
			b = append(b, hdr...)
			ipAt[i] = r.Off
			add(&r, "traffic_class", 0, 4, 8, kindInt)
			add(&r, "flow_label", 1, 4, 20, kindInt)
			add(&r, "length", 4, 0, 16, kindInt)
			add(&r, "next_header", 6, 0, 8, kindInt)
			add(&r, "hop_limit", 7, 0, 8, kindInt)
			add(&r, "src", 8, 0, 128, kindIPv6)
			add(&r, "dst", 24, 0, 128, kindIPv6)
		case "udp":
			u := l.UDP
			dp := uint16(9)
			if next == "vxlan" {
				dp = 4789
			}
			h := make([]byte, 8)
			binary.BigEndian.PutUint16(h, u16(u.SrcPort, cx.port))
			binary.BigEndian.PutUint16(h[2:], u16(u.DstPort, dp))
			b = append(b, h...)
			add(&r, "src_port", 0, 0, 16, kindInt)
			add(&r, "dst_port", 2, 0, 16, kindInt)
			add(&r, "length", 4, 0, 16, kindInt)
			add(&r, "checksum", 6, 0, 16, kindInt)
		case "tcp":
			c := l.TCP
			flags, err := parseFlags(c.Flags)
			if err != nil {
				return nil, fmt.Errorf("layer %d: %w", i+1, err)
			}
			opts, err := parseHex(c.Options)
			if err != nil {
				return nil, fmt.Errorf("layer %d: options: %w", i+1, err)
			}
			opts = pad4(opts)
			if len(opts) > 40 {
				return nil, fmt.Errorf("layer %d: at most 40 bytes of options", i+1)
			}
			h := make([]byte, 20+len(opts))
			binary.BigEndian.PutUint16(h, u16(c.SrcPort, cx.port))
			binary.BigEndian.PutUint16(h[2:], c.DstPort)
			binary.BigEndian.PutUint32(h[4:], c.Seq)
			binary.BigEndian.PutUint32(h[8:], c.Ack)
			h[12] = byte(len(h)/4) << 4
			h[13] = flags
			binary.BigEndian.PutUint16(h[14:], u16(c.Window, 65535))
			binary.BigEndian.PutUint16(h[18:], c.Urgent)
			copy(h[20:], opts)
			b = append(b, h...)
			add(&r, "src_port", 0, 0, 16, kindInt)
			add(&r, "dst_port", 2, 0, 16, kindInt)
			add(&r, "seq", 4, 0, 32, kindInt)
			add(&r, "ack", 8, 0, 32, kindInt)
			add(&r, "flags", 13, 0, 8, kindInt)
			add(&r, "window", 14, 0, 16, kindInt)
			add(&r, "checksum", 16, 0, 16, kindInt)
		case "icmp", "icmpv6":
			c := l.ICMP
			if c == nil {
				c = l.ICMPv6
			}
			typ := uint8(8)
			if k == "icmpv6" {
				typ = 128
			}
			h := make([]byte, 8)
			h[0] = u8(c.Type, typ)
			h[1] = c.Code
			binary.BigEndian.PutUint16(h[4:], c.ID)
			binary.BigEndian.PutUint16(h[6:], c.Seq)
			b = append(b, h...)
			add(&r, "type", 0, 0, 8, kindInt)
			add(&r, "code", 1, 0, 8, kindInt)
			add(&r, "checksum", 2, 0, 16, kindInt)
			add(&r, "id", 4, 0, 16, kindInt)
			add(&r, "seq", 6, 0, 16, kindInt)
		case "vxlan":
			x := l.VXLAN
			if x.VNI > 1<<24-1 {
				return nil, fmt.Errorf("layer %d: vni 0-16777215", i+1)
			}
			h := make([]byte, 8)
			h[0] = u8(x.Flags, 0x08)
			binary.BigEndian.PutUint32(h[4:], x.VNI<<8)
			b = append(b, h...)
			add(&r, "vni", 4, 0, 24, kindInt)
		case "raw":
			h, err := parseHex(l.Raw.Hex)
			if err != nil {
				return nil, fmt.Errorf("layer %d: raw hex: %w", i+1, err)
			}
			b = append(b, h...)
		case "payload":
			p := l.Payload
			if p.Size < 0 || p.Size > 9000 {
				return nil, fmt.Errorf("layer %d: payload size 0-9000", i+1)
			}
			pat := p.Pattern
			if pat == "" {
				pat = "increment"
			}
			if (pat == "text" || pat == "hex") && p.Data == "" {
				return nil, fmt.Errorf("layer %d: payload pattern %s needs data", i+1, pat)
			}
			if pat == "hex" {
				if _, err := parseHex(p.Data); err != nil {
					return nil, fmt.Errorf("layer %d: payload data: %w", i+1, err)
				}
			}
			data := ipflow.Fill(ipflow.Payload{PayloadSize: p.Size, PayloadPattern: pat, PayloadData: p.Data})
			if (p.Signature == nil || *p.Signature) && p.Size >= ipflow.SigLen {
				t.sigOff = len(b)
			}
			b = append(b, data...)
		}
		r.Len = len(b) - r.Off
		t.regions = append(t.regions, r)
	}
	t.frame = b

	// Lengths, then checksums (per packet: modifiers and the signature change them).
	end := len(b)
	for ri := len(t.regions) - 1; ri >= 0; ri-- {
		r := t.regions[ri]
		if r.Layer < 0 {
			continue
		}
		l := ls[r.Layer]
		off := r.Off
		switch r.Kind {
		case "ipv4":
			binary.BigEndian.PutUint16(b[off+2:], u16(l.IPv4.Length, uint16(end-off)))
			mode := l.IPv4.Checksum
			hl := r.Len
			t.fixups = append(t.fixups, func(b []byte) {
				b[off+10], b[off+11] = 0, 0
				binary.BigEndian.PutUint16(b[off+10:], ipflow.Checksum(orCorrect(mode), pkt.Checksum(b[off:off+hl], 0)))
			})
		case "ipv6":
			binary.BigEndian.PutUint16(b[off+4:], u16(l.IPv6.Length, uint16(end-off-40)))
		case "udp", "tcp", "icmpv6":
			if r.Kind == "udp" {
				binary.BigEndian.PutUint16(b[off+4:], u16(l.UDP.Length, uint16(end-off)))
			}
			ip := ipAt[lastIP(ls[:r.Layer])]
			v4 := ls[lastIP(ls[:r.Layer])].IPv4 != nil
			csOff, mode := off+6, ""
			switch r.Kind {
			case "udp":
				mode = l.UDP.Checksum
			case "tcp":
				csOff, mode = off+16, l.TCP.Checksum
			case "icmpv6":
				csOff, mode = off+2, l.ICMPv6.Checksum
			}
			proto := ipProtos[r.Kind]
			udp := r.Kind == "udp"
			t.fixups = append(t.fixups, func(b []byte) {
				var src, dst netip.Addr
				if v4 {
					src, dst = netip.AddrFrom4([4]byte(b[ip+12:ip+16])), netip.AddrFrom4([4]byte(b[ip+16:ip+20]))
				} else {
					src, dst = netip.AddrFrom16([16]byte(b[ip+8:ip+24])), netip.AddrFrom16([16]byte(b[ip+24:ip+40]))
				}
				b[csOff], b[csOff+1] = 0, 0
				cs := pkt.Checksum(b[off:end], pkt.PseudoSum(src, dst, proto, end-off))
				if udp && cs == 0 {
					cs = 0xffff
				}
				binary.BigEndian.PutUint16(b[csOff:], ipflow.Checksum(orCorrect(mode), cs))
			})
		case "icmp":
			mode := l.ICMP.Checksum
			t.fixups = append(t.fixups, func(b []byte) {
				b[off+2], b[off+3] = 0, 0
				binary.BigEndian.PutUint16(b[off+2:], ipflow.Checksum(orCorrect(mode), pkt.Checksum(b[off:end], 0)))
			})
		}
	}

	// Field names: "ipv4.src", and "vlan[2].vid" for later layers of a kind.
	for _, r := range t.regions {
		seen[r.Kind]++
		for _, f := range r.Fields {
			if seen[r.Kind] == 1 {
				t.fields[r.Kind+"."+f.Name] = f
			}
			t.fields[fmt.Sprintf("%s[%d].%s", r.Kind, seen[r.Kind], f.Name)] = f
		}
	}
	return t, nil
}

func orCorrect(m string) string {
	if m == "" {
		return "correct"
	}
	return m
}

func kindAt(ls []Layer, i int) string {
	if i < len(ls) {
		return ls[i].kind()
	}
	return ""
}

func ipSrc(s string, dst netip.Addr, cx buildCtx, i int) (netip.Addr, error) {
	if s != "" {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Is4() != dst.Is4() {
			return a, fmt.Errorf("layer %d: src: an IPv%d address", i+1, map[bool]int{true: 4, false: 6}[dst.Is4()])
		}
		return a, nil
	}
	if a, ok := cx.source(dst); ok {
		return a, nil
	}
	return netip.Addr{}, fmt.Errorf("layer %d: the device has no IPv%d address to send from: set src", i+1, map[bool]int{true: 4, false: 6}[dst.Is4()])
}

// finish redoes the checksums of b (a copy of the template's frame) after
// modifiers and the signature changed it.
func (t *template) finish(b []byte) {
	for _, fx := range t.fixups {
		fx(b)
	}
}
