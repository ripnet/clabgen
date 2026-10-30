package dhcp

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

func TestMsg4RoundTrip(t *testing.T) {
	long := bytes.Repeat([]byte{7}, 300)
	m := &Msg4{Op: OpReply, XID: 0x01020304, Secs: 3, Broadcast: true, YIAddr: netip.MustParseAddr("10.0.0.9"),
		CHAddr: pkt.MAC{2, 0, 0, 0, 0, 1}}
	m.Add(OptMsgType, []byte{MsgAck})
	m.Add(OptRouter, []byte{10, 0, 0, 1, 10, 0, 0, 2})
	m.Add(OptVendorClass, long)
	b := m.Encode()
	if len(b) < 300 {
		t.Fatalf("%d bytes: BOOTP wants at least 300", len(b))
	}
	got, err := Decode4(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type() != MsgAck || got.XID != m.XID || !got.Broadcast || got.YIAddr != m.YIAddr || got.CHAddr != m.CHAddr || got.CIAddr != netip.IPv4Unspecified() {
		t.Fatalf("%+v", got)
	}
	if rs := got.Addrs(OptRouter); len(rs) != 2 || rs[1] != netip.MustParseAddr("10.0.0.2") {
		t.Fatalf("routers %v", rs)
	}
	if !bytes.Equal(got.Get(OptVendorClass), long) {
		t.Fatalf("a 300-byte option comes back as %d bytes", len(got.Get(OptVendorClass)))
	}
}

func TestMsg6IARoundTrip(t *testing.T) {
	m := &Msg6{Type: MsgReply, XID: 0xabcdef}
	m.Add(Opt6ClientID, DUIDLL(pkt.MAC{2, 0, 0, 0, 0, 1}))
	m.Options = append(m.Options,
		IA{Kind: Opt6IANA, IAID: 1, T1: 10, T2: 16, Addrs: []IAAddr{{netip.MustParseAddr("2001:db8::5"), 20, 30}}}.Encode(),
		IA{Kind: Opt6IAPD, IAID: 1, Status: &Status6{StatusNoPrefixAvail, "none left"}}.Encode())
	got, err := Decode6(m.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.XID != 0xabcdef || got.Type != MsgReply {
		t.Fatalf("%+v", got)
	}
	ias := got.IAs()
	if len(ias) != 2 || ias[0].T2 != 16 || ias[0].Addrs[0] != (IAAddr{netip.MustParseAddr("2001:db8::5"), 20, 30}) {
		t.Fatalf("%+v", ias)
	}
	if s := ias[1].Status; s == nil || s.Code != StatusNoPrefixAvail || s.Message != "none left" {
		t.Fatalf("status %+v", s)
	}
	pd := IA{Kind: Opt6IAPD, IAID: 1, Prefixes: []IAPrefix{{netip.MustParsePrefix("2001:db8:100::/56"), 1, 2}}}
	back, err := DecodeIA(pd.Encode())
	if err != nil || len(back.Prefixes) != 1 || back.Prefixes[0] != pd.Prefixes[0] {
		t.Fatalf("%+v %v", back, err)
	}
}

func TestPDAddress(t *testing.T) {
	for _, c := range []struct {
		prefix string
		x      PDAddress
		want   string
	}{
		{"2001:db8:100::/56", PDAddress{0, 1}, "2001:db8:100::1"},
		{"2001:db8:100::/56", PDAddress{1, 1}, "2001:db8:100:1::1"},
		{"2001:db8:100::/56", PDAddress{255, 0x10}, "2001:db8:100:ff::10"},
		{"2001:db8:100::/56", PDAddress{256, 1}, ""},
		{"2001:db8:100:5::/64", PDAddress{0, 2}, "2001:db8:100:5::2"},
		{"2001:db8:100:5::/64", PDAddress{1, 2}, ""},
		{"2001:db8:100:5::/120", PDAddress{0, 0x20}, "2001:db8:100:5::20"},
		{"2001:db8:100:5::/120", PDAddress{0, 0x100}, ""},
	} {
		a, ok := pdAddress(netip.MustParsePrefix(c.prefix), c.x)
		if got := map[bool]string{true: a.String(), false: ""}[ok]; got != c.want {
			t.Errorf("%s %+v: %q, want %q", c.prefix, c.x, got, c.want)
		}
	}
}

func TestDomainList(t *testing.T) {
	b := []byte("\x07example\x03com\x00\x03lab\x00")
	if got := domainList(b); len(got) != 2 || got[0] != "example.com" || got[1] != "lab" {
		t.Fatalf("%q", got)
	}
}

func TestSaneTimes(t *testing.T) {
	s := time.Second
	for _, c := range []struct{ t1, t2, life, w1, w2 time.Duration }{
		{2 * s, 3 * s, 4 * s, 2 * s, 3 * s},               // fine as given
		{2 * s, 0, 4 * s, 2 * s, 3500 * time.Millisecond}, // a T2 rounded to 0
		{0, 0, s, 500 * time.Millisecond, 875 * time.Millisecond},
		{5 * s, 4 * s, 10 * s, 4 * s * 4 / 7, 4 * s},    // T1 after T2: before it
		{s, 20 * s, 10 * s, s, 8750 * time.Millisecond}, // T2 past the lease
	} {
		g1, g2 := saneTimes(c.t1, c.t2, c.life)
		if g1 != c.w1 || g2 != c.w2 || !(0 < g1 && g1 < g2 && g2 < c.life) {
			t.Errorf("%v %v %v: got %v %v, want %v %v", c.t1, c.t2, c.life, g1, g2, c.w1, c.w2)
		}
	}
}
