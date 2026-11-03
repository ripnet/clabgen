package rsvp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
)

// Message types (RFC 2205, RFC 3209).
const (
	MsgPath     uint8 = 1
	MsgResv     uint8 = 2
	MsgPathErr  uint8 = 3
	MsgResvErr  uint8 = 4
	MsgPathTear uint8 = 5
	MsgResvTear uint8 = 6
	MsgHello    uint8 = 20
)

// Object classes.
const (
	classSession     uint8 = 1
	classHop         uint8 = 3
	classTimeValues  uint8 = 5
	classErrorSpec   uint8 = 6
	classStyle       uint8 = 8
	classFlowSpec    uint8 = 9
	classFilterSpec  uint8 = 10
	classSenderTempl uint8 = 11
	classSenderTSpec uint8 = 12
	classLabel       uint8 = 16
	classLabelReq    uint8 = 19
	classERO         uint8 = 20
	classRRO         uint8 = 21
	classHello       uint8 = 22
	classFastReroute uint8 = 205
	classSessionAttr uint8 = 207
)

// ProtoRSVP is RSVP's IP protocol number.
const ProtoRSVP uint8 = 46

// Reservation styles.
const (
	StyleFF uint32 = 0x0a
	StyleSE uint32 = 0x12
)

// SESSION_ATTRIBUTE flags.
const (
	AttrLocalProtection uint8 = 0x01
	AttrLabelRecording  uint8 = 0x02
	AttrSE              uint8 = 0x04
	AttrBandwidthProt   uint8 = 0x08
	AttrNodeProtection  uint8 = 0x10
)

// RECORD_ROUTE IPv4 subobject flags.
const (
	RRProtectionAvail uint8 = 0x01
	RRProtectionInUse uint8 = 0x02
	RRBandwidthProt   uint8 = 0x04
	RRNodeProt        uint8 = 0x08
)

// ERROR_SPEC flags, codes and values.
const (
	ErrFlagPathStateRemoved uint8 = 0x04

	ErrAdmission    uint8 = 1
	ErrPolicy       uint8 = 2
	ErrNoPath       uint8 = 3
	ErrNoSender     uint8 = 4
	ErrUnknownStyle uint8 = 6
	ErrPreempted    uint8 = 12
	ErrRouting      uint8 = 24
	ErrNotify       uint8 = 25

	ErrValNoRoute       uint16 = 5
	ErrValBadL3PID      uint16 = 10
	ErrValLocalRepaired uint16 = 3
)

// Reserved labels.
const (
	LabelExplicitNull uint32 = 0
	LabelImplicitNull uint32 = 3
	MinLabel          uint32 = 16
	MaxLabel          uint32 = 1<<20 - 1
)

// SessionID is the SESSION object of an LSP tunnel (RFC 3209 section 4.6.1).
type SessionID struct {
	To       netip.Addr
	TunnelID uint16
	ExtID    netip.Addr
}

func (s SessionID) String() string {
	return fmt.Sprintf("%s tunnel %d from %s", s.To, s.TunnelID, s.ExtID)
}

// SenderID is a SENDER_TEMPLATE or FILTER_SPEC.
type SenderID struct {
	From  netip.Addr
	LSPID uint16
}

// Hop is an RSVP_HOP object.
type Hop struct {
	Addr netip.Addr
	LIH  uint32
}

// ErrorSpec is an ERROR_SPEC object.
type ErrorSpec struct {
	Node  netip.Addr
	Flags uint8
	Code  uint8
	Value uint16
}

// TSpec is an IntServ token bucket (bytes per second).
type TSpec struct {
	Rate      float32
	Bucket    float32
	Peak      float32
	MinPolice uint32
	MaxPacket uint32
}

// Attr is a SESSION_ATTRIBUTE object.
type Attr struct {
	Setup, Hold uint8
	Flags       uint8
	Name        string
}

// FRR is a FAST_REROUTE object (RFC 4090).
type FRR struct {
	Setup, Hold, HopLimit uint8
	Flags                 uint8 // 1: one-to-one, 2: facility
	Bandwidth             float32
}

// EROHop is an EXPLICIT_ROUTE subobject.
type EROHop struct {
	Addr  netip.Addr
	Bits  uint8
	Loose bool
}

