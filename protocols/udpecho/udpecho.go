// Package udpecho is a UDP echo responder: datagrams to the device's
// addresses on the configured ports are sent back to their source with
// the ports swapped (RFC 862 style), so udp flows with reply "echo" can
// measure round trips to a clabgen device.
package udpecho

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "udp-echo"

func init() { protocol.Register(echoType{}) }

type Config struct {
	Ports []uint16 `json:"ports" jsonschema:"description=UDP ports to answer on"`
	TTL   uint8    `json:"ttl" jsonschema:"minimum=1,maximum=255,description=TTL / hop limit of replies"`
}

func (c *Config) Validate() error {
	if len(c.Ports) == 0 {
		return errors.New("at least one port")
	}
	for _, p := range c.Ports {
		if p == 0 {
			return errors.New("ports must be 1-65535")
		}
	}
	if c.TTL == 0 {
		return errors.New("ttl must be 1-255")
	}
	return nil
}

type echoType struct{}

func (echoType) Name() string { return TypeName }
func (echoType) Description() string {
	return "UDP echo responder: reflects datagrams on the given ports back to their sender"
}
func (echoType) NewConfig() any       { return &Config{Ports: []uint16{7}, TTL: 64} }
func (echoType) EtherTypes() []uint16 { return []uint16{pkt.EtherTypeIPv4, pkt.EtherTypeIPv6} }
func (echoType) Actions() []protocol.Action {
	return []protocol.Action{{Name: "clear", Description: "Reset counters"}}
}

type responder struct {
	env   protocol.Env
	cfg   Config
	ports map[uint16]bool

	mu       sync.Mutex
	rx, tx   uint64
	txErrors uint64
	lastFrom netip.AddrPort // formatted by Status
}

func (echoType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	r := &responder{env: env, cfg: cfg, ports: map[uint16]bool{}}
	for _, p := range cfg.Ports {
		r.ports[p] = true
	}
	return r, nil
}

func (r *responder) ours(a netip.Addr) bool {
	ip := r.env.IP()
	if ip == nil {
		return false
	}
	for _, p := range ip.Addresses() {
		if p.Addr() == a {
			return true
		}
	}
	return false
}

func (r *responder) HandleRx(fr *pkt.Frame) {
	if fr.Dst.IsMulticast() {
		return
	}
	var src, dst netip.Addr
	var b []byte
	switch fr.EtherType {
	case pkt.EtherTypeIPv4:
		h, err := pkt.DecodeIPv4(fr.Payload)
		if err != nil || h.Protocol != pkt.ProtoUDP || h.FragOff != 0 || h.Flags&pkt.IPv4MF != 0 {
			return
		}
		src, dst, b = h.Src, h.Dst, h.Payload
	case pkt.EtherTypeIPv6:
		h, err := pkt.DecodeIPv6(fr.Payload)
		if err != nil || h.NextHeader != pkt.ProtoUDP {
			return
		}
		src, dst, b = h.Src, h.Dst, h.Payload
	default:
		return
	}
	if len(b) < 8 || !r.ports[binary.BigEndian.Uint16(b[2:])] || !r.ours(dst) {
		return
	}
	n := int(binary.BigEndian.Uint16(b[4:]))
	if n < 8 || n > len(b) {
		return
	}
	b = b[:n]
	if cs := binary.BigEndian.Uint16(b[6:]); (cs != 0 || dst.Is6()) && pkt.Checksum(b, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(b))) != 0 {
		return // the stack counts it as a UDP error
	}
	out := make([]byte, len(b))
	copy(out, b)
	copy(out[0:2], b[2:4])
	copy(out[2:4], b[0:2])
	out[6], out[7] = 0, 0
	cs := pkt.Checksum(out, pkt.PseudoSum(dst, src, pkt.ProtoUDP, len(out)))
	if cs == 0 {
		cs = 0xffff
	}
	binary.BigEndian.PutUint16(out[6:], cs)
	var ipb []byte
	et := pkt.EtherTypeIPv4
	if dst.Is4() {
		h := pkt.IPv4{TTL: r.cfg.TTL, Protocol: pkt.ProtoUDP, Src: dst, Dst: src, Payload: out}
		ipb = h.Encode()
	} else {
		h := pkt.IPv6{NextHeader: pkt.ProtoUDP, HopLimit: r.cfg.TTL, Src: dst, Dst: src, Payload: out}
		ipb, et = h.Encode(), pkt.EtherTypeIPv6
	}
	err := r.env.Send(fr.Src, et, ipb, protocol.SendOpts{})
	r.mu.Lock()
	r.rx++
	if err != nil {
		r.txErrors++
	} else {
		r.tx++
	}
	r.lastFrom = netip.AddrPortFrom(src, binary.BigEndian.Uint16(b[0:]))
	r.mu.Unlock()
}

type Status struct {
	Ports    []uint16 `json:"ports"`
	Rx       uint64   `json:"rx"`
	Tx       uint64   `json:"tx"`
	TxErrors uint64   `json:"tx_errors"`
	LastFrom string   `json:"last_from,omitempty"`
}

func (r *responder) Status() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{Ports: r.cfg.Ports, Rx: r.rx, Tx: r.tx, TxErrors: r.txErrors}
	if r.lastFrom.IsValid() {
		st.LastFrom = r.lastFrom.String()
	}
	return st
}

func (r *responder) Action(_ context.Context, name string, _ any) (any, error) {
	if name != "clear" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	r.mu.Lock()
	r.rx, r.tx, r.txErrors, r.lastFrom = 0, 0, 0, netip.AddrPort{}
	r.mu.Unlock()
	return map[string]any{"cleared": true}, nil
}

func (r *responder) Stop() {}
