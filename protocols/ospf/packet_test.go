package ospf

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"net/netip"
	"reflect"
	"strings"
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
	keys := []LSAKey{{Type: LSARouter, ID: r1, AdvRtr: r1}, {Type: LSAExternal, ID: netip.MustParseAddr("198.18.0.0"), AdvRtr: r2}}
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

func TestPackets3(t *testing.T) {
	c := codec{v3: true}
	h := Hello{IfID: 7, Interval: 10, Options: options3, Priority: 1, Dead: 40, DR: r1, BDR: zero4, Neighbors: []netip.Addr{r2}}
	p := c.encodeHeader(TypeHello, r1, zero4, 3, c.encodeHello(h))
	hd, err := c.decodeHeader(p)
	if err != nil || hd.Version != 3 || hd.InstanceID != 3 || hd.RouterID != r1 || len(hd.Body) != 24 {
		t.Fatalf("header %+v %v", hd, err)
	}
	if got, err := c.decodeHello(hd.Body); err != nil || !reflect.DeepEqual(*got, h) {
		t.Fatalf("hello %+v %v", got, err)
	}
	if _, err := (codec{}).decodeHeader(p); err == nil {
		t.Fatal("v3 packet accepted as v2")
	}
	d := DBD{MTU: 1500, Options: options3, Flags: DDMore, Seq: 9,
		Headers: []LSAHeader{{Age: 1, Type: LSA3Link, ID: id4(1), AdvRtr: r1, Seq: InitialSeq, Checksum: 0xabcd, Length: 56}}}
	if gd, err := c.decodeDBD(c.encodeDBD(d)); err != nil || !reflect.DeepEqual(*gd, d) {
		t.Fatalf("dbd %+v %v", gd, err)
	}

	// LSAs: build, checksum, decode.
	ll := netip.MustParseAddr("fe80::1")
	pfx := netip.MustParsePrefix("2001:db8:1::/64")
	for _, x := range []struct {
		typ  uint16
		body []byte
		want string
	}{
		{LSA3Router, Router3LSA{Flags: RouterE, Options: options3, Links: []Router3Link{{Type: LinkP2P, Metric: 10, IfID: 1, NbrIfID: 5, NbrRouter: r2}}}.Encode(), "1 point-to-point, ASBR"},
		{LSA3Network, Network3LSA{Options: options3, Routers: []netip.Addr{r1, r2}}.Encode(), "2 routers"},
		{LSA3Link, LinkLSA{Priority: 1, Options: options3, LinkLocal: ll, Prefixes: []Prefix6{{Prefix: pfx}}}.Encode(), "fe80::1 · 2001:db8:1::/64"},
		{LSA3IntraPrefix, IntraPrefixLSA{RefType: LSA3Router, RefID: zero4, RefAdvRtr: r1, Prefixes: []Prefix6{{Prefix: pfx, Metric: 10},
			{Prefix: netip.MustParsePrefix("2001:db8:ffff::60/128"), Options: PrefixLA}}}.Encode(), "router → 2001:db8:1::/64 (10), 2001:db8:ffff::60/128"},
		{LSA3External, External3LSA{E2: true, Metric: 20, Prefix: Prefix6{Prefix: netip.MustParsePrefix("2001:db8:66::/48")}, Tag: 600}.Encode(), "2001:db8:66::/48 E2 metric 20 tag 600"},
		{LSA3External, External3LSA{Metric: 5, Prefix: Prefix6{Prefix: netip.MustParsePrefix("2001:db8:67::/48")}, Forward: ll}.Encode(), "2001:db8:67::/48 E1 metric 5 fwd fe80::1"},
	} {
		raw := c.buildLSA(LSAHeader{Type: x.typ, ID: id4(1), AdvRtr: r1, Seq: InitialSeq}, x.body)
		if !LSAChecksumOK(raw) || c.lsaHeader(raw).Type != x.typ {
			t.Fatalf("%#x: header/checksum", x.typ)
		}
		if got := summary3(x.typ, raw[lsaHeaderLen:]); got != x.want {
			t.Errorf("%s: %q, want %q", lsa3TypeName(x.typ), got, x.want)
		}
	}
	if !c.linkScope(LSA3Link) || c.linkScope(LSA3Router) || c.linkScope(LSA3External) || !c.linkScope(0x0042) || c.linkScope(0xa042) {
		t.Fatal("flooding scopes")
	}
	// Prefixes that don't fill whole words.
	b := appendPrefix6(nil, Prefix6{Prefix: netip.MustParsePrefix("2001:db8::/29"), Metric: 3}, true)
	if len(b) != 8 {
		t.Fatalf("/29 takes %d bytes", len(b))
	}
	if p, m, rest, err := readPrefix6(b); err != nil || p.Prefix.String() != "2001:db8::/29" || m != 3 || len(rest) != 0 {
		t.Fatalf("readPrefix6 %v %d %v", p, m, err)
	}
}

