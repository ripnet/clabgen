package pkt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

const (
	EtherTypeIPv4 uint16 = 0x0800
	EtherTypeARP  uint16 = 0x0806
	EtherTypeIPv6 uint16 = 0x86dd

	ProtoICMP   uint8 = 1
	ProtoTCP    uint8 = 6
	ProtoUDP    uint8 = 17
	ProtoICMPv6 uint8 = 58
)

var Broadcast = MAC{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// Checksum is the Internet checksum (RFC 1071) of b, continuing from a
// partial sum (e.g. a pseudo-header).
func Checksum(b []byte, sum uint32) uint16 {
	for len(b) >= 2 {
		sum += uint32(b[0])<<8 | uint32(b[1])
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	for sum > 0xffff {
		sum = sum>>16 + sum&0xffff
	}
	return ^uint16(sum)
}

// PseudoSum is the partial checksum of the IPv4 or IPv6 pseudo-header used
// by TCP, UDP and ICMPv6.
func PseudoSum(src, dst netip.Addr, proto uint8, length int) uint32 {
	var sum uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(b[i])<<8 | uint32(b[i+1])
		}
	}
	s, d := src.AsSlice(), dst.AsSlice()
	add(s)
	add(d)
	sum += uint32(proto) + uint32(length)&0xffff + uint32(length)>>16
	return sum
}

// ---- ARP ----

const (
	ARPRequest uint16 = 1
	ARPReply   uint16 = 2
)

// ARP is an Ethernet/IPv4 ARP packet.
type ARP struct {
	Op       uint16
	SHA, THA MAC
	SPA, TPA netip.Addr
}

func DecodeARP(b []byte) (ARP, error) {
	if len(b) < 28 {
		return ARP{}, ErrShort
	}
	if binary.BigEndian.Uint16(b[0:]) != 1 || binary.BigEndian.Uint16(b[2:]) != EtherTypeIPv4 || b[4] != 6 || b[5] != 4 {
		return ARP{}, errors.New("not Ethernet/IPv4 ARP")
	}
	a := ARP{Op: binary.BigEndian.Uint16(b[6:])}
	copy(a.SHA[:], b[8:14])
	a.SPA = netip.AddrFrom4([4]byte(b[14:18]))
	copy(a.THA[:], b[18:24])
	a.TPA = netip.AddrFrom4([4]byte(b[24:28]))
	return a, nil
}

func (a ARP) Encode() []byte {
	b := make([]byte, 28)
	binary.BigEndian.PutUint16(b[0:], 1)
	binary.BigEndian.PutUint16(b[2:], EtherTypeIPv4)
	b[4], b[5] = 6, 4
	binary.BigEndian.PutUint16(b[6:], a.Op)
	copy(b[8:], a.SHA[:])
	s, t := a.SPA.As4(), a.TPA.As4()
	copy(b[14:], s[:])
	copy(b[18:], a.THA[:])
	copy(b[24:], t[:])
	return b
}

// ---- IPv4 ----

// IPv4 flag bits (in the top 3 bits of the flags/fragment field).
const (
	IPv4DF uint8 = 0x2
	IPv4MF uint8 = 0x1
)

// IPv4 is a decoded IPv4 header. Payload aliases the input and is trimmed
// to the header's total length (dropping Ethernet padding).
type IPv4 struct {
	TOS      uint8
	ID       uint16
	Flags    uint8  // IPv4DF, IPv4MF
	FragOff  uint16 // in 8-byte units
	TTL      uint8
	Protocol uint8
	Src, Dst netip.Addr
	Options  []byte
	Payload  []byte

	// Checksum is the received value; Encode computes it unless
	// ChecksumSet is true (to send a deliberately wrong one).
	Checksum    uint16
	ChecksumSet bool
}

var ErrChecksum = errors.New("bad checksum")