// RROHop is a RECORD_ROUTE hop: an address, with the label it asks for.
type RROHop struct {
	Addr     netip.Addr
	Flags    uint8
	Label    uint32
	HasLabel bool
}

// Flow is one sender of a Resv: its filter, label and recorded route.
type Flow struct {
	Sender SenderID
	Label  uint32
	HasLbl bool
	RRO    []RROHop
	HasRRO bool
}

// Message is a decoded RSVP message, with the objects clabgen uses.
type Message struct {
	Type    uint8
	SendTTL uint8

	Session  SessionID
	HasSess  bool
	Hop      Hop
	HasHop   bool
	RefreshM uint32 // refresh period in ms
	Error    *ErrorSpec
	Style    uint32
	Sender   SenderID
	HasSndr  bool
	TSpec    *TSpec
	LabelReq uint16 // L3PID; 0: no LABEL_REQUEST
	HasLReq  bool
	ERO      []EROHop
	HasERO   bool
	RRO      []RROHop
	HasRRO   bool
	Attr     *Attr
	FRR      *FRR
	Flows    []Flow // Resv, ResvTear, ResvErr
	HelloAck bool
	HasHello bool
	SrcInst  uint32
	DstInst  uint32
	// Mandatory-looking objects we couldn't parse (their class numbers), for
	// a useful error.
	UnknownCType uint8
}

var errShort = errors.New("short RSVP message")

// Decode parses an RSVP message and checks its checksum.
func Decode(b []byte) (*Message, error) {
	if len(b) < 8 {
		return nil, errShort
	}
	if b[0]>>4 != 1 {
		return nil, fmt.Errorf("RSVP version %d", b[0]>>4)
	}
	n := int(binary.BigEndian.Uint16(b[6:]))
	if n < 8 || n > len(b) {
		return nil, errShort
	}
	b = b[:n]
	if binary.BigEndian.Uint16(b[2:]) != 0 && checksum(b) != 0 {
		return nil, errors.New("bad RSVP checksum")
	}
	m := &Message{Type: b[1], SendTTL: b[4]}
	rest := b[8:]
	var flow *Flow
	for len(rest) > 0 {
		if len(rest) < 4 {
			return nil, errShort
		}
		l := int(binary.BigEndian.Uint16(rest))
		if l < 4 || l%4 != 0 || l > len(rest) {
			return nil, fmt.Errorf("bad RSVP object length %d", l)
		}
		class, ctype, v := rest[2], rest[3], rest[4:l]
		rest = rest[l:]
		switch class {
		case classSession:
			if ctype != 7 {
				m.UnknownCType = class
				continue
			}
			if len(v) != 12 {
				return nil, errors.New("bad SESSION")
			}
			m.Session = SessionID{To: addr4(v), TunnelID: binary.BigEndian.Uint16(v[6:]), ExtID: addr4(v[8:])}
			m.HasSess = true
		case classHop:
			if ctype != 1 || len(v) != 8 {
				m.UnknownCType = class
				continue
			}
			m.Hop, m.HasHop = Hop{addr4(v), binary.BigEndian.Uint32(v[4:])}, true
		case classTimeValues:
			if len(v) == 4 {
				m.RefreshM = binary.BigEndian.Uint32(v)
			}
		case classErrorSpec:
			if ctype == 1 && len(v) == 8 {
				m.Error = &ErrorSpec{Node: addr4(v), Flags: v[4], Code: v[5], Value: binary.BigEndian.Uint16(v[6:])}
			}
		case classStyle:
			if len(v) == 4 {
				m.Style = binary.BigEndian.Uint32(v) & 0xffffff
			}
		case classFilterSpec:
			if ctype != 7 || len(v) != 8 {
				m.UnknownCType = class
				continue
			}
			m.Flows = append(m.Flows, Flow{Sender: SenderID{addr4(v), binary.BigEndian.Uint16(v[6:])}})
			flow = &m.Flows[len(m.Flows)-1]
		case classSenderTempl:
			if ctype != 7 || len(v) != 8 {
				m.UnknownCType = class
				continue
			}
			m.Sender, m.HasSndr = SenderID{addr4(v), binary.BigEndian.Uint16(v[6:])}, true
		case classSenderTSpec:
			if ctype == 2 && len(v) >= 32 {
				m.TSpec = &TSpec{Rate: f32(v[12:]), Bucket: f32(v[16:]), Peak: f32(v[20:]), MinPolice: binary.BigEndian.Uint32(v[24:]), MaxPacket: binary.BigEndian.Uint32(v[28:])}
			}
		case classLabel:
			if ctype != 1 || len(v) != 4 {
				m.UnknownCType = class
				continue
			}
			if flow != nil {
				flow.Label, flow.HasLbl = binary.BigEndian.Uint32(v)&MaxLabel, true
			}
		case classLabelReq:
			if ctype != 1 || len(v) != 4 {
				m.UnknownCType = class
				continue
			}
			m.LabelReq, m.HasLReq = binary.BigEndian.Uint16(v[2:]), true
		case classERO:
			if ctype != 1 {
				continue
			}
			ero, err := decodeERO(v)
			if err != nil {
				return nil, err
			}
			m.ERO, m.HasERO = ero, true
		case classRRO:
			if ctype != 1 {
				continue
			}
			rro, err := decodeRRO(v)
			if err != nil {
				return nil, err
			}
			if flow != nil {
				flow.RRO, flow.HasRRO = rro, true
			} else {
				m.RRO, m.HasRRO = rro, true
			}
		case classHello:
			if len(v) == 8 && (ctype == 1 || ctype == 2) {
				m.HasHello, m.HelloAck = true, ctype == 2
				m.SrcInst, m.DstInst = binary.BigEndian.Uint32(v), binary.BigEndian.Uint32(v[4:])
			}
		case classSessionAttr:
			if ctype == 7 && len(v) >= 4 && int(v[3]) <= len(v)-4 {
				m.Attr = &Attr{Setup: v[0], Hold: v[1], Flags: v[2], Name: string(v[4 : 4+v[3]])}
			}
		case classFastReroute:
			if ctype == 1 && len(v) >= 8 {
				m.FRR = &FRR{Setup: v[0], Hold: v[1], HopLimit: v[2], Flags: v[3], Bandwidth: f32(v[4:])}
			}
		}
	}
	return m, nil
}

