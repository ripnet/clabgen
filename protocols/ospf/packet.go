package ospf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"github.com/ripnet/clabgen/internal/pkt"
)

// OSPFv2 packets and LSAs (RFC 2328 appendix A).

const (
	ProtoOSPF = 89

	TypeHello = 1
	TypeDBD   = 2
	TypeLSR   = 3
	TypeLSU   = 4
	TypeAck   = 5

	headerLen    = 24
	lsaHeaderLen = 20

	// LSA types.
	LSARouter      = 1
	LSANetwork     = 2
	LSASummary     = 3
	LSAASBRSummary = 4
	LSAExternal    = 5

	// Options.
	OptE  = 0x02 // external routing capable (not a stub area)
	OptMC = 0x04
	OptNP = 0x08
	OptO  = 0x40 // opaque LSAs

	// DBD flags.
	DDInit   = 0x04
	DDMore   = 0x02
	DDMaster = 0x01

	// Router LSA flags and link types.
	RouterB = 0x01 // area border router
	RouterE = 0x02 // AS boundary router
	RouterV = 0x04

	LinkP2P     = 1
	LinkTransit = 2
	LinkStub    = 3
	LinkVirtual = 4

	// Authentication types.
	AuthNull   = 0
	AuthSimple = 1
	AuthCrypto = 2

	MaxAge        = 3600
	MaxAgeDiff    = 900
	InitialSeq    = 0x80000001
	MaxSeq        = 0x7fffffff
	LSInfinity    = 0xffffff
	MaxLinkMetric = 0xffff // RFC 6987 max-metric
)

var (
	AllSPFRouters = netip.MustParseAddr("224.0.0.5")
	AllDRouters   = netip.MustParseAddr("224.0.0.6")
)

var typeNames = map[uint8]string{TypeHello: "hello", TypeDBD: "dbd", TypeLSR: "lsr", TypeLSU: "lsu", TypeAck: "ack"}

// LSATypeName names an LSA type for status.
func LSATypeName(t uint8) string {
	switch t {
	case LSARouter:
		return "router"
	case LSANetwork:
		return "network"
	case LSASummary:
		return "summary"
	case LSAASBRSummary:
		return "asbr-summary"
	case LSAExternal:
		return "external"
	case 9, 10, 11:
		return fmt.Sprintf("opaque-%d", t)
	}
	return fmt.Sprintf("type-%d", t)
}

// Header is the common packet header. Packet bodies follow it in the
// packet's Body.
type Header struct {
	Type     uint8
	RouterID netip.Addr
	AreaID   netip.Addr
	AuType   uint16
	Auth     [8]byte // simple password; crypto: key ID, length, sequence
	Body     []byte  // after the header, up to the packet length
	Packet   []byte  // the whole packet (header + body), for auth checks
	Trailer  []byte  // crypto: the digest after the packet
}

func addr4(b []byte) netip.Addr { return netip.AddrFrom4([4]byte(b[:4])) }

func put4(b []byte, a netip.Addr) {
	if a.Is4() {
		x := a.As4()
		copy(b, x[:])
	}
}

// DecodeHeader parses and length-checks a packet (without verifying the
// checksum or authentication).
func DecodeHeader(b []byte) (*Header, error) {
	if len(b) < headerLen {
		return nil, errors.New("short packet")
	}
	if b[0] != 2 {
		return nil, fmt.Errorf("version %d", b[0])
	}
	n := int(binary.BigEndian.Uint16(b[2:]))
	if n < headerLen || n > len(b) {
		return nil, fmt.Errorf("bad length %d", n)
	}
	h := &Header{Type: b[1], RouterID: addr4(b[4:]), AreaID: addr4(b[8:]), AuType: binary.BigEndian.Uint16(b[14:]),
		Body: b[headerLen:n], Packet: b[:n], Trailer: b[n:]}
	copy(h.Auth[:], b[16:24])
	return h, nil
}

// checksumOK verifies the IP-style checksum over the packet, excluding the
// authentication field (null and simple auth only).
func checksumOK(p []byte) bool {
	c := make([]byte, len(p))
	copy(c, p)
	for i := 16; i < 24; i++ {
		c[i] = 0
	}
	return pkt.Checksum(c, 0) == 0
}

// encodeHeader writes a packet: header + body, checksum unless crypto
// authenticated (the caller signs it afterwards).
func encodeHeader(typ uint8, rid, area netip.Addr, body []byte) []byte {
	b := make([]byte, headerLen+len(body))
	b[0], b[1] = 2, typ
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	put4(b[4:], rid)
	put4(b[8:], area)
	copy(b[headerLen:], body)
	return b
}

