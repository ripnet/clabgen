package isis

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

var (
	sysA = SysID{0, 0, 0, 0, 0, 1}
	sysB = SysID{0, 0, 0, 0, 0, 2}
	area = []byte{0x49, 0x00, 0x01}
)

func TestIDs(t *testing.T) {
	s, err := ParseSysID("1921.6800.1001")
	if err != nil || s.String() != "1921.6800.1001" {
		t.Fatal(s, err)
	}
	if _, err := ParseSysID("1921.6800"); err == nil {
		t.Fatal("short sysid accepted")
	}
	if id := MakeLSPID(s, 1, 2); id.String() != "1921.6800.1001.01-02" {
		t.Fatal(id)
	}
	a, err := ParseArea("49.0001")
	if err != nil || AreaString(a) != "49.0001" {
		t.Fatal(a, err)
	}
}

func TestP2PHello(t *testing.T) {
	v4 := []netip.Addr{netip.MustParseAddr("10.0.0.1")}
	v6 := []netip.Addr{netip.MustParseAddr("fe80::1")}
	b := EncodeP2PHello(circuitL2, sysA, 30, 1, [][]byte{area}, v4, v6, nil,
		ThreeWay{State: AdjInitializing, ExtCircuit: 7, NeighborSys: sysB, HasNeighbor: true, NeighborExtCircuit: 9}, 1497)
	if len(b) != 1497 {
		t.Fatalf("padded to %d", len(b))
	}
	p, err := Decode(frame(b))
	if err != nil || p.P2P == nil {
		t.Fatal(err)
	}
	h := p.P2P
	if h.Source != sysA || h.Hold != 30 || h.Circuit != circuitL2 || h.ThreeWay == nil || h.ThreeWay.State != AdjInitializing ||
		h.ThreeWay.NeighborSys != sysB || h.ThreeWay.ExtCircuit != 7 || h.ThreeWay.NeighborExtCircuit != 9 {
		t.Fatalf("%+v %+v", h, h.ThreeWay)
	}
	if len(p.Areas) != 1 || AreaString(p.Areas[0]) != "49.0001" {
		t.Fatal(p.Areas)
	}
}

func TestLANHello(t *testing.T) {
	lan := NodeID{0, 0, 0, 0, 0, 1, 3}
	b := EncodeLANHello(1, circuitL1, sysA, 9, 64, lan, [][]byte{area}, nil, nil, nil, []pkt.MAC{{2, 0, 0, 0, 0, 2}}, 0)
	p, err := Decode(frame(b))
	if err != nil || p.LAN == nil || p.LAN.Level != 1 || p.LAN.Priority != 64 || p.LAN.LANID != lan || len(p.LAN.Neighbors) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestLSP(t *testing.T) {
	body := lspBody{Areas: [][]byte{area}, Hostname: "gen1", RouterID: netip.MustParseAddr("192.0.2.1"),
		V4Addrs: []netip.Addr{netip.MustParseAddr("10.0.0.1")}, Protocols: []byte{NLPIDIPv4, NLPIDIPv6},
		Neighbors: []ISReach{{Neighbor: NodeID{0, 0, 0, 0, 0, 2, 0}, Metric: 10}},
		Prefixes: []IPReach{{Prefix: netip.MustParsePrefix("10.0.0.0/30"), Metric: 10, Down: false}, {Prefix: netip.MustParsePrefix("198.51.100.0/24"), Metric: 20, Down: true},
			{Prefix: netip.MustParsePrefix("2001:db8::/48"), Metric: 30, Down: false}, {Prefix: netip.MustParsePrefix("0.0.0.0/0"), Metric: 1, Down: false}},
		Overload: true}
	id := MakeLSPID(sysA, 0, 0)
	b := EncodeLSP(2, id, 5, 1200, lspTypeL1L2, body)
	if !fletcherOK(b[12:]) {
		t.Fatal("checksum doesn't verify")
	}
	p, err := Decode(frame(b))
	if err != nil || p.LSP == nil {
		t.Fatal(err)
	}
	l := p.LSP
	if l.Level != 2 || l.ID != id || l.Seq != 5 || l.Lifetime != 1200 || !l.Overload() {
		t.Fatalf("%+v", l)
	}
	in := l.Info()
	if in.Hostname != "gen1" || in.RouterID != "192.0.2.1" || len(in.Neighbors) != 1 || in.Neighbors[0].ID != "0000.0000.0002.00" || in.Neighbors[0].Metric != 10 {
		t.Fatalf("%+v", in)
	}
	want := []string{"10.0.0.0/30", "198.51.100.0/24", "0.0.0.0/0", "2001:db8::/48"}
	if len(in.Prefixes) != 4 {
		t.Fatalf("prefixes %+v", in.Prefixes)
	}
	for i, w := range want {
		if in.Prefixes[i].Prefix != w {
			t.Fatalf("prefix %d: %+v", i, in.Prefixes[i])
		}
	}
	if !in.Prefixes[1].Down || in.Prefixes[3].Metric != 30 {
		t.Fatalf("%+v", in.Prefixes)
	}
	// Corrupting any covered byte breaks the checksum.
	b[30] ^= 1
	if fletcherOK(b[12:]) {
		t.Fatal("corruption not detected")
	}
}

func TestManyPrefixesSplitTLVs(t *testing.T) {
	var ps []IPReach
	for i := 0; i < 100; i++ {
		ps = append(ps, IPReach{Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i), 0, 0}), 16), Metric: 1})
	}
	b := EncodeLSP(1, MakeLSPID(sysA, 0, 0), 1, 1200, lspTypeL1, lspBody{Prefixes: ps})
	p, err := Decode(frame(b))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.LSP.TLVs.All(TLVExtIPReach)); n < 3 {
		t.Fatalf("%d TLVs", n)
	}
	if got := len(p.LSP.Info().Prefixes); got != 100 {
		t.Fatalf("%d prefixes", got)
	}
}

