package ospf

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
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

func TestFragments(t *testing.T) {
	s := &session{counters: map[string]uint64{}}
	payload := make([]byte, 4000)
	for i := range payload {
		payload[i] = byte(i)
	}
	src, dst := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("224.0.0.5")
	frags := fragment4(pkt.IPv4{ID: 9, TTL: 1, Protocol: ProtoOSPF, Src: src, Dst: dst, Payload: payload}, 1500)
	if len(frags) != 3 {
		t.Fatalf("%d fragments", len(frags))
	}
	var whole []byte
	now := time.Now()
	for i := len(frags) - 1; i >= 0; i-- { // out of order
		ip, err := pkt.DecodeIPv4(frags[i])
		if err != nil || len(frags[i]) > 1500 {
			t.Fatalf("fragment %d: %v (%d bytes)", i, err, len(frags[i]))
		}
		if b, ok := s.reassemble(reasmKey{src, dst, uint32(ip.ID)}, int(ip.FragOff)*8, ip.Flags&pkt.IPv4MF != 0, ip.Payload, now); ok {
			whole = b
		}
	}
	if !bytes.Equal(whole, payload) {
		t.Fatalf("reassembled %d bytes", len(whole))
	}
	src6, dst6 := netip.MustParseAddr("fe80::1"), netip.MustParseAddr("ff02::5")
	frags = fragment6(pkt.IPv6{NextHeader: ProtoOSPF, HopLimit: 1, Src: src6, Dst: dst6, Payload: payload}, 1500, 77)
	whole = nil
	for _, f := range frags {
		ip, _ := pkt.DecodeIPv6(f)
		h := ip.Payload
		fo := binary.BigEndian.Uint16(h[2:])
		if ip.NextHeader != ipv6FragHeader || h[0] != ProtoOSPF || binary.BigEndian.Uint32(h[4:]) != 77 || len(f) > 1500 {
			t.Fatalf("bad IPv6 fragment")
		}
		if b, ok := s.reassemble(reasmKey{src6, dst6, 77}, int(fo>>3)*8, fo&1 != 0, h[8:], now); ok {
			whole = b
		}
	}
	if !bytes.Equal(whole, payload) {
		t.Fatalf("IPv6 reassembled %d bytes", len(whole))
	}
}

func TestKeychainSelection(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	a := Auth{Type: "keychain", Keys: []AuthKey{
		{ID: 1, Type: "md5", Key: "one", SendUntil: "2026-10-22T12:00:00Z"},
		{ID: 2, Type: "hmac-sha256", Key: "two", SendFrom: "2026-10-22T11:00:00Z", AcceptFrom: "2026-10-22T11:00:00Z"},
		{ID: 3, Type: "hmac-sha256", Key: "three", SendFrom: "2026-10-23T00:00:00Z", AcceptFrom: "2026-10-23T00:00:00Z"},
	}}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if s := a.sending(now, false); s.Type != "hmac-sha256" || s.KeyID != 2 {
		t.Fatalf("sending %+v", s)
	}
	if _, err := a.accepting(3, now, false); err == nil {
		t.Fatal("key 3 accepted before its window")
	}
	if k, err := a.accepting(1, now, false); err != nil || k.Type != "md5" {
		t.Fatalf("key 1: %v %v", k, err)
	}
	if a.maxDigest() != 32 || !a.crypto() {
		t.Fatalf("digest room %d", a.maxDigest())
	}
	for _, bad := range []Auth{
		{Type: "keychain"},
		{Type: "keychain", Keys: []AuthKey{{ID: 300, Type: "md5", Key: "x"}}},
		{Type: "keychain", Keys: []AuthKey{{ID: 1, Type: "md5", Key: "x", SendFrom: "noon"}}},
	} {
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if (Auth3{Type: "keychain", Keys: []AuthKey{{ID: 1, Type: "md5", Key: "x"}}}).Validate() == nil {
		t.Fatal("OSPFv3 keychain with md5 accepted")
	}
}

func TestAreaConfig(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"area": 1, "areas": [{"id": 2, "type": "nssa"}], "interfaces": [{"device": "x", "area": "0.0.0.2"}]}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Area != "1" || c.Areas[0].ID != "2" {
		t.Fatalf("%+v", c)
	}
	d := defaultConfig()
	d.Areas = []AreaConfig{{ID: "0", Type: "stub"}}
	if d.Validate() == nil {
		t.Fatal("stub backbone accepted")
	}
	d.Areas = []AreaConfig{{ID: "1", NoSummary: true}}
	if d.Validate() == nil {
		t.Fatal("no_summary on a normal area accepted")
	}
}

func TestOpaqueTLVs(t *testing.T) {
	// A 3-byte value is padded to 8 bytes; the length says 3.
	b := tlv(9, []byte{1, 2, 3})
	if len(b) != 8 || binary.BigEndian.Uint16(b[2:]) != 3 {
		t.Fatalf("% x", b)
	}
	body := extPrefixBody(routeIntra, netip.MustParsePrefix("192.0.2.1/32"), extPfxNode, []prefixSID{{index: 5}}, map[byte]uint32{128: 7})
	sum, d := opaqueDetail(opaqueID(opqExtPfx, 1), body)
	if sum != "192.0.2.1/32 index 5 algo 0 flags 0x00; algo 128 metric 7" || d["kind"] != "extended-prefix" {
		t.Fatalf("%q %v", sum, d)
	}
}
