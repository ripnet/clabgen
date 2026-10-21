package isis

import (
	"bytes"
	"fmt"
	"math/rand"
	"net/netip"
	"sort"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Broadcast (LAN) circuits, ISO 10589 8.4: per-level hellos to the level's
// multicast address listing the neighbors we hear, an adjacency per
// neighbor, DIS election, and (as DIS) a pseudonode LSP and periodic CSNPs.

// pnID is our pseudonode number on this (only) circuit.
const pnID byte = 1

type lanAdj struct {
	sys       SysID
	mac       pkt.MAC
	state     string // initializing or up
	priority  uint8
	lanID     NodeID // the LAN ID this neighbor announces
	hold      time.Duration
	lastHello time.Time
	since     time.Time
	areas     []string
	addrs     []string
	hostname  string
}

type lanLevel struct {
	n         int
	adjs      map[pkt.MAC]*lanAdj
	isDIS     bool
	disMAC    pkt.MAC
	lanID     NodeID // current LAN ID (ours if DIS)
	pnSeq     uint32
	nextHello time.Time
	elections int
}

func (s *session) lanDst(n int) pkt.MAC {
	if n == 1 {
		return AllL1ISs
	}
	return AllL2ISs
}

func (s *session) ourLANID() NodeID {
	var id NodeID
	copy(id[:], s.sys[:])
	id[6] = pnID
	return id
}

// lanHelloLocked builds our LAN hello for level n.
func (s *session) lanHelloLocked(n int, flapping bool) []byte {
	ll := s.lan[n]
	v4, v6ll, _ := s.addrs()
	var a4, a6 []netip.Addr
	for _, p := range v4 {
		a4 = append(a4, p.Addr())
	}
	for _, p := range v6ll {
		a6 = append(a6, p.Addr())
	}
	var nbrs []pkt.MAC
	if !flapping {
		for m := range ll.adjs {
			nbrs = append(nbrs, m)
		}
		sort.Slice(nbrs, func(i, j int) bool { return bytes.Compare(nbrs[i][:], nbrs[j][:]) < 0 })
	}
	hold := s.cfg.HelloInterval * s.cfg.HelloMultiplier
	if ll.isDIS {
		hold = max(1, hold/3) // the DIS hellos three times as often
	}
	padTo := 0
	if s.cfg.HelloPadding {
		padTo = 1497
	}
	return EncodeLANHello(n, s.cfg.circuitType(), s.sys, uint16(hold), s.cfg.Priority, ll.lanID, s.areas, a4, a6, nbrs, padTo)
}

// tickLANLocked sends due hellos and expires LAN adjacencies.
func (s *session) tickLANLocked(now time.Time, flapping bool, add func(dst pkt.MAC, b []byte, kind string)) {
	for _, n := range s.sortedLevels() {
		ll := s.lan[n]
		changed := false
		for m, a := range ll.adjs {
			if now.Sub(a.lastHello) > a.hold {
				delete(ll.adjs, m)
				changed = true
				s.lv[n].dirty = true
				if a.state == stUp {
					go s.event("adjacency-down", fmt.Sprintf("L%d adjacency with %s down: hold time expired", n, a.sys))
				}
			}
		}
		if changed {
			s.electLocked(n, now)
		}
		if flapping && s.flapMode == "silent" {
			continue
		}
		if !now.Before(ll.nextHello) {
			add(s.lanDst(n), s.lanHelloLocked(n, flapping), "hello")
			iv := time.Duration(s.cfg.HelloInterval) * time.Second
			if ll.isDIS {
				iv /= 3
			}
			ll.nextHello = now.Add(iv - time.Duration(rand.Int63n(int64(iv)/4+1)))
		}
	}
}

func (s *session) rxLANHello(f *pkt.Frame, p *PDU) {
	h := p.LAN
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count("rx_hello")
	ll := s.lan[h.Level]
	if ll == nil {
		s.count("rx_hello_level_mismatch")
		return
	}
	if now.Before(s.flapTill) {
		return
	}
	if h.Source == s.sys {
		s.count("rx_hello_duplicate_system_id")
		return
	}
	if h.Level == 1 {
		common := false
		for _, a := range p.Areas {
			for _, b := range s.areas {
				if bytes.Equal(a, b) {
					common = true
				}
			}
		}
		if !common {
			s.count("rx_hello_area_mismatch")
			return
		}
	}
	a := ll.adjs[f.Src]
	if a == nil {
		if len(ll.adjs) >= 256 {
			return
		}
		a = &lanAdj{mac: f.Src, state: stInitializing, since: now}
		ll.adjs[f.Src] = a
	}
	a.sys, a.priority, a.lanID = h.Source, h.Priority, h.LANID
	a.hold, a.lastHello = time.Duration(h.Hold)*time.Second, now
	a.areas, a.addrs = nil, nil
	for _, x := range p.Areas {
		a.areas = append(a.areas, AreaString(x))
	}
	for _, v := range p.TLVs.All(TLVIPAddresses) {
		for ; len(v) >= 4; v = v[4:] {
			a.addrs = append(a.addrs, netip.AddrFrom4([4]byte(v[:4])).String())
		}
	}
	for _, v := range p.TLVs.All(TLVIPv6Addresses) {
		for ; len(v) >= 16; v = v[16:] {
			a.addrs = append(a.addrs, netip.AddrFrom16([16]byte(v[:16])).String())
		}
	}
	// Up once the neighbor lists our MAC.
	seesUs := false
	for _, m := range h.Neighbors {
		if m == s.mac {
			seesUs = true
		}
	}
	next := stInitializing
	if seesUs {
		next = stUp
	}
	if next != a.state {
		prev := a.state
		a.state, a.since = next, now
		ll.nextHello = now // tell it we hear it, quickly
		s.lv[h.Level].dirty = true // our LSP and (as DIS) the pseudonode change
		if next == stUp {
			s.adjTransitions++
			go s.event("adjacency-up", fmt.Sprintf("L%d adjacency with %s up", h.Level, h.Source))
			l := s.lv[h.Level]
			l.lastCSNP = time.Time{}
			for _, e := range l.db {
				e.srm = true
			}
		} else if prev == stUp {
			s.adjTransitions++
			go s.event("adjacency-down", fmt.Sprintf("L%d adjacency with %s down: it no longer lists us", h.Level, h.Source))
		}
	}
	s.electLocked(h.Level, now)
}

// electLocked runs the DIS election for level n (ISO 10589 8.4.5): the
// highest priority, then the highest MAC, among us and Up neighbors. The
// LAN ID is ours if we win, else the one the winner announces.
func (s *session) electLocked(n int, now time.Time) {
	ll := s.lan[n]
	bestMAC, bestPrio, us := s.mac, s.cfg.Priority, true
	anyUp := false
	for m, a := range ll.adjs {
		if a.state != stUp {
			continue
		}
		anyUp = true
		if a.priority > bestPrio || a.priority == bestPrio && bytes.Compare(m[:], bestMAC[:]) > 0 {
			bestMAC, bestPrio, us = m, a.priority, false
		}
	}
	// Alone on the LAN we still announce ourselves as DIS, so neighbors
	// coming up converge on one LAN ID.
	was, wasID := ll.isDIS, ll.lanID
	ll.isDIS = us
	ll.disMAC = bestMAC
	if us {
		ll.lanID = s.ourLANID()
	} else {
		ll.lanID = ll.adjs[bestMAC].lanID
		// Until the winner names itself, use its system ID.
		if ll.lanID.Sys() != ll.adjs[bestMAC].sys {
			var id NodeID
			copy(id[:], ll.adjs[bestMAC].sys[:])
			ll.lanID = id
		}
	}
	if was != ll.isDIS || wasID != ll.lanID {
		ll.elections++
		s.lv[n].dirty = true
		ll.nextHello = now
		if anyUp || was {
			who := "us"
			if !us {
				who = ll.adjs[bestMAC].sys.String()
			}
			go s.event("dis-changed", fmt.Sprintf("L%d DIS is %s (LAN ID %s)", n, who, ll.lanID))
		}
	}
}

// lanAdjUp reports whether level n has any Up neighbor.
func (s *session) lanAdjUp(n int) bool {
	ll := s.lan[n]
	if ll == nil {
		return false
	}
	for _, a := range ll.adjs {
		if a.state == stUp {
			return true
		}
	}
	return false
}

// pseudonodeLocked originates (or purges) our pseudonode LSP for level n.
func (s *session) pseudonodeLocked(l *level, isType byte, now time.Time) {
	ll := s.lan[l.n]
	id := MakeLSPID(s.sys, pnID, 0)
	old := l.db[id]
	if !ll.isDIS || !s.lanAdjUp(l.n) {
		if old != nil && old.own && old.purgeAt.IsZero() {
			ll.pnSeq++
			l.db[id] = &dbEntry{lsp: mustLSP(purgeRaw(l.n, id, ll.pnSeq, isType)), rxAt: now, own: true, srm: true, purgeAt: now.Add(60 * time.Second)}
		}
		return
	}
	var nbrs []ISReach
	var me NodeID
	copy(me[:], s.sys[:])
	nbrs = append(nbrs, ISReach{me, 0})
	for _, a := range ll.adjs {
		if a.state == stUp {
			var nb NodeID
			copy(nb[:], a.sys[:])
			nbrs = append(nbrs, ISReach{nb, 0})
		}
	}
	sort.Slice(nbrs, func(i, j int) bool { return bytes.Compare(nbrs[i].Neighbor[:], nbrs[j].Neighbor[:]) < 0 })
	life := uint16(s.cfg.LSPLifetime)
	body := lspBody{Neighbors: nbrs}
	raw := EncodeLSP(l.n, id, ll.pnSeq, life, isType, body)
	if old == nil || !old.own || !sameContent(old.lsp.Raw, raw) || now.After(l.refreshAt) || ll.pnSeq == 0 || old.lsp.Seq != ll.pnSeq || !old.purgeAt.IsZero() {
		ll.pnSeq++
		raw = EncodeLSP(l.n, id, ll.pnSeq, life, isType, body)
		l.db[id] = &dbEntry{lsp: mustLSP(raw), rxAt: now, life: life, own: true, srm: true}
	}
}

// LANStatus is per-level DIS state.
type LANStatus struct {
	Level     int    `json:"level"`
	WeAreDIS  bool   `json:"we_are_dis"`
	DIS       string `json:"dis"`
	LANID     string `json:"lan_id"`
	Priority  uint8  `json:"priority"`
	Elections int    `json:"elections"`
}

func (s *session) lanStatusLocked(now time.Time) ([]AdjStatus, []LANStatus) {
	var adjs []AdjStatus
	var lans []LANStatus
	for _, n := range s.sortedLevels() {
		ll := s.lan[n]
		ls := LANStatus{Level: n, WeAreDIS: ll.isDIS, LANID: ll.lanID.String(), Priority: s.cfg.Priority, Elections: ll.elections, DIS: s.sys.String()}
		if !ll.isDIS {
			if a := ll.adjs[ll.disMAC]; a != nil {
				ls.DIS = a.sys.String()
			}
		}
		lans = append(lans, ls)
		macs := make([]pkt.MAC, 0, len(ll.adjs))
		for m := range ll.adjs {
			macs = append(macs, m)
		}
		sort.Slice(macs, func(i, j int) bool { return bytes.Compare(macs[i][:], macs[j][:]) < 0 })
		for _, m := range macs {
			a := ll.adjs[m]
			adjs = append(adjs, AdjStatus{State: a.state, Neighbor: a.sys.String(), MAC: m.String(), Levels: []int{n},
				HoldS: (a.hold - now.Sub(a.lastHello)).Seconds(), Since: a.since.Format(time.RFC3339), Areas: a.areas, Addresses: a.addrs,
				Priority: a.priority, DIS: ll.disMAC == m && !ll.isDIS})
		}
	}
	return adjs, lans
}
