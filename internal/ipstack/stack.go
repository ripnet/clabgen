// Package ipstack is the minimal host IP stack each device runs: its
// addresses, ARP and IPv6 neighbor discovery (answering, learning,
// resolving, announcing), and ICMP/ICMPv6 echo replies. It sits on a
// device's send function and receives the frames the engine dispatches to
// the device, so it is testable without ports.
package ipstack

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ripnet/clabgen/internal/flowrx"
	"github.com/ripnet/clabgen/internal/pkt"
)

// Config is a device's IP configuration.
type Config struct {
	// Addresses with their on-link prefix, IPv4 and IPv6 mixed. When any
	// IPv6 address is set and no fe80::/10 address is, a modified EUI-64
	// link-local address is added.
	Addresses []netip.Prefix
	Gateway4  netip.Addr
	Gateway6  netip.Addr
}

// ParseConfig parses and validates CIDR addresses and optional gateways.
func ParseConfig(addrs []string, gw4, gw6 string) (Config, error) {
	var c Config
	seen := map[netip.Addr]bool{}
	for _, s := range addrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return c, fmt.Errorf("address %q: want address/prefix-length, e.g. 10.0.0.2/24 or 2001:db8::2/64", s)
		}
		a := p.Addr()
		if a.Zone() != "" || a.Is4In6() {
			return c, fmt.Errorf("address %q: zones and IPv4-mapped addresses are not supported", s)
		}
		if a.IsMulticast() || a.IsUnspecified() || a.IsLoopback() {
			return c, fmt.Errorf("address %q is not a unicast host address", s)
		}
		if a.Is4() && p.Bits() < 31 {
			if a == p.Masked().Addr() {
				return c, fmt.Errorf("address %q is the subnet's network address", s)
			}
			if a == broadcast(p) {
				return c, fmt.Errorf("address %q is the subnet's broadcast address", s)
			}
		}
		if seen[a] {
			return c, fmt.Errorf("address %s is configured twice", a)
		}
		seen[a] = true
		c.Addresses = append(c.Addresses, p)
	}
	var err error
	if c.Gateway4, err = parseGateway(gw4, true, c.Addresses); err != nil {
		return c, err
	}
	if c.Gateway6, err = parseGateway(gw6, false, c.Addresses); err != nil {
		return c, err
	}
	return c, nil
}

func parseGateway(s string, v4 bool, prefixes []netip.Prefix) (netip.Addr, error) {
	if s == "" {
		return netip.Addr{}, nil
	}
	fam := map[bool]string{true: "gateway4", false: "gateway6"}[v4]
	g, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || g.Is4() != v4 || g.Zone() != "" {
		return g, fmt.Errorf("%s %q is not an IPv%s address", fam, s, map[bool]string{true: "4", false: "6"}[v4])
	}
	if !v4 && g.IsLinkLocalUnicast() {
		for _, p := range prefixes {
			if p.Addr().Is6() {
				return g, nil
			}
		}
		return g, fmt.Errorf("%s %s needs an IPv6 address on the device", fam, g)
	}
	for _, p := range prefixes {
		if p.Contains(g) {
			if p.Addr() == g {
				return g, fmt.Errorf("%s %s is the device's own address", fam, g)
			}
			return g, nil
		}
	}
	return g, fmt.Errorf("%s %s is not in any of the device's subnets", fam, g)
}

func broadcast(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) | (1<<(32-p.Bits()) - 1)
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// Strings renders the config for specs and scenarios.
func (c Config) Strings() (addrs []string, gw4, gw6 string) {
	for _, p := range c.Addresses {
		addrs = append(addrs, p.String())
	}
	if c.Gateway4.IsValid() {
		gw4 = c.Gateway4.String()
	}
	if c.Gateway6.IsValid() {
		gw6 = c.Gateway6.String()
	}
	return
}

// SendFunc transmits an Ethernet payload from the device.
type SendFunc func(dst pkt.MAC, etherType uint16, payload []byte) error

// EmitFunc publishes an event about the device.
type EmitFunc func(kind, msg string, data map[string]any)

const (
	reachableTime = 30 * time.Second
	resolveTries  = 3
	resolveWait   = time.Second
	maxNeighbors  = 4096
)

type neighbor struct {
	mac     pkt.MAC
	updated time.Time
	waiters []chan pkt.MAC
}

