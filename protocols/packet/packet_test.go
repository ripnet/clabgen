package packet

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/pcapng"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

var devMAC = pkt.MAC{2, 0, 0, 0, 0, 1}

func cx() buildCtx {
	return buildCtx{devMAC: devMAC, port: 50000, source: func(dst netip.Addr) (netip.Addr, bool) {
		if dst.Is4() {
			return netip.MustParseAddr("10.0.0.1"), true
		}
		return netip.MustParseAddr("2001:db8::1"), true
	}}
}

func layers(t *testing.T, y string) []Layer {
	t.Helper()
	var ls []Layer
	if err := json.Unmarshal([]byte(y), &ls); err != nil {
		t.Fatal(err)
	}
	return ls
}

func mustBuild(t *testing.T, ls []Layer) *template {
	t.Helper()
	tp, err := build(ls, cx())
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

func frameOf(u unit) []byte {
	return append(append(append(append([]byte(nil), u.dst[:]...), u.src[:]...), byte(u.et>>8), byte(u.et)), u.payload...)
}

func TestIPv4UDP(t *testing.T) {
	tp := mustBuild(t, layers(t, `[{"ethernet":{"dst":"02:00:00:00:00:02"}},{"ipv4":{"dst":"10.0.0.2","dscp":46,"df":true}},{"udp":{"dst_port":5001}},{"payload":{"size":32}}]`))
	u := tp.packet(nil, 7, 0xabcd0001, time.Unix(1, 0))
	if u.et != pkt.EtherTypeIPv4 || u.src != devMAC || u.dst != (pkt.MAC{2, 0, 0, 0, 0, 2}) {
		t.Fatalf("ethernet %v %v %#x", u.dst, u.src, u.et)
	}
	ip, err := pkt.DecodeIPv4(u.payload)
	if err != nil {
		t.Fatal(err)
	}
	if ip.Src.String() != "10.0.0.1" || ip.Dst.String() != "10.0.0.2" || ip.Protocol != 17 || ip.TTL != 64 || ip.TOS != 46<<2 || ip.ID != 7 || ip.Flags != pkt.IPv4DF {
		t.Fatalf("ipv4 %+v", ip)
	}
	if pkt.Checksum(u.payload[:20], 0) != 0 {
		t.Error("bad IPv4 checksum")
	}
	udp := ip.Payload
	if len(udp) != 40 || binary.BigEndian.Uint16(udp) != 50000 || binary.BigEndian.Uint16(udp[2:]) != 5001 || binary.BigEndian.Uint16(udp[4:]) != 40 {
		t.Fatalf("udp % x", udp[:8])
	}
	if pkt.Checksum(udp, pkt.PseudoSum(ip.Src, ip.Dst, 17, len(udp))) != 0 {
		t.Error("bad UDP checksum")
	}
	if !bytes.Equal(udp[8:12], []byte("CLGN")) || binary.BigEndian.Uint64(udp[16:]) != 7 {
		t.Errorf("signature % x", udp[8:32])
	}
}

func TestIPv6TCPAndICMPv6(t *testing.T) {
	tp := mustBuild(t, layers(t, `[{"ipv6":{"dst":"2001:db8::2","flow_label":5}},{"tcp":{"dst_port":179,"flags":"SA","seq":100,"options":"020405b4"}}]`))
	u := tp.packet(nil, 0, 1, time.Now())
	ip, err := pkt.DecodeIPv6(u.payload)
	if err != nil || ip.NextHeader != 6 || ip.FlowLabel != 5 {
		t.Fatalf("ipv6 %+v %v", ip, err)
	}
	seg := ip.Payload
	if len(seg) != 24 || seg[12] != 6<<4 || seg[13] != 0x12 || binary.BigEndian.Uint32(seg[4:]) != 100 {
		t.Fatalf("tcp % x", seg)
	}
	if pkt.Checksum(seg, pkt.PseudoSum(ip.Src, ip.Dst, 6, len(seg))) != 0 {
		t.Error("bad TCP checksum")
	}
	tp = mustBuild(t, layers(t, `[{"ipv6":{"dst":"2001:db8::2"}},{"icmpv6":{"id":1,"seq":2}},{"payload":{"size":8,"signature":false}}]`))
	u = tp.packet(nil, 0, 1, time.Now())
	ip, _ = pkt.DecodeIPv6(u.payload)
	if ip.Payload[0] != 128 || !pkt.ICMPv6Valid(ip.Src, ip.Dst, ip.Payload) {
		t.Errorf("icmpv6 % x", ip.Payload)
	}
}

func TestVLANMPLSAndVXLAN(t *testing.T) {
	tp := mustBuild(t, layers(t, `[{"vlan":{"tpid":34984,"vid":200}},{"vlan":{"vid":10,"pcp":5}},{"mpls":{"label":16001}},{"mpls":{"label":3,"ttl":1}},{"ipv4":{"dst":"10.9.9.9"}},{"udp":{}},{"vxlan":{"vni":5000}},{"ethernet":{"dst":"02:00:00:00:00:09"}},{"payload":{"size":46}}]`))
	u := tp.packet(nil, 0, 1, time.Now())
	b := u.payload
	if u.et != 0x88a8 || binary.BigEndian.Uint16(b) != 200 || binary.BigEndian.Uint16(b[2:]) != 0x8100 {
		t.Fatalf("outer tag % x (et %#x)", b[:4], u.et)
	}
	if binary.BigEndian.Uint16(b[4:]) != 5<<13|10 || binary.BigEndian.Uint16(b[6:]) != 0x8847 {
		t.Fatalf("inner tag % x", b[4:8])
	}
	l1, l2 := binary.BigEndian.Uint32(b[8:]), binary.BigEndian.Uint32(b[12:])
	if l1>>12 != 16001 || l1&0x100 != 0 || l1&0xff != 64 || l2>>12 != 3 || l2&0x100 == 0 || l2&0xff != 1 {
		t.Fatalf("mpls %08x %08x", l1, l2)
	}
	ip, _ := pkt.DecodeIPv4(b[16:])
	udp := ip.Payload
	if binary.BigEndian.Uint16(udp[2:]) != 4789 || binary.BigEndian.Uint32(udp[12:])>>8 != 5000 || udp[8] != 0x08 {
		t.Fatalf("vxlan % x", udp[:16])
	}
	inner := udp[16:]
	if !bytes.Equal(inner[:6], []byte{2, 0, 0, 0, 0, 9}) || !bytes.Equal(inner[6:12], devMAC[:]) || binary.BigEndian.Uint16(inner[12:]) != 0x88b5 {
		t.Errorf("inner ethernet % x", inner[:14])
	}
	// The fields of later layers of a kind are numbered.
	if _, ok := tp.fields["vlan[2].vid"]; !ok {
		t.Error("no vlan[2].vid")
	}
	if tp.fields["ethernet[2].dst"].Pos/8 != 14+16+20+8+8 {
		t.Errorf("inner dst at byte %d", tp.fields["ethernet[2].dst"].Pos/8)
	}
}

func TestARPAndRaw(t *testing.T) {
	tp := mustBuild(t, layers(t, `[{"arp":{"target_ip":"10.0.0.9"}}]`))
	u := tp.packet(nil, 0, 1, time.Now())
	a, err := pkt.DecodeARP(u.payload)
	if err != nil || u.et != pkt.EtherTypeARP || a.Op != 1 || a.SHA != devMAC || a.SPA.String() != "10.0.0.1" || a.TPA.String() != "10.0.0.9" || u.dst[0] != 0xff {
		t.Fatalf("arp %+v %v", a, err)
	}
	tp = mustBuild(t, layers(t, `[{"ethernet":{"ethertype":2054}},{"raw":{"hex":"de ad be ef"}}]`))
	if u := tp.packet(nil, 0, 1, time.Now()); u.et != 0x0806 || !bytes.Equal(u.payload, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("raw %#x % x", u.et, u.payload)
	}
}

func TestChecksumModes(t *testing.T) {
	tp := mustBuild(t, layers(t, `[{"ipv4":{"dst":"10.0.0.2","checksum":"invalid"}},{"udp":{"checksum":"zero"}}]`))
	u := tp.packet(nil, 0, 1, time.Now())
	if pkt.Checksum(u.payload[:20], 0) == 0 || binary.BigEndian.Uint16(u.payload[26:]) != 0 {
		t.Errorf("modes not applied % x", u.payload[:28])
	}
}

func TestModifiers(t *testing.T) {
	ls := layers(t, `[{"ipv4":{"dst":"10.0.0.2"}},{"udp":{"dst_port":7}},{"payload":{"size":30}}]`)
	tp := mustBuild(t, ls)
	ms := []Modifier{
		{Field: "ipv4.src", Mode: "increment", Start: "10.0.0.250", Step: "0.0.0.4", Count: 3},
		{Field: "udp.dst_port", Mode: "list", Values: []string{"80", "443"}, Repeat: 2},
		{Field: "ethernet.src", Mode: "decrement", Start: "02:00:00:00:01:00"},
		{Field: "ipv4.ttl", Mode: "random", Start: "10", Count: 5},
	}
	mods, err := compile(ms, tp)
	if err != nil {
		t.Fatal(err)
	}
	var srcs, ports, macs []string
	for n := uint64(0); n < 5; n++ {
		u := tp.packet(mods, n, 1, time.Now())
		ip, _ := pkt.DecodeIPv4(u.payload)
		if pkt.Checksum(u.payload[:20], 0) != 0 || pkt.Checksum(ip.Payload, pkt.PseudoSum(ip.Src, ip.Dst, 17, len(ip.Payload))) != 0 {
			t.Fatalf("packet %d: checksums not redone", n)
		}
		if ip.TTL < 10 || ip.TTL > 14 {
			t.Errorf("ttl %d", ip.TTL)
		}
		srcs = append(srcs, ip.Src.String())
		ports = append(ports, strconv.Itoa(int(binary.BigEndian.Uint16(ip.Payload[2:]))))
		macs = append(macs, u.src.String())
	}
	if want := []string{"10.0.0.250", "10.0.0.254", "10.0.1.2", "10.0.0.250", "10.0.0.254"}; !reflect.DeepEqual(srcs, want) {
		t.Errorf("srcs %v", srcs)
	}
	if want := []string{"80", "80", "443", "443", "80"}; !reflect.DeepEqual(ports, want) {
		t.Errorf("ports %v", ports)
	}
	if macs[1] != "02:00:00:00:00:ff" {
		t.Errorf("macs %v", macs)
	}
	// Random values are the same for the same packet (previews match what's sent).
	a, b := tp.packet(mods, 3, 1, time.Unix(0, 0)), tp.packet(mods, 3, 1, time.Unix(0, 0))
	if !bytes.Equal(a.payload, b.payload) {
		t.Error("random modifier not reproducible")
	}
	if _, err := compile([]Modifier{{Field: "tcp.seq", Mode: "increment"}}, tp); err == nil || !strings.Contains(err.Error(), "no such field") {
		t.Errorf("unknown field: %v", err)
	}
	if _, err := compile([]Modifier{{Field: "ipv4.src", Mode: "list", Values: []string{"2001:db8::1"}}}, tp); err == nil {
		t.Error("v6 value for a v4 field accepted")
	}
}

// Decoding a built frame gives layers that build the same frame.
func TestDecodeRoundTrip(t *testing.T) {
	for _, y := range []string{
		`[{"ethernet":{"dst":"02:00:00:00:00:02"}},{"vlan":{"vid":5}},{"ipv4":{"dst":"10.0.0.2","options":"94040000"}},{"udp":{"dst_port":5001}},{"payload":{"size":40}}]`,
		`[{"ipv6":{"dst":"2001:db8::2"}},{"tcp":{"dst_port":22,"flags":"PA","options":"0101080a00000001"}},{"payload":{"size":10,"signature":false}}]`,
		`[{"mpls":{"label":100}},{"ipv4":{"dst":"10.0.0.2"}},{"icmp":{"id":3,"seq":4}},{"payload":{"size":30}}]`,
		`[{"ipv4":{"dst":"10.0.0.2"}},{"udp":{}},{"vxlan":{"vni":77}},{"ethernet":{"dst":"02:00:00:00:00:03"}},{"arp":{"target_ip":"10.1.1.1"}}]`,
		`[{"ethernet":{"ethertype":1234}},{"payload":{"size":20,"signature":false}}]`,
	} {
		u := mustBuild(t, layers(t, y)).packet(nil, 0, 1, time.Unix(5, 0))
		frame := frameOf(u)
		dec := decode(frame)
		u2 := mustBuild(t, dec).packet(nil, 0, 1, time.Unix(5, 0))
		if got := frameOf(u2); !bytes.Equal(got, frame) {
			j, _ := json.Marshal(dec)
			t.Errorf("%s\n decoded %s\n want % x\n got  % x", y, j, frame, got)
		}
	}
}

func classicPcap(frames ...[]byte) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 0xa1b2c3d4)
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint16(b, 4)
	b = append(b, make([]byte, 8)...)
	b = binary.LittleEndian.AppendUint32(b, 65535)
	b = binary.LittleEndian.AppendUint32(b, 1)
	for _, f := range frames {
		b = append(b, make([]byte, 8)...)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(f)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return b
}

