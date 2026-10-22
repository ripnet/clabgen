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
	mts       []uint16
}

type lanLevel struct {
	n         int
	adjs      map[pkt.MAC]*lanAdj
	isDIS     bool
	disMAC    pkt.MAC
	lanID     NodeID // current LAN ID (ours if DIS)
	pnSeq     uint32
	pnForce   bool // re-originate the pseudonode even if unchanged
	nextHello time.Time
	lastCSNP  time.Time
	elections int
}

func (s *session) lanDst(n int) pkt.MAC {
	if n == 1 {
		return AllL1ISs
	}
	return AllL2ISs
}

func (s *session) ourLANID(c *circuit) NodeID {
	var id NodeID
	copy(id[:], s.sys[:])
	id[6] = c.pn
	return id
}

// lanHelloLocked builds our LAN hello for level n.
func (s *session) lanHelloLocked(c *circuit, n int, flapping bool) []byte {
	ll := c.lans[n]
	v4, v6ll, _ := s.addrs(c)
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
		padTo = 1497 - c.helloAuth.tlvLen()
	}
	return c.helloAuth.sign(EncodeLANHello(n, s.cfg.circuitType(), s.sys, uint16(hold), c.priority, ll.lanID, s.areas, a4, a6, s.mts, nbrs, padTo))
}

// tickLANLocked sends due hellos and expires LAN adjacencies.
func (s *session) tickLANLocked(c *circuit, now time.Time, flapping bool, add func(c *circuit, dst pkt.MAC, b []byte, kind string)) {
	for _, n := range s.sortedLevels() {
		ll := c.lans[n]
		changed := false
		for m, a := range ll.adjs {
			if now.Sub(a.lastHello) > a.hold {
				delete(ll.adjs, m)
				changed = true
				s.lv[n].dirty = true
				if a.state == stUp {
					c.transitions++
					go s.event("adjacency-down", fmt.Sprintf("%s L%d adjacency with %s down: hold time expired", c.device, n, a.sys))
				}
			}
		}
		if changed {
			s.electLocked(c, n, now)
		}
		if flapping && c.flapMode == "silent" {
			continue
		}
		if !now.Before(ll.nextHello) {
			add(c, s.lanDst(n), s.lanHelloLocked(c, n, flapping), "hello")
			iv := time.Duration(s.cfg.HelloInterval) * time.Second
			if ll.isDIS {
				iv /= 3
			}
			ll.nextHello = now.Add(iv - time.Duration(rand.Int63n(int64(iv)/4+1)))
		}
	}
}

func (s *session) rxLANHello(c *circuit, f *pkt.Frame, p *PDU) {
	h := p.LAN
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count("rx_hello")
	ll := c.lans[h.Level]
	if ll == nil {
		s.count("rx_hello_level_mismatch")
		return
	}
	if now.Before(c.flapTill) {
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
	mts, ok := s.commonMTs(p)
	if !ok {
		s.count("rx_hello_mt_mismatch")
		return
	}
	a := ll.adjs[f.Src]
	if a == nil {
		if len(ll.adjs) >= 256 {
			return
		}
		a = &lanAdj{mac: f.Src, state: stInitializing, since: now}
		ll.adjs[f.Src] = a
	}
	a.sys, a.priority, a.lanID, a.mts = h.Source, h.Priority, h.LANID, mts
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
		if m == c.mac {
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
		ll.nextHello = now         // tell it we hear it, quickly
		s.lv[h.Level].dirty = true // our LSP and (as DIS) the pseudonode change
		if next == stUp {
			c.transitions++
			go s.event("adjacency-up", fmt.Sprintf("%s L%d adjacency with %s up", c.device, h.Level, h.Source))
			ll.lastCSNP = time.Time{}
			for _, e := range s.lv[h.Level].db {
				e.flag(c.idx)
			}
		} else if prev == stUp {
			c.transitions++
			go s.event("adjacency-down", fmt.Sprintf("%s L%d adjacency with %s down: it no longer lists us", c.device, h.Level, h.Source))
		}
	}
	s.electLocked(c, h.Level, now)
}

// electLocked runs the DIS election for level n (ISO 10589 8.4.5): the
// highest priority, then the highest MAC, among us and Up neighbors. The
// LAN ID is ours if we win, else the one the winner announces.
func (s *session) electLocked(c *circuit, n int, now time.Time) {
	ll := c.lans[n]
	bestMAC, bestPrio, us := c.mac, c.priority, true
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
		ll.lanID = s.ourLANID(c)
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
			go s.event("dis-changed", fmt.Sprintf("%s L%d DIS is %s (LAN ID %s)", c.device, n, who, ll.lanID))
		}
	}
}

