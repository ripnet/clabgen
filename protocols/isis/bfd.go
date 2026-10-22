package isis

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net/netip"
	"sort"
	"time"

	"github.com/ripnet/clabgen/internal/bfd"
	"github.com/ripnet/clabgen/internal/pkt"
)

// BFD for IS-IS adjacencies (RFC 5882; single hop, RFC 5881): a session per
// neighbor and address family once the adjacency is up. With the
// BFD-enabled TLV (RFC 6213) in both sides' hellos, the sessions start
// before that and the adjacency waits for them. A session that fails
// after being up drops the neighbor's adjacencies at once.

type BFDConfig struct {
	Enabled    bool     `json:"enabled"`
	MinTxMs    int      `json:"min_tx_ms,omitempty" jsonschema:"minimum=100,maximum=10000,description=Desired transmit interval. Default 300"`
	MinRxMs    int      `json:"min_rx_ms,omitempty" jsonschema:"minimum=100,maximum=10000,description=Required receive interval. Default 300"`
	Multiplier int      `json:"multiplier,omitempty" jsonschema:"minimum=2,maximum=255,description=Detection multiplier. Default 3"`
	Families   []string `json:"families,omitempty" jsonschema:"enum=ipv4,enum=ipv6,description=Address families with a session per neighbor (IPv6 over link-local addresses). Default ipv4"`
	TLV        bool     `json:"tlv" jsonschema:"description=Send the BFD-enabled TLV (RFC 6213): with a neighbor that sends it too, the adjacency comes up only once BFD is up"`
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
	seen := map[string]bool{}
	for _, f := range c.Families {
		if f != "ipv4" && f != "ipv6" || seen[f] {
			return fmt.Errorf("bfd: families are ipv4 and ipv6, once each")
		}
		seen[f] = true
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

// families returns the NLPIDs BFD runs over.
func (c BFDConfig) families() []byte {
	if len(c.Families) == 0 {
		return []byte{NLPIDIPv4}
	}
	var out []byte
	for _, f := range c.Families {
		if f == "ipv6" {
			out = append(out, NLPIDIPv6)
		} else {
			out = append(out, NLPIDIPv4)
		}
	}
	return out
}

func famName(f byte) string {
	if f == NLPIDIPv6 {
		return "ipv6"
	}
	return "ipv4"
}

// bfdKey names a session: the neighbor's MAC and the address family.
type bfdKey struct {
	mac pkt.MAC
	fam byte
}

type bfdPeer struct {
	sess  *bfd.Session
	addr  netip.Addr // the neighbor's
	src   netip.Addr // ours
	sport uint16
}

// nbrAddrs are the addresses a neighbor's hellos list.
type nbrAddrs struct {
	v4   []netip.Addr
	v6ll []netip.Addr
	bfd  []BFDFamily // its BFD-enabled TLV
}

// parseNbrAddrs reads a hello's interface addresses and BFD-enabled TLV.
func parseNbrAddrs(ts TLVs) nbrAddrs {
	var a nbrAddrs
	for _, v := range ts.All(TLVIPAddresses) {
		for ; len(v) >= 4; v = v[4:] {
			a.v4 = append(a.v4, netip.AddrFrom4([4]byte(v[:4])))
		}
	}
	for _, v := range ts.All(TLVIPv6Addresses) {
		for ; len(v) >= 16; v = v[16:] {
			a.v6ll = append(a.v6ll, netip.AddrFrom16([16]byte(v[:16])))
		}
	}
	a.bfd = parseBFDEnabled(ts)
	return a
}

// ourBFDFamilies are the <MT, NLPID> pairs we run BFD for (RFC 6213).
func (s *session) ourBFDFamilies() []BFDFamily {
	var out []BFDFamily
	for _, f := range s.cfg.BFD.families() {
		mt := MTIPv4Unicast
		if f == NLPIDIPv6 {
			for _, m := range s.mts {
				if m == MTIPv6Unicast {
					mt = MTIPv6Unicast
				}
			}
		}
		out = append(out, BFDFamily{MT: mt, NLPID: f})
	}
	return out
}

// bfdHello is the BFD-enabled TLV for c's hellos (nil: none).
func (s *session) bfdHello(c *circuit) []TLV {
	if !c.bfd || !s.cfg.BFD.TLV {
		return nil
	}
	return []TLV{bfdEnabledTLV(s.ourBFDFamilies())}
}

// bfdHolds reports whether a neighbor's adjacency waits for BFD: both
// sides send the BFD-enabled TLV for a common pair (RFC 6213 3.2).
func (s *session) bfdHolds(c *circuit, a nbrAddrs) bool {
	if !c.bfd || !s.cfg.BFD.TLV {
		return false
	}
	for _, x := range s.ourBFDFamilies() {
		for _, y := range a.bfd {
			if x == y {
				return true
			}
		}
	}
	return false
}

// bfdTarget is where a session for a neighbor and family goes, if it can.
func (s *session) bfdTarget(c *circuit, a nbrAddrs, fam byte) (dst, src netip.Addr, ok bool) {
	ip := s.circuitIP(c)
	if ip == nil {
		return
	}
	if fam == NLPIDIPv4 {
		for _, n := range a.v4 {
			for _, p := range ip.Addresses() {
				if p.Addr().Is4() && p.Contains(n) {
					return n, p.Addr(), true
				}
			}
		}
		return
	}
	if len(a.v6ll) == 0 {
		return
	}
	for _, p := range ip.Addresses() {
		if p.Addr().IsLinkLocalUnicast() {
			return a.v6ll[0], p.Addr(), true
		}
	}
	return
}

// bfdUpLocked reports whether every session we run toward mac on c is up
// (and there is one).
func (c *circuit) bfdUp(mac pkt.MAC) bool {
	n := 0
	for k, p := range c.bfdPeers {
		if k.mac != mac {
			continue
		}
		n++
		if p.sess.State() != bfd.Up {
			return false
		}
	}
	return n > 0
}

// bfdTickLocked keeps c's sessions in line with its adjacencies, sends
// what's due, and acts on failures.
func (s *session) bfdTickLocked(c *circuit, now time.Time, send func(c *circuit, dst pkt.MAC, et uint16, b []byte)) {
	if !c.bfd || !c.attached() || c.passive {
		c.bfdPeers = nil
		return
	}
	type want struct {
		dst, src netip.Addr
	}
	wants := map[bfdKey]want{}
	consider := func(mac pkt.MAC, state string, a nbrAddrs) {
		if state != stUp && !(state == stInitializing && s.bfdHolds(c, a)) {
			return
		}
		for _, f := range s.cfg.BFD.families() {
			if dst, src, ok := s.bfdTarget(c, a, f); ok {
				wants[bfdKey{mac, f}] = want{dst, src}
			}
		}
	}
	if c.lan {
		for _, ll := range c.lans {
			for m, a := range ll.adjs {
				consider(m, a.state, a.nbr)
			}
		}
	} else if c.adj.state != stDown {
		consider(c.adj.mac, c.adj.state, c.adj.nbr)
	}
	if c.bfdPeers == nil {
		c.bfdPeers = map[bfdKey]*bfdPeer{}
	}
	for k, p := range c.bfdPeers {
		if w, ok := wants[k]; !ok || w.dst != p.addr || w.src != p.src {
			delete(c.bfdPeers, k)
		}
	}
	keys := make([]bfdKey, 0, len(wants))
	for k := range wants {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return string(keys[i].mac[:])+string(keys[i].fam) < string(keys[j].mac[:])+string(keys[j].fam)
	})
	for _, k := range keys {
		p := c.bfdPeers[k]
		if p == nil {
			w := wants[k]
			p = &bfdPeer{sess: bfd.New(s.cfg.BFD.session(), rand.Uint32()|1, now), addr: w.dst, src: w.src,
				sport: uint16(bfd.SrcPortMin + rand.Intn(65536-bfd.SrcPortMin))}
			c.bfdPeers[k] = p
		}
		p.sess.SetSilent(now.Before(c.bfdSilentTill))
		was := p.sess.State()
		if out, _ := p.sess.Tick(now); out != nil {
			et, b := s.bfdFrameLocked(p.src, p.addr, p.sport, out)
			send(c, k.mac, et, b)
		}
		s.bfdChangedLocked(c, k, p, was, now)
	}
}

func (s *session) bfdChangedLocked(c *circuit, k bfdKey, p *bfdPeer, was byte, now time.Time) {
	st := p.sess.State()
	if st == was {
		return
	}
	go s.event("bfd", fmt.Sprintf("BFD (%s) with %s on %s: %s → %s", famName(k.fam), p.addr, c.device, bfd.StateNames[was], bfd.StateNames[st]))
	switch {
	case st == bfd.Up:
		s.count("bfd_up")
		// An adjacency waiting for BFD may come up: answer its next hello now.
		c.nextHelo = now
		for _, ll := range c.lans {
			ll.nextHello = now
		}
	case was == bfd.Up && st == bfd.Down && p.sess.PeerAdminDown():
		s.count("bfd_peer_admin_down")
	case was == bfd.Up && st == bfd.Down:
		s.count("bfd_down")
		delete(c.bfdPeers, k)
		s.dropNeighborLocked(c, k.mac, now, "BFD down")
	}
}

// dropNeighborLocked takes down every adjacency with the neighbor at mac.
func (s *session) dropNeighborLocked(c *circuit, mac pkt.MAC, now time.Time, why string) {
	if !c.lan {
		if c.adj.state != stDown && c.adj.mac == mac {
			s.adjDownLocked(c, now, why)
		}
		return
	}
	for _, n := range s.sortedLevels() {
		ll := c.lans[n]
		a := ll.adjs[mac]
		if a == nil {
			continue
		}
		delete(ll.adjs, mac)
		if a.state == stUp {
			c.transitions++
			go s.event("adjacency-down", fmt.Sprintf("%s L%d adjacency with %s down: %s", c.device, n, a.sys, why))
		}
		s.electLocked(c, n, now)
		s.lv[n].dirty = true
	}
}

// rxBFD takes a BFD control packet (UDP 3784, TTL 255) for one of c's
// sessions.
func (s *session) rxBFD(c *circuit, fr *pkt.Frame) {
	var src, dst netip.Addr
	var ttl uint8
	var udp []byte
	switch fr.EtherType {
	case pkt.EtherTypeIPv4:
		ip, err := pkt.DecodeIPv4(fr.Payload)
		if err != nil || ip.Protocol != pkt.ProtoUDP || ip.FragOff != 0 {
			return
		}
		src, dst, ttl, udp = ip.Src, ip.Dst, ip.TTL, ip.Payload
	case pkt.EtherTypeIPv6:
		ip, err := pkt.DecodeIPv6(fr.Payload)
		if err != nil || ip.NextHeader != pkt.ProtoUDP {
			return
		}
		src, dst, ttl, udp = ip.Src, ip.Dst, ip.HopLimit, ip.Payload
	default:
		return
	}
	if len(udp) < 8 || binary.BigEndian.Uint16(udp[2:]) != bfd.Port {
		return
	}
	p, err := bfd.Decode(udp[8:])
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !c.bfd {
		return
	}
	for k, x := range c.bfdPeers {
		if x.addr != src || x.src != dst || k.mac != fr.Src {
			continue
		}
		if err != nil || ttl != 255 {
			s.count("rx_bfd_errors")
			return
		}
		was := x.sess.State()
		x.sess.Receive(p, now)
		s.count("rx_bfd")
		s.bfdChangedLocked(c, k, x, was, now)
		return
	}
	s.count("rx_bfd_unknown")
}

// bfdFrameLocked is a control packet in UDP and IP from src to dst.
func (s *session) bfdFrameLocked(src, dst netip.Addr, sport uint16, b []byte) (uint16, []byte) {
	u := make([]byte, 8, 8+len(b))
	binary.BigEndian.PutUint16(u, sport)
	binary.BigEndian.PutUint16(u[2:], bfd.Port)
	binary.BigEndian.PutUint16(u[4:], uint16(8+len(b)))
	u = append(u, b...)
	cs := pkt.Checksum(u, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(u)))
	if cs == 0 {
		cs = 0xffff
	}
	binary.BigEndian.PutUint16(u[6:], cs)
	if dst.Is6() {
		ip := pkt.IPv6{TrafficClass: 0xc0, NextHeader: pkt.ProtoUDP, HopLimit: 255, Src: src, Dst: dst, Payload: u}
		return pkt.EtherTypeIPv6, ip.Encode()
	}
	s.ipID++
	ip := pkt.IPv4{TOS: 0xc0, ID: s.ipID, TTL: 255, Protocol: pkt.ProtoUDP, Src: src, Dst: dst, Payload: u}
	return pkt.EtherTypeIPv4, ip.Encode()
}

// BFDPeerStatus is one BFD session of an adjacency.
type BFDPeerStatus struct {
	Family  string `json:"family"`
	Address string `json:"address"`
	*bfd.Status
}

func (c *circuit) bfdStatus(mac pkt.MAC) []BFDPeerStatus {
	var out []BFDPeerStatus
	for k, p := range c.bfdPeers {
		if k.mac == mac {
			st := p.sess.Status()
			out = append(out, BFDPeerStatus{Family: famName(k.fam), Address: p.addr.String(), Status: &st})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Family < out[j].Family })
	return out
}