// Stack is one device's IP stack. All methods are safe for concurrent use.
type Stack struct {
	mac  pkt.MAC
	send SendFunc
	emit EmitFunc
	now  func() time.Time

	mu        sync.Mutex
	cfg       Config
	dyn       []*dynamic     // addresses and gateways protocols added (AddAddress)
	addrs     []netip.Prefix // cfg.Addresses, dyn's, and the automatic link-local
	gw4, gw6  netip.Addr     // the configured gateways, else the first of dyn's
	neighbors map[netip.Addr]*neighbor
	flows     *flowrx.Table      // signed flow packets received
	ownFlows  map[uint32]int     // flow IDs sent from this device (not recorded)
	locals    map[netip.Addr]int // AddLocal addresses (reference counts)
	tcp       *tcpStack          // netstack for TCP (created on first use)

	ipID     atomic.Uint32
	counters counters
}

// dynamic is one AddAddress call's address and gateway.
type dynamic struct {
	addr netip.Prefix
	gw   netip.Addr
}

func New(mac pkt.MAC, cfg Config, send SendFunc, emit EmitFunc) *Stack {
	s := &Stack{mac: mac, send: send, emit: emit, now: time.Now, neighbors: map[netip.Addr]*neighbor{}, flows: flowrx.New(), ownFlows: map[uint32]int{}, locals: map[netip.Addr]int{}}
	s.SetConfig(cfg)
	return s
}

// SetConfig replaces the addresses and gateways. Learned neighbors are kept.
func (s *Stack) SetConfig(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.syncLocked()
	s.mu.Unlock()
}

// syncLocked recomputes the effective addresses and gateways from the
// config and the dynamic ones. Caller holds mu.
func (s *Stack) syncLocked() {
	addrs := append([]netip.Prefix(nil), s.cfg.Addresses...)
	seen := map[netip.Addr]bool{}
	for _, p := range addrs {
		seen[p.Addr()] = true
	}
	s.gw4, s.gw6 = s.cfg.Gateway4, s.cfg.Gateway6
	for _, d := range s.dyn {
		if d.addr.IsValid() && !seen[d.addr.Addr()] {
			seen[d.addr.Addr()] = true
			addrs = append(addrs, d.addr)
		}
		if d.gw.Is4() && !s.gw4.IsValid() {
			s.gw4 = d.gw
		}
		if d.gw.Is6() && !s.gw6.IsValid() {
			s.gw6 = d.gw
		}
	}
	has6, hasLL := false, false
	for _, p := range addrs {
		if p.Addr().Is6() {
			has6 = true
			hasLL = hasLL || p.Addr().IsLinkLocalUnicast()
		}
	}
	if has6 && !hasLL {
		addrs = append(addrs, netip.PrefixFrom(pkt.LinkLocal(s.mac), 64))
	}
	s.addrs = addrs
	s.syncTCPAddrsLocked()
}

// Dynamic returns the addresses protocols added, and the gateways they
// added that are in use (no gateway of that family is configured).
func (s *Stack) Dynamic() (addrs []string, gw4, gw6 string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.dyn {
		if d.addr.IsValid() {
			addrs = append(addrs, d.addr.String())
		}
	}
	if s.gw4.IsValid() && !s.cfg.Gateway4.IsValid() {
		gw4 = s.gw4.String()
	}
	if s.gw6.IsValid() && !s.cfg.Gateway6.IsValid() {
		gw6 = s.gw6.String()
	}
	return
}

// AddAddress implements protocol.IP.
func (s *Stack) AddAddress(p netip.Prefix, gateway netip.Addr) func() {
	d := &dynamic{addr: p, gw: gateway.Unmap()}
	s.mu.Lock()
	_, had := s.own(p.Addr())
	s.dyn = append(s.dyn, d)
	s.syncLocked()
	s.mu.Unlock()
	if p.IsValid() && !had {
		s.announce(p.Addr())
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			for i, x := range s.dyn {
				if x == d {
					s.dyn = append(s.dyn[:i:i], s.dyn[i+1:]...)
					break
				}
			}
			s.syncLocked()
			s.mu.Unlock()
		})
	}
}

// Addresses returns the effective addresses, including the automatic
// link-local.
func (s *Stack) Addresses() []netip.Prefix {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]netip.Prefix(nil), s.addrs...)
}