func addr4(b []byte) netip.Addr { return netip.AddrFrom4([4]byte(b[:4])) }

func f32(b []byte) float32 { return math.Float32frombits(binary.BigEndian.Uint32(b)) }

func decodeERO(v []byte) ([]EROHop, error) {
	var out []EROHop
	for len(v) > 0 {
		if len(v) < 2 || v[1] < 2 || int(v[1]) > len(v) {
			return nil, errors.New("bad EXPLICIT_ROUTE")
		}
		t, l := v[0]&0x7f, int(v[1])
		if t == 1 && l == 8 {
			out = append(out, EROHop{Addr: addr4(v[2:]), Bits: v[6], Loose: v[0]&0x80 != 0})
		}
		v = v[l:]
	}
	return out, nil
}

// decodeRRO reads hops. A label subobject follows the address it belongs to,
// and belongs to the same node.
func decodeRRO(v []byte) ([]RROHop, error) {
	var out []RROHop
	for len(v) > 0 {
		if len(v) < 2 || v[1] < 2 || int(v[1]) > len(v) {
			return nil, errors.New("bad RECORD_ROUTE")
		}
		t, l := v[0], int(v[1])
		switch {
		case t == 1 && l == 8:
			out = append(out, RROHop{Addr: addr4(v[2:]), Flags: v[7]})
		case t == 3 && l == 8 && len(out) > 0:
			out[len(out)-1].Label = binary.BigEndian.Uint32(v[4:]) & MaxLabel
			out[len(out)-1].HasLabel = true
		}
		v = v[l:]
	}
	return out, nil
}

