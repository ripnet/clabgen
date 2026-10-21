package isis

import (
	"encoding/hex"
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
	b := EncodeP2PHello(circuitL2, sysA, 30, 1, [][]byte{area}, v4, v6,
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
	b := EncodeLANHello(1, circuitL1, sysA, 9, 64, lan, [][]byte{area}, nil, nil, []pkt.MAC{{2, 0, 0, 0, 0, 2}}, 0)
	p, err := Decode(frame(b))
	if err != nil || p.LAN == nil || p.LAN.Level != 1 || p.LAN.Priority != 64 || p.LAN.LANID != lan || len(p.LAN.Neighbors) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestLSP(t *testing.T) {
	body := lspBody{Areas: [][]byte{area}, Hostname: "gen1", RouterID: netip.MustParseAddr("192.0.2.1"),
		V4Addrs: []netip.Addr{netip.MustParseAddr("10.0.0.1")}, Protocols: []byte{NLPIDIPv4, NLPIDIPv6},
		Neighbors: []ISReach{{NodeID{0, 0, 0, 0, 0, 2, 0}, 10}},
		Prefixes: []IPReach{{netip.MustParsePrefix("10.0.0.0/30"), 10, false}, {netip.MustParsePrefix("198.51.100.0/24"), 20, true},
			{netip.MustParsePrefix("2001:db8::/48"), 30, false}, {netip.MustParsePrefix("0.0.0.0/0"), 1, false}},
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