func TestSNPs(t *testing.T) {
	src := NodeID{0, 0, 0, 0, 0, 1, 0}
	es := []LSPEntry{{1200, MakeLSPID(sysA, 0, 0), 3, 0xabcd}, {900, MakeLSPID(sysB, 0, 0), 9, 0x1234}}
	end := LSPID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	p, err := Decode(frame(EncodeCSNP(2, src, LSPID{}, end, es)))
	if err != nil || p.SNP == nil || !p.SNP.Complete || p.SNP.Level != 2 || p.SNP.End != end || len(p.SNP.Entries) != 2 || p.SNP.Entries[1] != es[1] {
		t.Fatalf("%+v %v", p.SNP, err)
	}
	p, err = Decode(frame(EncodePSNP(1, src, es[:1])))
	if err != nil || p.SNP.Complete || p.SNP.Level != 1 || p.SNP.Entries[0] != es[0] {
		t.Fatalf("%+v %v", p.SNP, err)
	}
}

func TestDecodeRejects(t *testing.T) {
	for _, b := range [][]byte{nil, {0xaa, 0xaa, 0x03, 0x83}, frame([]byte{0x83, 20, 1, 0, 17, 1, 0, 0})} {
		if _, err := Decode(b); err == nil {
			t.Errorf("accepted % x", b)
		}
	}
}

