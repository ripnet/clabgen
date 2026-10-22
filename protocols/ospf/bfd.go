package ospf

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net/netip"
	"time"

	"github.com/ripnet/clabgen/internal/bfd"
	"github.com/ripnet/clabgen/internal/pkt"
)

// BFD for OSPF neighbors (RFC 5882): a single-hop session (RFC 5881) with
// each neighbor from 2-Way on; when it goes down after being up, the
// neighbor is dropped at once instead of after the dead interval.

type BFDConfig struct {
	Enabled    bool `json:"enabled"`
	MinTxMs    int  `json:"min_tx_ms,omitempty" jsonschema:"minimum=100,maximum=10000,description=Desired transmit interval. Default 300"`
	MinRxMs    int  `json:"min_rx_ms,omitempty" jsonschema:"minimum=100,maximum=10000,description=Required receive interval. Default 300"`
	Multiplier int  `json:"multiplier,omitempty" jsonschema:"minimum=2,maximum=255,description=Detection multiplier. Default 3"`
}

type BFDArgs struct {
	Seconds   int    `json:"seconds" jsonschema:"minimum=1,maximum=3600,description=How long our BFD sessions stay silent"`
	Interface string `json:"interface,omitempty" jsonschema:"description=Device name; empty: every interface"`
}

func (c BFDConfig) validate() error {
	if !c.Enabled {
		return nil
	}
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

// bfdTimersLocked runs neighbor n's BFD session: starts it from 2-Way,
// sends what's due, and drops the neighbor when the session fails.
func (s *session) bfdTimersLocked(n *nbr, now time.Time) {
	f := n.ifc
	if !f.bfd || n.state < n2Way && n.bfd == nil { // a session runs on through 1-way (a restarting neighbor)
		return
	}
	if k := f.keptBFD[n.addr]; n.bfd == nil && k != nil {
		n.bfd, n.bfdPort = k.bfd, k.bfdPort // from before our graceful restart
		delete(f.keptBFD, n.addr)
	}
	if n.bfd == nil {
		n.bfd = bfd.New(s.cfg.BFD.session(), rand.Uint32()|1, now)
		n.bfdPort = uint16(bfd.SrcPortMin + rand.Intn(65536-bfd.SrcPortMin))
	}
	n.bfd.SetSilent(now.Before(f.bfdSilentTill))
	was := n.bfd.State()
	out, _ := n.bfd.Tick(now)
	if out != nil {
		s.out = append(s.out, outPkt{ifc: f, dst: n.addr, mac: n.mac, bfd: out, sport: n.bfdPort})
	}
	s.bfdChangedLocked(n, was, now)
}

func (s *session) bfdChangedLocked(n *nbr, was byte, now time.Time) {
	st := n.bfd.State()
	if st == was {
		return
	}
	go s.event("bfd", fmt.Sprintf("BFD with %s on %s: %s → %s", n.rid, n.ifc.device, bfd.StateNames[was], bfd.StateNames[st]))
	if st == bfd.Up {
		s.count("bfd_up")
	}
	if was == bfd.Up && st == bfd.Down && n.bfd.PeerAdminDown() {
		s.count("bfd_peer_admin_down")
		return
	}
	if was == bfd.Up && st == bfd.Down {
		s.count("bfd_down")
		f := n.ifc
		s.nbrDownLocked(n, now, "BFD down")
		delete(f.nbrs, n.rid)
		if !f.noDR() {
			s.electLocked(f, now)
		}
	}
}

// rxBFD takes a BFD control packet (UDP 3784, TTL 255) for one of f's
// neighbors and reports whether the frame was one.
func (s *session) rxBFD(f *iface, fr *pkt.Frame) bool {
	var src, dst netip.Addr
	var ttl uint8
	var udp []byte
	if s.cd.v3 {
		ip, err := pkt.DecodeIPv6(fr.Payload)
		if err != nil || ip.NextHeader != pkt.ProtoUDP {
			return false
		}
		src, dst, ttl, udp = ip.Src, ip.Dst, ip.HopLimit, ip.Payload
	} else {
		ip, err := pkt.DecodeIPv4(fr.Payload)
		if err != nil || ip.Protocol != pkt.ProtoUDP || ip.FragOff != 0 {
			return false
		}
		src, dst, ttl, udp = ip.Src, ip.Dst, ip.TTL, ip.Payload
	}
	if len(udp) < 8 || binary.BigEndian.Uint16(udp[2:]) != bfd.Port {
		return false
	}
	p, err := bfd.Decode(udp[8:])
	s.mu.Lock()
	defer func() {
		out := s.takeOutLocked()
		s.mu.Unlock()
		s.flush(out)
	}()
	if mine, ok := s.ifSourceLocked(f); !ok || dst != mine {
		return true
	}
	if err != nil || ttl != 255 {
		s.count("rx_bfd_errors")
		return true
	}
	if k := f.keptBFD[src]; k != nil {
		k.bfd.Receive(p, time.Now())
		s.count("rx_bfd")
		return true
	}
	for _, n := range f.nbrs {
		if n.addr == src && n.bfd != nil {
			was := n.bfd.State()
			n.bfd.Receive(p, time.Now())
			s.count("rx_bfd")
			s.bfdChangedLocked(n, was, time.Now())
			return true
		}
	}
	s.count("rx_bfd_unknown")
	return true
}

// bfdFrame is a control packet in UDP and IP from src to dst (with mu
// held, for the IP ID).
func (s *session) bfdFrame(src, dst netip.Addr, sport uint16, b []byte) (uint16, []byte) {
	u := make([]byte, 8, 8+len(b))
	binary.BigEndian.PutUint16(u, sport)
	binary.BigEndian.PutUint16(u[2:], bfd.Port)
	binary.BigEndian.PutUint16(u[4:], uint16(8+len(b)))
	u = append(u, b...)
	c := pkt.Checksum(u, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(u)))
	if c == 0 {
		c = 0xffff
	}
	binary.BigEndian.PutUint16(u[6:], c)
	if s.cd.v3 {
		ip := pkt.IPv6{TrafficClass: 0xc0, NextHeader: pkt.ProtoUDP, HopLimit: 255, Src: src, Dst: dst, Payload: u}
		return pkt.EtherTypeIPv6, ip.Encode()
	}
	s.ipID++
	ip := pkt.IPv4{TOS: 0xc0, ID: s.ipID, TTL: 255, Protocol: pkt.ProtoUDP, Src: src, Dst: dst, Payload: u}
	return pkt.EtherTypeIPv4, ip.Encode()
}
