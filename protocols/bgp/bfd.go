package bgp

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net/netip"
	"time"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"

	"github.com/ripnet/clabgen/internal/bfd"
	"github.com/ripnet/clabgen/internal/ipstack"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// BFD for BGP peers (RFC 5882): a session per peer while its BGP session
// is Established, single hop (RFC 5881: UDP 3784, TTL 255) toward a peer
// on a connected subnet, multihop (RFC 5883: UDP 4784) toward any other,
// e.g. a loopback. A BFD session that fails after being up takes the BGP
// session down (Cease, BFD Down: RFC 9384); the peer going AdminDown
// doesn't.

type BFDConfig struct {
	Enabled    bool `json:"enabled" jsonschema_description:"BFD for every peer (a peer's bfd overrides it)"`
	MinTxMs    int  `json:"min_tx_ms,omitempty" jsonschema:"minimum=100,maximum=10000,description=Desired transmit interval. Default 300"`
	MinRxMs    int  `json:"min_rx_ms,omitempty" jsonschema:"minimum=100,maximum=10000,description=Required receive interval. Default 300"`
	Multiplier int  `json:"multiplier,omitempty" jsonschema:"minimum=2,maximum=255,description=Detection multiplier. Default 3"`
}

type BFDArgs struct {
	Peer    string `json:"peer,omitempty" jsonschema:"description=Peer address; empty: every peer"`
	Seconds int    `json:"seconds" jsonschema:"minimum=1,maximum=3600,description=How long our BFD sessions stay silent"`
}

const bfdMultihopPort = 4784

func (c BFDConfig) validate() error {
	for _, v := range []int{c.MinTxMs, c.MinRxMs} {
		if v != 0 && (v < 100 || v > 10000) {
			return fmt.Errorf("bfd: intervals are 100-10000 ms")
		}
	}
	if c.Multiplier != 0 && (c.Multiplier < 2 || c.Multiplier > 255) {
		return fmt.Errorf("bfd: multiplier must be 2-255")
	}
	return nil
}

func (c BFDConfig) session() bfd.Config {
	tx, rx, m := c.MinTxMs, c.MinRxMs, c.Multiplier
	if tx == 0 {
		tx = 300
	}
	if rx == 0 {
		rx = 300
	}
	if m == 0 {
		m = 3
	}
	return bfd.Config{MinTx: time.Duration(tx) * time.Millisecond, MinRx: time.Duration(rx) * time.Millisecond, Mult: byte(m)}
}

// bfdOn reports whether a peer runs BFD.
func bfdOn(c Config, p Peer) bool {
	if p.BFD != nil {
		return *p.BFD
	}
	return c.BFD.Enabled
}

// usesBFD reports whether any peer runs BFD: the speaker then takes IP
// frames (for BFD's UDP) besides its TCP.
func usesBFD(c Config) bool {
	for _, p := range c.Peers {
		if bfdOn(c, p) {
			return true
		}
	}
	return false
}

func (bgpType) EtherTypesFor(c any) []uint16 {
	if usesBFD(*c.(*Config)) {
		return []uint16{pkt.EtherTypeIPv4, pkt.EtherTypeIPv6}
	}
	return nil
}

// bfdPeer is a peer's BFD session.
type bfdPeer struct {
	sess     *bfd.Session
	dst, src netip.Addr
	sport    uint16
	multihop bool
	mac      pkt.MAC // the next hop toward dst
	macAt    time.Time
}

func (b *bfdPeer) port() uint16 {
	if b.multihop {
		return bfdMultihopPort
	}
	return bfd.Port
}

// startBFDLocked starts p's BFD session for the Established connection c.
func (s *session) startBFDLocked(p *peer, c *conn) {
	if !bfdOn(s.cfg, p.cfg) {
		return
	}
	src := ipstack.AddrPortOf(c.c.LocalAddr()).Addr().Unmap()
	multihop := true
	if ip := s.env.IP(); ip != nil {
		for _, a := range ip.Addresses() {
			if a.Contains(p.addr) {
				multihop = false
			}
		}
	}
	p.bfd = &bfdPeer{sess: bfd.New(s.cfg.BFD.session(), rand.Uint32()|1, time.Now()), dst: p.addr, src: src, multihop: multihop,
		sport: uint16(bfd.SrcPortMin + rand.Intn(65536-bfd.SrcPortMin))}
}

func (s *session) bfdLoop() {
	defer s.wg.Done()
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.bfdTick(now)
		}
	}
}

// bfdTick sends what's due and acts on failures.
func (s *session) bfdTick(now time.Time) {
	type send struct {
		b     *bfdPeer
		et    uint16
		frame []byte
	}
	var out []send
	var down []*conn
	s.mu.Lock()
	for _, p := range s.peers {
		b := p.bfd
		if b == nil {
			continue
		}
		b.sess.SetSilent(now.Before(p.bfdSilentTill))
		was := b.sess.State()
		if pdu, _ := b.sess.Tick(now); pdu != nil {
			et, f := s.bfdFrameLocked(b, pdu)
			out = append(out, send{b, et, f})
		}
		if c := s.bfdChangedLocked(p, b, was); c != nil {
			down = append(down, c)
		}
	}
	s.mu.Unlock()
	for _, x := range out {
		s.sendBFD(x.b, x.et, x.frame, now)
	}
	for _, c := range down {
		c.notify(gb.BGP_ERROR_CEASE, ceaseBFDDown, nil)
	}
}

