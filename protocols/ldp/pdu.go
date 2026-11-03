package ldp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// Wire format of RFC 5036.

const (
	Port    = 646
	version = 1

	pduHeaderLen  = 10 // version, length, LDP ID
	msgHeaderLen  = 8  // type, length, message ID
	maxPDUDefault = 4096
)

// Message types (section 3.7).
const (
	MsgNotification    uint16 = 0x0001
	MsgHello           uint16 = 0x0100
	MsgInit            uint16 = 0x0200
	MsgKeepAlive       uint16 = 0x0201
	MsgAddress         uint16 = 0x0300
	MsgAddressWithdraw uint16 = 0x0301
	MsgLabelMapping    uint16 = 0x0400
	MsgLabelRequest    uint16 = 0x0401
	MsgLabelWithdraw   uint16 = 0x0402
	MsgLabelRelease    uint16 = 0x0403
	MsgLabelAbort      uint16 = 0x0404
)

// TLV types (section 3.4).
const (
	TLVFEC           uint16 = 0x0100
	TLVAddressList   uint16 = 0x0101
	TLVGenericLabel  uint16 = 0x0200
	TLVStatus        uint16 = 0x0300
	TLVCommonHello   uint16 = 0x0400
	TLVIPv4Transport uint16 = 0x0401
	TLVConfigSeq     uint16 = 0x0402
	TLVCommonSession uint16 = 0x0500
)

// Status codes (section 3.9); fatal ones have the E bit.
const (
	StatusSuccess           uint32 = 0x00000000
	StatusBadLDPID          uint32 = 0x80000001
	StatusBadVersion        uint32 = 0x80000002
	StatusBadPDULen         uint32 = 0x80000003
	StatusUnknownMsg        uint32 = 0x00000004
	StatusBadMsgLen         uint32 = 0x80000005
	StatusUnknownTLV        uint32 = 0x00000006
	StatusBadTLVLen         uint32 = 0x80000007
	StatusMalformedTLV      uint32 = 0x80000008
	StatusHoldExpired       uint32 = 0x80000009
	StatusShutdown          uint32 = 0x8000000a
	StatusUnknownFEC        uint32 = 0x0000000c
	StatusNoRoute           uint32 = 0x0000000d
	StatusNoLabelResources  uint32 = 0x0000000e
	StatusRejectedNoHello   uint32 = 0x80000010
	StatusRejectedAdvMode   uint32 = 0x80000011
	StatusRejectedMaxPDU    uint32 = 0x80000012
	StatusKeepAliveExpired  uint32 = 0x80000014
	StatusMissingParam      uint32 = 0x00000016
	StatusUnsupportedAF     uint32 = 0x00000017
	StatusRejectedKeepAlive uint32 = 0x80000018
	StatusInternal          uint32 = 0x80000019
)

var statusNames = map[uint32]string{
	0: "success", 1: "bad LDP identifier", 2: "bad protocol version", 3: "bad PDU length", 4: "unknown message type",
	5: "bad message length", 6: "unknown TLV", 7: "bad TLV length", 8: "malformed TLV value", 9: "hold timer expired",
	0xa: "shutdown", 0xb: "loop detected", 0xc: "unknown FEC", 0xd: "no route", 0xe: "no label resources",
	0xf: "label resources available", 0x10: "session rejected: no hello", 0x11: "session rejected: advertisement mode",
	0x12: "session rejected: max PDU length", 0x13: "session rejected: label range", 0x14: "keepalive timer expired",
	0x15: "label request aborted", 0x16: "missing message parameters", 0x17: "unsupported address family",
	0x18: "session rejected: keepalive time", 0x19: "internal error",
}

// StatusText names a status code.
func StatusText(code uint32) string {
	c := code &^ 0xc0000000
	if n, ok := statusNames[c]; ok {
		return n
	}
	return fmt.Sprintf("status %#x", c)
}