// ---- hello ----

type Hello struct {
	Mask      netip.Addr
	Interval  uint16
	Options   uint8
	Priority  uint8
	Dead      uint32
	DR, BDR   netip.Addr
	Neighbors []netip.Addr
}

var zero4 = netip.AddrFrom4([4]byte{})

func (h Hello) Encode() []byte {
	b := make([]byte, 20+4*len(h.Neighbors))
	put4(b, h.Mask)
	binary.BigEndian.PutUint16(b[4:], h.Interval)
	b[6], b[7] = h.Options, h.Priority
	binary.BigEndian.PutUint32(b[8:], h.Dead)
	put4(b[12:], h.DR)
	put4(b[16:], h.BDR)
	for i, n := range h.Neighbors {
		put4(b[20+4*i:], n)
	}
	return b
}

func DecodeHello(b []byte) (*Hello, error) {
	if len(b) < 20 || (len(b)-20)%4 != 0 {
		return nil, errors.New("bad hello length")
	}
	h := &Hello{Mask: addr4(b), Interval: binary.BigEndian.Uint16(b[4:]), Options: b[6], Priority: b[7],
		Dead: binary.BigEndian.Uint32(b[8:]), DR: addr4(b[12:]), BDR: addr4(b[16:])}
	for i := 20; i < len(b); i += 4 {
		h.Neighbors = append(h.Neighbors, addr4(b[i:]))
	}
	return h, nil
}

// ---- database description ----

type DBD struct {
	MTU     uint16
	Options uint8
	Flags   uint8
	Seq     uint32
	Headers []LSAHeader
}

func (d DBD) Encode() []byte {
	b := make([]byte, 8, 8+lsaHeaderLen*len(d.Headers))
	binary.BigEndian.PutUint16(b, d.MTU)
	b[2], b[3] = d.Options, d.Flags
	binary.BigEndian.PutUint32(b[4:], d.Seq)
	for _, h := range d.Headers {
		b = h.append(b)
	}
	return b
}

func DecodeDBD(b []byte) (*DBD, error) {
	if len(b) < 8 || (len(b)-8)%lsaHeaderLen != 0 {
		return nil, errors.New("bad DBD length")
	}
	d := &DBD{MTU: binary.BigEndian.Uint16(b), Options: b[2], Flags: b[3], Seq: binary.BigEndian.Uint32(b[4:])}
	for i := 8; i < len(b); i += lsaHeaderLen {
		d.Headers = append(d.Headers, decodeLSAHeader(b[i:]))
	}
	return d, nil
}

// ---- link state request / ack ----

// LSAKey identifies an LSA.
type LSAKey struct {
	Type   uint8
	ID     netip.Addr
	AdvRtr netip.Addr
}

func (k LSAKey) String() string { return fmt.Sprintf("%s %s %s", LSATypeName(k.Type), k.ID, k.AdvRtr) }

func EncodeLSR(keys []LSAKey) []byte {
	b := make([]byte, 12*len(keys))
	for i, k := range keys {
		binary.BigEndian.PutUint32(b[12*i:], uint32(k.Type))
		put4(b[12*i+4:], k.ID)
		put4(b[12*i+8:], k.AdvRtr)
	}
	return b
}

func DecodeLSR(b []byte) ([]LSAKey, error) {
	if len(b)%12 != 0 {
		return nil, errors.New("bad LSR length")
	}
	var out []LSAKey
	for i := 0; i < len(b); i += 12 {
		out = append(out, LSAKey{Type: uint8(binary.BigEndian.Uint32(b[i:])), ID: addr4(b[i+4:]), AdvRtr: addr4(b[i+8:])})
	}
	return out, nil
}

func EncodeAck(hs []LSAHeader) []byte {
	b := make([]byte, 0, lsaHeaderLen*len(hs))
	for _, h := range hs {
		b = h.append(b)
	}
	return b
}

func DecodeAck(b []byte) ([]LSAHeader, error) {
	if len(b)%lsaHeaderLen != 0 {
		return nil, errors.New("bad ack length")
	}
	var out []LSAHeader
	for i := 0; i < len(b); i += lsaHeaderLen {
		out = append(out, decodeLSAHeader(b[i:]))
	}
	return out, nil
}

// ---- link state update ----

