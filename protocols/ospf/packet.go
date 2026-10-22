package ospf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"github.com/ripnet/clabgen/internal/pkt"
)

// OSPF packets and LSAs: OSPFv2 (RFC 2328 appendix A) and OSPFv3 (RFC
// 5340 appendix A). Both share the packet types, the 20-byte LSA header
// layout (v2: options + 8-bit type, v3: 16-bit type), LSUs and acks; a
// codec picks the version's encoding of the rest.

const (
	ProtoOSPF = 89

	TypeHello = 1
	TypeDBD   = 2
	TypeLSR   = 3
	TypeLSU   = 4
	TypeAck   = 5

	headerLen    = 24 // OSPFv2
	header3Len   = 16 // OSPFv3
	lsaHeaderLen = 20

	// OSPFv2 LSA types.
	LSARouter      = 1
	LSANetwork     = 2
	LSASummary     = 3
	LSAASBRSummary = 4
	LSAExternal    = 5
	LSANSSA        = 7 // RFC 3101

	// Options (the low bits are the same in both versions).
	OptV6 = 0x01 // OSPFv3: take part in IPv6 routing
	OptE  = 0x02 // external routing capable (not a stub area)
	OptMC = 0x04
	OptNP = 0x08 // N in hellos (NSSA), P in type-7 LSA headers (translate)
	OptR  = 0x10 // OSPFv3: an active router
	OptO  = 0x40 // OSPFv2: opaque LSAs

	// DBD flags.
	DDInit   = 0x04
	DDMore   = 0x02
	DDMaster = 0x01

	// Router LSA flags and link types.
	RouterB  = 0x01 // area border router
	RouterE  = 0x02 // AS boundary router
	RouterV  = 0x04
	RouterNt = 0x10 // RFC 3101: an NSSA border router that always translates

	LinkP2P     = 1
	LinkTransit = 2
	LinkStub    = 3
	LinkVirtual = 4

	// Authentication types (OSPFv2).
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
	AllSPFRouters  = netip.MustParseAddr("224.0.0.5")
	AllDRouters    = netip.MustParseAddr("224.0.0.6")
	AllSPFRouters6 = netip.MustParseAddr("ff02::5")
	AllDRouters6   = netip.MustParseAddr("ff02::6")
)

var typeNames = map[uint8]string{TypeHello: "hello", TypeDBD: "dbd", TypeLSR: "lsr", TypeLSU: "lsu", TypeAck: "ack"}

// codec encodes the version-specific parts of packets and LSA headers.
type codec struct{ v3 bool }

func (c codec) version() byte {
	if c.v3 {
		return 3
	}
	return 2
}

func (c codec) headerLen() int {
	if c.v3 {
		return header3Len
	}
	return headerLen
}

// typeName names an LSA type for status.
func (c codec) typeName(t uint16) string {
	if c.v3 {
		return lsa3TypeName(t)
	}
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
	case LSANSSA:
		return "nssa"
	case LSAOpaqueLink:
		return "opaque-link"
	case LSAOpaqueArea:
		return "opaque-area"
	case LSAOpaqueAS:
		return "opaque-as"
	}
	return fmt.Sprintf("type-%d", t)
}

// known reports whether an LSA type may be exchanged. OSPFv3 takes every
// type (unknown ones are flooded by their scope bits).
func (c codec) known(t uint16) bool {
	if c.v3 {
		return true
	}
	return t >= LSARouter && t <= LSAExternal || t == LSANSSA || t >= 9 && t <= 11
}

// linkScope: LSAs flooded only on the link they came from (OSPFv2 opaque
// type 9; OSPFv3 link scope, which includes unknown types without the U
// bit).
func (c codec) linkScope(t uint16) bool {
	if !c.v3 {
		return t == 9
	}
	if t&0x8000 == 0 && !known3(t) {
		return true
	}
	return t&0x6000 == 0
}