// ceaseBFDDown is the Cease subcode for BFD Down (RFC 9384).
const ceaseBFDDown = 10

// bfdChangedLocked reports a state change; it returns the connection to
// take down when an up session failed.
func (s *session) bfdChangedLocked(p *peer, b *bfdPeer, was byte) *conn {
	st := b.sess.State()
	if st == was {
		return nil
	}
	go s.event("bfd", fmt.Sprintf("BFD with BGP peer %s: %s → %s", p.addr, bfd.StateNames[was], bfd.StateNames[st]))
	switch {
	case st == bfd.Up:
		p.counters["bfd_up"]++
	case was == bfd.Up && st == bfd.Down && b.sess.PeerAdminDown():
		p.counters["bfd_peer_admin_down"]++
	case was == bfd.Up && st == bfd.Down:
		p.counters["bfd_down"]++
		p.bfd = nil
		return p.est
	}
	return nil
}

// sendBFD sends a control packet to the next hop toward the peer (looked
// up again every few seconds).
func (s *session) sendBFD(b *bfdPeer, et uint16, frame []byte, now time.Time) {
	ip := s.env.IP()
	if ip == nil {
		return
	}
	s.mu.Lock()
	mac, at := b.mac, b.macAt
	s.mu.Unlock()
	if mac.IsZero() || now.Sub(at) > 5*time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, m, err := ip.ResolveNextHop(ctx, b.dst)
		cancel()
		if err != nil {
			return
		}
		mac = m
		s.mu.Lock()
		b.mac, b.macAt = m, now
		s.mu.Unlock()
	}
	s.env.Send(mac, et, frame, protocol.SendOpts{})
}

// bfdFrameLocked is a control packet in UDP and IP (TTL 255).
func (s *session) bfdFrameLocked(b *bfdPeer, pdu []byte) (uint16, []byte) {
	u := make([]byte, 8, 8+len(pdu))
	binary.BigEndian.PutUint16(u, b.sport)
	binary.BigEndian.PutUint16(u[2:], b.port())
	binary.BigEndian.PutUint16(u[4:], uint16(8+len(pdu)))
	u = append(u, pdu...)
	cs := pkt.Checksum(u, pkt.PseudoSum(b.src, b.dst, pkt.ProtoUDP, len(u)))
	if cs == 0 {
		cs = 0xffff
	}
	binary.BigEndian.PutUint16(u[6:], cs)
	if b.dst.Is6() {
		ip := pkt.IPv6{TrafficClass: 0xc0, NextHeader: pkt.ProtoUDP, HopLimit: 255, Src: b.src, Dst: b.dst, Payload: u}
		return pkt.EtherTypeIPv6, ip.Encode()
	}
	s.ipID++
	ip := pkt.IPv4{TOS: 0xc0, ID: s.ipID, TTL: 255, Protocol: pkt.ProtoUDP, Src: b.src, Dst: b.dst, Payload: u}
	return pkt.EtherTypeIPv4, ip.Encode()
}

// HandleRx takes BFD control packets for our peers' sessions (the IP
// stack has the rest).
func (s *session) HandleRx(f *pkt.Frame) {
	var src, dst netip.Addr
	var ttl uint8
	var udp []byte
	switch f.EtherType {
	case pkt.EtherTypeIPv4:
		ip, err := pkt.DecodeIPv4(f.Payload)
		if err != nil || ip.Protocol != pkt.ProtoUDP || ip.FragOff != 0 {
			return
		}
		src, dst, ttl, udp = ip.Src, ip.Dst, ip.TTL, ip.Payload
	case pkt.EtherTypeIPv6:
		ip, err := pkt.DecodeIPv6(f.Payload)
		if err != nil || ip.NextHeader != pkt.ProtoUDP {
			return
		}
		src, dst, ttl, udp = ip.Src, ip.Dst, ip.HopLimit, ip.Payload
	default:
		return
	}
	if len(udp) < 8 {
		return
	}
	port := binary.BigEndian.Uint16(udp[2:])
	if port != bfd.Port && port != bfdMultihopPort {
		return
	}
	p, err := bfd.Decode(udp[8:])
	s.mu.Lock()
	var down *conn
	for _, x := range s.peers {
		b := x.bfd
		if b == nil || b.dst != src.Unmap() || b.src != dst.Unmap() || b.port() != port {
			continue
		}
		// Single hop: only from the link itself (RFC 5881 5).
		if err != nil || !b.multihop && ttl != 255 {
			x.counters["rx_bfd_errors"]++
			break
		}
		was := b.sess.State()
		b.sess.Receive(p, time.Now())
		x.counters["rx_bfd"]++
		down = s.bfdChangedLocked(x, b, was)
		break
	}
	s.mu.Unlock()
	if down != nil {
		down.notify(gb.BGP_ERROR_CEASE, ceaseBFDDown, nil)
	}
}
