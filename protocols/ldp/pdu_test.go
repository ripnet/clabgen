package ldp

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"
)

var (
	idA = ID{LSR: netip.MustParseAddr("192.0.2.1")}
	idB = ID{LSR: netip.MustParseAddr("192.0.2.2")}
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A hello built by hand from RFC 5036's layouts.
func TestHelloBytes(t *testing.T) {
	m := Message{Type: MsgHello, ID: 7, TLVs: []TLV{
		HelloParams{Hold: 15}.tlv(),
		{Type: TLVIPv4Transport, Value: netip.MustParseAddr("192.0.2.1").AsSlice()},
	}}
	got := EncodePDU(idA, m.Encode())
	want := unhex(t, `0001 001e c0000201 0000
		0100 0014 00000007
		0400 0004 000f 0000
		0401 0004 c0000201`)
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
	p, err := DecodePDU(got)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != idA || len(p.Messages) != 1 || p.Messages[0].ID != 7 || len(p.Messages[0].TLVs) != 2 {
		t.Fatalf("%+v", p)
	}
	h, _ := decodeHelloParams(p.Messages[0].tlv(TLVCommonHello).Value)
	if h.Hold != 15 || h.Targeted || h.Request {
		t.Fatalf("%+v", h)
	}
}

func TestHelloFlags(t *testing.T) {
	h, _ := decodeHelloParams(HelloParams{Hold: 45, Targeted: true, Request: true}.tlv().Value)
	if !h.Targeted || !h.Request || h.Hold != 45 {
		t.Fatalf("%+v", h)
	}
	if _, err := decodeHelloParams([]byte{1}); err == nil {
		t.Fatal("short")
	}
}

func TestSessionParams(t *testing.T) {
	in := SessionParams{KeepAlive: 30, MaxPDU: 4096, Receiver: idB}
	v := in.tlv().Value
	want := unhex(t, "0001 001e 00 00 1000 c0000202 0000")
	if !bytes.Equal(v, want) {
		t.Fatalf("got %x want %x", v, want)
	}
	out, err := decodeSessionParams(v)
	if err != nil || out != in {
		t.Fatalf("%+v %v", out, err)
	}
	v[1] = 2
	if _, err := decodeSessionParams(v); err == nil {
		t.Fatal("bad version accepted")
	}
}

func TestFEC(t *testing.T) {
	ps := []netip.Prefix{netip.MustParsePrefix("192.0.2.2/32"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("10.0.0.0/9"), netip.MustParsePrefix("0.0.0.0/0")}
	v := fecTLV(ps...).Value
	want := unhex(t, "02 0001 20 c0000202  02 0001 18 c63364  02 0001 09 0a00  02 0001 00")
	if !bytes.Equal(v, want) {
		t.Fatalf("got %x want %x", v, want)
	}
	f, err := decodeFEC(v)
	if err != nil || len(f.Prefixes) != 4 {
		t.Fatalf("%+v %v", f, err)
	}
	for i := range ps {
		if f.Prefixes[i] != ps[i] {
			t.Errorf("%d: %s", i, f.Prefixes[i])
		}
	}
	// IPv6 elements are skipped, a wildcard noted, a pseudowire element counted.
	f, err = decodeFEC(unhex(t, "02 0002 80 20010db8000000000000000000000001  01  80 05"))
	if err != nil || f.Other != 2 || !f.Wildcard || len(f.Prefixes) != 0 {
		t.Fatalf("%+v %v", f, err)
	}
	for _, bad := range []string{"02", "02 0001 20 c0", "02 0001 21 c0000202ff"} {
		if _, err := decodeFEC(unhex(t, bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestLabelAddressStatus(t *testing.T) {
	if l, err := decodeLabel(labelTLV(0xabcde).Value); err != nil || l != 0xabcde {
		t.Fatalf("%x %v", l, err)
	}
	as := []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("192.0.2.1")}
	got, err := decodeAddresses(addressTLV(as).Value)
	if err != nil || len(got) != 2 || got[1] != as[1] {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := decodeAddresses(unhex(t, "0002 20010db8000000000000000000000001")); err != nil || got != nil {
		t.Fatalf("v6: %v %v", got, err)
	}
	s, err := decodeStatus(StatusTLV{Code: StatusKeepAliveExpired, MsgID: 9, MsgType: MsgKeepAlive}.tlv().Value)
	if err != nil || s.Code != StatusKeepAliveExpired || s.MsgID != 9 || s.MsgType != MsgKeepAlive || !Fatal(s.Code) {
		t.Fatalf("%+v %v", s, err)
	}
	if StatusText(StatusShutdown) != "shutdown" || Fatal(StatusNoRoute) || !strings.Contains(StatusText(0x1234), "0x1234") {
		t.Fatal(StatusText(StatusShutdown))
	}
}

func TestDecodeRejects(t *testing.T) {
	ok := EncodePDU(idA, Message{Type: MsgKeepAlive, ID: 1}.Encode())
	for name, f := range map[string]func([]byte){
		"version":    func(b []byte) { b[1] = 2 },
		"pdu length": func(b []byte) { b[3] = 200 },
		"msg length": func(b []byte) { b[13] = 200 },
	} {
		b := append([]byte(nil), ok...)
		f(b)
		if _, err := DecodePDU(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Unknown TLV and message bits survive, and a TLV overrun is refused.
	m := Message{Type: 0x3f00, Unknown: true, ID: 2, TLVs: []TLV{{Type: 0x3333, Unknown: true, Forward: true, Value: []byte{1, 2}}}}
	p, err := DecodePDU(EncodePDU(idA, m.Encode()))
	if err != nil || !p.Messages[0].Unknown || p.Messages[0].TLVs[0].Type != 0x3333 || !p.Messages[0].TLVs[0].Forward {
		t.Fatalf("%+v %v", p, err)
	}
	b := EncodePDU(idA, m.Encode())
	b[len(b)-3] = 99 // the TLV's length
	if _, err := DecodePDU(b); err == nil {
		t.Fatal("TLV overrun accepted")
	}
}