// Source picks the address to send from toward dst: the one on dst's
// subnet, else the first of the family (link-local only for link-local
// destinations).
func (s *Stack) Source(dst netip.Addr) (netip.Addr, bool) {
	dst = dst.Unmap()
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.sourceFor(dst); ok {
		return a, true
	}
	for _, p := range s.addrs {
		a := p.Addr()
		if a.Is4() == dst.Is4() && !a.IsLinkLocalUnicast() {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// ResolveNextHop resolves the MAC to send to for dst (dst when on-link,
// else the gateway).
func (s *Stack) ResolveNextHop(ctx context.Context, dst netip.Addr) (netip.Addr, pkt.MAC, error) {
	nh, err := s.NextHop(dst)
	if err != nil {
		return nh, pkt.MAC{}, err
	}
	mac, _, err := s.Resolve(ctx, nh)
	return nh, mac, err
}

// Enabled reports whether the device has any address.
func (s *Stack) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.addrs) > 0
}

// Close fails any pending resolutions and ends TCP connections.
func (s *Stack) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tcp != nil {
		s.tcp.close()
		s.tcp = nil
	}
	for _, n := range s.neighbors {
		for _, w := range n.waiters {
			close(w)
		}
		n.waiters = nil
	}
}

// own returns the configured prefix holding a, if a is one of our addresses.
func (s *Stack) own(a netip.Addr) (netip.Prefix, bool) {
	for _, p := range s.addrs {
		if p.Addr() == a {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// forUs reports whether IP packets to a are ours: an address of the device
// or a local (loopback) one. Caller holds mu.
func (s *Stack) forUs(a netip.Addr) bool {
	_, ok := s.own(a)
	return ok || s.locals[a] > 0
}

// AddLocal implements protocol.IP.
func (s *Stack) AddLocal(a netip.Addr) func() {
	a = a.Unmap()
	s.mu.Lock()
	s.locals[a]++
	s.syncTCPAddrsLocked()
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.locals[a]--; s.locals[a] <= 0 {
				delete(s.locals, a)
				s.syncTCPAddrsLocked()
			}
			s.mu.Unlock()
		})
	}
}

// sourceFor picks our address to use toward an on-link neighbor.
func (s *Stack) sourceFor(dst netip.Addr) (netip.Addr, bool) {
	var fallback netip.Addr
	for _, p := range s.addrs {
		a := p.Addr()
		if a.Is4() != dst.Is4() {
			continue
		}
		if p.Contains(dst) && a.IsLinkLocalUnicast() == dst.IsLinkLocalUnicast() {
			return a, true
		}
		if dst.Is6() && dst.IsLinkLocalUnicast() && a.IsLinkLocalUnicast() {
			fallback = a
		}
	}
	return fallback, fallback.IsValid()
}

// NextHop returns the on-link address to resolve for dst: dst itself if it
// is in one of the device's subnets, otherwise the gateway.
func (s *Stack) NextHop(dst netip.Addr) (netip.Addr, error) {
	dst = dst.Unmap()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.addrs {
		if p.Contains(dst) {
			return dst, nil
		}
	}
	gw := s.gw6
	if dst.Is4() {
		gw = s.gw4
	}
	if !gw.IsValid() {
		return netip.Addr{}, fmt.Errorf("%s is not on-link and the device has no IPv%d gateway", dst, map[bool]int{true: 4, false: 6}[dst.Is4()])
	}
	return gw, nil
}

// HandleRx processes an ARP, IPv4 or IPv6 frame addressed to the device. It
// returns false if the frame was not for this device's IP stack.
func (s *Stack) HandleRx(f *pkt.Frame) bool {
	switch f.EtherType {
	case pkt.EtherTypeARP:
		return s.rxARP(f)
	case pkt.EtherTypeIPv4:
		return s.rxIPv4(f)
	case pkt.EtherTypeIPv6:
		return s.rxIPv6(f)
	}
	return false
}

// learn records ip → mac. With create false it only refreshes an existing
// entry (RFC 826's merge rule for ARP not aimed at us).
func (s *Stack) learn(ip netip.Addr, mac pkt.MAC, create bool) {
	if !ip.IsValid() || ip.IsUnspecified() || mac.IsMulticast() || mac.IsZero() {
		return
	}
	s.mu.Lock()
	n, ok := s.neighbors[ip]
	if !ok {
		if !create || len(s.neighbors) >= maxNeighbors {
			s.mu.Unlock()
			return
		}
		n = &neighbor{}
		s.neighbors[ip] = n
	}
	old := n.mac
	n.mac, n.updated = mac, s.now()
	waiters := n.waiters
	n.waiters = nil
	s.mu.Unlock()
	for _, w := range waiters {
		w <- mac
	}
	switch {
	case old.IsZero():
		s.emit("neighbor-added", fmt.Sprintf("neighbor %s is at %s", ip, mac), map[string]any{"ip": ip.String(), "mac": mac.String()})
	case old != mac:
		s.emit("neighbor-changed", fmt.Sprintf("neighbor %s moved from %s to %s", ip, old, mac), map[string]any{"ip": ip.String(), "mac": mac.String(), "old_mac": old.String()})
	}
}

// ---- ARP ----