func readHex(t *testing.T, name string) []byte {
	t.Helper()
	h, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(h)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A point-to-point hello captured from SR-SIM 26.7.R1.
func TestDecodeSRHello(t *testing.T) {
	f, err := pkt.Decode(readHex(t, "sr-p2p-iih.hex"))
	if err != nil || f.EtherType != pkt.EtherTypeLLC || len(f.Tags) != 1 || f.Tags[0].VID != 500 {
		t.Fatalf("frame %+v %v", f, err)
	}
	p, err := Decode(f.Payload)
	if err != nil || p.P2P == nil {
		t.Fatal(err)
	}
	h := p.P2P
	if h.Source.String() != "1920.0000.2100" || h.Hold != 27 || h.Circuit != circuitL2 || h.ThreeWay == nil || h.ThreeWay.State != AdjDown || h.ThreeWay.HasNeighbor {
		t.Fatalf("%+v %+v", h, h.ThreeWay)
	}
	if len(p.Areas) != 1 || AreaString(p.Areas[0]) != "49.0001" {
		t.Fatal(p.Areas)
	}
}

func TestNestedSchemaOrder(t *testing.T) {
	s := protocol.Schema(&PrefixArgs{})
	if !strings.Contains(s, `"x-order":["prefix","metric","count"]`) {
		t.Fatalf("nested order missing: %s", s)
	}
}

func TestMultiTopologyLSP(t *testing.T) {
	nb := []ISReach{{Neighbor: NodeID{0, 0, 0, 0, 0, 2, 0}, Metric: 10}}
	body := lspBody{MTs: []uint16{MTIPv4Unicast, MTIPv6Unicast, MTIPv4Multicast}, Overload: true,
		Neighbors: nb, MTNeighbors: map[uint16][]ISReach{MTIPv6Unicast: nb, MTIPv4Multicast: nb},
		Prefixes: []IPReach{{Prefix: netip.MustParsePrefix("10.0.0.0/24"), Metric: 10},
			{Prefix: netip.MustParsePrefix("10.0.0.0/24"), Metric: 10, MT: MTIPv4Multicast},
			{Prefix: netip.MustParsePrefix("2001:db8::/64"), Metric: 10, MT: MTIPv6Unicast}}}
	p, err := Decode(frame(EncodeLSP(2, MakeLSPID(sysA, 0, 0), 1, 1200, lspTypeL1L2, body)))
	if err != nil {
		t.Fatal(err)
	}
	in := p.LSP.Info()
	if strings.Join(in.Topologies, ",") != "ipv4-unicast (overload),ipv6-unicast (overload),ipv4-multicast (overload)" {
		t.Fatalf("topologies %v", in.Topologies)
	}
	mts := map[uint16]int{}
	for _, n := range in.Neighbors {
		mts[n.MT]++
	}
	if mts[0] != 1 || mts[2] != 1 || mts[3] != 1 {
		t.Fatalf("neighbors %+v", in.Neighbors)
	}
	got := map[string]bool{}
	for _, x := range in.Prefixes {
		got[fmt.Sprintf("%s@%d", x.Prefix, x.MT)] = true
	}
	if len(got) != 3 || !got["10.0.0.0/24@0"] || !got["10.0.0.0/24@3"] || !got["2001:db8::/64@2"] {
		t.Fatalf("prefixes %v", got)
	}
	if len(p.LSP.TLVs.All(TLVIPv6Reach)) != 0 || len(p.LSP.TLVs.All(TLVMTIPv6Reach)) != 1 {
		t.Fatal("IPv6 prefixes must use TLV 237 in MT 2")
	}
	// Hellos carry TLV 229.
	h, _ := Decode(frame(EncodeP2PHello(circuitL2, sysA, 9, 1, [][]byte{area}, nil, nil, []uint16{0, 2}, ThreeWay{}, 0)))
	if !h.HasMT || len(h.MTs) != 2 || h.MTs[1] != 2 {
		t.Fatalf("hello MTs %v", h.MTs)
	}
	if h2, _ := Decode(frame(EncodeP2PHello(circuitL2, sysA, 9, 1, [][]byte{area}, nil, nil, nil, ThreeWay{}, 0))); h2.HasMT {
		t.Fatal("single-topology hello with TLV 229")
	}
}

// Many MT prefixes split over several 237 TLVs, each with its MT ID; the
// fragment-size estimate matches what EncodeLSP produces.
func TestMTPrefixesFit(t *testing.T) {
	s := &session{cfg: *defaultConfig()}
	var ps []IPReach
	for i := 0; i < 400; i++ {
		ps = append(ps, IPReach{Prefix: netip.PrefixFrom(netip.AddrFrom16([16]byte{0x20, 0x01, 0xd, 0xb8, byte(i >> 8), byte(i)}), 64), Metric: 1, MT: MTIPv6Unicast})
	}
	body := lspBody{MTs: []uint16{0, 2}}
	n := s.fitPrefixes(2, lspTypeL1L2, body, ps, true)
	body.Prefixes = ps[:n]
	raw := EncodeLSP(2, MakeLSPID(sysA, 0, 0), 1, 1200, lspTypeL1L2, body)
	if len(raw)+3 > s.cfg.LSPMTU || n < 50 {
		t.Fatalf("%d prefixes → %d bytes (mtu %d)", n, len(raw), s.cfg.LSPMTU)
	}
	body.Prefixes = ps[:n+1]
	if raw := EncodeLSP(2, MakeLSPID(sysA, 0, 0), 1, 1200, lspTypeL1L2, body); len(raw)+3 <= s.cfg.LSPMTU {
		t.Fatalf("one more prefix would still fit (%d bytes): estimate too low", len(raw))
	}
	p, _ := Decode(frame(raw))
	for _, v := range p.LSP.TLVs.All(TLVMTIPv6Reach) {
		if v[0] != 0 || v[1] != 2 {
			t.Fatalf("TLV 237 without MT 2: % x", v[:2])
		}
	}
}

func TestCommonMTs(t *testing.T) {
	s := &session{mts: []uint16{0, 2}}
	if m, ok := s.commonMTs(&PDU{}); !ok || len(m) != 1 || m[0] != 0 {
		t.Fatalf("vs single topology: %v %v", m, ok)
	}
	if m, ok := s.commonMTs(&PDU{HasMT: true, MTs: []uint16{0, 2}}); !ok || len(m) != 2 {
		t.Fatalf("vs MT 0+2: %v", m)
	}
	if _, ok := s.commonMTs(&PDU{HasMT: true, MTs: []uint16{4}}); ok {
		t.Fatal("no common topology")
	}
	if m, ok := (&session{}).commonMTs(&PDU{HasMT: true, MTs: []uint16{0, 2}}); !ok || len(m) != 1 {
		t.Fatalf("single topology vs MT: %v", m)
	}
}

func TestSegmentRoutingTLVs(t *testing.T) {
	nb := SysID{0, 0, 0, 0, 0, 7}
	body := lspBody{RouterID: netip.MustParseAddr("192.0.2.1"),
		SR: &SRCap{RouterID: netip.MustParseAddr("192.0.2.1"), SRGBBase: 16000, SRGBSize: 8000, IPv6: true},
		Neighbors: []ISReach{
			{Neighbor: NodeID{0, 0, 0, 0, 0, 2, 0}, Metric: 10, AdjSIDs: []AdjSID{{Label: 24000}, {Label: 24001, Flags: ASIDAddrFamily}}},
			{Neighbor: NodeID{0, 0, 0, 0, 0, 3, 1}, Metric: 10, AdjSIDs: []AdjSID{{Label: 24002, Neighbor: &nb}}},
		},
		Prefixes: []IPReach{
			{Prefix: netip.MustParsePrefix("192.0.2.1/32"), SID: &PrefixSID{Index: 1, Flags: PSIDNode}},
			{Prefix: netip.MustParsePrefix("2001:db8::1/128"), SID: &PrefixSID{Index: 101, Flags: PSIDNode | PSIDNoPHP}},
			{Prefix: netip.MustParsePrefix("10.0.0.0/24"), Metric: 10},
		}}
	p, err := Decode(frame(EncodeLSP(2, MakeLSPID(sysA, 0, 0), 1, 1200, lspTypeL1L2, body)))
	if err != nil {
		t.Fatal(err)
	}
	in := p.LSP.Info()
	if in.SR == nil || in.SR.RouterID != "192.0.2.1" || len(in.SR.SRGB) != 1 || in.SR.SRGB[0] != (SRGBInfo{16000, 8000}) || !in.SR.MPLSIPv4 || !in.SR.MPLSIPv6 || len(in.SR.Algorithms) != 1 {
		t.Fatalf("router capability %+v", in.SR)
	}
	if a := in.Neighbors[0].AdjSIDs; len(a) != 2 || a[0].Label != 24000 || a[0].IPv6 || a[1].Label != 24001 || !a[1].IPv6 {
		t.Fatalf("adj SIDs %+v", a)
	}
	if a := in.Neighbors[1].AdjSIDs; len(a) != 1 || a[0].Label != 24002 || a[0].Neighbor != "0000.0000.0007" {
		t.Fatalf("LAN adj SID %+v", a)
	}
	byP := map[string]PfxInfo{}
	for _, x := range in.Prefixes {
		byP[x.Prefix] = x
	}
	if s := byP["192.0.2.1/32"].SID; s == nil || s.Index != 1 || s.Flags != "N" {
		t.Fatalf("v4 prefix SID %+v", s)
	}
	if s := byP["2001:db8::1/128"].SID; s == nil || s.Index != 101 || s.Flags != "NP" {
		t.Fatalf("v6 prefix SID %+v", s)
	}
	if x, ok := byP["10.0.0.0/24"]; !ok || x.SID != nil || x.Metric != 10 {
		t.Fatalf("plain prefix after a SID one: %+v", x)
	}
}

// Prefixes with SIDs: the fragment-size estimate still matches.
func TestSIDPrefixesFit(t *testing.T) {
	s := &session{cfg: *defaultConfig()}
	var ps []IPReach
	for i := 0; i < 300; i++ {
		ps = append(ps, IPReach{Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 255, byte(i >> 8), byte(i)}), 32), SID: &PrefixSID{Index: uint32(i), Flags: PSIDNode}})
	}
	n := s.fitPrefixes(2, lspTypeL1L2, lspBody{}, ps, true)
	if raw := EncodeLSP(2, MakeLSPID(sysA, 0, 0), 1, 1200, lspTypeL1L2, lspBody{Prefixes: ps[:n]}); len(raw)+3 > s.cfg.LSPMTU {
		t.Fatalf("%d prefixes → %d bytes", n, len(raw))
	}
	if raw := EncodeLSP(2, MakeLSPID(sysA, 0, 0), 1, 1200, lspTypeL1L2, lspBody{Prefixes: ps[:n+1]}); len(raw)+3 <= s.cfg.LSPMTU {
		t.Fatalf("estimate too low: %d+1 prefixes still fit", n)
	}
}
