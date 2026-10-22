package ospf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// OSPFv3 LSAs (RFC 5340 A.4). Router and network LSAs carry topology only;
// addresses travel in link LSAs (per link) and intra-area-prefix LSAs.

const (
	LSA3Router      = 0x2001
	LSA3Network     = 0x2002
	LSA3InterPrefix = 0x2003
	LSA3InterRouter = 0x2004
	LSA3External    = 0x4005
	LSA3NSSA        = 0x2007
	LSA3Link        = 0x0008
	LSA3IntraPrefix = 0x2009

	// Prefix options.
	PrefixNU = 0x01
	PrefixLA = 0x02 // a local address (a /128 of the router)
	PrefixP  = 0x08
	PrefixDN = 0x10

	// AS-external flags.
	ExtE = 0x04 // type 2 metric
	ExtF = 0x02 // forwarding address present
	ExtT = 0x01 // route tag present

	options3 = OptV6 | OptE | OptR
)

var lsa3Names = map[uint16]string{LSA3Router: "router", LSA3Network: "network", LSA3InterPrefix: "inter-area-prefix",
	LSA3InterRouter: "inter-area-router", LSA3External: "external", LSA3NSSA: "nssa", LSA3Link: "link", LSA3IntraPrefix: "intra-area-prefix", LSA3Grace: "grace"}

func lsa3TypeName(t uint16) string {
	if n, ok := lsa3Names[t]; ok {
		return n
	}
	if n, ok := lsa3ExtNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type-%#04x", t)
}

func known3(t uint16) bool { _, ok := lsa3Names[t]; return ok }

// Prefix6 is an OSPFv3 address prefix with its options and metric.
type Prefix6 struct {
	Prefix  netip.Prefix
	Options uint8
	Metric  uint16 // intra-area-prefix (in external LSAs the metric is elsewhere)
}

func appendPrefix6(b []byte, p Prefix6, metric bool) []byte {
	n := (p.Prefix.Bits() + 31) / 32 * 4
	b = append(b, byte(p.Prefix.Bits()), p.Options)
	if metric {
		b = binary.BigEndian.AppendUint16(b, p.Metric)
	} else {
		b = append(b, 0, 0) // referenced LS type 0 (externals)
	}
	a := p.Prefix.Masked().Addr().As16()
	return append(b, a[:n]...)
}

// readPrefix6 parses one prefix; the third and fourth bytes are the
// metric (intra-area) or other data (externals, returned as aux).
func readPrefix6(b []byte) (Prefix6, uint16, []byte, error) {
	if len(b) < 4 {
		return Prefix6{}, 0, nil, errors.New("truncated prefix")
	}
	bits := int(b[0])
	n := (bits + 31) / 32 * 4
	if bits > 128 || len(b) < 4+n {
		return Prefix6{}, 0, nil, errors.New("bad prefix length")
	}
	var a [16]byte
	copy(a[:], b[4:4+n])
	p := Prefix6{Prefix: netip.PrefixFrom(netip.AddrFrom16(a), bits).Masked(), Options: b[1]}
	aux := binary.BigEndian.Uint16(b[2:])
	return p, aux, b[4+n:], nil
}

// Router3Link is one interface of an OSPFv3 router LSA.
type Router3Link struct {
	Type      uint8
	Metric    uint16
	IfID      uint32
	NbrIfID   uint32
	NbrRouter netip.Addr
}

type Router3LSA struct {
	Flags   uint8
	Options uint32
	Links   []Router3Link
}

func (r Router3LSA) Encode() []byte {
	b := make([]byte, 4, 4+16*len(r.Links))
	binary.BigEndian.PutUint32(b, r.Options&0xffffff)
	b[0] = r.Flags
	for _, l := range r.Links {
		var x [16]byte
		x[0] = l.Type
		binary.BigEndian.PutUint16(x[2:], l.Metric)
		binary.BigEndian.PutUint32(x[4:], l.IfID)
		binary.BigEndian.PutUint32(x[8:], l.NbrIfID)
		put4(x[12:], l.NbrRouter)
		b = append(b, x[:]...)
	}
	return b
}