// EncodeLSU packs LSAs (already encoded, with their ages set).
func EncodeLSU(lsas [][]byte) []byte {
	n := 4
	for _, l := range lsas {
		n += len(l)
	}
	b := make([]byte, 4, n)
	binary.BigEndian.PutUint32(b, uint32(len(lsas)))
	for _, l := range lsas {
		b = append(b, l...)
	}
	return b
}

// DecodeLSU splits an update into its LSAs (each a copy).
func DecodeLSU(b []byte) ([][]byte, error) {
	if len(b) < 4 {
		return nil, errors.New("short LSU")
	}
	n := int(binary.BigEndian.Uint32(b))
	var out [][]byte
	b = b[4:]
	for i := 0; i < n; i++ {
		if len(b) < lsaHeaderLen {
			return nil, errors.New("truncated LSU")
		}
		l := int(binary.BigEndian.Uint16(b[18:]))
		if l < lsaHeaderLen || l > len(b) {
			return nil, fmt.Errorf("bad LSA length %d", l)
		}
		out = append(out, append([]byte(nil), b[:l]...))
		b = b[l:]
	}
	return out, nil
}

// ---- LSAs ----

type LSAHeader struct {
	Age      uint16
	Options  uint8
	Type     uint8
	ID       netip.Addr
	AdvRtr   netip.Addr
	Seq      uint32
	Checksum uint16
	Length   uint16
}

func (h LSAHeader) Key() LSAKey { return LSAKey{h.Type, h.ID, h.AdvRtr} }

func decodeLSAHeader(b []byte) LSAHeader {
	return LSAHeader{Age: binary.BigEndian.Uint16(b), Options: b[2], Type: b[3], ID: addr4(b[4:]), AdvRtr: addr4(b[8:]),
		Seq: binary.BigEndian.Uint32(b[12:]), Checksum: binary.BigEndian.Uint16(b[16:]), Length: binary.BigEndian.Uint16(b[18:])}
}

func (h LSAHeader) append(b []byte) []byte {
	var x [lsaHeaderLen]byte
	binary.BigEndian.PutUint16(x[:], h.Age)
	x[2], x[3] = h.Options, h.Type
	put4(x[4:], h.ID)
	put4(x[8:], h.AdvRtr)
	binary.BigEndian.PutUint32(x[12:], h.Seq)
	binary.BigEndian.PutUint16(x[16:], h.Checksum)
	binary.BigEndian.PutUint16(x[18:], h.Length)
	return append(b, x[:]...)
}

// LSAHeaderOf parses an encoded LSA's header.
func LSAHeaderOf(l []byte) LSAHeader { return decodeLSAHeader(l) }

// SetAge writes an LSA's age (the checksum doesn't cover it).
func SetAge(l []byte, age uint16) { binary.BigEndian.PutUint16(l, age) }

// LSAChecksumOK verifies an LSA's Fletcher checksum (over all but the age).
func LSAChecksumOK(l []byte) bool { return pkt.FletcherOK(l[2:]) }

// BuildLSA assembles an LSA: header fields from h (age, options, type, IDs,
// sequence) and the body, with the length and checksum filled in.
func BuildLSA(h LSAHeader, body []byte) []byte {
	h.Length = uint16(lsaHeaderLen + len(body))
	h.Checksum = 0
	b := h.append(make([]byte, 0, int(h.Length)))
	b = append(b, body...)
	binary.BigEndian.PutUint16(b[16:], pkt.Fletcher(b[2:], 14))
	return b
}

// Compare tells which of two instances of an LSA is newer (RFC 2328
// 13.1): 1 when a is, -1 when b is, 0 when they are the same instance.
func Compare(a, b LSAHeader) int {
	if sa, sb := int32(a.Seq), int32(b.Seq); sa != sb {
		if sa > sb {
			return 1
		}
		return -1
	}
	if a.Checksum != b.Checksum {
		if a.Checksum > b.Checksum {
			return 1
		}
		return -1
	}
	if (a.Age >= MaxAge) != (b.Age >= MaxAge) {
		if a.Age >= MaxAge {
			return 1
		}
		return -1
	}
	if d := int(a.Age) - int(b.Age); d > MaxAgeDiff || d < -MaxAgeDiff {
		if a.Age < b.Age {
			return 1
		}
		return -1
	}
	return 0
}

// RouterLink is one link of a router LSA.
type RouterLink struct {
	ID     netip.Addr `json:"id"`
	Data   netip.Addr `json:"data"`
	Type   uint8      `json:"-"`
	Metric uint16     `json:"metric"`
}