// Fatal reports the status's E bit.
func Fatal(code uint32) bool { return code&0x80000000 != 0 }

// Reserved labels.
const (
	LabelExplicitNull uint32 = 0
	LabelImplicitNull uint32 = 3
	MinLabel          uint32 = 16
	MaxLabel          uint32 = 1<<20 - 1
)

// ID is an LDP identifier: an LSR ID and a label space.
type ID struct {
	LSR   netip.Addr
	Space uint16
}

func (i ID) String() string { return fmt.Sprintf("%s:%d", i.LSR, i.Space) }

// TLV is a raw TLV. Unknown is the U bit, Forward the F bit.
type TLV struct {
	Type    uint16
	Unknown bool
	Forward bool
	Value   []byte
}

// Message is one message; Unknown is its U bit.
type Message struct {
	Type    uint16
	Unknown bool
	ID      uint32
	TLVs    []TLV
}

func (m *Message) tlv(t uint16) *TLV {
	for i := range m.TLVs {
		if m.TLVs[i].Type == t {
			return &m.TLVs[i]
		}
	}
	return nil
}

// PDU is one PDU: an LDP ID and its messages.
type PDU struct {
	ID       ID
	Messages []Message
}

// PDULen reads the total size of the PDU that starts b (at least 4 bytes).
func PDULen(b []byte) int { return int(binary.BigEndian.Uint16(b[2:])) + 4 }

// DecodePDU parses one complete PDU.
func DecodePDU(b []byte) (*PDU, error) {
	if len(b) < pduHeaderLen {
		return nil, errors.New("short PDU")
	}
	if v := binary.BigEndian.Uint16(b); v != version {
		return nil, fmt.Errorf("protocol version %d", v)
	}
	n := PDULen(b)
	if n > len(b) || n < pduHeaderLen {
		return nil, fmt.Errorf("PDU length %d of %d", n, len(b))
	}
	p := &PDU{ID: ID{LSR: netip.AddrFrom4([4]byte(b[4:8])), Space: binary.BigEndian.Uint16(b[8:])}}
	for rest := b[pduHeaderLen:n]; len(rest) > 0; {
		if len(rest) < msgHeaderLen {
			return nil, errors.New("short message")
		}
		size := int(binary.BigEndian.Uint16(rest[2:])) + 4
		if size < msgHeaderLen || size > len(rest) {
			return nil, fmt.Errorf("message length %d of %d", size, len(rest))
		}
		t := binary.BigEndian.Uint16(rest)
		m := Message{Type: t &^ 0x8000, Unknown: t&0x8000 != 0, ID: binary.BigEndian.Uint32(rest[4:])}
		for tl := rest[msgHeaderLen:size]; len(tl) > 0; {
			if len(tl) < 4 {
				return nil, errors.New("short TLV")
			}
			vl := int(binary.BigEndian.Uint16(tl[2:]))
			if 4+vl > len(tl) {
				return nil, fmt.Errorf("TLV length %d of %d", vl, len(tl)-4)
			}
			h := binary.BigEndian.Uint16(tl)
			m.TLVs = append(m.TLVs, TLV{Type: h &^ 0xc000, Unknown: h&0x8000 != 0, Forward: h&0x4000 != 0, Value: tl[4 : 4+vl]})
			tl = tl[4+vl:]
		}
		p.Messages = append(p.Messages, m)
		rest = rest[size:]
	}
	return p, nil
}

// Encode returns the message's bytes.
func (m Message) Encode() []byte {
	b := make([]byte, msgHeaderLen)
	t := m.Type
	if m.Unknown {
		t |= 0x8000
	}
	binary.BigEndian.PutUint16(b, t)
	binary.BigEndian.PutUint32(b[4:], m.ID)
	for _, x := range m.TLVs {
		h := x.Type
		if x.Unknown {
			h |= 0x8000
		}
		if x.Forward {
			h |= 0x4000
		}
		b = binary.BigEndian.AppendUint16(b, h)
		b = binary.BigEndian.AppendUint16(b, uint16(len(x.Value)))
		b = append(b, x.Value...)
	}
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)-4))
	return b
}

