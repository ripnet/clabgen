package ospf

import (
	"net/netip"
	"reflect"
	"testing"
)

var (
	r1 = netip.MustParseAddr("192.0.2.1")
	r2 = netip.MustParseAddr("192.0.2.2")
)

func TestPackets(t *testing.T) {
	h := Hello{Mask: MaskOf(24), Interval: 10, Options: OptE, Priority: 1, Dead: 40, DR: r1, BDR: zero4, Neighbors: []netip.Addr{r2}}
	p := sign(encodeHeader(TypeHello, r1, zero4, h.Encode()), Auth{Type: "none"}, 0)
	hd, err := DecodeHeader(p)
	if err != nil || hd.Type != TypeHello || hd.RouterID != r1 {
		t.Fatalf("header %+v %v", hd, err)
	}
	if _, _, err := verify(hd, Auth{Type: "none"}); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeHello(hd.Body)
	if err != nil || !reflect.DeepEqual(*got, h) {
		t.Fatalf("hello %+v %v", got, err)
	}
	p[30] ^= 1
	hd, _ = DecodeHeader(p)
	if _, _, err := verify(hd, Auth{Type: "none"}); err == nil {
		t.Fatal("corrupted packet passed the checksum")
	}

	// Simple and MD5 authentication.
	for _, a := range []Auth{{Type: "simple", Key: "secret"}, {Type: "md5", Key: "k3y", KeyID: 7}} {
		p := sign(encodeHeader(TypeHello, r1, zero4, h.Encode()), a, 42)
		hd, _ := DecodeHeader(p)
		seq, crypto, err := verify(hd, a)
		if err != nil || crypto != (a.Type == "md5") || (crypto && seq != 42) {
			t.Fatalf("%s: seq %d crypto %v err %v", a.Type, seq, crypto, err)
		}
		bad := a
		bad.Key = "wrong"
		if _, _, err := verify(hd, bad); err == nil {
			t.Fatalf("%s: wrong key accepted", a.Type)
		}
		if _, _, err := verify(hd, Auth{Type: "none"}); err == nil {
			t.Fatalf("%s: accepted as unauthenticated", a.Type)
		}
	}

	d := DBD{MTU: 1500, Options: OptE, Flags: DDInit | DDMore | DDMaster, Seq: 77,
		Headers: []LSAHeader{{Age: 3, Options: OptE, Type: LSARouter, ID: r1, AdvRtr: r1, Seq: InitialSeq, Checksum: 0x1234, Length: 36}}}
	if gd, err := DecodeDBD(d.Encode()); err != nil || !reflect.DeepEqual(*gd, d) {
		t.Fatalf("dbd %+v %v", gd, err)
	}
	keys := []LSAKey{{LSARouter, r1, r1}, {LSAExternal, netip.MustParseAddr("198.18.0.0"), r2}}
	if gk, err := DecodeLSR(EncodeLSR(keys)); err != nil || !reflect.DeepEqual(gk, keys) {
		t.Fatalf("lsr %v %v", gk, err)
	}
}

func TestLSAs(t *testing.T) {
	rl := RouterLSA{Flags: RouterE, Links: []RouterLink{
		{ID: r2, Data: netip.MustParseAddr("10.0.0.1"), Type: LinkP2P, Metric: 10},
		{ID: netip.MustParseAddr("10.0.0.0"), Data: MaskOf(30), Type: LinkStub, Metric: 10},
	}}
	raw := BuildLSA(LSAHeader{Age: 0, Options: OptE, Type: LSARouter, ID: r1, AdvRtr: r1, Seq: InitialSeq}, rl.Encode())
	if !LSAChecksumOK(raw) {
		t.Fatal("checksum")
	}
	SetAge(raw, 1234) // the age isn't covered
	if !LSAChecksumOK(raw) {
		t.Fatal("checksum after ageing")
	}
	h := LSAHeaderOf(raw)
	if h.Length != uint16(len(raw)) || h.Age != 1234 {
		t.Fatalf("header %+v", h)
	}
	got, err := DecodeRouterLSA(raw[lsaHeaderLen:])
	if err != nil || !reflect.DeepEqual(*got, rl) {
		t.Fatalf("router %+v %v", got, err)
	}
	e := ExternalLSA{Mask: MaskOf(24), E2: true, Metric: 20, Forward: zero4, Tag: 100}
	if ge, err := DecodeExternalLSA(e.Encode()); err != nil || *ge != e {
		t.Fatalf("external %+v %v", ge, err)
	}
	lsus, err := DecodeLSU(EncodeLSU([][]byte{raw, raw}))
	if err != nil || len(lsus) != 2 {
		t.Fatalf("lsu %d %v", len(lsus), err)
	}

	// Which instance is newer (RFC 2328 13.1).
	a := LSAHeader{Seq: InitialSeq + 1, Checksum: 1, Age: 10}
	for _, c := range []struct {
		b    LSAHeader
		want int
	}{
		{LSAHeader{Seq: InitialSeq, Checksum: 9, Age: 0}, 1},       // higher sequence (signed compare)
		{LSAHeader{Seq: InitialSeq + 2}, -1},                       //
		{LSAHeader{Seq: InitialSeq + 1, Checksum: 2, Age: 10}, -1}, // higher checksum
		{LSAHeader{Seq: InitialSeq + 1, Checksum: 1, Age: MaxAge}, -1},
		{LSAHeader{Seq: InitialSeq + 1, Checksum: 1, Age: 2000}, 1}, // much older
		{LSAHeader{Seq: InitialSeq + 1, Checksum: 1, Age: 500}, 0},
	} {
		if got := Compare(a, c.b); got != c.want {
			t.Errorf("Compare(%+v, %+v) = %d, want %d", a, c.b, got, c.want)
		}
	}
	if BitsOf(MaskOf(27)) != 27 || BitsOf(netip.MustParseAddr("255.0.255.0")) != -1 {
		t.Fatal("masks")
	}
}

func TestConfigValidate(t *testing.T) {
	c := defaultConfig()
	c.Prefixes = []Prefix{{Prefix: "198.18.0.0/24", Count: 1000}, {Prefix: "198.19.60.0/24", Type: "internal"}}
	if err := c.Validate(); err == nil {
		t.Fatal("overlapping prefixes accepted")
	}
	c.Prefixes[1].Prefix = "100.65.60.0/24"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.DeadInterval = c.HelloInterval
	if err := c.Validate(); err == nil {
		t.Fatal("dead interval equal to hello accepted")
	}
}