func DecodeIPv4(b []byte) (*IPv4, error) {
	if len(b) < 20 {
		return nil, ErrShort
	}
	if b[0]>>4 != 4 {
		return nil, fmt.Errorf("IP version %d", b[0]>>4)
	}
	ihl := int(b[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(b[2:]))
	if ihl < 20 || total < ihl || len(b) < total {
		return nil, ErrShort
	}
	h := &IPv4{
		TOS:      b[1],
		ID:       binary.BigEndian.Uint16(b[4:]),
		Flags:    b[6] >> 5,
		FragOff:  binary.BigEndian.Uint16(b[6:]) & 0x1fff,
		TTL:      b[8],
		Protocol: b[9],
		Checksum: binary.BigEndian.Uint16(b[10:]),
		Src:      netip.AddrFrom4([4]byte(b[12:16])),
		Dst:      netip.AddrFrom4([4]byte(b[16:20])),
		Options:  b[20:ihl],
		Payload:  b[ihl:total],
	}
	if Checksum(b[:ihl], 0) != 0 {
		return h, ErrChecksum
	}
	return h, nil
}

// Encode serialises the header and payload. Options must be a multiple of
// 4 bytes.
func (h *IPv4) Encode() []byte {
	ihl := 20 + len(h.Options)
	b := make([]byte, ihl+len(h.Payload))
	b[0] = 0x40 | uint8(ihl/4)
	b[1] = h.TOS
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	binary.BigEndian.PutUint16(b[4:], h.ID)
	binary.BigEndian.PutUint16(b[6:], uint16(h.Flags)<<13|h.FragOff&0x1fff)
	b[8], b[9] = h.TTL, h.Protocol
	s, d := h.Src.As4(), h.Dst.As4()
	copy(b[12:], s[:])
	copy(b[16:], d[:])
	copy(b[20:], h.Options)
	cs := h.Checksum
	if !h.ChecksumSet {
		cs = Checksum(b[:ihl], 0)
	}
	binary.BigEndian.PutUint16(b[10:], cs)
	copy(b[ihl:], h.Payload)
	return b
}

// ---- IPv6 ----

// IPv6 is a decoded fixed IPv6 header; extension headers are left in
// Payload (NextHeader names the first one).
type IPv6 struct {
	TrafficClass uint8
	FlowLabel    uint32
	NextHeader   uint8
	HopLimit     uint8
	Src, Dst     netip.Addr
	Payload      []byte
}

func DecodeIPv6(b []byte) (*IPv6, error) {
	if len(b) < 40 {
		return nil, ErrShort
	}
	if b[0]>>4 != 6 {
		return nil, fmt.Errorf("IP version %d", b[0]>>4)
	}
	plen := int(binary.BigEndian.Uint16(b[4:]))
	if len(b) < 40+plen {
		return nil, ErrShort
	}
	vtf := binary.BigEndian.Uint32(b[0:])
	return &IPv6{
		TrafficClass: uint8(vtf >> 20),
		FlowLabel:    vtf & 0xfffff,
		NextHeader:   b[6],
		HopLimit:     b[7],
		Src:          netip.AddrFrom16([16]byte(b[8:24])),
		Dst:          netip.AddrFrom16([16]byte(b[24:40])),
		Payload:      b[40 : 40+plen],
	}, nil
}

func (h *IPv6) Encode() []byte {
	b := make([]byte, 40+len(h.Payload))
	binary.BigEndian.PutUint32(b[0:], 6<<28|uint32(h.TrafficClass)<<20|h.FlowLabel&0xfffff)
	binary.BigEndian.PutUint16(b[4:], uint16(len(h.Payload)))
	b[6], b[7] = h.NextHeader, h.HopLimit
	s, d := h.Src.As16(), h.Dst.As16()
	copy(b[8:], s[:])
	copy(b[24:], d[:])
	copy(b[40:], h.Payload)
	return b
}

// SolicitedNode returns the solicited-node multicast address of a.
func SolicitedNode(a netip.Addr) netip.Addr {
	x := a.As16()
	return netip.AddrFrom16([16]byte{0xff, 0x02, 10: 0, 11: 1, 12: 0xff, 13: x[13], 14: x[14], 15: x[15]})
}

// MulticastMAC returns the Ethernet group address for a multicast address:
// 01:00:5e + the low 23 bits for IPv4 (RFC 1112), 33:33 + the low 32 bits
// for IPv6 (RFC 2464).
func MulticastMAC(a netip.Addr) MAC {
	if a = a.Unmap(); a.Is4() {
		x := a.As4()
		return MAC{0x01, 0x00, 0x5e, x[1] & 0x7f, x[2], x[3]}
	}
	x := a.As16()
	return MAC{0x33, 0x33, x[12], x[13], x[14], x[15]}
}

// LinkLocal returns the modified EUI-64 link-local address for a MAC.
func LinkLocal(m MAC) netip.Addr {
	return netip.AddrFrom16([16]byte{0xfe, 0x80, 8: m[0] ^ 0x02, 9: m[1], 10: m[2], 11: 0xff, 12: 0xfe, 13: m[3], 14: m[4], 15: m[5]})
}

// ---- ICMP ----

// ICMP type numbers used by the stack.
const (
	ICMPEchoReply   uint8 = 0
	ICMPEchoRequest uint8 = 8

	ICMPv6EchoRequest uint8  = 128
	ICMPv6EchoReply   uint8  = 129
	ICMPv6RouterSol   uint8  = 133
	ICMPv6RouterAdv   uint8  = 134
	ICMPv6NeighborSol uint8  = 135
	ICMPv6NeighborAdv uint8  = 136
	NDOptSourceLLAddr uint8  = 1
	NDOptTargetLLAddr uint8  = 2
	NAFlagRouter      uint32 = 1 << 31
	NAFlagSolicited   uint32 = 1 << 30
	NAFlagOverride    uint32 = 1 << 29
)

// ICMPEcho builds an ICMPv4 echo message (type 0 or 8) with its checksum.
func ICMPEcho(typ uint8, id, seq uint16, data []byte) []byte {
	b := make([]byte, 8+len(data))
	b[0] = typ
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	copy(b[8:], data)
	binary.BigEndian.PutUint16(b[2:], Checksum(b, 0))
	return b
}

// ICMPv6Checksum fills in the checksum of an ICMPv6 message in place.
func ICMPv6Checksum(src, dst netip.Addr, msg []byte) {
	msg[2], msg[3] = 0, 0
	binary.BigEndian.PutUint16(msg[2:], Checksum(msg, PseudoSum(src, dst, ProtoICMPv6, len(msg))))
}

// ICMPv6Valid checks an ICMPv6 message's checksum.
func ICMPv6Valid(src, dst netip.Addr, msg []byte) bool {
	return len(msg) >= 4 && Checksum(msg, PseudoSum(src, dst, ProtoICMPv6, len(msg))) == 0
}

// NDMessage builds a neighbor solicitation or advertisement (without the
// checksum) carrying one link-layer address option.
func NDMessage(typ uint8, flags uint32, target netip.Addr, optType uint8, ll MAC) []byte {
	b := make([]byte, 32)
	b[0] = typ
	binary.BigEndian.PutUint32(b[4:], flags)
	t := target.As16()
	copy(b[8:], t[:])
	b[24], b[25] = optType, 1
	copy(b[26:], ll[:])
	return b
}

// NDOption returns the first link-layer address option of the given type in
// an NS/NA message's options.
func NDOption(opts []byte, typ uint8) (MAC, bool) {
	for len(opts) >= 8 {
		n := int(opts[1]) * 8
		if n == 0 || n > len(opts) {
			break
		}
		if opts[0] == typ && n >= 8 {
			return MAC(opts[2:8]), true
		}
		opts = opts[n:]
	}
	return MAC{}, false
}