func (s *Stack) rxARP(f *pkt.Frame) bool {
	a, err := pkt.DecodeARP(f.Payload)
	if err != nil {
		return false
	}
	s.mu.Lock()
	_, forUs := s.own(a.TPA)
	_, conflict := s.own(a.SPA)
	enabled := len(s.addrs) > 0
	s.mu.Unlock()
	if !enabled {
		return false
	}
	if conflict && a.SHA != s.mac {
		s.counters.addressConflicts.Add(1)
		s.emit("address-conflict", fmt.Sprintf("%s is also used by %s", a.SPA, a.SHA), map[string]any{"ip": a.SPA.String(), "mac": a.SHA.String()})
		return true
	}
	s.learn(a.SPA, a.SHA, forUs)
	switch a.Op {
	case pkt.ARPRequest:
		s.counters.arpRxRequests.Add(1)
		if forUs && a.SPA != a.TPA {
			reply := pkt.ARP{Op: pkt.ARPReply, SHA: s.mac, SPA: a.TPA, THA: a.SHA, TPA: a.SPA}
			if s.send(a.SHA, pkt.EtherTypeARP, reply.Encode()) == nil {
				s.counters.arpTxReplies.Add(1)
			}
		}
	case pkt.ARPReply:
		s.counters.arpRxReplies.Add(1)
	}
	return forUs
}

func (s *Stack) sendARPRequest(src, target netip.Addr) error {
	r := pkt.ARP{Op: pkt.ARPRequest, SHA: s.mac, SPA: src, TPA: target}
	err := s.send(pkt.Broadcast, pkt.EtherTypeARP, r.Encode())
	if err == nil {
		s.counters.arpTxRequests.Add(1)
	}
	return err
}

// ---- IPv4 ----

func (s *Stack) rxIPv4(f *pkt.Frame) bool {
	h, err := pkt.DecodeIPv4(f.Payload)
	if h == nil {
		return false
	}
	s.mu.Lock()
	forUs := s.forUs(h.Dst)
	s.mu.Unlock()
	if !forUs {
		return false
	}
	s.counters.ipv4Rx.Add(1)
	if err != nil {
		s.counters.ipv4Errors.Add(1)
		return true
	}
	if h.Flags&pkt.IPv4MF != 0 || h.FragOff != 0 {
		s.counters.ipv4Fragments.Add(1)
		return true
	}
	if h.Protocol == pkt.ProtoUDP {
		s.rxUDP(f, h.Src, h.Dst, h.Payload)
		return true
	}
	if h.Protocol == pkt.ProtoTCP {
		if !s.rxTCP(false, f.Payload[:int(binary.BigEndian.Uint16(f.Payload[2:]))]) {
			s.counters.ipv4Unhandled.Add(1)
		}
		return true
	}
	if h.Protocol != pkt.ProtoICMP {
		s.counters.ipv4Unhandled.Add(1)
		return true
	}
	m := h.Payload
	if len(m) < 8 || pkt.Checksum(m, 0) != 0 {
		s.counters.ipv4Errors.Add(1)
		return true
	}
	if m[0] != pkt.ICMPEchoRequest {
		s.counters.icmpRxOther.Add(1)
		return true
	}
	s.counters.icmpEchoRequests.Add(1)
	s.recordFlow(f, "icmp", h.Src, h.Dst, 0, 0, m[8:], len(m))
	reply := pkt.ICMPEcho(pkt.ICMPEchoReply, binary.BigEndian.Uint16(m[4:]), binary.BigEndian.Uint16(m[6:]), m[8:])
	out := pkt.IPv4{TOS: h.TOS, ID: uint16(s.ipID.Add(1)), TTL: 64, Protocol: pkt.ProtoICMP, Src: h.Dst, Dst: h.Src, Payload: reply}
	if s.send(f.Src, pkt.EtherTypeIPv4, out.Encode()) == nil {
		s.counters.icmpEchoReplies.Add(1)
	}
	return true
}

// ---- IPv6 ----

var allNodes = netip.MustParseAddr("ff02::1")