// EncodePDU wraps encoded messages in a PDU.
func EncodePDU(id ID, msgs ...[]byte) []byte {
	b := make([]byte, pduHeaderLen)
	binary.BigEndian.PutUint16(b, version)
	a := id.LSR.As4()
	copy(b[4:], a[:])
	binary.BigEndian.PutUint16(b[8:], id.Space)
	for _, m := range msgs {
		b = append(b, m...)
	}
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)-4))
	return b
}

// ---- values ----

// HelloParams is the Common Hello Parameters TLV.
type HelloParams struct {
	Hold     uint16 // 0: the default, 0xffff: forever
	Targeted bool
	Request  bool // ask the peer to send targeted hellos back
}

func (h HelloParams) tlv() TLV {
	v := make([]byte, 4)
	binary.BigEndian.PutUint16(v, h.Hold)
	if h.Targeted {
		v[2] |= 0x80
	}
	if h.Request {
		v[2] |= 0x40
	}
	return TLV{Type: TLVCommonHello, Value: v}
}

func decodeHelloParams(v []byte) (HelloParams, error) {
	if len(v) != 4 {
		return HelloParams{}, errors.New("common hello parameters: bad length")
	}
	return HelloParams{Hold: binary.BigEndian.Uint16(v), Targeted: v[2]&0x80 != 0, Request: v[2]&0x40 != 0}, nil
}

// SessionParams is the Common Session Parameters TLV.
type SessionParams struct {
	KeepAlive uint16
	DoD       bool // downstream on demand, else unsolicited
	Loop      bool
	PathLimit uint8
	MaxPDU    uint16 // 0: 4096
	Receiver  ID
}

func (s SessionParams) tlv() TLV {
	v := make([]byte, 14)
	binary.BigEndian.PutUint16(v, version)
	binary.BigEndian.PutUint16(v[2:], s.KeepAlive)
	if s.DoD {
		v[4] |= 0x80
	}
	if s.Loop {
		v[4] |= 0x40
	}
	v[5] = s.PathLimit
	binary.BigEndian.PutUint16(v[6:], s.MaxPDU)
	a := s.Receiver.LSR.As4()
	copy(v[8:], a[:])
	binary.BigEndian.PutUint16(v[12:], s.Receiver.Space)
	return TLV{Type: TLVCommonSession, Value: v}
}

func decodeSessionParams(v []byte) (SessionParams, error) {
	if len(v) != 14 {
		return SessionParams{}, errors.New("common session parameters: bad length")
	}
	if binary.BigEndian.Uint16(v) != version {
		return SessionParams{}, errors.New("common session parameters: bad version")
	}
	return SessionParams{KeepAlive: binary.BigEndian.Uint16(v[2:]), DoD: v[4]&0x80 != 0, Loop: v[4]&0x40 != 0, PathLimit: v[5],
		MaxPDU: binary.BigEndian.Uint16(v[6:]), Receiver: ID{LSR: netip.AddrFrom4([4]byte(v[8:12])), Space: binary.BigEndian.Uint16(v[12:])}}, nil
}

// FECValue is a FEC TLV's elements: a wildcard, or prefixes. Other element types
// (pseudowires) are counted in Other and not understood.
type FECValue struct {
	Wildcard bool
	Prefixes []netip.Prefix
	Other    int
}

// fecTLV encodes prefix elements.
func fecTLV(ps ...netip.Prefix) TLV {
	var v []byte
	for _, p := range ps {
		v = append(v, 2, 0, 1, byte(p.Bits()))
		a := p.Masked().Addr().As4()
		v = append(v, a[:(p.Bits()+7)/8]...)
	}
	return TLV{Type: TLVFEC, Value: v}
}