// Header is the common packet header. Packet bodies follow it in the
// packet's Body.
type Header struct {
	Version    byte
	Type       uint8
	RouterID   netip.Addr
	AreaID     netip.Addr
	AuType     uint16  // OSPFv2
	Auth       [8]byte // OSPFv2 simple password; crypto: key ID, length, sequence
	InstanceID uint8   // OSPFv3
	Body       []byte  // after the header, up to the packet length
	Packet     []byte  // the whole packet (header + body), for auth checks
	Trailer    []byte  // OSPFv2 crypto: the digest after the packet
}

func addr4(b []byte) netip.Addr { return netip.AddrFrom4([4]byte(b[:4])) }

func put4(b []byte, a netip.Addr) {
	if a.Is4() {
		x := a.As4()
		copy(b, x[:])
	}
}

// id4 is a 32-bit value (an OSPFv3 interface ID or LSID) as a dotted ID.
func id4(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func u32(a netip.Addr) uint32 { return binary.BigEndian.Uint32(a.AsSlice()) }

// decodeHeader parses and length-checks a packet (without verifying the
// checksum or authentication).
func (c codec) decodeHeader(b []byte) (*Header, error) {
	hl := c.headerLen()
	if len(b) < hl {
		return nil, errors.New("short packet")
	}
	if b[0] != c.version() {
		return nil, fmt.Errorf("version %d", b[0])
	}
	n := int(binary.BigEndian.Uint16(b[2:]))
	if n < hl || n > len(b) {
		return nil, fmt.Errorf("bad length %d", n)
	}
	h := &Header{Version: b[0], Type: b[1], RouterID: addr4(b[4:]), AreaID: addr4(b[8:]), Body: b[hl:n], Packet: b[:n], Trailer: b[n:]}
	if c.v3 {
		h.InstanceID = b[14]
	} else {
		h.AuType = binary.BigEndian.Uint16(b[14:])
		copy(h.Auth[:], b[16:24])
	}
	return h, nil
}

// DecodeHeader parses an OSPFv2 packet header.
func DecodeHeader(b []byte) (*Header, error) { return codec{}.decodeHeader(b) }

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

// encodeHeader writes a packet: header + body. The OSPFv2 checksum and
// authentication are added by sign, the OSPFv3 checksum (over the IPv6
// pseudo-header) when the addresses are known.
func (c codec) encodeHeader(typ uint8, rid, area netip.Addr, instance uint8, body []byte) []byte {
	hl := c.headerLen()
	b := make([]byte, hl+len(body))
	b[0], b[1] = c.version(), typ
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	put4(b[4:], rid)
	put4(b[8:], area)
	if c.v3 {
		b[14] = instance
	}
	copy(b[hl:], body)
	return b
}

func encodeHeader(typ uint8, rid, area netip.Addr, body []byte) []byte {
	return codec{}.encodeHeader(typ, rid, area, 0, body)
}

// ---- hello ----

type Hello struct {
	IfID      uint32     // OSPFv3
	Mask      netip.Addr // OSPFv2
	Interval  uint16
	Options   uint32
	Priority  uint8
	Dead      uint32
	DR, BDR   netip.Addr // OSPFv2: interface addresses; OSPFv3: router IDs
	Neighbors []netip.Addr
}

var zero4 = netip.AddrFrom4([4]byte{})

func (c codec) encodeHello(h Hello) []byte {
	b := make([]byte, 20+4*len(h.Neighbors))
	if c.v3 {
		binary.BigEndian.PutUint32(b, h.IfID)
		binary.BigEndian.PutUint32(b[4:], h.Options&0xffffff)
		b[4] = h.Priority
		binary.BigEndian.PutUint16(b[8:], h.Interval)
		binary.BigEndian.PutUint16(b[10:], uint16(h.Dead))
	} else {
		put4(b, h.Mask)
		binary.BigEndian.PutUint16(b[4:], h.Interval)
		b[6], b[7] = byte(h.Options), h.Priority
		binary.BigEndian.PutUint32(b[8:], h.Dead)
	}
	put4(b[12:], h.DR)
	put4(b[16:], h.BDR)
	for i, n := range h.Neighbors {
		put4(b[20+4*i:], n)
	}
	return b
}

func (c codec) decodeHello(b []byte) (*Hello, error) {
	if len(b) < 20 || (len(b)-20)%4 != 0 {
		return nil, errors.New("bad hello length")
	}
	var h Hello
	if c.v3 {
		h = Hello{IfID: binary.BigEndian.Uint32(b), Priority: b[4], Options: binary.BigEndian.Uint32(b[4:]) & 0xffffff,
			Interval: binary.BigEndian.Uint16(b[8:]), Dead: uint32(binary.BigEndian.Uint16(b[10:]))}
	} else {
		h = Hello{Mask: addr4(b), Interval: binary.BigEndian.Uint16(b[4:]), Options: uint32(b[6]), Priority: b[7],
			Dead: binary.BigEndian.Uint32(b[8:])}
	}
	h.DR, h.BDR = addr4(b[12:]), addr4(b[16:])
	for i := 20; i < len(b); i += 4 {
		h.Neighbors = append(h.Neighbors, addr4(b[i:]))
	}
	return &h, nil
}

func (h Hello) Encode() []byte             { return codec{}.encodeHello(h) }
func DecodeHello(b []byte) (*Hello, error) { return codec{}.decodeHello(b) }

// ---- database description ----

type DBD struct {
	MTU     uint16
	Options uint32
	Flags   uint8
	Seq     uint32
	Headers []LSAHeader
}

func (c codec) dbdLen() int {
	if c.v3 {
		return 12
	}
	return 8
}

func (c codec) encodeDBD(d DBD) []byte {
	n := c.dbdLen()
	b := make([]byte, n, n+lsaHeaderLen*len(d.Headers))
	if c.v3 {
		binary.BigEndian.PutUint32(b, d.Options&0xffffff)
		binary.BigEndian.PutUint16(b[4:], d.MTU)
		b[7] = d.Flags
		binary.BigEndian.PutUint32(b[8:], d.Seq)
	} else {
		binary.BigEndian.PutUint16(b, d.MTU)
		b[2], b[3] = byte(d.Options), d.Flags
		binary.BigEndian.PutUint32(b[4:], d.Seq)
	}
	for _, h := range d.Headers {
		b = c.appendLSAHeader(b, h)
	}
	return b
}

func (c codec) decodeDBD(b []byte) (*DBD, error) {
	n := c.dbdLen()
	if len(b) < n || (len(b)-n)%lsaHeaderLen != 0 {
		return nil, errors.New("bad DBD length")
	}
	var d DBD
	if c.v3 {
		d = DBD{Options: binary.BigEndian.Uint32(b) & 0xffffff, MTU: binary.BigEndian.Uint16(b[4:]), Flags: b[7], Seq: binary.BigEndian.Uint32(b[8:])}
	} else {
		d = DBD{MTU: binary.BigEndian.Uint16(b), Options: uint32(b[2]), Flags: b[3], Seq: binary.BigEndian.Uint32(b[4:])}
	}
	for i := n; i < len(b); i += lsaHeaderLen {
		d.Headers = append(d.Headers, c.lsaHeader(b[i:]))
	}
	return &d, nil
}

func (d DBD) Encode() []byte           { return codec{}.encodeDBD(d) }
func DecodeDBD(b []byte) (*DBD, error) { return codec{}.decodeDBD(b) }

// ---- link state request / ack ----

// LSAKey identifies an LSA. Link is set for link-scoped LSAs: the
// interface (index + 1) whose link they belong to.
type LSAKey struct {
	Type   uint16
	ID     netip.Addr
	AdvRtr netip.Addr
	Link   int
	Area   int // area-scoped (and link-scoped): the area (index + 1); 0: AS scope
}

// EncodeLSR writes request entries; the type is a 32-bit field in v2 and
// reserved + 16-bit type in v3, the same bytes for the values used.
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
		out = append(out, LSAKey{Type: uint16(binary.BigEndian.Uint32(b[i:])), ID: addr4(b[i+4:]), AdvRtr: addr4(b[i+8:])})
	}
	return out, nil
}

