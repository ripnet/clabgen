package dhcp

import (
	"encoding/binary"
	"errors"
	"net/netip"

	"github.com/ripnet/clabgen/internal/pkt"
)

// DHCPv4 (RFC 2131, options RFC 2132).
const (
	Port4Server = 67
	Port4Client = 68

	OpRequest uint8 = 1
	OpReply   uint8 = 2

	MsgDiscover uint8 = 1
	MsgOffer    uint8 = 2
	MsgRequest  uint8 = 3
	MsgDecline  uint8 = 4
	MsgAck      uint8 = 5
	MsgNak      uint8 = 6
	MsgRelease  uint8 = 7
	MsgInform   uint8 = 8

	OptSubnetMask  uint8 = 1
	OptRouter      uint8 = 3
	OptDNS         uint8 = 6
	OptHostname    uint8 = 12
	OptDomainName  uint8 = 15
	OptRequestedIP uint8 = 50
	OptLeaseTime   uint8 = 51
	OptMsgType     uint8 = 53
	OptServerID    uint8 = 54
	OptParamList   uint8 = 55
	OptMessage     uint8 = 56
	OptMaxSize     uint8 = 57
	OptT1          uint8 = 58
	OptT2          uint8 = 59
	OptVendorClass uint8 = 60
	OptClientID    uint8 = 61
	OptRapidCommit uint8 = 80
)

var magicCookie = [4]byte{99, 130, 83, 99}

// Option4 is one DHCPv4 option.
type Option4 struct {
	Code uint8
	Data []byte
}

// Msg4 is a DHCPv4 message. Unset addresses are sent as 0.0.0.0.
type Msg4 struct {
	Op        uint8
	XID       uint32
	Secs      uint16
	Broadcast bool
	CIAddr    netip.Addr
	YIAddr    netip.Addr
	SIAddr    netip.Addr
	GIAddr    netip.Addr
	CHAddr    pkt.MAC
	Options   []Option4
}

// Type is the message type option (0 for BOOTP).
func (m *Msg4) Type() uint8 {
	if b := m.Get(OptMsgType); len(b) == 1 {
		return b[0]
	}
	return 0
}

// Get returns an option's data, nil when absent.
func (m *Msg4) Get(code uint8) []byte {
	for _, o := range m.Options {
		if o.Code == code {
			return o.Data
		}
	}
	return nil
}

// Has reports whether an option is there (rapid commit has no data).
func (m *Msg4) Has(code uint8) bool {
	for _, o := range m.Options {
		if o.Code == code {
			return true
		}
	}
	return false
}

// Addr returns an option holding one IPv4 address.
func (m *Msg4) Addr(code uint8) netip.Addr {
	if b := m.Get(code); len(b) >= 4 {
		return netip.AddrFrom4([4]byte(b[:4]))
	}
	return netip.Addr{}
}

// Addrs returns an option holding a list of IPv4 addresses.
func (m *Msg4) Addrs(code uint8) []netip.Addr {
	var out []netip.Addr
	b := m.Get(code)
	for ; len(b) >= 4; b = b[4:] {
		out = append(out, netip.AddrFrom4([4]byte(b[:4])))
	}
	return out
}

// Uint32 returns a four-byte option, and whether it is there.
func (m *Msg4) Uint32(code uint8) (uint32, bool) {
	if b := m.Get(code); len(b) == 4 {
		return binary.BigEndian.Uint32(b), true
	}
	return 0, false
}

// Add appends an option.
func (m *Msg4) Add(code uint8, data []byte) { m.Options = append(m.Options, Option4{code, data}) }

func put4(b []byte, a netip.Addr) {
	if a.Is4() {
		x := a.As4()
		copy(b, x[:])
	}
}

// Encode serialises the message, padded to BOOTP's 300 bytes. Options
// longer than 255 bytes are split (RFC 3396).
func (m *Msg4) Encode() []byte {
	b := make([]byte, 240, 300)
	b[0], b[1], b[2] = m.Op, 1, 6
	binary.BigEndian.PutUint32(b[4:], m.XID)
	binary.BigEndian.PutUint16(b[8:], m.Secs)
	if m.Broadcast {
		b[10] = 0x80
	}
	put4(b[12:], m.CIAddr)
	put4(b[16:], m.YIAddr)
	put4(b[20:], m.SIAddr)
	put4(b[24:], m.GIAddr)
	copy(b[28:], m.CHAddr[:])
	copy(b[236:], magicCookie[:])
	for _, o := range m.Options {
		d := o.Data
		for first := true; first || len(d) > 0; first = false {
			n := min(len(d), 255)
			b = append(b, o.Code, byte(n))
			b = append(b, d[:n]...)
			d = d[n:]
		}
	}
	b = append(b, 255)
	for len(b) < 300 {
		b = append(b, 0)
	}
	return b
}

var errNotDHCP = errors.New("not a DHCP message")

// Decode4 parses a DHCPv4 message. Repeated options are joined (RFC 3396).
func Decode4(b []byte) (*Msg4, error) {
	if len(b) < 240 || b[1] != 1 || b[2] != 6 || [4]byte(b[236:240]) != magicCookie {
		return nil, errNotDHCP
	}
	m := &Msg4{
		Op:        b[0],
		XID:       binary.BigEndian.Uint32(b[4:]),
		Secs:      binary.BigEndian.Uint16(b[8:]),
		Broadcast: b[10]&0x80 != 0,
		CIAddr:    netip.AddrFrom4([4]byte(b[12:16])),
		YIAddr:    netip.AddrFrom4([4]byte(b[16:20])),
		SIAddr:    netip.AddrFrom4([4]byte(b[20:24])),
		GIAddr:    netip.AddrFrom4([4]byte(b[24:28])),
		CHAddr:    pkt.MAC(b[28:34]),
	}
	at := map[uint8]int{}
	for o := b[240:]; len(o) > 0; {
		code := o[0]
		if code == 255 {
			break
		}
		if code == 0 {
			o = o[1:]
			continue
		}
		if len(o) < 2 || len(o) < 2+int(o[1]) {
			return nil, errors.New("truncated option")
		}
		d := append([]byte(nil), o[2:2+int(o[1])]...)
		if i, ok := at[code]; ok {
			m.Options[i].Data = append(m.Options[i].Data, d...)
		} else {
			at[code] = len(m.Options)
			m.Add(code, d)
		}
		o = o[2+int(o[1]):]
	}
	return m, nil
}