func DecodeRouter3LSA(body []byte) (*Router3LSA, error) {
	if len(body) < 4 || (len(body)-4)%16 != 0 {
		return nil, errors.New("bad router LSA")
	}
	r := &Router3LSA{Flags: body[0], Options: binary.BigEndian.Uint32(body) & 0xffffff}
	for b := body[4:]; len(b) >= 16; b = b[16:] {
		r.Links = append(r.Links, Router3Link{Type: b[0], Metric: binary.BigEndian.Uint16(b[2:]), IfID: binary.BigEndian.Uint32(b[4:]),
			NbrIfID: binary.BigEndian.Uint32(b[8:]), NbrRouter: addr4(b[12:])})
	}
	return r, nil
}

type Network3LSA struct {
	Options uint32
	Routers []netip.Addr
}

func (n Network3LSA) Encode() []byte {
	b := make([]byte, 4+4*len(n.Routers))
	binary.BigEndian.PutUint32(b, n.Options&0xffffff)
	for i, r := range n.Routers {
		put4(b[4+4*i:], r)
	}
	return b
}

func DecodeNetwork3LSA(body []byte) (*Network3LSA, error) {
	if len(body) < 4 || len(body)%4 != 0 {
		return nil, errors.New("bad network LSA")
	}
	n := &Network3LSA{Options: binary.BigEndian.Uint32(body) & 0xffffff}
	for i := 4; i < len(body); i += 4 {
		n.Routers = append(n.Routers, addr4(body[i:]))
	}
	return n, nil
}

type LinkLSA struct {
	Priority  uint8
	Options   uint32
	LinkLocal netip.Addr
	Prefixes  []Prefix6
}

func (l LinkLSA) Encode() []byte {
	b := make([]byte, 24, 64)
	binary.BigEndian.PutUint32(b, l.Options&0xffffff)
	b[0] = l.Priority
	a := l.LinkLocal.As16()
	copy(b[4:], a[:])
	binary.BigEndian.PutUint32(b[20:], uint32(len(l.Prefixes)))
	for _, p := range l.Prefixes {
		p.Metric = 0
		b = appendPrefix6(b, p, true)
	}
	return b
}

func DecodeLinkLSA(body []byte) (*LinkLSA, error) {
	if len(body) < 24 {
		return nil, errors.New("short link LSA")
	}
	l := &LinkLSA{Priority: body[0], Options: binary.BigEndian.Uint32(body) & 0xffffff, LinkLocal: netip.AddrFrom16([16]byte(body[4:20]))}
	n := int(binary.BigEndian.Uint32(body[20:]))
	b := body[24:]
	for i := 0; i < n; i++ {
		p, _, rest, err := readPrefix6(b)
		if err != nil {
			return nil, err
		}
		l.Prefixes = append(l.Prefixes, p)
		b = rest
	}
	return l, nil
}

// IntraPrefixLSA lists prefixes of a router (referencing its router LSA)
// or of a transit network (referencing the DR's network LSA).
type IntraPrefixLSA struct {
	RefType   uint16
	RefID     netip.Addr
	RefAdvRtr netip.Addr
	Prefixes  []Prefix6
}

func (l IntraPrefixLSA) Encode() []byte {
	b := make([]byte, 12, 12+20*len(l.Prefixes))
	binary.BigEndian.PutUint16(b, uint16(len(l.Prefixes)))
	binary.BigEndian.PutUint16(b[2:], l.RefType)
	put4(b[4:], l.RefID)
	put4(b[8:], l.RefAdvRtr)
	for _, p := range l.Prefixes {
		b = appendPrefix6(b, p, true)
	}
	return b
}