func (s *Stack) rxIPv6(f *pkt.Frame) bool {
	h, err := pkt.DecodeIPv6(f.Payload)
	if err != nil {
		return false
	}
	s.mu.Lock()
	forUs := s.forUs(h.Dst)
	if !forUs && len(s.addrs) > 0 && h.Dst.IsMulticast() {
		forUs = h.Dst == allNodes
		for _, p := range s.addrs {
			if p.Addr().Is6() && h.Dst == pkt.SolicitedNode(p.Addr()) {
				forUs = true
			}
		}
	}
	s.mu.Unlock()
	if !forUs {
		return false
	}
	s.counters.ipv6Rx.Add(1)
	if h.NextHeader == pkt.ProtoUDP && !h.Dst.IsMulticast() {
		s.rxUDP(f, h.Src, h.Dst, h.Payload)
		return true
	}
	if h.NextHeader == pkt.ProtoTCP && !h.Dst.IsMulticast() {
		if !s.rxTCP(true, f.Payload[:40+int(binary.BigEndian.Uint16(f.Payload[4:]))]) {
			s.counters.ipv6Unhandled.Add(1)
		}
		return true
	}
	if h.NextHeader != pkt.ProtoICMPv6 {
		s.counters.ipv6Unhandled.Add(1)
		return true
	}
	m := h.Payload
	if !pkt.ICMPv6Valid(h.Src, h.Dst, m) || len(m) < 4 {
		s.counters.ipv6Errors.Add(1)
		return true
	}
	switch m[0] {
	case pkt.ICMPv6EchoRequest:
		if h.Dst.IsMulticast() || len(m) < 8 {
			return true
		}
		s.counters.icmpv6EchoRequests.Add(1)
		s.recordFlow(f, "icmp", h.Src, h.Dst, 0, 0, m[8:], len(m))
		reply := append([]byte{pkt.ICMPv6EchoReply, 0, 0, 0}, m[4:]...)
		pkt.ICMPv6Checksum(h.Dst, h.Src, reply)
		out := pkt.IPv6{TrafficClass: h.TrafficClass, NextHeader: pkt.ProtoICMPv6, HopLimit: 64, Src: h.Dst, Dst: h.Src, Payload: reply}
		if s.send(f.Src, pkt.EtherTypeIPv6, out.Encode()) == nil {
			s.counters.icmpv6EchoReplies.Add(1)
		}
	case pkt.ICMPv6NeighborSol:
		s.rxNS(f, h)
	case pkt.ICMPv6NeighborAdv:
		s.rxNA(f, h)
	default:
		s.counters.icmpv6RxOther.Add(1)
	}
	return true
}

func (s *Stack) rxNS(f *pkt.Frame, h *pkt.IPv6) {
	m := h.Payload
	if h.HopLimit != 255 || len(m) < 24 || m[1] != 0 {
		s.counters.ipv6Errors.Add(1)
		return
	}
	s.counters.ndRxSolicits.Add(1)
	target := netip.AddrFrom16([16]byte(m[8:24]))
	sll, hasSLL := pkt.NDOption(m[24:], pkt.NDOptSourceLLAddr)
	s.mu.Lock()
	_, ours := s.own(target)
	s.mu.Unlock()
	if !ours {
		return
	}
	dad := h.Src.IsUnspecified()
	if dad && f.Src != s.mac {
		// Someone is probing for our address: defend it.
		s.counters.addressConflicts.Add(1)
		s.emit("address-conflict", fmt.Sprintf("%s probed for our address %s (DAD)", f.Src, target), map[string]any{"ip": target.String(), "mac": f.Src.String()})
	}
	if hasSLL && !dad {
		s.learn(h.Src, sll, true)
	}
	flags := pkt.NAFlagOverride
	dst, dstMAC := allNodes, pkt.MulticastMAC(allNodes)
	if !dad {
		flags |= pkt.NAFlagSolicited
		dst, dstMAC = h.Src, f.Src
		if hasSLL {
			dstMAC = sll
		}
	}
	s.sendND(pkt.ICMPv6NeighborAdv, flags, target, target, dst, dstMAC, pkt.NDOptTargetLLAddr)
}

func (s *Stack) rxNA(f *pkt.Frame, h *pkt.IPv6) {
	m := h.Payload
	if h.HopLimit != 255 || len(m) < 24 || m[1] != 0 {
		s.counters.ipv6Errors.Add(1)
		return
	}
	s.counters.ndRxAdverts.Add(1)
	target := netip.AddrFrom16([16]byte(m[8:24]))
	s.mu.Lock()
	_, ours := s.own(target)
	s.mu.Unlock()
	mac := f.Src
	if tll, ok := pkt.NDOption(m[24:], pkt.NDOptTargetLLAddr); ok {
		mac = tll
	}
	if ours {
		if mac != s.mac {
			s.counters.addressConflicts.Add(1)
			s.emit("address-conflict", fmt.Sprintf("%s is also used by %s", target, mac), map[string]any{"ip": target.String(), "mac": mac.String()})
		}
		return
	}
	s.learn(target, mac, true)
}

