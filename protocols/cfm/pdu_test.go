package cfm

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"
)

func TestMAIDFormats(t *testing.T) {
	cases := []struct {
		mdf, md, maf, ma, want string
		wantLen                int // bytes before zero padding
	}{
		{"none", "", "icc", "ABCDEFGHIJKLM", "icc(ABCDEFGHIJKLM)", 1 + 2 + 13},
		{"none", "", "icc", "AB", "icc(AB)", 1 + 2 + 13},
		{"none", "", "string", "clabgen", "string(clabgen)", 1 + 2 + 7},
		{"string", "dom", "string", "assoc", "string(dom)/string(assoc)", 2 + 3 + 2 + 5},
		{"dns", "example.com", "integer", "42", "dns(example.com)/integer(42)", 2 + 11 + 2 + 2},
		{"mac-int", "00:11:22:33:44:55:7", "primary-vid", "100", "mac-int(00:11:22:33:44:55:7)/primary-vid(100)", 2 + 8 + 2 + 2},
		{"none", "", "vpn-id", "00aabb:00000001", "vpn-id(00aabb:00000001)", 1 + 2 + 7},
	}
	for _, c := range cases {
		m, err := encodeMAID(c.mdf, c.md, c.maf, c.ma)
		if err != nil {
			t.Fatalf("%v: %v", c, err)
		}
		if got := m.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
		for i := c.wantLen; i < 48; i++ {
			if m[i] != 0 {
				t.Errorf("%s: byte %d not zero padded", c.want, i)
				break
			}
		}
	}
	// Byte-exact check of the Y.1731 ICC format.
	m, _ := encodeMAID("none", "", "icc", "ABC")
	if m[0] != 1 || m[1] != 32 || m[2] != 13 || string(m[3:6]) != "ABC" || m[6] != 0 {
		t.Errorf("unexpected ICC encoding % x", m[:16])
	}

	bad := [][4]string{
		{"string", "", "string", "x"},
		{"string", "d", "icc", "x"},
		{"none", "", "icc", "ABCDEFGHIJKLMN"},
		{"none", "", "primary-vid", "5000"},
		{"string", string(make([]byte, 40)), "string", string(make([]byte, 10))},
		{"bogus", "", "string", "x"},
	}
	for _, b := range bad {
		if _, err := encodeMAID(b[0], b[1], b[2], b[3]); err == nil {
			t.Errorf("expected error for %q", b)
		}
	}
}

func TestCCMRoundTrip(t *testing.T) {
	maid, _ := encodeMAID("none", "", "icc", "CLABGEN000001")
	c := &ccm{Level: 5, RDI: true, Interval: 4, Seq: 0xdeadbeef, MEPID: 8191, MAID: maid, TxFCf: 1, RxFCb: 2, TxFCb: 3,
		PortStatus: PortBlocked, IntfStatus: 2}
	b := c.encode()
	if len(b) != 4+70+4+4+1 {
		t.Fatalf("length %d", len(b))
	}
	if b[0] != 5<<5 || b[1] != OpCCM || b[2] != 0x84 || b[3] != 70 {
		t.Fatalf("header % x", b[:4])
	}
	// Ethernet padding after the End TLV must be ignored.
	b = append(b, make([]byte, 20)...)
	h, err := parseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	g, err := parseCCM(h, b)
	if err != nil {
		t.Fatal(err)
	}
	if *g != *c {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", g, c)
	}
}

func TestLTMLTRRoundTrip(t *testing.T) {
	l := &ltm{Level: 2, Flags: flagUseFDBOnly, TransID: 99, TTL: 64,
		Orig: [6]byte{2, 0, 0, 0, 0, 1}, Target: [6]byte{2, 0, 0, 0, 0, 2}, EgressID: []byte{0, 0, 2, 0, 0, 0, 0, 1}}
	b := l.encode()
	h, _ := parseHeader(b)
	g, err := parseLTM(h, b)
	if err != nil || g.TransID != 99 || g.TTL != 64 || g.Orig != l.Orig || g.Target != l.Target || string(g.EgressID) != string(l.EgressID) {
		t.Fatalf("LTM mismatch %+v %v", g, err)
	}
	r := &ltr{Level: 2, Flags: flagTerminalMEP, TransID: 99, ReplyTTL: 63, RelayAction: RlyHit,
		LastEgress: l.EgressID, NextEgress: []byte{0, 0, 2, 0, 0, 0, 0, 2}, IngressAct: 1, IngressMAC: l.Target}
	b = r.encode()
	h, _ = parseHeader(b)
	gr, err := parseLTR(h, b)
	if err != nil || gr.ReplyTTL != 63 || gr.RelayAction != RlyHit || gr.IngressMAC != l.Target || string(gr.NextEgress) != string(r.NextEgress) {
		t.Fatalf("LTR mismatch %+v %v", gr, err)
	}
}

