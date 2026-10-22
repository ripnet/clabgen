// Package icmp is the ICMP traffic flow: a continuous (or counted, or
// timed) stream of ICMP packets whose IP and ICMP fields are all
// configurable. Echo requests are matched with their replies for loss,
// outage and RTT; ICMP errors that quote a sent packet are reported too.
package icmp

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"

	"github.com/ripnet/clabgen/internal/ipflow"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "icmp"

func init() { protocol.Register(flowType{}) }

type flowType struct{}

func (flowType) Name() string { return TypeName }
func (flowType) Description() string {
	return "ICMP flow: echo (ping) or any crafted ICMP/ICMPv6 packet; continuous, counted or timed, with loss, outage and RTT"
}
func (flowType) Category() string           { return protocol.CategoryTraffic }
func (flowType) NewConfig() any             { return defaultConfig() }
func (flowType) EtherTypes() []uint16       { return []uint16{pkt.EtherTypeIPv4, pkt.EtherTypeIPv6} }
func (flowType) Actions() []protocol.Action { return ipflow.Actions() }

type flow struct {
	*ipflow.Flow
	cfg   Config
	ident uint16
}

func (flowType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	dst, _ := cfg.Dst()
	f := &flow{cfg: cfg, ident: binary.BigEndian.Uint16(ipflow.Hash(env, 0))}
	if cfg.Identifier != nil {
		f.ident = *cfg.Identifier
	}
	f.Flow = ipflow.New(env, cfg.spec(), cfg.tracksReplies(dst), f)
	f.Begin()
	return f, nil
}

func (f *flow) seq16(n uint64) uint16 {
	if f.cfg.SeqMode == "fixed" {
		return f.cfg.SeqStart
	}
	return f.cfg.SeqStart + uint16(n)
}

// Message implements ipflow.Transport.
func (f *flow) Message(n uint64, src, dst netip.Addr, data []byte) ([]byte, uint8) {
	msg := make([]byte, 8+len(data))
	msg[0], msg[1] = f.cfg.icmpType(dst), f.cfg.ICMPCode
	binary.BigEndian.PutUint16(msg[4:], f.ident)
	binary.BigEndian.PutUint16(msg[6:], f.seq16(n))
	copy(msg[8:], data)
	if dst.Is4() {
		binary.BigEndian.PutUint16(msg[2:], ipflow.Checksum(f.cfg.ICMPChecksum, pkt.Checksum(msg, 0)))
		return msg, pkt.ProtoICMP
	}
	pkt.ICMPv6Checksum(src, dst, msg)
	binary.BigEndian.PutUint16(msg[2:], ipflow.Checksum(f.cfg.ICMPChecksum, binary.BigEndian.Uint16(msg[2:])))
	return msg, pkt.ProtoICMPv6
}

func (f *flow) HeaderLen() int { return 8 }

func (f *flow) Describe(n uint64) string {
	return fmt.Sprintf("ICMP type %d code %d id %d seq %d", f.cfg.icmpType(f.Dst), f.cfg.ICMPCode, f.ident, f.seq16(n))
}

// index maps a received 16-bit sequence number back to the sequence index
// (the most recent one sent with that number).
func (f *flow) index(s16 uint16) (uint64, bool) {
	if f.cfg.SeqMode == "fixed" {
		return 0, false
	}
	n := f.SentCount()
	if n == 0 {
		return 0, false
	}
	last := n - 1
	back := uint64(uint16(f.seq16(last) - s16))
	if back > last {
		return 0, false
	}
	return last - back, true
}

func (f *flow) HandleRx(fr *pkt.Frame) {
	src := f.Source()
	if !src.IsValid() {
		return
	}
	proto := pkt.ProtoICMP
	if fr.EtherType == pkt.EtherTypeIPv6 {
		proto = pkt.ProtoICMPv6
	}
	from, ttl, m, v4, ok := ipflow.DecodeIP(fr, src, proto)
	if !ok || len(m) < 8 {
		return
	}
	if v4 && pkt.Checksum(m, 0) != 0 || !v4 && !pkt.ICMPv6Valid(from, src, m) {
		return
	}
	reply := pkt.ICMPEchoReply
	if !v4 {
		reply = pkt.ICMPv6EchoReply
	}
	if m[0] == reply {
		if !f.Echo || binary.BigEndian.Uint16(m[4:]) != f.ident || from != f.Dst {
			return
		}
		var n uint64
		var sent time.Time // zero: the tracker uses its own send time
		if f.cfg.Signature {
			id, seq, at, ok := ipflow.ParseSignature(m[8:])
			if !ok || id != f.FlowID {
				return
			}
			n, sent = seq, at
		} else if n, ok = f.index(binary.BigEndian.Uint16(m[6:])); !ok {
			return
		}
		ttlName := "ttl"
		if !v4 {
			ttlName = "hlim"
		}
		size := len(m) - 8
		f.Tracker().Received(n, ipflow.RxTime(fr), sent, func() string { return fmt.Sprintf("%d B from %s %s=%d", size, from, ttlName, ttl) })
		return
	}
	e, ok := ipflow.ParseICMPError(m, v4, from)
	if !ok || e.Proto != proto || e.InnerSrc != src || e.InnerDst != f.Dst || binary.BigEndian.Uint16(e.Quoted[4:]) != f.ident {
		return
	}
	n, mapped := f.index(binary.BigEndian.Uint16(e.Quoted[6:]))
	if !mapped || !f.Echo {
		n = ^uint64(0)
	}
	f.Tracker().ErrorReceived(n, ipflow.RxTime(fr), e.String())
	f.SetError(e.String())
}

type Status struct {
	ipflow.Base
	Identifier uint16 `json:"identifier"`
}

func (f *flow) Status() any {
	s := Status{Base: f.BaseStatus(), Identifier: f.ident}
	s.Kind = fmt.Sprintf("type %d code %d", f.cfg.icmpType(f.Dst), f.cfg.ICMPCode)
	if f.Echo {
		s.Kind = "echo"
	}
	return s
}

func (f *flow) Action(ctx context.Context, name string, args any) (any, error) {
	return f.Flow.Action(ctx, name, args)
}