func TestNextPrefix(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"10.0.0.0/24", "10.0.1.0/24"}, {"10.0.255.0/24", "10.1.0.0/24"}, {"2001:db8::/48", "2001:db8:1::/48"},
		{"2001:db8:ffff::/48", "2001:db9::/48"}, {"10.0.0.0/30", "10.0.0.4/30"},
	} {
		got, ok := nextPrefix(netip.MustParsePrefix(c.in))
		if !ok || got.String() != c.want {
			t.Errorf("%s: %v, want %s", c.in, got, c.want)
		}
	}
	if _, ok := nextPrefix(netip.MustParsePrefix("255.255.255.0/24")); ok {
		t.Error("ran past the end")
	}
}

func TestHMACAuth(t *testing.T) {
	h := Hello{Mask: MaskOf(24), Interval: 10, Options: OptE, Priority: 1, Dead: 40, DR: r1, BDR: zero4}
	for _, typ := range []string{"hmac-sha1", "hmac-sha256", "hmac-sha384", "hmac-sha512"} {
		// v2 (RFC 5709). A key longer than the digest is hashed first.
		for _, key := range []string{"k", strings.Repeat("long key ", 20)} {
			a := Auth{Type: typ, Key: key, KeyID: 3}
			p := sign(encodeHeader(TypeHello, r1, zero4, h.Encode()), a, 77)
			hd, _ := DecodeHeader(p)
			if seq, crypto, err := verify(hd, a); err != nil || !crypto || seq != 77 {
				t.Fatalf("%s v2: %d %v %v", typ, seq, crypto, err)
			}
			if len(hd.Trailer) != digestLen(a) {
				t.Fatalf("%s v2: %d digest bytes", typ, len(hd.Trailer))
			}
			hd.Packet[30] ^= 1
			if _, _, err := verify(hd, a); err == nil {
				t.Fatalf("%s v2: tampered packet accepted", typ)
			}
		}
		// v3 (RFC 7166): the source address is covered too.
		c := codec{v3: true}
		a := Auth{Type: typ, Key: "secret", SAID: 9}
		src := netip.MustParseAddr("fe80::1")
		p := c.encodeHeader(TypeHello, r1, zero4, 0, c.encodeHello(Hello{IfID: 1, Options: options3 | OptAT, Interval: 10, Dead: 40}))
		p = append(p, trailer3(p, src, a, 1<<32|5)...)
		hd, err := c.decodeHeader(p)
		if err != nil {
			t.Fatal(err)
		}
		if seq, err := verify3(hd, src, a); err != nil || seq != 1<<32|5 {
			t.Fatalf("%s v3: %d %v", typ, seq, err)
		}
		if _, err := verify3(hd, netip.MustParseAddr("fe80::2"), a); err == nil {
			t.Fatalf("%s v3: accepted from another source", typ)
		}
		bad := a
		bad.SAID = 10
		if _, err := verify3(hd, src, bad); err == nil {
			t.Fatalf("%s v3: wrong SA accepted", typ)
		}
	}
	// RFC 5709 key handling: shorter keys are zero padded to the digest
	// length, which plain HMAC also does, so the digest equals a plain
	// HMAC-SHA-256 over packet + Apad with the padded key.
	a := Auth{Type: "hmac-sha256", Key: "abc", KeyID: 1}
	p := sign(encodeHeader(TypeHello, r1, zero4, h.Encode()), a, 1)
	m := hmac.New(sha256.New, []byte("abc"))
	m.Write(p[:len(p)-32])
	m.Write(apad(32, netip.Addr{}))
	if !bytes.Equal(m.Sum(nil), p[len(p)-32:]) {
		t.Fatal("digest differs from plain HMAC")
	}
}