var linkTypeNames = map[uint8]string{LinkP2P: "point-to-point", LinkTransit: "transit", LinkStub: "stub", LinkVirtual: "virtual"}

type RouterLSA struct {
	Flags uint8
	Links []RouterLink
}

func (r RouterLSA) Encode() []byte {
	b := make([]byte, 4, 4+12*len(r.Links))
	b[0] = r.Flags
	binary.BigEndian.PutUint16(b[2:], uint16(len(r.Links)))
	for _, l := range r.Links {
		var x [12]byte
		put4(x[:], l.ID)
		put4(x[4:], l.Data)
		x[8] = l.Type
		binary.BigEndian.PutUint16(x[10:], l.Metric)
		b = append(b, x[:]...)
	}
	return b
}

func DecodeRouterLSA(body []byte) (*RouterLSA, error) {
	if len(body) < 4 {
		return nil, errors.New("short router LSA")
	}
	r := &RouterLSA{Flags: body[0]}
	n := int(binary.BigEndian.Uint16(body[2:]))
	b := body[4:]
	for i := 0; i < n; i++ {
		if len(b) < 12 {
			return nil, errors.New("truncated router LSA")
		}
		l := RouterLink{ID: addr4(b), Data: addr4(b[4:]), Type: b[8], Metric: binary.BigEndian.Uint16(b[10:])}
		tos := int(b[9])
		if len(b) < 12+4*tos {
			return nil, errors.New("truncated router LSA")
		}
		r.Links = append(r.Links, l)
		b = b[12+4*tos:]
	}
	return r, nil
}

type NetworkLSA struct {
	Mask    netip.Addr
	Routers []netip.Addr
}

func (n NetworkLSA) Encode() []byte {
	b := make([]byte, 4+4*len(n.Routers))
	put4(b, n.Mask)
	for i, r := range n.Routers {
		put4(b[4+4*i:], r)
	}
	return b
}

func DecodeNetworkLSA(body []byte) (*NetworkLSA, error) {
	if len(body) < 4 || len(body)%4 != 0 {
		return nil, errors.New("bad network LSA")
	}
	n := &NetworkLSA{Mask: addr4(body)}
	for i := 4; i < len(body); i += 4 {
		n.Routers = append(n.Routers, addr4(body[i:]))
	}
	return n, nil
}

// SummaryLSA is a type 3 or 4 LSA.
type SummaryLSA struct {
	Mask   netip.Addr
	Metric uint32
}

func DecodeSummaryLSA(body []byte) (*SummaryLSA, error) {
	if len(body) < 8 {
		return nil, errors.New("short summary LSA")
	}
	return &SummaryLSA{Mask: addr4(body), Metric: binary.BigEndian.Uint32(body[4:]) & 0xffffff}, nil
}

type ExternalLSA struct {
	Mask    netip.Addr
	E2      bool // type 2 metric
	Metric  uint32
	Forward netip.Addr
	Tag     uint32
}

func (e ExternalLSA) Encode() []byte {
	b := make([]byte, 16)
	put4(b, e.Mask)
	binary.BigEndian.PutUint32(b[4:], e.Metric&0xffffff)
	if e.E2 {
		b[4] |= 0x80
	}
	put4(b[8:], e.Forward)
	binary.BigEndian.PutUint32(b[12:], e.Tag)
	return b
}

func DecodeExternalLSA(body []byte) (*ExternalLSA, error) {
	if len(body) < 16 {
		return nil, errors.New("short external LSA")
	}
	return &ExternalLSA{Mask: addr4(body), E2: body[4]&0x80 != 0, Metric: binary.BigEndian.Uint32(body[4:]) & 0xffffff,
		Forward: addr4(body[8:]), Tag: binary.BigEndian.Uint32(body[12:])}, nil
}

// MaskOf is a prefix length's netmask.
func MaskOf(bits int) netip.Addr {
	m := uint32(0)
	if bits > 0 {
		m = ^uint32(0) << (32 - bits)
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], m)
	return netip.AddrFrom4(b)
}

// BitsOf is a netmask's prefix length (-1 if not contiguous).
func BitsOf(mask netip.Addr) int {
	m := binary.BigEndian.Uint32(mask.AsSlice())
	n := 0
	for m&0x80000000 != 0 {
		n++
		m <<= 1
	}
	if m != 0 {
		return -1
	}
	return n
}