// lanAdjUp reports whether level n of a LAN circuit has any Up neighbor.
func (c *circuit) lanAdjUp(n int) bool {
	ll := c.lans[n]
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

// pseudonodeLocked originates (or purges) our pseudonode LSP for a LAN
// circuit at level n.
func (s *session) pseudonodeLocked(c *circuit, l *level, isType byte, now time.Time) {
	ll := c.lans[l.n]
	id := MakeLSPID(s.sys, c.pn, 0)
	old := l.db[id]
	if !ll.isDIS || !c.lanAdjUp(l.n) {
		if old != nil && old.own && old.purgeAt.IsZero() {
			ll.pnSeq++
			l.db[id] = &dbEntry{lsp: mustLSP(s.signLSP(purgeRaw(l.n, id, ll.pnSeq, isType))), rxAt: now, own: true, srm: s.allSRM(), purgeAt: now.Add(60 * time.Second)}
		}
		return
	}
	var nbrs []ISReach
	var me NodeID
	copy(me[:], s.sys[:])
	nbrs = append(nbrs, ISReach{Neighbor: me})
	for _, a := range ll.adjs {
		if a.state == stUp {
			var nb NodeID
			copy(nb[:], a.sys[:])
			nbrs = append(nbrs, ISReach{Neighbor: nb})
		}
	}
	sort.Slice(nbrs, func(i, j int) bool { return bytes.Compare(nbrs[i].Neighbor[:], nbrs[j].Neighbor[:]) < 0 })
	life := uint16(s.cfg.LSPLifetime)
	body := lspBody{Neighbors: nbrs}
	raw := s.signLSP(EncodeLSP(l.n, id, ll.pnSeq, life, isType, body))
	if old == nil || !old.own || !sameContent(old.lsp.Raw, raw) || now.After(l.refreshAt) || ll.pnSeq == 0 || old.lsp.Seq != ll.pnSeq || !old.purgeAt.IsZero() || ll.pnForce {
		ll.pnForce = false
		ll.pnSeq++
		raw = s.signLSP(EncodeLSP(l.n, id, ll.pnSeq, life, isType, body))
		l.db[id] = &dbEntry{lsp: mustLSP(raw), rxAt: now, life: life, own: true, srm: s.allSRM()}
	}
}

// LANStatus is per-level DIS state.
type LANStatus struct {
	Interface string `json:"interface"`
	Level     int    `json:"level"`
	WeAreDIS  bool   `json:"we_are_dis"`
	DIS       string `json:"dis"`
	LANID     string `json:"lan_id"`
	Priority  uint8  `json:"priority"`
	Elections int    `json:"elections"`
}

func (s *session) lanStatusLocked(c *circuit, now time.Time) ([]AdjStatus, []LANStatus) {
	var adjs []AdjStatus
	var lans []LANStatus
	for _, n := range s.sortedLevels() {
		ll := c.lans[n]
		ls := LANStatus{Interface: c.device, Level: n, WeAreDIS: ll.isDIS, LANID: ll.lanID.String(), Priority: c.priority, Elections: ll.elections, DIS: s.sys.String()}
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
			adjs = append(adjs, AdjStatus{Interface: c.device, State: a.state, Neighbor: a.sys.String(), MAC: m.String(), Levels: []int{n}, Transitions: c.transitions,
				HoldS: (a.hold - now.Sub(a.lastHello)).Seconds(), Since: a.since.Format(time.RFC3339), Areas: a.areas, Addresses: a.addrs,
				Priority: a.priority, DIS: ll.disMAC == m && !ll.isDIS, Topologies: mtNameList(a.mts)})
		}
	}
	return adjs, lans
}
