package dhcp

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// DHCPv6 (RFC 8415).
const (
	Port6Client = 546
	Port6Server = 547

	MsgSolicit     uint8 = 1
	MsgAdvertise   uint8 = 2
	MsgRequest6    uint8 = 3
	MsgConfirm     uint8 = 4
	MsgRenew       uint8 = 5
	MsgRebind      uint8 = 6
	MsgReply       uint8 = 7
	MsgRelease6    uint8 = 8
	MsgDecline6    uint8 = 9
	MsgReconfigure uint8 = 10
	MsgInfoRequest uint8 = 11
	MsgRelayForw   uint8 = 12
	MsgRelayRepl   uint8 = 13

	Opt6ClientID     uint16 = 1
	Opt6ServerID     uint16 = 2
	Opt6IANA         uint16 = 3
	Opt6IAAddr       uint16 = 5
	Opt6ORO          uint16 = 6
	Opt6Preference   uint16 = 7
	Opt6ElapsedTime  uint16 = 8
	Opt6RelayMsg     uint16 = 9
	Opt6StatusCode   uint16 = 13
	Opt6RapidCommit  uint16 = 14
	Opt6InterfaceID  uint16 = 18
	Opt6DNS          uint16 = 23
	Opt6DomainList   uint16 = 24
	Opt6IAPD         uint16 = 25
	Opt6IAPrefix     uint16 = 26
	Opt6RemoteID     uint16 = 37 // RFC 4649
	Opt6SubscriberID uint16 = 38 // RFC 4580
	Opt6ClientLLAddr uint16 = 79 // client link-layer address (RFC 6939)

	StatusSuccess       uint16 = 0
	StatusUnspecFail    uint16 = 1
	StatusNoAddrsAvail  uint16 = 2
	StatusNoBinding     uint16 = 3
	StatusNotOnLink     uint16 = 4
	StatusUseMulticast  uint16 = 5
	StatusNoPrefixAvail uint16 = 6
)

// AllServers is All_DHCP_Relay_Agents_and_Servers.
var AllServers = netip.MustParseAddr("ff02::1:2")

// Option6 is one DHCPv6 option.
type Option6 struct {
	Code uint16
	Data []byte
}

// Msg6 is a client/server DHCPv6 message (not a relay message).
type Msg6 struct {
	Type    uint8
	XID     uint32 // 24 bits
	Options []Option6
}

// Get returns an option's data, nil when absent.
func (m *Msg6) Get(code uint16) []byte {
	for _, o := range m.Options {
		if o.Code == code {
			return o.Data
		}
	}
	return nil
}

// Has reports whether an option is there (rapid commit has no data).
func (m *Msg6) Has(code uint16) bool {
	for _, o := range m.Options {
		if o.Code == code {
			return true
		}
	}
	return false
}

func (m *Msg6) Add(code uint16, data []byte) { m.Options = append(m.Options, Option6{code, data}) }

func encodeOptions(b []byte, opts []Option6) []byte {
	for _, o := range opts {
		b = binary.BigEndian.AppendUint16(b, o.Code)
		b = binary.BigEndian.AppendUint16(b, uint16(len(o.Data)))
		b = append(b, o.Data...)
	}
	return b
}

func decodeOptions(b []byte) ([]Option6, error) {
	var out []Option6
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, errors.New("truncated option")
		}
		n := int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+n {
			return nil, errors.New("truncated option")
		}
		out = append(out, Option6{binary.BigEndian.Uint16(b), append([]byte(nil), b[4:4+n]...)})
		b = b[4+n:]
	}
	return out, nil
}

func (m *Msg6) Encode() []byte {
	b := []byte{m.Type, byte(m.XID >> 16), byte(m.XID >> 8), byte(m.XID)}
	return encodeOptions(b, m.Options)
}

func Decode6(b []byte) (*Msg6, error) {
	if len(b) < 4 || b[0] == 12 || b[0] == 13 {
		return nil, errNotDHCP // too short, or a relay message
	}
	opts, err := decodeOptions(b[4:])
	if err != nil {
		return nil, err
	}
	return &Msg6{Type: b[0], XID: uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), Options: opts}, nil
}

// Status6 is a Status Code option.
type Status6 struct {
	Code    uint16
	Message string
}

func (s Status6) Encode() []byte {
	return append(binary.BigEndian.AppendUint16(nil, s.Code), s.Message...)
}

func decodeStatus(b []byte) *Status6 {
	if len(b) < 2 {
		return nil
	}
	return &Status6{binary.BigEndian.Uint16(b), string(b[2:])}
}

// StatusOf returns a message's (or an IA's) status code option.
func StatusOf(opts []Option6) *Status6 {
	for _, o := range opts {
		if o.Code == Opt6StatusCode {
			return decodeStatus(o.Data)
		}
	}
	return nil
}

// IAAddr is an IA Address option.
type IAAddr struct {
	Addr             netip.Addr
	Preferred, Valid uint32
}

// IAPrefix is an IA Prefix option.
type IAPrefix struct {
	Prefix           netip.Prefix
	Preferred, Valid uint32
}

// IA is an IA_NA (Opt6IANA) or IA_PD (Opt6IAPD) option.
type IA struct {
	Kind     uint16
	IAID     uint32
	T1, T2   uint32
	Addrs    []IAAddr
	Prefixes []IAPrefix
	Status   *Status6
}