func (s *Stack) sendND(typ uint8, flags uint32, target, src, dst netip.Addr, dstMAC pkt.MAC, opt uint8) error {
	msg := pkt.NDMessage(typ, flags, target, opt, s.mac)
	pkt.ICMPv6Checksum(src, dst, msg)
	out := pkt.IPv6{NextHeader: pkt.ProtoICMPv6, HopLimit: 255, Src: src, Dst: dst, Payload: msg}
	err := s.send(dstMAC, pkt.EtherTypeIPv6, out.Encode())
	if err == nil {
		if typ == pkt.ICMPv6NeighborSol {
			s.counters.ndTxSolicits.Add(1)
		} else {
			s.counters.ndTxAdverts.Add(1)
		}
	}
	return err
}

// ---- UDP and received flows ----

// rxUDP checks a datagram addressed to the device and records it if it
// carries a flow signature. Responders (e.g. udp-echo) see the frame too,
// as protocol plugins.
func (s *Stack) rxUDP(f *pkt.Frame, src, dst netip.Addr, b []byte) {
	if len(b) < 8 || int(binary.BigEndian.Uint16(b[4:])) < 8 || int(binary.BigEndian.Uint16(b[4:])) > len(b) {
		s.counters.udpErrors.Add(1)
		return
	}
	b = b[:binary.BigEndian.Uint16(b[4:])]
	// A zero IPv4 checksum means "none"; IPv6 requires one.
	if cs := binary.BigEndian.Uint16(b[6:]); (cs != 0 || dst.Is6()) && pkt.Checksum(b, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(b))) != 0 {
		s.counters.udpErrors.Add(1)
		return
	}
	s.counters.udpRx.Add(1)
	s.recordFlow(f, "udp", src, dst, binary.BigEndian.Uint16(b[0:]), binary.BigEndian.Uint16(b[2:]), b[8:], len(b))
}

func (s *Stack) recordFlow(f *pkt.Frame, proto string, src, dst netip.Addr, sport, dport uint16, data []byte, size int) {
	id, seq, sent, ok := flowrx.ParseSignature(data)
	if !ok {
		return
	}
	s.mu.Lock()
	own := s.ownFlows[id] > 0
	s.mu.Unlock()
	if own {
		return // our own flow's echo coming back
	}
	if f.Dst.IsMulticast() && f.FlowCounted {
		return // flooded: another device on the port counted it
	}
	f.FlowCounted = true
	rx := f.RxTime
	if rx.IsZero() {
		rx = s.now()
	}
	s.flows.Record(flowrx.Packet{FlowID: id, Seq: seq, Sent: sent, Rx: rx, Proto: proto, Src: src, Dst: dst, SrcPort: sport, DstPort: dport, Bytes: size})
}

// ClaimFlow implements protocol.IP.
func (s *Stack) ClaimFlow(id uint32) func() {
	s.mu.Lock()
	s.ownFlows[id]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.ownFlows[id]--; s.ownFlows[id] <= 0 {
				delete(s.ownFlows, id)
			}
			s.mu.Unlock()
		})
	}
}

// RecordL2 counts a signed raw L2 frame (an l2 stream) addressed to the
// device, and reports whether it was one.
func (s *Stack) RecordL2(f *pkt.Frame) bool {
	id, seq, sent, ok := flowrx.ParseSignature(f.Payload)
	if !ok {
		return false
	}
	s.mu.Lock()
	own := s.ownFlows[id] > 0
	s.mu.Unlock()
	if own {
		return true // our own stream's echo coming back
	}
	if f.Dst.IsMulticast() && f.FlowCounted {
		return true // flooded: another device on the port counted it
	}
	f.FlowCounted = true
	rx := f.RxTime
	if rx.IsZero() {
		rx = s.now()
	}
	s.flows.Record(flowrx.Packet{FlowID: id, Seq: seq, Sent: sent, Rx: rx, Proto: "l2", Bytes: 14 + 4*len(f.Tags) + len(f.Payload),
		SrcMAC: f.Src.String(), DstMAC: f.Dst.String(), EtherType: f.EtherType})
	return true
}

// ReceivedFlows returns the signed flows received by the device.
func (s *Stack) ReceivedFlows() []flowrx.Flow { return s.flows.Snapshot(s.now()) }

// ClearReceivedFlows forgets them.
func (s *Stack) ClearReceivedFlows() { s.flows.Clear() }

// ClearReceivedFlow forgets one flow ID.
func (s *Stack) ClearReceivedFlow(id uint32) { s.flows.ClearFlow(id) }

// ---- resolution and announcements ----

var ErrUnresolved = errors.New("no reply")

