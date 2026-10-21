// Package udp is the UDP traffic flow: a stream of UDP datagrams with
// every IP and UDP field configurable. Receivers that are clabgen devices
// measure it one way (loss, reordering, latency, outages) from the payload
// signature; with reply "echo", the destination reflects each datagram
// (udp-echo on a clabgen device, or any UDP echo service) and the sender
// measures round-trip loss, outages and RTT as well.
package udp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"github.com/ripnet/clabgen/internal/ipflow"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "udp"

func init() { protocol.Register(flowType{}) }

type Config struct {
	ipflow.Target
	ipflow.Run
	ipflow.Addressing
	ipflow.IPHeader

	DstPort      uint16  `json:"dst_port" jsonschema:"minimum=1,maximum=65535,description=Destination UDP port"`
	SrcPort      *uint16 `json:"src_port,omitempty" jsonschema:"minimum=1,maximum=65535,description=First source port. Default: derived from the flow name (49152-65535)"`
	SrcPortCount uint16  `json:"src_port_count" jsonschema:"minimum=1,description=Cycle through this many source ports; one per packet (spreads ECMP/LAG hashing)"`
	UDPChecksum  string  `json:"udp_checksum" jsonschema:"enum=correct,enum=zero,enum=invalid,description=zero means no checksum (invalid for IPv6)"`
	Reply        string  `json:"reply" jsonschema:"enum=none,enum=echo,description=echo: the destination sends each datagram back (udp-echo on a clabgen device or any UDP echo service) for round-trip loss and RTT"`

	ipflow.Payload
}

func defaultConfig() *Config {
	return &Config{Run: ipflow.DefaultRun, IPHeader: ipflow.DefaultIPHeader, Payload: ipflow.DefaultPayload,
		DstPort: 7, SrcPortCount: 1, UDPChecksum: "correct", Reply: "none"}
}

func (c *Config) spec() ipflow.Spec {
	return ipflow.Spec{Target: c.Target, Run: c.Run, Addressing: c.Addressing, IPHeader: c.IPHeader, Payload: c.Payload}
}

func (c *Config) Validate() error {
	if _, err := c.spec().Validate(); err != nil {
		return err
	}
	if c.DstPort == 0 {
		return errors.New("dst_port must be 1-65535")
	}
	if c.SrcPortCount == 0 {
		return errors.New("src_port_count must be at least 1")
	}
	if c.SrcPort != nil && (*c.SrcPort == 0 || int(*c.SrcPort)+int(c.SrcPortCount)-1 > 65535) {
		return errors.New("src_port .. src_port+src_port_count-1 must stay within 1-65535")
	}
	if err := ipflow.CheckChecksumMode(c.UDPChecksum); err != nil {
		return err
	}
	switch c.Reply {
	case "none":
	case "echo":
		if !c.Signature {
			return errors.New("reply echo needs the signature to match replies")
		}
	default:
		return fmt.Errorf("invalid reply %q", c.Reply)
	}
	return nil
}

type flowType struct{}

func (flowType) Name() string { return TypeName }
func (flowType) Description() string {
	return "UDP flow: crafted datagrams; one-way stats at clabgen receivers; round-trip loss and RTT with reply echo"
}
func (flowType) Category() string           { return protocol.CategoryTraffic }
func (flowType) NewConfig() any             { return defaultConfig() }
func (flowType) EtherTypes() []uint16       { return []uint16{pkt.EtherTypeIPv4, pkt.EtherTypeIPv6} }
func (flowType) Actions() []protocol.Action { return ipflow.Actions() }

type flow struct {
	*ipflow.Flow
	cfg   Config
	sport uint16 // first source port
}

func (flowType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	f := &flow{cfg: cfg}
	if cfg.SrcPort != nil {
		f.sport = *cfg.SrcPort
	} else {
		// 49152 + hash, leaving room for the port range.
		span := 16384 - uint32(cfg.SrcPortCount)
		if cfg.SrcPortCount > 16384 {
			span = 1
		}
		f.sport = uint16(49152 + binary.BigEndian.Uint32(ipflow.Hash(env, 8))%span)
	}
	f.Flow = ipflow.New(env, cfg.spec(), cfg.Reply == "echo", f)
	f.Begin()
	return f, nil
}