func (c codec) encodeAck(hs []LSAHeader) []byte {
	b := make([]byte, 0, lsaHeaderLen*len(hs))
	for _, h := range hs {
		b = c.appendLSAHeader(b, h)
	}
	return b
}

func (c codec) decodeAck(b []byte) ([]LSAHeader, error) {
	if len(b)%lsaHeaderLen != 0 {
		return nil, errors.New("bad ack length")
	}
	var out []LSAHeader
	for i := 0; i < len(b); i += lsaHeaderLen {
		out = append(out, c.lsaHeader(b[i:]))
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
	Options  uint8 // OSPFv2
	Type     uint16
	ID       netip.Addr
	AdvRtr   netip.Addr
	Seq      uint32
	Checksum uint16
	Length   uint16
}

// Key is the LSA's identity; area- and link-scoped LSAs also need their
// area and link set.
func (h LSAHeader) Key() LSAKey { return LSAKey{Type: h.Type, ID: h.ID, AdvRtr: h.AdvRtr} }

func (c codec) lsaHeader(b []byte) LSAHeader {
	h := LSAHeader{Age: binary.BigEndian.Uint16(b), ID: addr4(b[4:]), AdvRtr: addr4(b[8:]),
		Seq: binary.BigEndian.Uint32(b[12:]), Checksum: binary.BigEndian.Uint16(b[16:]), Length: binary.BigEndian.Uint16(b[18:])}
	if c.v3 {
		h.Type = binary.BigEndian.Uint16(b[2:])
	} else {
		h.Options, h.Type = b[2], uint16(b[3])
	}
	return h
}

func (c codec) appendLSAHeader(b []byte, h LSAHeader) []byte {
	var x [lsaHeaderLen]byte
	binary.BigEndian.PutUint16(x[:], h.Age)
	if c.v3 {
		binary.BigEndian.PutUint16(x[2:], h.Type)
	} else {
		x[2], x[3] = h.Options, byte(h.Type)
	}
	put4(x[4:], h.ID)
	put4(x[8:], h.AdvRtr)
	binary.BigEndian.PutUint32(x[12:], h.Seq)
	binary.BigEndian.PutUint16(x[16:], h.Checksum)
	binary.BigEndian.PutUint16(x[18:], h.Length)
	return append(b, x[:]...)
}

// buildLSA assembles an LSA: header fields from h (age, options, type,
// IDs, sequence) and the body, with the length and checksum filled in.
func (c codec) buildLSA(h LSAHeader, body []byte) []byte {
	h.Length = uint16(lsaHeaderLen + len(body))
	h.Checksum = 0
	b := c.appendLSAHeader(make([]byte, 0, int(h.Length)), h)
	b = append(b, body...)
	binary.BigEndian.PutUint16(b[16:], pkt.Fletcher(b[2:], 14))
	return b
}

// OSPFv2 forms.
func LSAHeaderOf(l []byte) LSAHeader           { return codec{}.lsaHeader(l) }
func BuildLSA(h LSAHeader, body []byte) []byte { return codec{}.buildLSA(h, body) }
func EncodeAck(hs []LSAHeader) []byte          { return codec{}.encodeAck(hs) }
func DecodeAck(b []byte) ([]LSAHeader, error)  { return codec{}.decodeAck(b) }

// SetAge writes an LSA's age (the checksum doesn't cover it).
func SetAge(l []byte, age uint16) { binary.BigEndian.PutUint16(l, age) }

// LSAChecksumOK verifies an LSA's Fletcher checksum (over all but the age).
func LSAChecksumOK(l []byte) bool { return pkt.FletcherOK(l[2:]) }

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

func (x SummaryLSA) Encode() []byte {
	b := make([]byte, 8)
	put4(b, x.Mask)
	binary.BigEndian.PutUint32(b[4:], x.Metric&0xffffff)
	return b
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