func DecodeIntraPrefixLSA(body []byte) (*IntraPrefixLSA, error) {
	if len(body) < 12 {
		return nil, errors.New("short intra-area-prefix LSA")
	}
	l := &IntraPrefixLSA{RefType: binary.BigEndian.Uint16(body[2:]), RefID: addr4(body[4:]), RefAdvRtr: addr4(body[8:])}
	n := int(binary.BigEndian.Uint16(body))
	b := body[12:]
	for i := 0; i < n; i++ {
		p, m, rest, err := readPrefix6(b)
		if err != nil {
			return nil, err
		}
		p.Metric = m
		l.Prefixes = append(l.Prefixes, p)
		b = rest
	}
	return l, nil
}

type External3LSA struct {
	E2      bool
	Metric  uint32
	Prefix  Prefix6
	Forward netip.Addr // invalid: none
	Tag     uint32     // 0: none
}

func (e External3LSA) Encode() []byte {
	b := make([]byte, 4, 44)
	binary.BigEndian.PutUint32(b, e.Metric&0xffffff)
	flags := byte(0)
	if e.E2 {
		flags |= ExtE
	}
	if e.Forward.IsValid() {
		flags |= ExtF
	}
	if e.Tag != 0 {
		flags |= ExtT
	}
	b[0] = flags
	b = appendPrefix6(b, e.Prefix, false)
	if e.Forward.IsValid() {
		a := e.Forward.As16()
		b = append(b, a[:]...)
	}
	if e.Tag != 0 {
		b = binary.BigEndian.AppendUint32(b, e.Tag)
	}
	return b
}

func DecodeExternal3LSA(body []byte) (*External3LSA, error) {
	if len(body) < 8 {
		return nil, errors.New("short external LSA")
	}
	e := &External3LSA{E2: body[0]&ExtE != 0, Metric: binary.BigEndian.Uint32(body) & 0xffffff}
	p, _, rest, err := readPrefix6(body[4:])
	if err != nil {
		return nil, err
	}
	e.Prefix = p
	if body[0]&ExtF != 0 {
		if len(rest) < 16 {
			return nil, errors.New("truncated forwarding address")
		}
		e.Forward = netip.AddrFrom16([16]byte(rest[:16]))
		rest = rest[16:]
	}
	if body[0]&ExtT != 0 {
		if len(rest) < 4 {
			return nil, errors.New("truncated tag")
		}
		e.Tag = binary.BigEndian.Uint32(rest)
	}
	return e, nil
}

// InterPrefixLSA is a border router's summary of a prefix in another area.
type InterPrefixLSA struct {
	Metric uint32
	Prefix Prefix6
}

func (x InterPrefixLSA) Encode() []byte {
	b := binary.BigEndian.AppendUint32(make([]byte, 0, 24), x.Metric&0xffffff)
	return appendPrefix6(b, x.Prefix, false)
}

// InterRouterLSA is a border router's summary of an ASBR in another area.
type InterRouterLSA struct {
	Options uint32
	Metric  uint32
	Router  netip.Addr
}

func (x InterRouterLSA) Encode() []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint32(b, x.Options&0xffffff)
	binary.BigEndian.PutUint32(b[4:], x.Metric&0xffffff)
	put4(b[8:], x.Router)
	return b
}

func DecodeInterRouterLSA(body []byte) (*InterRouterLSA, error) {
	if len(body) < 12 {
		return nil, errors.New("short inter-area-router LSA")
	}
	return &InterRouterLSA{Options: binary.BigEndian.Uint32(body) & 0xffffff, Metric: binary.BigEndian.Uint32(body[4:]) & 0xffffff, Router: addr4(body[8:])}, nil
}

func DecodeInterPrefixLSA(body []byte) (*InterPrefixLSA, error) {
	if len(body) < 8 {
		return nil, errors.New("short inter-area-prefix LSA")
	}
	p, _, _, err := readPrefix6(body[4:])
	if err != nil {
		return nil, err
	}
	return &InterPrefixLSA{Metric: binary.BigEndian.Uint32(body) & 0xffffff, Prefix: p}, nil
}
