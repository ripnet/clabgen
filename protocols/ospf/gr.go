package ospf

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"
)

// Graceful restart (RFC 3623; OSPFv3: RFC 5187). As a helper we keep a
// restarting neighbor in our LSAs while its grace LSA is in force, past
// its dead timer and through its new database exchange. As the restarting
// router (the graceful-restart action) we announce a grace period, drop
// all protocol state as a restart would, resynchronise without
// originating, and re-originate once the adjacencies we had are full again
// (or the grace period ends).

const (
	opqGrace     = 3      // OSPFv2 opaque type of the grace LSA (in a type-9 LSA)
	LSA3Grace    = 0x000b // OSPFv3 grace LSA (link scope)
	graceTLVTime = 1
	graceTLVWhy  = 2
	graceTLVAddr = 3 // OSPFv2 interface address
)

type GracefulRestart struct {
	Helper      bool `json:"helper" jsonschema:"description=Help restarting neighbors: keep them adjacent during their grace period"`
	GracePeriod int  `json:"grace_period,omitempty" jsonschema:"minimum=1,maximum=1800,description=Our grace period in seconds when restarting (graceful-restart action). Default 120"`
}

type GRArgs struct {
	Seconds int `json:"seconds,omitempty" jsonschema:"minimum=1,maximum=1800,description=Grace period. Default: the configured one"`
}

// gr is our own restart in progress.
type gr struct {
	phase   string // announcing, restarting
	at      time.Time
	until   time.Time
	period  int
	waitFor map[string]bool // interface/neighbor that were full
}

func (s *session) isGrace(t uint16, id netip.Addr) bool {
	if s.cd.v3 {
		return t == LSA3Grace
	}
	return t == LSAOpaqueLink && byte(u32(id)>>24) == opqGrace
}

func graceBody(period uint32, addr netip.Addr) []byte {
	b := tlv(graceTLVTime, binary.BigEndian.AppendUint32(nil, period))
	b = append(b, tlv(graceTLVWhy, []byte{1})...) // software restart
	if addr.IsValid() && addr.Is4() {
		b = append(b, tlv(graceTLVAddr, addr.AsSlice())...)
	}
	return b
}

func gracePeriodOf(b []byte) uint32 {
	for _, t := range readTLVs(b) {
		if t.t == graceTLVTime && len(t.v) >= 4 {
			return binary.BigEndian.Uint32(t.v)
		}
	}
	return 0
}

// adjacent: a full neighbor, or one we help through its restart.
func (n *nbr) adjacent(now time.Time) bool {
	return n.state == nFull || now.Before(n.graceUntil)
}

// rxGraceLocked handles a neighbor's grace LSA (installed in the LSDB):
// start helping, or stop when it is flushed.
func (s *session) rxGraceLocked(f *iface, l *lsa, now time.Time) {
	n := f.nbrs[l.hdr.AdvRtr]
	if n == nil {
		return
	}
	if l.ageAt(now) >= MaxAge {
		if now.Before(n.graceUntil) {
			n.graceUntil = time.Time{}
			s.dirty = true
			go s.event("gr-helper", fmt.Sprintf("neighbor %s on %s finished its graceful restart", n.rid, f.device))
		}
		return
	}
	if !s.cfg.GracefulRestart.Helper {
		s.count("gr_not_helping")
		return
	}
	if n.state != nFull && !now.Before(n.graceUntil) {
		s.count("gr_not_helping") // RFC 3623 3.1: it must be fully adjacent
		return
	}
	left := int(gracePeriodOf(l.body())) - int(l.ageAt(now))
	if left <= 0 {
		return
	}
	n.graceUntil = now.Add(time.Duration(left) * time.Second)
	s.count("gr_helping")
	go s.event("gr-helper", fmt.Sprintf("helping neighbor %s on %s through its restart (%d s)", n.rid, f.device, left))
}

// grTimersLocked ends helping when a grace period runs out, and drives our
// own restart.
func (s *session) grTimersLocked(now time.Time) {
	for _, f := range s.ifs {
		for _, n := range f.nbrs {
			if !n.graceUntil.IsZero() && !now.Before(n.graceUntil) {
				n.graceUntil = time.Time{}
				s.dirty = true
				if n.state != nFull {
					go s.event("gr-helper", fmt.Sprintf("neighbor %s on %s: grace period over before it was full again", n.rid, f.device))
				}
			}
		}
	}
	g := s.gr
	if g == nil {
		return
	}
	switch g.phase {
	case "announcing":
		acked := true
		for _, n := range s.allNbrs() {
			for k := range n.rxmt {
				if s.isGrace(k.Type, k.ID) {
					acked = false
				}
			}
		}
		if acked || !now.Before(g.at) {
			s.restartLocked(now)
		}
	case "restarting":
		done := true
		for k := range g.waitFor {
			done = done && s.fullAgain(k)
		}
		if done || !now.Before(g.until) {
			for _, f := range s.ifs {
				f.keptBFD = nil
			}
			why := "adjacencies full again"
			if !done {
				why = "grace period over"
			}
			s.gr = nil
			s.dirty, s.genAt = true, now
			s.count("gr_restarts")
			go s.event("gr-restart", "graceful restart done: "+why)
		}
	}
}

func (s *session) fullAgain(k string) bool {
	for _, f := range s.ifs {
		for _, n := range f.nbrs {
			if fmt.Sprintf("%d/%s", f.idx, n.rid) == k {
				return n.state == nFull
			}
		}
	}
	return false
}

