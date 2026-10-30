// Package dhcp is DHCP clients for devices without configured addresses:
// "dhcp" (DHCPv4, RFC 2131) and "dhcpv6" (RFC 8415: addresses, prefix
// delegation, and the default router from router advertisements). A
// lease's address and gateway become the device's (protocol.IP's
// AddAddress) until it ends, so flows and pings run from it.
package dhcp

import (
	"encoding/binary"
	"math/rand/v2"
	"net/netip"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// udpPacket builds an IP packet carrying a UDP datagram.
func udpPacket(src, dst netip.Addr, sport, dport uint16, hops uint8, data []byte) (uint16, []byte) {
	u := make([]byte, 8+len(data))
	binary.BigEndian.PutUint16(u[0:], sport)
	binary.BigEndian.PutUint16(u[2:], dport)
	binary.BigEndian.PutUint16(u[4:], uint16(len(u)))
	copy(u[8:], data)
	cs := pkt.Checksum(u, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(u)))
	if cs == 0 {
		cs = 0xffff
	}
	binary.BigEndian.PutUint16(u[6:], cs)
	if src.Is4() {
		h := pkt.IPv4{TTL: hops, Protocol: pkt.ProtoUDP, Src: src, Dst: dst, Payload: u}
		return pkt.EtherTypeIPv4, h.Encode()
	}
	h := pkt.IPv6{NextHeader: pkt.ProtoUDP, HopLimit: hops, Src: src, Dst: dst, Payload: u}
	return pkt.EtherTypeIPv6, h.Encode()
}

// udpOf returns a frame's UDP datagram to port dport, with its checksum
// checked.
func udpOf(f *pkt.Frame, dport uint16) (src, dst netip.Addr, data []byte, ok bool) {
	var b []byte
	switch f.EtherType {
	case pkt.EtherTypeIPv4:
		h, err := pkt.DecodeIPv4(f.Payload)
		if err != nil || h.Protocol != pkt.ProtoUDP || h.FragOff != 0 || h.Flags&pkt.IPv4MF != 0 {
			return
		}
		src, dst, b = h.Src, h.Dst, h.Payload
	case pkt.EtherTypeIPv6:
		h, err := pkt.DecodeIPv6(f.Payload)
		if err != nil || h.NextHeader != pkt.ProtoUDP {
			return
		}
		src, dst, b = h.Src, h.Dst, h.Payload
	default:
		return
	}
	if len(b) < 8 || binary.BigEndian.Uint16(b[2:]) != dport {
		return
	}
	n := int(binary.BigEndian.Uint16(b[4:]))
	if n < 8 || n > len(b) {
		return
	}
	b = b[:n]
	if cs := binary.BigEndian.Uint16(b[6:]); (cs != 0 || dst.Is6()) && pkt.Checksum(b, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(b))) != 0 {
		return
	}
	return src, dst, b[8:], true
}

// backoff is a retransmission timer: RFC 8415 15's (initial, doubling to
// a maximum, each ±10%), which also fits RFC 2131 4.1's 4, 8, … 64 s.
type backoff struct {
	initial, max time.Duration
	rt           time.Duration
}

func (b *backoff) next() time.Duration {
	if b.rt == 0 {
		b.rt = b.initial
	} else if b.rt *= 2; b.max > 0 && b.rt > b.max {
		b.rt = b.max
	}
	return jitter(b.rt, 0.1)
}

func jitter(d time.Duration, f float64) time.Duration {
	return time.Duration(float64(d) * (1 + f*(2*rand.Float64()-1)))
}

// saneTimes keeps renewing (t1) and rebinding (t2) inside a lifetime, in
// order: t1 < t2 < life (RFC 2131 4.4.5), so a server's odd values (or
// rounding of short ones) can't make a client renew in a loop.
func saneTimes(t1, t2, life time.Duration) (time.Duration, time.Duration) {
	if t2 <= 0 || t2 >= life {
		t2 = life * 7 / 8
	}
	if t1 <= 0 || t1 >= t2 {
		t1 = min(life/2, t2*4/7)
	}
	return t1, t2
}

// secs is seconds, for durations in status.
func secs(d time.Duration) float64 { return float64(d.Round(time.Millisecond)) / float64(time.Second) }

// lifetime converts a lease or lifetime in seconds (0xffffffff: forever).
func lifetime(s uint32) time.Duration {
	if s == 0xffffffff {
		return 100 * 365 * 24 * time.Hour
	}
	return time.Duration(s) * time.Second
}