func checksum(b []byte) uint16 {
	var sum uint32
	for len(b) >= 2 {
		sum += uint32(b[0])<<8 | uint32(b[1])
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// ---- encoding ----

type writer struct{ b []byte }

func (w *writer) obj(class, ctype uint8, v []byte) {
	if len(v)%4 != 0 {
		panic("RSVP object not padded")
	}
	var h [4]byte
	binary.BigEndian.PutUint16(h[:], uint16(4+len(v)))
	h[2], h[3] = class, ctype
	w.b = append(append(w.b, h[:]...), v...)
}

func be32(x uint32) []byte { return binary.BigEndian.AppendUint32(nil, x) }

func (w *writer) session(s SessionID) {
	v := s.To.AsSlice()
	v = append(v, 0, 0)
	v = binary.BigEndian.AppendUint16(v, s.TunnelID)
	w.obj(classSession, 7, append(v, s.ExtID.AsSlice()...))
}

func (w *writer) hop(h Hop) {
	w.obj(classHop, 1, append(h.Addr.AsSlice(), be32(h.LIH)...))
}

func senderValue(s SenderID) []byte {
	v := s.From.AsSlice()
	v = append(v, 0, 0)
	return binary.BigEndian.AppendUint16(v, s.LSPID)
}

func f32be(x float32) []byte { return be32(math.Float32bits(x)) }

// tspecValue is the IntServ body of a SENDER_TSPEC (RFC 2210).
func tspecValue(t TSpec) []byte {
	v := be32(7)                    // version 0, 7 words
	v = append(v, be32(1<<24|6)...) // service 1, 6 words
	v = append(v, be32(127<<24|5)...)
	v = append(v, f32be(t.Rate)...)
	v = append(v, f32be(t.Bucket)...)
	v = append(v, f32be(t.Peak)...)
	v = append(v, be32(t.MinPolice)...)
	return append(v, be32(t.MaxPacket)...)
}

// flowSpecValue is a controlled-load FLOWSPEC with the sender's TSpec.
func flowSpecValue(t TSpec) []byte {
	v := be32(7)
	v = append(v, be32(5<<24|6)...) // controlled load, 6 words
	v = append(v, be32(127<<24|5)...)
	v = append(v, f32be(t.Rate)...)
	v = append(v, f32be(t.Bucket)...)
	v = append(v, f32be(t.Peak)...)
	v = append(v, be32(t.MinPolice)...)
	return append(v, be32(t.MaxPacket)...)
}

func eroValue(hops []EROHop) []byte {
	var v []byte
	for _, h := range hops {
		t := uint8(1)
		if h.Loose {
			t |= 0x80
		}
		v = append(v, t, 8)
		v = append(v, h.Addr.AsSlice()...)
		bits := h.Bits
		if bits == 0 {
			bits = 32
		}
		v = append(v, bits, 0)
	}
	return v
}

func rroValue(hops []RROHop) []byte {
	var v []byte
	for _, h := range hops {
		v = append(v, 1, 8)
		v = append(v, h.Addr.AsSlice()...)
		v = append(v, 32, h.Flags)
		if h.HasLabel {
			v = append(v, 3, 8, 1, 1) // global label, RSVP label C-type
			v = append(v, be32(h.Label)...)
		}
	}
	return v
}

func attrValue(a Attr) []byte {
	name := a.Name
	if len(name) > 252 {
		name = name[:252]
	}
	v := []byte{a.Setup, a.Hold, a.Flags, uint8(len(name))}
	v = append(v, name...)
	for len(v)%4 != 0 {
		v = append(v, 0)
	}
	return v
}

func frrValue(f FRR) []byte {
	v := []byte{f.Setup, f.Hold, f.HopLimit, f.Flags}
	v = append(v, f32be(f.Bandwidth)...)
	return append(v, make([]byte, 12)...) // include-any, exclude-any, include-all
}

// finish adds the common header. The objects go in the RFC 3209 order.
func finish(typ uint8, ttl uint8, objs []byte) []byte {
	b := make([]byte, 8+len(objs))
	b[0] = 1 << 4
	b[1] = typ
	b[4] = ttl
	binary.BigEndian.PutUint16(b[6:], uint16(len(b)))
	copy(b[8:], objs)
	binary.BigEndian.PutUint16(b[2:], checksum(b))
	return b
}

// PathMsg is what a head end signals.
type PathMsg struct {
	Session   SessionID
	Hop       Hop
	RefreshMs uint32
	ERO       []EROHop
	Attr      Attr
	FRR       *FRR
	Sender    SenderID
	TSpec     TSpec
	RRO       []RROHop
	Record    bool
}

// Encode builds the Path message.
func (p PathMsg) Encode(ttl uint8) []byte {
	var w writer
	w.session(p.Session)
	w.hop(p.Hop)
	w.obj(classTimeValues, 1, be32(p.RefreshMs))
	if len(p.ERO) > 0 {
		w.obj(classERO, 1, eroValue(p.ERO))
	}
	w.obj(classLabelReq, 1, []byte{0, 0, 0x08, 0x00})
	w.obj(classSessionAttr, 7, attrValue(p.Attr))
	if p.FRR != nil {
		w.obj(classFastReroute, 1, frrValue(*p.FRR))
	}
	w.obj(classSenderTempl, 7, senderValue(p.Sender))
	w.obj(classSenderTSpec, 2, tspecValue(p.TSpec))
	if p.Record {
		w.obj(classRRO, 1, rroValue(p.RRO))
	}
	return finish(MsgPath, ttl, w.b)
}

// ResvMsg answers Path state with labels.
type ResvMsg struct {
	Session   SessionID
	Hop       Hop
	RefreshMs uint32
	Style     uint32
	TSpec     TSpec
	Flows     []Flow
}

// Encode builds the Resv message: one FLOWSPEC with its senders for shared
// explicit, a FLOWSPEC per sender for fixed filter.
func (r ResvMsg) Encode() []byte {
	var w writer
	w.session(r.Session)
	w.hop(r.Hop)
	w.obj(classTimeValues, 1, be32(r.RefreshMs))
	w.obj(classStyle, 1, be32(r.Style))
	for i, f := range r.Flows {
		if r.Style != StyleSE || i == 0 {
			w.obj(classFlowSpec, 2, flowSpecValue(r.TSpec))
		}
		w.obj(classFilterSpec, 7, senderValue(f.Sender))
		w.obj(classLabel, 1, be32(f.Label))
		if f.HasRRO {
			w.obj(classRRO, 1, rroValue(f.RRO))
		}
	}
	return finish(MsgResv, 255, w.b)
}

// PathTearMsg ends a sender's Path state.
func PathTearMsg(s SessionID, h Hop, sender SenderID, ts TSpec) []byte {
	var w writer
	w.session(s)
	w.hop(h)
	w.obj(classSenderTempl, 7, senderValue(sender))
	w.obj(classSenderTSpec, 2, tspecValue(ts))
	return finish(MsgPathTear, 255, w.b)
}

// ResvTearMsg ends reservations.
func ResvTearMsg(s SessionID, h Hop, style uint32, senders []SenderID) []byte {
	var w writer
	w.session(s)
	w.hop(h)
	w.obj(classStyle, 1, be32(style))
	for _, x := range senders {
		w.obj(classFilterSpec, 7, senderValue(x))
	}
	return finish(MsgResvTear, 255, w.b)
}

func errorValue(e ErrorSpec) []byte {
	v := e.Node.AsSlice()
	v = append(v, e.Flags, e.Code)
	return binary.BigEndian.AppendUint16(v, e.Value)
}

// PathErrMsg reports an error toward the sender.
func PathErrMsg(s SessionID, e ErrorSpec, sender SenderID, ts TSpec) []byte {
	var w writer
	w.session(s)
	w.obj(classErrorSpec, 1, errorValue(e))
	w.obj(classSenderTempl, 7, senderValue(sender))
	w.obj(classSenderTSpec, 2, tspecValue(ts))
	return finish(MsgPathErr, 255, w.b)
}

// ResvErrMsg reports an error toward the receiver.
func ResvErrMsg(s SessionID, h Hop, e ErrorSpec, style uint32, sender SenderID, ts TSpec) []byte {
	var w writer
	w.session(s)
	w.hop(h)
	w.obj(classErrorSpec, 1, errorValue(e))
	w.obj(classStyle, 1, be32(style))
	w.obj(classFlowSpec, 2, flowSpecValue(ts))
	w.obj(classFilterSpec, 7, senderValue(sender))
	return finish(MsgResvErr, 255, w.b)
}

// HelloMsg is a hello request or ack (RFC 3209 section 5).
func HelloMsg(ack bool, src, dst uint32) []byte {
	var w writer
	ct := uint8(1)
	if ack {
		ct = 2
	}
	w.obj(classHello, ct, append(be32(src), be32(dst)...))
	return finish(MsgHello, 1, w.b)
}
