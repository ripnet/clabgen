// Package pkt holds the low-level frame encoding shared by all protocols.
package pkt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

const (
	TPIDDot1Q  uint16 = 0x8100
	TPIDDot1AD uint16 = 0x88a8
	TPIDQinQ   uint16 = 0x9100

	EtherTypeCFM uint16 = 0x8902

	// EtherTypeLLC stands for an IEEE 802.3 frame whose type field is a
	// length (< 0x600), i.e. one carrying an LLC header (IS-IS, STP, …).
	// Decode reports such frames with this value and the payload cut to
	// the length; Encode writes the payload length in its place.
	EtherTypeLLC uint16 = 0
)

// MAC is a 48-bit Ethernet address. It is a value type so it can be used as
// a map key and compared with ==.
type MAC [6]byte

func ParseMAC(s string) (MAC, error) {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return MAC{}, err
	}
	if len(hw) != 6 {
		return MAC{}, fmt.Errorf("not a 48-bit MAC: %q", s)
	}
	var m MAC
	copy(m[:], hw)
	return m, nil
}

func MACFrom(hw net.HardwareAddr) MAC {
	var m MAC
	copy(m[:], hw)
	return m
}

func (m MAC) String() string               { return net.HardwareAddr(m[:]).String() }
func (m MAC) IsMulticast() bool            { return m[0]&0x01 != 0 }
func (m MAC) IsZero() bool                 { return m == MAC{} }
func (m MAC) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

func (m *MAC) UnmarshalText(b []byte) error {
	v, err := ParseMAC(string(b))
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// VLANTag is one 802.1Q/802.1ad tag.
type VLANTag struct {
	TPID uint16 `json:"tpid,omitempty"`
	VID  uint16 `json:"vid"`
	PCP  uint8  `json:"pcp,omitempty"`
	DEI  bool   `json:"dei,omitempty"`
}

func (t VLANTag) tpid() uint16 {
	if t.TPID == 0 {
		return TPIDDot1Q
	}
	return t.TPID
}

func (t VLANTag) tci() uint16 {
	v := uint16(t.PCP&7)<<13 | t.VID&0x0fff
	if t.DEI {
		v |= 1 << 12
	}
	return v
}

// Frame is a decoded Ethernet frame. Payload aliases the receive buffer
// for frames produced by Decode, so handlers must copy it if they keep it.
type Frame struct {
	Dst, Src  MAC
	Tags      []VLANTag // outermost first
	EtherType uint16
	Payload   []byte

	// Set on receive. Local: sent by another device on the same port of
	// this agent, not received from the wire.
	Port   string
	RxTime time.Time
	Local  bool
}

func IsTPID(v uint16) bool { return v == TPIDDot1Q || v == TPIDDot1AD || v == TPIDQinQ }

var ErrShort = errors.New("frame too short")

// Decode parses an Ethernet header and any number of VLAN tags.
func Decode(b []byte) (*Frame, error) {
	if len(b) < 14 {
		return nil, ErrShort
	}
	f := &Frame{}
	copy(f.Dst[:], b[0:6])
	copy(f.Src[:], b[6:12])
	off := 12
	for {
		if len(b) < off+2 {
			return nil, ErrShort
		}
		et := binary.BigEndian.Uint16(b[off:])
		if et < 0x600 {
			off += 2
			if int(et) > len(b)-off || et == 0 {
				return nil, ErrShort
			}
			f.EtherType, f.Payload = EtherTypeLLC, b[off:off+int(et)]
			return f, nil
		}
		if !IsTPID(et) {
			f.EtherType = et
			off += 2
			break
		}
		if len(b) < off+4 {
			return nil, ErrShort
		}
		tci := binary.BigEndian.Uint16(b[off+2:])
		f.Tags = append(f.Tags, VLANTag{TPID: et, VID: tci & 0x0fff, PCP: uint8(tci >> 13), DEI: tci&(1<<12) != 0})
		off += 4
	}
	f.Payload = b[off:]
	return f, nil
}

// Encode serialises the frame, padding to the 60-byte Ethernet minimum.
func (f *Frame) Encode() []byte {
	n := 14 + 4*len(f.Tags) + len(f.Payload)
	if n < 60 {
		n = 60
	}
	b := make([]byte, n)
	copy(b[0:6], f.Dst[:])
	copy(b[6:12], f.Src[:])
	off := 12
	for _, t := range f.Tags {
		binary.BigEndian.PutUint16(b[off:], t.tpid())
		binary.BigEndian.PutUint16(b[off+2:], t.tci())
		off += 4
	}
	if f.EtherType == EtherTypeLLC {
		binary.BigEndian.PutUint16(b[off:], uint16(len(f.Payload)))
	} else {
		binary.BigEndian.PutUint16(b[off:], f.EtherType)
	}
	copy(b[off+2:], f.Payload)
	return b
}

// SameVLANs reports whether two tag stacks carry the same TPIDs and VIDs
// (PCP/DEI are ignored). A lone priority tag (VID 0) is treated as untagged.
func SameVLANs(a, b []VLANTag) bool {
	a, b = stripPrio(a), stripPrio(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].tpid() != b[i].tpid() || a[i].VID != b[i].VID {
			return false
		}
	}
	return true
}

func stripPrio(t []VLANTag) []VLANTag {
	if len(t) == 1 && t[0].VID == 0 {
		return nil
	}
	return t
}
