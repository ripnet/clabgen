package ospf

import (
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// An iface is one interface of the router: the instance's own device
// (iface 0, the top-level settings) or one of cfg.Interfaces, which the
// session attaches to (protocol.Env.Attach). Neighbors, hellos and the DR
// role are per interface; the LSDB is the router's (one area).
type iface struct {
	idx     int
	device  string
	p2p     bool
	passive bool
	cost    uint16
	prio    uint8
	auth    Auth
	mac     pkt.MAC

	att     protocol.Attachment
	locals  []func()
	err     string // why the interface is down
	authErr string // last authentication failure
	retryAt time.Time

	state     string
	dr, bdr   netip.Addr // interface addresses (zero: none)
	waitUntil time.Time
	nextHello time.Time
	nbrs      map[netip.Addr]*nbr // by router ID
	acks      []LSAHeader         // delayed acknowledgements
	ackAt     time.Time
	cryptoSeq uint32
	flapTill  time.Time
	flapMode  string
	ups       int // neighbors reaching Full
}

// Interface states.
const (
	ifDown    = "down"
	ifWaiting = "waiting"
	ifP2P     = "point-to-point"
	ifDROther = "dr-other"
	ifBackup  = "backup"
	ifDR      = "dr"
)

const attachRetry = time.Second

func (s *session) newIface(idx int, ic Interface) *iface {
	f := &iface{idx: idx, device: ic.Device, p2p: ic.Network == "point-to-point", passive: ic.Passive, cost: ic.Cost,
		prio: s.cfg.Priority, auth: s.cfg.Auth, state: ifDown, nbrs: map[netip.Addr]*nbr{}, dr: zero4, bdr: zero4,
		cryptoSeq: uint32(time.Now().Unix())}
	if ic.Network == "" {
		f.p2p = s.cfg.Network == "point-to-point"
	}
	if f.cost == 0 {
		f.cost = s.cfg.Cost
	}
	if ic.Priority != nil {
		f.prio = *ic.Priority
	}
	if ic.Auth != nil {
		f.auth = *ic.Auth
	}
	return f
}

func (f *iface) attached() bool { return f.idx == 0 || f.att != nil }

func (f *iface) network() string {
	if f.p2p {
		return "point-to-point"
	}
	return "broadcast"
}

func (s *session) ifIP(f *iface) protocol.IP {
	if f.idx == 0 {
		return s.env.IP()
	}
	if f.att != nil {
		return f.att.IP()
	}
	return nil
}

// ifAddrLocked is the interface's (first) IPv4 address.
func (s *session) ifAddrLocked(f *iface) (netip.Prefix, bool) {
	ip := s.ifIP(f)
	if ip == nil {
		return netip.Prefix{}, false
	}
	for _, p := range ip.Addresses() {
		if p.Addr().Is4() {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// ifUpLocked starts an interface: point-to-point at once, broadcast in
// Waiting (or straight to DR Other at priority 0).
func (s *session) ifUpLocked(f *iface, now time.Time) {
	if _, ok := s.ifAddrLocked(f); !ok {
		f.state, f.err = ifDown, "no IPv4 address"
		return
	}
	f.err, f.nextHello = "", now
	f.dr, f.bdr = zero4, zero4
	switch {
	case f.passive:
		f.state = ifP2P
		if !f.p2p {
			f.state = ifDROther
		}
	case f.p2p:
		f.state = ifP2P
	case f.prio == 0:
		f.state = ifDROther
	default:
		f.state = ifWaiting
		f.waitUntil = now.Add(time.Duration(s.cfg.DeadInterval) * time.Second)
	}
	s.dirty = true
}

func (s *session) ifDownLocked(f *iface, now time.Time, why string) {
	for _, n := range f.nbrs {
		s.nbrDownLocked(n, now, why)
	}
	f.nbrs = map[netip.Addr]*nbr{}
	f.state, f.err = ifDown, why
	f.dr, f.bdr = zero4, zero4
	s.dirty = true
}

// attachLoop attaches further interfaces that aren't (or no longer are),
// and notices attachments that closed. Engine calls happen without mu.
func (s *session) attachLoop(now time.Time) {
	var need []*iface
	s.mu.Lock()
	for _, f := range s.ifs {
		if f.idx > 0 && f.att != nil {
			select {
			case <-f.att.Closed():
				s.releaseLocalsLocked(f)
				f.att = nil
				f.retryAt = time.Time{}
				s.ifDownLocked(f, now, "device changed or deleted")
				go s.event("interface-down", fmt.Sprintf("interface %s: device changed or deleted", f.device))
			default:
			}
		}
		if f.idx > 0 && f.att == nil && !now.Before(f.retryAt) {
			f.retryAt = now.Add(attachRetry)
			need = append(need, f)
		}
		// An interface whose device had no address yet.
		if f.attached() && f.state == ifDown && f.err == "no IPv4 address" {
			s.ifUpLocked(f, now)
		}
	}
	s.mu.Unlock()
	for _, f := range need {
		f := f
		att, err := s.env.Attach(f.device, func(fr *pkt.Frame) { s.rx(f, fr) })
		s.mu.Lock()
		if err != nil {
			if f.err != err.Error() {
				f.err = err.Error()
				go s.event("interface-down", fmt.Sprintf("interface %s: %v", f.device, err))
			}
		} else {
			f.att, f.mac = att, att.Device().MAC()
			s.addLocalsLocked(f)
			s.ifUpLocked(f, now)
			go s.event("interface-up", fmt.Sprintf("interface %s attached", f.device))
		}
		s.mu.Unlock()
	}
}

func (s *session) addLocalsLocked(f *iface) {
	ip := s.ifIP(f)
	if ip == nil {
		return
	}
	for _, p := range s.loopbacks {
		f.locals = append(f.locals, ip.AddLocal(p.Addr()))
	}
}

func (s *session) releaseLocalsLocked(f *iface) {
	for _, r := range f.locals {
		r()
	}
	f.locals = nil
}

// ---- timers ----

func (s *session) ifTimersLocked(f *iface, now time.Time) {
	if f.passive {
		return
	}
	if f.state == ifWaiting && !now.Before(f.waitUntil) {
		s.electLocked(f, now)
	}
	if !now.Before(f.nextHello) {
		s.helloLocked(f, now)
	}
	for _, n := range f.sortedNbrs() {
		if now.Sub(n.lastHello) > n.dead {
			s.nbrDownLocked(n, now, "dead timer expired")
			delete(f.nbrs, n.rid)
			if !f.p2p {
				s.electLocked(f, now)
			}
			continue
		}
		s.nbrTimersLocked(n, now)
	}
	if len(f.acks) > 0 && !now.Before(f.ackAt) {
		s.sendAcksLocked(f)
	}
}

func (f *iface) flapping(now time.Time) bool { return now.Before(f.flapTill) }

// helloLocked sends a hello. While flapping in reset mode it lists no
// neighbors (they fall back to Init); in silent mode nothing is sent.
func (s *session) helloLocked(f *iface, now time.Time) {
	f.nextHello = now.Add(time.Duration(s.cfg.HelloInterval) * time.Second)
	flap := f.flapping(now)
	if flap && f.flapMode == "silent" {
		return
	}
	mine, ok := s.ifAddrLocked(f)
	if !ok {
		return
	}
	h := Hello{Mask: MaskOf(mine.Bits()), Interval: uint16(s.cfg.HelloInterval), Options: OptE, Priority: f.prio,
		Dead: uint32(s.cfg.DeadInterval), DR: f.dr, BDR: f.bdr}
	if f.p2p {
		h.Mask = zero4
	}
	if !flap {
		for _, n := range f.sortedNbrs() {
			h.Neighbors = append(h.Neighbors, n.rid)
		}
	}
	s.queue(f, AllSPFRouters, pkt.MAC{}, TypeHello, h.Encode())
}

func (f *iface) sortedNbrs() []*nbr {
	out := make([]*nbr, 0, len(f.nbrs))
	for _, n := range f.nbrs {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rid.Less(out[j].rid) })
	return out
}

// ---- hello processing (RFC 2328 10.5) ----

func (s *session) rxHelloLocked(f *iface, h *Header, src netip.Addr, mac pkt.MAC, hl *Hello, seq uint32, crypto bool, now time.Time) {
	if f.flapping(now) {
		return
	}
	mine, _ := s.ifAddrLocked(f)
	switch {
	case !f.p2p && hl.Mask != MaskOf(mine.Bits()):
		s.count("rx_hello_mismatch")
		f.authErr = fmt.Sprintf("hello from %s: netmask %s, ours %s", src, hl.Mask, MaskOf(mine.Bits()))
		return
	case int(hl.Interval) != s.cfg.HelloInterval || int(hl.Dead) != s.cfg.DeadInterval:
		s.count("rx_hello_mismatch")
		f.authErr = fmt.Sprintf("hello from %s: intervals %d/%d, ours %d/%d", src, hl.Interval, hl.Dead, s.cfg.HelloInterval, s.cfg.DeadInterval)
		return
	case hl.Options&OptE == 0:
		s.count("rx_hello_mismatch")
		f.authErr = fmt.Sprintf("hello from %s: E bit clear (stub area)", src)
		return
	}
	n := f.nbrs[h.RouterID]
	if n == nil {
		n = &nbr{ifc: f, rid: h.RouterID, state: nDown, since: now}
		f.nbrs[h.RouterID] = n
	}
	if crypto {
		if seq < n.cryptoSeq {
			s.count("rx_auth_failures")
			f.authErr = "sequence number went down"
			return
		}
		n.cryptoSeq = seq
	}
	oldPrio, oldDR, oldBDR, oldState := n.prio, n.dr, n.bdr, n.state
	n.addr, n.mac, n.prio, n.dr, n.bdr, n.options = src, mac, hl.Priority, hl.DR, hl.BDR, hl.Options
	n.lastHello, n.dead = now, time.Duration(hl.Dead)*time.Second
	if n.state == nDown {
		s.setNbrLocked(n, nInit, now, "hello received")
	}
	two := false
	for _, x := range hl.Neighbors {
		if x == s.rid {
			two = true
		}
	}
	switch {
	case !two && n.state >= n2Way:
		s.nbrResetLocked(n, nInit, now, "1-way: we're not in its hello")
	case two && n.state == nInit:
		s.twoWayLocked(n, now, false)
	}
	if f.p2p {
		return
	}
	if f.state == ifWaiting {
		if two && (hl.BDR == src || (hl.DR == src && hl.BDR == zero4)) {
			s.electLocked(f, now) // BackupSeen
		}
		return
	}
	// NeighborChange (RFC 2328 9.2): two-way contact gained or lost, a
	// changed priority, or a new or dropped claim to be DR or BDR.
	bidir := func(st int) bool { return st >= n2Way }
	if bidir(oldState) != bidir(n.state) || oldPrio != n.prio || (oldDR == src) != (n.dr == src) || (oldBDR == src) != (n.bdr == src) {
		s.electLocked(f, now)
	}
}

// twoWayLocked handles 2-WayReceived in Init (from a hello, or a DBD,
// which also shows the neighbor hears us): ExStart if we should be
// adjacent, else 2-Way. From a DBD it also runs the election, as the
// hello path does after its other checks.
func (s *session) twoWayLocked(n *nbr, now time.Time, elect bool) {
	f := n.ifc
	if s.shouldAdjLocked(f, n) {
		s.exStartLocked(n, now)
	} else {
		s.setNbrLocked(n, n2Way, now, "2-way")
	}
	if elect && !f.p2p && f.state != ifWaiting {
		s.electLocked(f, now) // NeighborChange: two-way contact
	}
}

// shouldAdjLocked: point-to-point always; broadcast with the DR and BDR
// (or when we are one of them).
func (s *session) shouldAdjLocked(f *iface, n *nbr) bool {
	if f.p2p {
		return true
	}
	if f.state == ifDR || f.state == ifBackup {
		return true
	}
	return n.addr == f.dr || n.addr == f.bdr
}

// ---- DR election (RFC 2328 9.4) ----

type candidate struct {
	rid, addr netip.Addr
	prio      uint8
	dr, bdr   netip.Addr // declared
}

func better(a, b candidate) bool {
	if a.prio != b.prio {
		return a.prio > b.prio
	}
	return b.rid.Less(a.rid)
}

func (s *session) electLocked(f *iface, now time.Time) {
	if f.p2p || f.state == ifDown || f.passive {
		return
	}
	mine, _ := s.ifAddrLocked(f)
	me := mine.Addr()
	oldDR, oldBDR, oldState := f.dr, f.bdr, f.state
	var cands []candidate
	self := func() candidate { return candidate{rid: s.rid, addr: me, prio: f.prio, dr: f.dr, bdr: f.bdr} }
	if f.prio > 0 {
		cands = append(cands, self())
	}
	for _, n := range f.nbrs {
		if n.state >= n2Way && n.prio > 0 {
			cands = append(cands, candidate{rid: n.rid, addr: n.addr, prio: n.prio, dr: n.dr, bdr: n.bdr})
		}
	}
	pick := func(cands []candidate) (dr, bdr netip.Addr) {
		// BDR: from those not declaring themselves DR, preferring those
		// declaring themselves BDR.
		var declared, best *candidate
		for i := range cands {
			c := &cands[i]
			if c.dr == c.addr {
				continue
			}
			if c.bdr == c.addr && (declared == nil || better(*c, *declared)) {
				declared = c
			}
			if best == nil || better(*c, *best) {
				best = c
			}
		}
		if declared != nil {
			best = declared
		}
		bdr = zero4
		if best != nil {
			bdr = best.addr
		}
		best = nil
		for i := range cands { // DR: from those declaring themselves DR
			c := &cands[i]
			if c.dr == c.addr && (best == nil || better(*c, *best)) {
				best = c
			}
		}
		if best != nil {
			return best.addr, bdr
		}
		return bdr, zero4
	}
	dr, bdr := pick(cands)
	// When we became or stopped being DR/BDR, run steps 2-3 again with our
	// new declaration.
	wasUs := oldDR == me || oldBDR == me
	isUs := dr == me || bdr == me
	if wasUs != isUs || (dr == me) != (oldDR == me) {
		f.dr, f.bdr = dr, bdr
		for i := range cands {
			if cands[i].rid == s.rid {
				cands[i] = self()
			}
		}
		dr, bdr = pick(cands)
	}
	f.dr, f.bdr = dr, bdr
	switch {
	case dr == me:
		f.state = ifDR
	case bdr == me:
		f.state = ifBackup
	default:
		f.state = ifDROther
	}
	if f.dr != oldDR || f.bdr != oldBDR || f.state != oldState {
		s.dirty = true
		go s.event("dr-change", fmt.Sprintf("%s: %s, DR %s, BDR %s", f.device, f.state, f.dr, f.bdr))
		if f.state != oldState {
			s.helloLocked(f, now)
		}
	}
	// AdjOK? for every neighbor: form or drop adjacencies for the roles.
	for _, n := range f.nbrs {
		want := s.shouldAdjLocked(f, n)
		switch {
		case n.state == n2Way && want:
			s.exStartLocked(n, now)
		case n.state >= nExStart && !want:
			s.nbrResetLocked(n, n2Way, now, "no longer adjacent (DR change)")
		}
	}
}

// ---- status ----

type IfStatus struct {
	Name      string `json:"name"`
	Network   string `json:"network"`
	State     string `json:"state"`
	Address   string `json:"address,omitempty"`
	Cost      uint16 `json:"cost"`
	Priority  uint8  `json:"priority,omitempty"`
	Passive   bool   `json:"passive,omitempty"`
	DR        string `json:"dr,omitempty"`
	BDR       string `json:"bdr,omitempty"`
	Neighbors int    `json:"neighbors"`
	Full      int    `json:"full"`
	Auth      string `json:"auth"`
	Error     string `json:"error,omitempty"`
	AuthError string `json:"last_error,omitempty"`
	Flapping  bool   `json:"flapping,omitempty"`
}

func (s *session) ifStatusLocked(f *iface, now time.Time) IfStatus {
	st := IfStatus{Name: f.device, Network: f.network(), State: f.state, Cost: f.cost, Passive: f.passive,
		Neighbors: len(f.nbrs), Auth: f.auth.Type, Error: f.err, AuthError: f.authErr, Flapping: f.flapping(now)}
	if st.Auth == "" {
		st.Auth = "none"
	}
	if a, ok := s.ifAddrLocked(f); ok {
		st.Address = a.String()
	}
	if !f.p2p {
		st.Priority = f.prio
		if f.dr != zero4 {
			st.DR = f.dr.String()
		}
		if f.bdr != zero4 {
			st.BDR = f.bdr.String()
		}
	}
	for _, n := range f.nbrs {
		if n.state == nFull {
			st.Full++
		}
	}
	return st
}