// startGRLocked begins our graceful restart: grace LSAs on every
// interface with neighbors.
func (s *session) startGRLocked(a *GRArgs, now time.Time) (any, error) {
	if s.gr != nil {
		return nil, fmt.Errorf("a graceful restart is in progress (%s)", s.gr.phase)
	}
	period := s.cfg.GracefulRestart.GracePeriod
	if a.Seconds > 0 {
		period = a.Seconds
	}
	if period == 0 {
		period = 120
	}
	g := &gr{phase: "announcing", at: now.Add(2 * time.Second), period: period, waitFor: map[string]bool{}}
	for _, f := range s.ifs {
		if f.state == ifDown || f.passive {
			continue
		}
		full := false
		for _, n := range f.nbrs {
			if n.state == nFull {
				g.waitFor[fmt.Sprintf("%d/%s", f.idx, n.rid)] = true
				full = true
			}
		}
		if !full {
			continue
		}
		// The interface address identifies us on multi-access networks
		// (required there, RFC 3623 appendix A).
		src, _ := s.ifSourceLocked(f)
		if f.p2p {
			src = netip.Addr{}
		}
		h := LSAHeader{Type: LSAOpaqueLink, ID: opaqueID(opqGrace, 0), AdvRtr: s.rid, Seq: InitialSeq}
		if s.cd.v3 {
			h = LSAHeader{Type: LSA3Grace, ID: id4(f.ifID), AdvRtr: s.rid, Seq: InitialSeq}
			src = netip.Addr{}
		}
		k := s.keyFor(f, h)
		if cur := s.db[k]; cur != nil {
			h.Seq = cur.hdr.Seq + 1
		}
		l := s.installLocked(k, s.cd.buildLSA(h, graceBody(uint32(period), src)), now, true)
		s.floodLocked(l, nil, now)
	}
	if len(g.waitFor) == 0 {
		return nil, fmt.Errorf("no full adjacencies to restart gracefully with")
	}
	s.gr = g
	go s.event("gr-restart", fmt.Sprintf("graceful restart: grace period %d s", period))
	return map[string]any{"grace_period": period, "adjacencies": len(g.waitFor)}, nil
}

// restartLocked loses our protocol state as a restarting router would:
// neighbors and the LSDB (our own LSAs come back from the neighbors).
func (s *session) restartLocked(now time.Time) {
	g := s.gr
	g.phase, g.until = "restarting", now.Add(time.Duration(g.period)*time.Second)
	for _, f := range s.ifs {
		// BFD runs on (in the forwarding plane, independent of the
		// control plane): sessions carry over to the neighbors as they
		// come back.
		for _, n := range f.nbrs {
			if n.bfd != nil {
				if f.keptBFD == nil {
					f.keptBFD = map[netip.Addr]*nbr{}
				}
				f.keptBFD[n.addr] = &nbr{ifc: f, addr: n.addr, mac: n.mac, bfd: n.bfd, bfdPort: n.bfdPort}
			}
		}
		f.nbrs = map[netip.Addr]*nbr{}
		f.acks = nil
		// The interfaces keep their roles: a restarting DR is DR again as
		// soon as its neighbors' hellos say so (RFC 3623 2.2), and hellos
		// declaring no DR meanwhile would make the helpers re-elect. Our
		// first hello waits until we have heard the neighbors, so it lists
		// them: a hello without them is 1-way at the helpers, and on a LAN
		// that changes their DR election (a topology change ends helping).
		f.grQuiet = now.Add(time.Duration(f.deadIv) * time.Second / 2)
	}
	s.db = map[LSAKey]*lsa{}
	go s.event("gr-restart", "restarting: neighbors and LSDB cleared, resynchronising")
}

type GRStatus struct {
	Helper     bool     `json:"helper"`
	Phase      string   `json:"restart,omitempty"` // our own: announcing, restarting
	Until      string   `json:"restart_until,omitempty"`
	Helping    []string `json:"helping,omitempty"` // neighbors we help, with the time left
	GraceLSAs  int      `json:"grace_lsas,omitempty"`
	WaitingFor int      `json:"waiting_for,omitempty"`
}

func (s *session) grStatusLocked(now time.Time) GRStatus {
	st := GRStatus{Helper: s.cfg.GracefulRestart.Helper}
	if g := s.gr; g != nil {
		st.Phase = g.phase
		if !g.until.IsZero() {
			st.Until = g.until.Format(time.RFC3339)
		}
		for k := range g.waitFor {
			if !s.fullAgain(k) {
				st.WaitingFor++
			}
		}
	}
	for _, f := range s.ifs {
		for _, n := range f.sortedNbrs() {
			if now.Before(n.graceUntil) {
				st.Helping = append(st.Helping, fmt.Sprintf("%s on %s (%.0f s left)", n.rid, f.device, n.graceUntil.Sub(now).Seconds()))
			}
		}
	}
	for k := range s.db {
		if s.isGrace(k.Type, k.ID) {
			st.GraceLSAs++
		}
	}
	return st
}

// heardAllLocked: every neighbor that was full on f before our restart has
// been heard again.
func (s *session) heardAllLocked(f *iface) bool {
	if s.gr == nil {
		return true
	}
	for k := range s.gr.waitFor {
		var idx int
		var rid string
		fmt.Sscanf(k, "%d/%s", &idx, &rid)
		if idx == f.idx && f.nbrs[netip.MustParseAddr(rid)] == nil {
			return false
		}
	}
	return true
}
