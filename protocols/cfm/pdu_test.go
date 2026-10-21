package cfm

import (
	"testing"
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