func TestConfigValidate(t *testing.T) {
	c := defaultConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	c.RemoteMEPs = []uint16{1}
	if c.Validate() == nil {
		t.Error("expected error for own MEPID in remote_meps")
	}
	c = defaultConfig()
	c.CCMInterval = "2s"
	if c.Validate() == nil {
		t.Error("expected error for bad interval")
	}
}

func TestY1731Codecs(t *testing.T) {
	// TST: CRC and null patterns check out; a flipped bit is caught.
	for _, c := range []struct{ prbs, crc bool }{{false, false}, {false, true}, {true, true}} {
		b := encodeTST(3, 7, 64, c.prbs, c.crc)
		h, _ := parseHeader(b)
		x, err := parseTST(h, b)
		if err != nil || x.Seq != 7 || x.Bytes != 64 || !x.Checked || x.BitError {
			t.Fatalf("%+v: %+v %v", c, x, err)
		}
		b[20] ^= 1
		if x, _ := parseTST(h, b); !x.BitError {
			t.Errorf("%+v: bit error not caught", c)
		}
	}
	if b := encodeTST(3, 1, 16, true, false); func() bool { h, _ := parseHeader(b); x, _ := parseTST(h, b); return x.Checked }() {
		t.Error("PRBS without CRC claimed checked")
	}
	// A TST from SR OS (64 zero bytes, CRC TLV) checks out, and ours
	// carries the same CRC.
	sr, _ := hex.DecodeString("a025000400000003200045010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000006d6f784")
	h0, _ := parseHeader(sr)
	if x, err := parseTST(h0, sr); err != nil || !x.Checked || x.BitError || x.Bytes != 64 {
		t.Fatalf("SR OS TST: %+v %v", x, err)
	}
	ours := encodeTST(5, 3, 64, false, true)
	if !bytes.Equal(ours[:len(sr)], sr) {
		t.Errorf("our TST differs from SR OS's:\n%x\n%x", ours, sr)
	}

	// LMM round trip, 1DM timestamp, CSF type and period.
	l := &lm{Level: 2, Opcode: OpLMM, TxFCf: 10, RxFCf: 20, TxFCb: 30}
	b := l.encode()
	h, _ := parseHeader(b)
	if x, err := parseLM(h, b); err != nil || *x != *l {
		t.Fatalf("LM %+v %v", x, err)
	}
	now := time.Unix(1700000000, 123456789)
	if got := getTimestamp(encode1DM(1, now)[4:]); !got.Equal(now) {
		t.Errorf("1DM timestamp %v", got)
	}
	c := encodeSignal(4, OpCSF, csfTypes["rdi"]<<3|periodCode(time.Second))
	if h, _ := parseHeader(c); h.Opcode != OpCSF || h.Flags>>3&7 != 2 || periodOf(h.Flags) != time.Second {
		t.Errorf("CSF header %+v", h)
	}

	// Loss between two LMRs.
	ft, nt, far, near := lossBetween(lmReply{lm{TxFCf: 100, RxFCf: 50, TxFCb: 10}, 5}, lmReply{lm{TxFCf: 200, RxFCf: 140, TxFCb: 30}, 24})
	if ft != 100 || nt != 20 || far != 10 || near != 1 {
		t.Errorf("loss %d %d %d %d", ft, nt, far, near)
	}

	// Sender ID and Organization-Specific TLVs in a CCM.
	org, _ := (&OrgTLV{OUI: "00:16:4d", Subtype: 9, Value: "0102"}).encode()
	cc := (&ccm{Level: 1, Interval: 4, MEPID: 3}).encode()
	cc = withTLVs(cc, append(append(senderIDTLV("r1"), org...), dataTLV(8)...))
	h, _ = parseHeader(cc)
	x, err := parseCCM(h, cc)
	if err != nil || x.SenderID != "r1" || x.Org != "00:16:4d/9 0102" || x.Padding != 8 {
		t.Fatalf("CCM TLVs %+v %v", x, err)
	}
}