// Resolve returns the MAC of an on-link neighbor, sending ARP requests or
// neighbor solicitations if it is not known or no longer reachable.
// Cached reports whether the answer came from the cache.
func (s *Stack) Resolve(ctx context.Context, ip netip.Addr) (mac pkt.MAC, cached bool, err error) {
	ip = ip.Unmap()
	s.mu.Lock()
	src, ok := s.sourceFor(ip)
	if _, own := s.own(ip); own {
		s.mu.Unlock()
		return s.mac, true, nil
	}
	if !ok {
		s.mu.Unlock()
		return pkt.MAC{}, false, fmt.Errorf("%s is not in any of the device's subnets", ip)
	}
	if n := s.neighbors[ip]; n != nil && !n.mac.IsZero() && s.now().Sub(n.updated) < reachableTime {
		m := n.mac
		s.mu.Unlock()
		return m, true, nil
	}
	s.mu.Unlock()

	for try := 0; try < resolveTries; try++ {
		w := make(chan pkt.MAC, 1)
		s.mu.Lock()
		n := s.neighbors[ip]
		if n == nil {
			if len(s.neighbors) >= maxNeighbors {
				s.mu.Unlock()
				return pkt.MAC{}, false, errors.New("neighbor table full")
			}
			n = &neighbor{}
			s.neighbors[ip] = n
		}
		n.waiters = append(n.waiters, w)
		s.mu.Unlock()
		if ip.Is4() {
			err = s.sendARPRequest(src, ip)
		} else {
			sn := pkt.SolicitedNode(ip)
			err = s.sendND(pkt.ICMPv6NeighborSol, 0, ip, src, sn, pkt.MulticastMAC(sn), pkt.NDOptSourceLLAddr)
		}
		if err != nil {
			s.dropWaiter(ip, w)
			return pkt.MAC{}, false, err
		}
		t := time.NewTimer(resolveWait)
		select {
		case m, ok := <-w:
			t.Stop()
			if !ok {
				return pkt.MAC{}, false, errors.New("device stopped")
			}
			return m, false, nil
		case <-t.C:
			s.dropWaiter(ip, w)
		case <-ctx.Done():
			t.Stop()
			s.dropWaiter(ip, w)
			return pkt.MAC{}, false, ctx.Err()
		}
	}
	s.counters.resolveFailures.Add(1)
	s.mu.Lock()
	if n := s.neighbors[ip]; n != nil && n.mac.IsZero() && len(n.waiters) == 0 {
		delete(s.neighbors, ip)
	}
	s.mu.Unlock()
	return pkt.MAC{}, false, fmt.Errorf("resolve %s: %w after %d tries", ip, ErrUnresolved, resolveTries)
}

func (s *Stack) dropWaiter(ip netip.Addr, w chan pkt.MAC) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.neighbors[ip]; n != nil {
		for i, x := range n.waiters {
			if x == w {
				n.waiters = append(n.waiters[:i:i], n.waiters[i+1:]...)
				break
			}
		}
	}
}

// Announce sends a gratuitous ARP for each IPv4 address and an unsolicited
// neighbor advertisement for each IPv6 address, so switches and peers learn
// (or relearn) the device. The engine calls it when the device is created
// or changed and when its port comes up.
func (s *Stack) Announce() {
	s.mu.Lock()
	addrs := append([]netip.Prefix(nil), s.addrs...)
	s.mu.Unlock()
	for _, p := range addrs {
		s.announce(p.Addr())
	}
}

func (s *Stack) announce(a netip.Addr) {
	if a.Is4() {
		g := pkt.ARP{Op: pkt.ARPRequest, SHA: s.mac, SPA: a, TPA: a}
		if s.send(pkt.Broadcast, pkt.EtherTypeARP, g.Encode()) == nil {
			s.counters.arpTxGratuitous.Add(1)
		}
	} else {
		s.sendND(pkt.ICMPv6NeighborAdv, pkt.NAFlagOverride, a, a, allNodes, pkt.MulticastMAC(allNodes), pkt.NDOptTargetLLAddr)
	}
}

// ClearNeighbors empties the neighbor cache.
func (s *Stack) ClearNeighbors() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ip, n := range s.neighbors {
		if len(n.waiters) == 0 {
			delete(s.neighbors, ip)
		}
	}
}

// ---- status ----

type Neighbor struct {
	IP    string        `json:"ip"`
	MAC   string        `json:"mac,omitempty"`
	State string        `json:"state"` // reachable, stale, incomplete
	Age   time.Duration `json:"age"`
}