func ngBlock(typ uint32, body []byte) []byte {
	for len(body)%4 != 0 {
		body = append(body, 0)
	}
	n := uint32(12 + len(body))
	b := binary.LittleEndian.AppendUint32(nil, typ)
	b = binary.LittleEndian.AppendUint32(b, n)
	b = append(b, body...)
	return binary.LittleEndian.AppendUint32(b, n)
}

func pcapNG(frames ...[]byte) []byte {
	shb := binary.LittleEndian.AppendUint32(nil, 0x1a2b3c4d)
	shb = append(shb, 1, 0, 0, 0)
	shb = append(shb, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	b := ngBlock(0x0a0d0d0a, shb)
	b = append(b, ngBlock(1, []byte{1, 0, 0, 0, 0, 0, 0, 0})...)
	for _, f := range frames {
		epb := make([]byte, 20)
		binary.LittleEndian.PutUint32(epb[12:], uint32(len(f)))
		binary.LittleEndian.PutUint32(epb[16:], uint32(len(f)))
		b = append(b, ngBlock(6, append(epb, f...))...)
	}
	return b
}

func TestPcap(t *testing.T) {
	f1 := frameOf(mustBuild(t, layers(t, `[{"ipv4":{"dst":"10.0.0.2"}},{"udp":{}}]`)).packet(nil, 0, 1, time.Now()))
	f2 := append([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 0x81, 0, 0, 7}, f1[12:]...)
	for name, capture := range map[string][]byte{"pcap": classicPcap(f1, f2), "pcapng": pcapNG(f1, f2)} {
		fs, err := pcapng.Read(capture)
		if err != nil || len(fs) != 2 || !bytes.Equal(fs[0], f1) || !bytes.Equal(fs[1], f2) {
			t.Fatalf("%s: %d frames, %v", name, len(fs), err)
		}
	}
	p := &Pcap{Data: base64.StdEncoding.EncodeToString(classicPcap(f1, f2, f1)), Frames: "2-3"}
	fs, err := p.frames()
	if err != nil || len(fs) != 2 || !bytes.Equal(fs[0], f2) {
		t.Fatalf("selection: %d %v", len(fs), err)
	}
	us := units(fs, p)
	if us[0].et != pkt.EtherTypeIPv4 || !us[0].src.IsZero() || us[0].dst != (pkt.MAC{1, 2, 3, 4, 5, 6}) {
		t.Errorf("captured tag not replaced: %+v", us[0])
	}
	us = units(fs, &Pcap{VLANs: "keep", SrcMAC: "keep"})
	if us[0].et != 0x8100 || us[0].src != (pkt.MAC{7, 8, 9, 10, 11, 12}) {
		t.Errorf("keep: %+v", us[0])
	}
	if _, err := (&Pcap{Data: base64.StdEncoding.EncodeToString(classicPcap(f1)), Frames: "2"}).frames(); err == nil {
		t.Error("frame 2 of 1 accepted")
	}
	if _, err := pcapng.Read([]byte("nope, not a capture")); err == nil {
		t.Error("garbage accepted")
	}
}

func TestValidate(t *testing.T) {
	for _, y := range []string{
		`{"layers":[{"udp":{}}]}`,
		`{"layers":[{"payload":{"size":10}},{"ipv4":{"dst":"10.0.0.1"}}]}`,
		`{"layers":[{"ipv4":{"dst":"2001:db8::1"}}]}`,
		`{"layers":[{"ipv4":{"dst":"10.0.0.1"},"udp":{}}]}`,
		`{"layers":[{"ipv4":{"dst":"10.0.0.1"}}],"modifiers":[{"field":"ipv6.src","mode":"increment"}]}`,
		`{"layers":[{"ipv6":{"dst":"2001:db8::1"}},{"icmp":{}}]}`,
		`{"pcap":{"data":"!!"}}`,
	} {
		c := packetType{}.NewConfig().(*Config)
		if err := protocol.Decode(c, json.RawMessage(y)); err == nil {
			t.Errorf("%s accepted", y)
		}
	}
	c := packetType{}.NewConfig().(*Config)
	if err := protocol.Decode(c, json.RawMessage(`{}`)); err != nil {
		t.Errorf("defaults: %v", err)
	}
	// Layers replace the defaults' (not merged into them).
	if err := protocol.Decode(c, json.RawMessage(`{"layers":[{"ipv4":{"dst":"10.0.0.1"}},{"udp":{}}]}`)); err != nil || c.Layers[0].kind() != "ipv4" {
		t.Errorf("layers over the defaults: %v %+v", err, c.Layers[0])
	}
	if err := protocol.Decode(c, json.RawMessage(`{"layers":[{"ipv4":{"dst":"10.0.0.1","nope":1}}]}`)); err == nil {
		t.Error("unknown layer field accepted")
	}
}

func TestInspect(t *testing.T) {
	f := frameOf(mustBuild(t, layers(t, `[{"ethernet":{"dst":"02:00:00:00:00:02"}},{"vlan":{"vid":7}},{"ipv4":{"dst":"10.0.0.2"}},{"udp":{"dst_port":53}},{"payload":{"size":20}}]`)).packet(nil, 0, 1, time.Now()))
	r := Inspect(f)
	if r.Summary != "02:00:00:00:00:01 > 02:00:00:00:00:02, VLAN 7, IPv4 10.0.0.1 > 10.0.0.2, UDP 50000 > 53, 66 bytes" {
		t.Errorf("summary %q", r.Summary)
	}
	if len(r.Layers) != 5 || r.Layers[2].Title != "IPv4" || r.Layers[2].Offset != 18 || len(r.Decoded) != 5 {
		t.Errorf("layers %+v", r.Layers)
	}
	cfm := frameOf(mustBuild(t, layers(t, `[{"ethernet":{"dst":"01:80:c2:00:00:33","ethertype":35074}},{"raw":{"hex":"6001"}}]`)).packet(nil, 0, 1, time.Now()))
	if r := Inspect(cfm); r.Summary != "02:00:00:00:00:01 > 01:80:c2:00:00:33, CFM, 16 bytes" {
		t.Errorf("CFM summary %q", r.Summary)
	}
	if r := Inspect([]byte{1, 2, 3}); r.Summary != "3 bytes" {
		t.Errorf("short frame %q", r.Summary)
	}
}