func (f *flow) srcPort(n uint64) uint16 { return f.sport + uint16(n%uint64(f.cfg.SrcPortCount)) }

// Message implements ipflow.Transport.
func (f *flow) Message(n uint64, src, dst netip.Addr, data []byte) ([]byte, uint8) {
	b := make([]byte, 8+len(data))
	binary.BigEndian.PutUint16(b[0:], f.srcPort(n))
	binary.BigEndian.PutUint16(b[2:], f.cfg.DstPort)
	binary.BigEndian.PutUint16(b[4:], uint16(len(b)))
	copy(b[8:], data)
	cs := pkt.Checksum(b, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(b)))
	if cs == 0 {
		cs = 0xffff // 0 means "no checksum"
	}
	binary.BigEndian.PutUint16(b[6:], ipflow.Checksum(f.cfg.UDPChecksum, cs))
	return b, pkt.ProtoUDP
}

func (f *flow) HeaderLen() int { return 8 }

func (f *flow) Describe(n uint64) string {
	return fmt.Sprintf("UDP %d > %d", f.srcPort(n), f.cfg.DstPort)
}

func (f *flow) inRange(p uint16) bool {
	return p >= f.sport && int(p) < int(f.sport)+int(f.cfg.SrcPortCount)
}

func (f *flow) HandleRx(fr *pkt.Frame) {
	src := f.Source()
	if !src.IsValid() {
		return
	}
	// Echo replies.
	if from, ttl, b, _, ok := ipflow.DecodeIP(fr, src, pkt.ProtoUDP); ok && f.Echo && from == f.Dst && len(b) >= 8 {
		if binary.BigEndian.Uint16(b[0:]) != f.cfg.DstPort || !f.inRange(binary.BigEndian.Uint16(b[2:])) {
			return
		}
		id, n, sent, ok := ipflow.ParseSignature(b[8:])
		if !ok || id != f.FlowID {
			return
		}
		f.Tracker().Received(n, ipflow.RxTime(fr), sent, fmt.Sprintf("%d B from %s:%d ttl=%d", len(b)-8, from, f.cfg.DstPort, ttl))
		return
	}
	// ICMP errors quoting our datagrams (port unreachable, TTL exceeded, …).
	proto := pkt.ProtoICMP
	if fr.EtherType == pkt.EtherTypeIPv6 {
		proto = pkt.ProtoICMPv6
	}
	from, _, m, v4, ok := ipflow.DecodeIP(fr, src, proto)
	if !ok {
		return
	}
	e, ok := ipflow.ParseICMPError(m, v4, from)
	if !ok || e.Proto != pkt.ProtoUDP || e.InnerSrc != src || e.InnerDst != f.Dst ||
		!f.inRange(binary.BigEndian.Uint16(e.Quoted[0:])) || binary.BigEndian.Uint16(e.Quoted[2:]) != f.cfg.DstPort {
		return
	}
	// The quote has no sequence, so the error can't be tied to a packet.
	f.Tracker().ErrorReceived(^uint64(0), ipflow.RxTime(fr), e.String())
	f.SetError(e.String())
}

type Status struct {
	ipflow.Base
	DstPort  uint16 `json:"dst_port"`
	SrcPorts string `json:"src_ports"`
}

func (f *flow) Status() any {
	s := Status{Base: f.BaseStatus(), DstPort: f.cfg.DstPort, SrcPorts: fmt.Sprint(f.sport)}
	if f.cfg.SrcPortCount > 1 {
		s.SrcPorts = fmt.Sprintf("%d-%d", f.sport, int(f.sport)+int(f.cfg.SrcPortCount)-1)
	}
	s.Kind = fmt.Sprintf("→ :%d", f.cfg.DstPort)
	if f.Echo {
		s.Kind += " echo"
	}
	return s
}

func (f *flow) Action(ctx context.Context, name string, args any) (any, error) {
	return f.Flow.Action(ctx, name, args)
}