func (ia IA) Encode() Option6 {
	b := binary.BigEndian.AppendUint32(nil, ia.IAID)
	b = binary.BigEndian.AppendUint32(b, ia.T1)
	b = binary.BigEndian.AppendUint32(b, ia.T2)
	var sub []Option6
	for _, a := range ia.Addrs {
		x := a.Addr.As16()
		d := append(x[:], make([]byte, 8)...)
		binary.BigEndian.PutUint32(d[16:], a.Preferred)
		binary.BigEndian.PutUint32(d[20:], a.Valid)
		sub = append(sub, Option6{Opt6IAAddr, d})
	}
	for _, p := range ia.Prefixes {
		d := make([]byte, 25)
		binary.BigEndian.PutUint32(d, p.Preferred)
		binary.BigEndian.PutUint32(d[4:], p.Valid)
		d[8] = byte(p.Prefix.Bits())
		x := p.Prefix.Addr().As16()
		copy(d[9:], x[:])
		sub = append(sub, Option6{Opt6IAPrefix, d})
	}
	if ia.Status != nil {
		sub = append(sub, Option6{Opt6StatusCode, ia.Status.Encode()})
	}
	return Option6{ia.Kind, encodeOptions(b, sub)}
}

// DecodeIA parses an IA_NA or IA_PD option.
func DecodeIA(o Option6) (IA, error) {
	ia := IA{Kind: o.Code}
	if len(o.Data) < 12 {
		return ia, errors.New("short IA")
	}
	ia.IAID = binary.BigEndian.Uint32(o.Data)
	ia.T1 = binary.BigEndian.Uint32(o.Data[4:])
	ia.T2 = binary.BigEndian.Uint32(o.Data[8:])
	sub, err := decodeOptions(o.Data[12:])
	if err != nil {
		return ia, err
	}
	for _, s := range sub {
		switch {
		case s.Code == Opt6IAAddr && o.Code == Opt6IANA && len(s.Data) >= 24:
			ia.Addrs = append(ia.Addrs, IAAddr{netip.AddrFrom16([16]byte(s.Data[:16])), binary.BigEndian.Uint32(s.Data[16:]), binary.BigEndian.Uint32(s.Data[20:])})
		case s.Code == Opt6IAPrefix && o.Code == Opt6IAPD && len(s.Data) >= 25 && s.Data[8] <= 128:
			p := netip.PrefixFrom(netip.AddrFrom16([16]byte(s.Data[9:25])), int(s.Data[8])).Masked()
			ia.Prefixes = append(ia.Prefixes, IAPrefix{p, binary.BigEndian.Uint32(s.Data), binary.BigEndian.Uint32(s.Data[4:])})
		case s.Code == Opt6StatusCode:
			ia.Status = decodeStatus(s.Data)
		}
	}
	return ia, nil
}

// IAs returns a message's IA_NA and IA_PD options.
func (m *Msg6) IAs() []IA {
	var out []IA
	for _, o := range m.Options {
		if o.Code == Opt6IANA || o.Code == Opt6IAPD {
			if ia, err := DecodeIA(o); err == nil {
				out = append(out, ia)
			}
		}
	}
	return out
}

// DUIDLL is a DUID-LL (RFC 8415 11.4) for an Ethernet MAC.
func DUIDLL(mac [6]byte) []byte { return append([]byte{0, 3, 0, 1}, mac[:]...) }

// Relay6 is a Relay-forward or Relay-reply message (RFC 8415 9).
type Relay6 struct {
	Type       uint8
	Hops       uint8
	Link, Peer netip.Addr
	Options    []Option6 // the relay message (Opt6RelayMsg) among them
}

func (r *Relay6) Get(code uint16) []byte { return (&Msg6{Options: r.Options}).Get(code) }

func (r *Relay6) Encode() []byte {
	b := []byte{r.Type, r.Hops}
	l, p := r.Link.As16(), r.Peer.As16()
	b = append(append(b, l[:]...), p[:]...)
	return encodeOptions(b, r.Options)
}

func decodeRelay6(b []byte) (*Relay6, error) {
	if len(b) < 34 || b[0] != MsgRelayForw && b[0] != MsgRelayRepl {
		return nil, errNotDHCP
	}
	opts, err := decodeOptions(b[34:])
	if err != nil {
		return nil, err
	}
	return &Relay6{Type: b[0], Hops: b[1], Link: netip.AddrFrom16([16]byte(b[2:18])), Peer: netip.AddrFrom16([16]byte(b[18:34])), Options: opts}, nil
}

// maxRelayHops is HOP_COUNT_LIMIT (RFC 8415 7.6).
const maxRelayHops = 8

// Unwrap6 takes a message from the wire apart: the relays it passed,
// outermost (the one that sent it to us) first, and the client's message.
func Unwrap6(b []byte) ([]*Relay6, *Msg6, error) {
	var relays []*Relay6
	for len(b) > 0 && (b[0] == MsgRelayForw || b[0] == MsgRelayRepl) {
		if len(relays) > maxRelayHops {
			return nil, nil, errors.New("too many relays")
		}
		r, err := decodeRelay6(b)
		if err != nil {
			return nil, nil, err
		}
		relays = append(relays, r)
		if b = r.Get(Opt6RelayMsg); b == nil {
			return nil, nil, errors.New("relay message without a relay message option")
		}
	}
	m, err := Decode6(b)
	return relays, m, err
}

// Wrap6 puts a reply to a relayed message back in Relay-reply messages,
// one per relay it passed (relays as Unwrap6 returns them), echoing each
// relay's Interface-ID (RFC 8415 19.3).
func Wrap6(relays []*Relay6, reply []byte) []byte {
	for i := len(relays) - 1; i >= 0; i-- {
		r := relays[i]
		out := &Relay6{Type: MsgRelayRepl, Hops: r.Hops, Link: r.Link, Peer: r.Peer}
		if id := r.Get(Opt6InterfaceID); id != nil {
			out.Options = append(out.Options, Option6{Opt6InterfaceID, id})
		}
		out.Options = append(out.Options, Option6{Opt6RelayMsg, reply})
		reply = out.Encode()
	}
	return reply
}