func wildcardTLV() TLV { return TLV{Type: TLVFEC, Value: []byte{1}} }

func decodeFEC(v []byte) (FECValue, error) {
	var f FECValue
	for len(v) > 0 {
		switch v[0] {
		case 1:
			f.Wildcard = true
			v = v[1:]
		case 2:
			if len(v) < 4 {
				return f, errors.New("FEC prefix element: short")
			}
			fam, bits := binary.BigEndian.Uint16(v[1:]), int(v[3])
			n := (bits + 7) / 8
			if len(v) < 4+n {
				return f, errors.New("FEC prefix element: short address")
			}
			if fam == 1 {
				if bits > 32 {
					return f, fmt.Errorf("FEC prefix element: length %d", bits)
				}
				var a [4]byte
				copy(a[:], v[4:4+n])
				f.Prefixes = append(f.Prefixes, netip.PrefixFrom(netip.AddrFrom4(a), bits).Masked())
			} else {
				f.Other++
			}
			v = v[4+n:]
		default:
			// An element we don't know: its length isn't known either.
			f.Other++
			return f, nil
		}
	}
	return f, nil
}

func labelTLV(l uint32) TLV {
	return TLV{Type: TLVGenericLabel, Value: binary.BigEndian.AppendUint32(nil, l&MaxLabel)}
}

func decodeLabel(v []byte) (uint32, error) {
	if len(v) != 4 {
		return 0, errors.New("generic label: bad length")
	}
	return binary.BigEndian.Uint32(v) & MaxLabel, nil
}

func addressTLV(as []netip.Addr) TLV {
	v := []byte{0, 1}
	for _, a := range as {
		b := a.As4()
		v = append(v, b[:]...)
	}
	return TLV{Type: TLVAddressList, Value: v}
}

func decodeAddresses(v []byte) ([]netip.Addr, error) {
	if len(v) < 2 {
		return nil, errors.New("address list: short")
	}
	if binary.BigEndian.Uint16(v) != 1 {
		return nil, nil // another family
	}
	v = v[2:]
	if len(v)%4 != 0 {
		return nil, errors.New("address list: bad length")
	}
	var out []netip.Addr
	for ; len(v) > 0; v = v[4:] {
		out = append(out, netip.AddrFrom4([4]byte(v[:4])))
	}
	return out, nil
}

// StatusTLV is a Status TLV.
type StatusTLV struct {
	Code    uint32
	MsgID   uint32
	MsgType uint16
}

func (s StatusTLV) tlv() TLV {
	v := binary.BigEndian.AppendUint32(nil, s.Code)
	v = binary.BigEndian.AppendUint32(v, s.MsgID)
	v = binary.BigEndian.AppendUint16(v, s.MsgType)
	return TLV{Type: TLVStatus, Value: v}
}

func decodeStatus(v []byte) (StatusTLV, error) {
	if len(v) != 10 {
		return StatusTLV{}, errors.New("status: bad length")
	}
	return StatusTLV{Code: binary.BigEndian.Uint32(v), MsgID: binary.BigEndian.Uint32(v[4:]), MsgType: binary.BigEndian.Uint16(v[8:])}, nil
}

// MessageName names a message type.
func MessageName(t uint16) string {
	switch t {
	case MsgNotification:
		return "notification"
	case MsgHello:
		return "hello"
	case MsgInit:
		return "initialization"
	case MsgKeepAlive:
		return "keepalive"
	case MsgAddress:
		return "address"
	case MsgAddressWithdraw:
		return "address withdraw"
	case MsgLabelMapping:
		return "label mapping"
	case MsgLabelRequest:
		return "label request"
	case MsgLabelWithdraw:
		return "label withdraw"
	case MsgLabelRelease:
		return "label release"
	case MsgLabelAbort:
		return "label abort"
	}
	return fmt.Sprintf("message %#x", t)
}