type Status struct {
	Addresses []string          `json:"addresses"`       // effective, incl. automatic link-local
	Local     []string          `json:"local,omitempty"` // also answered for (router loopbacks)
	Dynamic   []string          `json:"dynamic,omitempty"` // added by protocols (DHCP), also in Addresses
	Gateway4  string            `json:"gateway4,omitempty"`
	Gateway6  string            `json:"gateway6,omitempty"`
	Neighbors []Neighbor        `json:"neighbors"`
	Counters  map[string]uint64 `json:"counters"`
}

func (s *Stack) Status() Status {
	s.mu.Lock()
	st := Status{Neighbors: []Neighbor{}}
	for _, p := range s.addrs {
		st.Addresses = append(st.Addresses, p.String())
	}
	for a := range s.locals {
		st.Local = append(st.Local, a.String())
	}
	sort.Strings(st.Local)
	for _, d := range s.dyn {
		if d.addr.IsValid() {
			st.Dynamic = append(st.Dynamic, d.addr.String())
		}
	}
	if s.gw4.IsValid() {
		st.Gateway4 = s.gw4.String()
	}
	if s.gw6.IsValid() {
		st.Gateway6 = s.gw6.String()
	}
	now := s.now()
	for ip, n := range s.neighbors {
		nb := Neighbor{IP: ip.String(), State: "incomplete"}
		if !n.mac.IsZero() {
			nb.MAC, nb.Age = n.mac.String(), now.Sub(n.updated)
			nb.State = "reachable"
			if nb.Age >= reachableTime {
				nb.State = "stale"
			}
		}
		st.Neighbors = append(st.Neighbors, nb)
	}
	s.mu.Unlock()
	sort.Slice(st.Neighbors, func(i, j int) bool {
		a, b := netip.MustParseAddr(st.Neighbors[i].IP), netip.MustParseAddr(st.Neighbors[j].IP)
		return a.Less(b)
	})
	st.Counters = s.counters.snapshot()
	return st
}

type counters struct {
	arpRxRequests, arpRxReplies, arpTxRequests, arpTxReplies, arpTxGratuitous atomic.Uint64
	ndRxSolicits, ndRxAdverts, ndTxSolicits, ndTxAdverts                      atomic.Uint64
	ipv4Rx, ipv4Errors, ipv4Fragments, ipv4Unhandled                          atomic.Uint64
	ipv6Rx, ipv6Errors, ipv6Unhandled                                         atomic.Uint64
	icmpEchoRequests, icmpEchoReplies, icmpRxOther                            atomic.Uint64
	icmpv6EchoRequests, icmpv6EchoReplies, icmpv6RxOther                      atomic.Uint64
	resolveFailures, addressConflicts                                         atomic.Uint64
	udpRx, udpErrors                                                          atomic.Uint64
	tcpRx, tcpTx, tcpTxErrors                                                 atomic.Uint64
}

func (c *counters) snapshot() map[string]uint64 {
	m := map[string]uint64{
		"arp_rx_requests": c.arpRxRequests.Load(), "arp_rx_replies": c.arpRxReplies.Load(),
		"arp_tx_requests": c.arpTxRequests.Load(), "arp_tx_replies": c.arpTxReplies.Load(),
		"arp_tx_gratuitous": c.arpTxGratuitous.Load(),
		"nd_rx_solicits":    c.ndRxSolicits.Load(), "nd_rx_adverts": c.ndRxAdverts.Load(),
		"nd_tx_solicits": c.ndTxSolicits.Load(), "nd_tx_adverts": c.ndTxAdverts.Load(),
		"ipv4_rx": c.ipv4Rx.Load(), "ipv4_errors": c.ipv4Errors.Load(),
		"ipv4_fragments": c.ipv4Fragments.Load(), "ipv4_unhandled": c.ipv4Unhandled.Load(),
		"ipv6_rx": c.ipv6Rx.Load(), "ipv6_errors": c.ipv6Errors.Load(), "ipv6_unhandled": c.ipv6Unhandled.Load(),
		"icmp_echo_requests": c.icmpEchoRequests.Load(), "icmp_echo_replies": c.icmpEchoReplies.Load(),
		"icmp_rx_other":        c.icmpRxOther.Load(),
		"icmpv6_echo_requests": c.icmpv6EchoRequests.Load(), "icmpv6_echo_replies": c.icmpv6EchoReplies.Load(),
		"icmpv6_rx_other":  c.icmpv6RxOther.Load(),
		"resolve_failures": c.resolveFailures.Load(), "address_conflicts": c.addressConflicts.Load(),
		"udp_rx": c.udpRx.Load(), "udp_errors": c.udpErrors.Load(),
		"tcp_rx": c.tcpRx.Load(), "tcp_tx": c.tcpTx.Load(), "tcp_tx_errors": c.tcpTxErrors.Load(),
	}
	return m
}
