package pkt

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"testing"
)

func TestIPv4Checksum(t *testing.T) {
	// Classic example header with checksum 0xb861.
	h, _ := hex.DecodeString("450000730000400040110000c0a80001c0a800c7")
	if got := Checksum(h, 0); got != 0xb861 {
		t.Fatalf("checksum %#04x, want 0xb861", got)
	}
}

func TestIPv4RoundTrip(t *testing.T) {
	in := IPv4{TOS: 0xb8, ID: 7, Flags: IPv4DF, TTL: 9, Protocol: ProtoICMP,
		Src: netip.MustParseAddr("10.0.0.1"), Dst: netip.MustParseAddr("10.0.0.2"), Payload: []byte("hello")}
	b := in.Encode()
	// Ethernet padding after the datagram must be trimmed.
	out, err := DecodeIPv4(append(b, 0, 0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if out.TOS != 0xb8 || out.ID != 7 || out.Flags != IPv4DF || out.TTL != 9 || out.Src != in.Src || out.Dst != in.Dst || string(out.Payload) != "hello" {
		t.Fatalf("round trip: %+v", out)
	}
	b[8]++ // TTL change invalidates the checksum
	if _, err := DecodeIPv4(b); err != ErrChecksum {
		t.Fatalf("want ErrChecksum, got %v", err)
	}
	bad := IPv4{Src: in.Src, Dst: in.Dst, Checksum: 0x1234, ChecksumSet: true}
	if _, err := DecodeIPv4(bad.Encode()); err != ErrChecksum {
		t.Fatalf("forced checksum: got %v", err)
	}
}

func TestARPRoundTrip(t *testing.T) {
	a := ARP{Op: ARPReply, SHA: MAC{2, 0, 0, 0, 0, 1}, SPA: netip.MustParseAddr("192.0.2.1"), THA: MAC{2, 0, 0, 0, 0, 2}, TPA: netip.MustParseAddr("192.0.2.2")}
	b, err := DecodeARP(a.Encode())
	if err != nil || b != a {
		t.Fatalf("got %+v, %v", b, err)
	}
}

func TestIPv6Helpers(t *testing.T) {
	m := MAC{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	if got := LinkLocal(m).String(); got != "fe80::42:acff:fe11:2" {
		t.Fatalf("link-local %s", got)
	}
	a := netip.MustParseAddr("2001:db8::1:2:3")
	sn := SolicitedNode(a)
	if sn.String() != "ff02::1:ff02:3" {
		t.Fatalf("solicited-node %s", sn)
	}
	if mm := MulticastMAC(sn); mm != (MAC{0x33, 0x33, 0xff, 0x02, 0x00, 0x03}) {
		t.Fatalf("multicast MAC %s", mm)
	}
	h := IPv6{TrafficClass: 0xa0, FlowLabel: 0x12345, NextHeader: ProtoICMPv6, HopLimit: 255, Src: LinkLocal(m), Dst: sn,
		Payload: NDMessage(ICMPv6NeighborSol, 0, a, NDOptSourceLLAddr, m)}
	ICMPv6Checksum(h.Src, h.Dst, h.Payload)
	out, err := DecodeIPv6(h.Encode())
	if err != nil || out.TrafficClass != 0xa0 || out.FlowLabel != 0x12345 || out.HopLimit != 255 || !bytes.Equal(out.Payload, h.Payload) {
		t.Fatalf("round trip %+v %v", out, err)
	}
	if !ICMPv6Valid(out.Src, out.Dst, out.Payload) {
		t.Fatal("checksum invalid")
	}
	if ll, ok := NDOption(out.Payload[24:], NDOptSourceLLAddr); !ok || ll != m {
		t.Fatalf("option %s %v", ll, ok)
	}
}

func TestMulticastMACIPv4(t *testing.T) {
	for addr, want := range map[string]MAC{
		"224.0.0.5":        {0x01, 0x00, 0x5e, 0x00, 0x00, 0x05}, // AllSPFRouters
		"239.129.2.3":      {0x01, 0x00, 0x5e, 0x01, 0x02, 0x03}, // only the low 23 bits
		"::ffff:224.0.0.6": {0x01, 0x00, 0x5e, 0x00, 0x00, 0x06},
		"ff02::5":          {0x33, 0x33, 0x00, 0x00, 0x00, 0x05},
	} {
		if got := MulticastMAC(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %s, want %s", addr, got, want)
		}
	}
}
