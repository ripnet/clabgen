package isis

import (
	"fmt"
	"time"
)

// Graceful restart (RFC 8706, restart TLV 211).
//
// As helper, a neighbor's hello with RR set doesn't reset its adjacency:
// we keep it up, answer with RA and the adjacency's remaining hold time,
// and send a complete set of CSNPs and every LSP so it resynchronises.
// While it sets SA we leave the adjacency out of our LSPs.
//
// The graceful-restart action restarts us gracefully: the LSPs we learned
// are dropped (as a restart loses them), our hellos set RR until each
// neighbor acknowledges, the LSDB is rebuilt from the neighbors' CSNPs,
// and our own LSPs aren't regenerated until that's done (or the hold time
// runs out), so neighbors keep forwarding as before.

type GRArgs struct {
	Seconds int `json:"seconds,omitempty" jsonschema:"minimum=1,maximum=65535,description=Restart hold time (how long neighbors keep our adjacencies and we wait for the LSDB). Default: graceful_restart.restart_hold_s or 60"`
}

// grState is our own graceful restart in progress.
type grState struct {
	since    time.Time
	deadline time.Time
	hold     uint16
	acked    map[grKey]bool   // circuit and level that sent RA
	csnp     map[grKey]bool   // and a complete CSNP set after it
	pending  map[LSPID]uint32 // LSPs the CSNPs listed that we lack
}

type grKey struct {
	circ  int
	level int
}

func (s *session) grHold() int {
	if h := s.cfg.GracefulRestart.HoldS; h > 0 {
		return h
	}
	return 60
}

// restartTLV is the restart TLV for our hellos on c at level n: RR while
// we restart (until that level acknowledges), RA toward a neighbor
// restarting through us.
func (s *session) restartTLV(c *circuit, n int, now time.Time) []TLV {
	var r Restart
	if g := s.gr; g != nil {
		// Point-to-point (n 0): until every level of the adjacency acked.
		levels := []int{n}
		if n == 0 {
			levels = c.adj.levels
		}
		for _, x := range levels {
			if !g.acked[grKey{c.idx, x}] {
				r.Flags |= restartRR
			}
		}
		if len(levels) == 0 {
			r.Flags |= restartRR
		}
	}
	if s.cfg.GracefulRestart.Helper {
		if !c.lan && c.adj.helping {
			r.Flags |= restartRA
			r.Remaining = uint16(max(0, (c.adj.hold - now.Sub(c.adj.lastHello)).Seconds()))
			r.Neighbor, r.HasNbr = c.adj.sys, true
		}
		if c.lan {
			for _, a := range c.lans[n].adjs {
				if a.helping {
					r.Flags |= restartRA
					r.Remaining = uint16(max(0, (a.hold - now.Sub(a.lastHello)).Seconds()))
					r.Neighbor, r.HasNbr = a.sys, true
					break
				}
			}
		}
	}
	if r.Flags == 0 {
		return nil
	}
	return []TLV{r.TLV()}
}

// helpLocked handles a neighbor's restart TLV as helper. up says whether
// we have an Up adjacency with it; it returns whether to keep that
// adjacency through a hello that would otherwise reset it.
func (s *session) helpLocked(c *circuit, levels []int, r Restart, has bool, up bool, helping *bool, suppress *bool, who SysID, now time.Time) bool {
	if !s.cfg.GracefulRestart.Helper {
		return false
	}
	sa := has && r.Flags&restartSA != 0
	if sa != *suppress {
		*suppress = sa
		for _, n := range levels {
			s.lv[n].dirty = true
		}
	}
	rr := has && r.Flags&restartRR != 0
	if !rr {
		if *helping {
			*helping = false
			s.count("gr_helper_done")
			go s.event("graceful-restart", fmt.Sprintf("%s: %s finished restarting", c.device, who))
		}
		return false
	}
	if !*helping {
		*helping = true
		s.count("gr_helper")
		go s.event("graceful-restart", fmt.Sprintf("%s: helping %s restart", c.device, who))
		// Resynchronise it: CSNPs now and every LSP.
		for _, n := range levels {
			if c.lan {
				c.lans[n].lastCSNP = time.Time{}
			} else {
				c.lastCSNP[n] = time.Time{}
			}
			for _, e := range s.lv[n].db {
				e.flag(c.idx)
			}
		}
		c.nextHelo = now
		for _, ll := range c.lans {
			ll.nextHello = now
		}
	}
	return up
}

// grAckLocked notes a neighbor's RA during our restart.
func (s *session) grAckLocked(c *circuit, levels []int, r Restart, has bool, now time.Time) {
	g := s.gr
	if g == nil || !has || r.Flags&restartRA == 0 || r.HasNbr && r.Neighbor != s.sys {
		return
	}
	for _, n := range levels {
		k := grKey{c.idx, n}
		if !g.acked[k] {
			g.acked[k] = true
			s.count("gr_acks")
			if r.Remaining > 0 {
				if d := now.Add(time.Duration(r.Remaining) * time.Second); d.Before(g.deadline) {
					g.deadline = d
				}
			}
		}
	}
}

// grCSNPLocked notes a complete SNP during our restart: once acknowledged,
// the set that reaches the end of the LSP ID space completes the circuit.
func (s *session) grCSNPLocked(c *circuit, sn *SNP) {
	g := s.gr
	if g == nil || !sn.Complete {
		return
	}
	k := grKey{c.idx, sn.Level}
	if g.acked[k] && sn.End == (LSPID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) {
		g.csnp[k] = true
	}
}

// grStartLocked begins our graceful restart.
func (s *session) grStartLocked(a *GRArgs, now time.Time) (any, error) {
	if s.gr != nil {
		return nil, fmt.Errorf("a graceful restart is already in progress")
	}
	up := 0
	for _, c := range s.circs {
		for _, n := range s.sortedLevels() {
			if c.adjUpAt(n) {
				up++
			}
		}
	}
	if up == 0 {
		return nil, fmt.Errorf("no adjacency is up")
	}
	hold := a.Seconds
	if hold == 0 {
		hold = s.grHold()
	}
	s.gr = &grState{since: now, deadline: now.Add(time.Duration(hold) * time.Second), hold: uint16(hold),
		acked: map[grKey]bool{}, csnp: map[grKey]bool{}, pending: map[LSPID]uint32{}}
	// A restart loses what we learned; our own LSPs we keep (and don't
	// regenerate until resynchronised).
	dropped := 0
	for _, l := range s.lv {
		for id, e := range l.db {
			if !e.own {
				delete(l.db, id)
				dropped++
			}
		}
		l.gen++
	}
	for _, c := range s.circs {
		c.nextHelo = now
		for _, ll := range c.lans {
			ll.nextHello = now
		}
	}
	s.count("gr_restarts")
	go s.event("graceful-restart", fmt.Sprintf("restarting gracefully: %d learned LSPs dropped, hold %d s", dropped, hold))
	return map[string]any{"dropped_lsps": dropped, "hold_s": hold}, nil
}

// grTickLocked ends our restart once every Up adjacency acknowledged and
// sent its CSNPs and we have the LSPs they listed, or at the deadline.
func (s *session) grTickLocked(now time.Time) {
	g := s.gr
	if g == nil {
		return
	}
	done := len(g.pending) == 0
	for _, c := range s.circs {
		for _, n := range s.sortedLevels() {
			if c.adjUpAt(n) && !g.csnp[grKey{c.idx, n}] {
				done = false
			}
		}
	}
	timedOut := !now.Before(g.deadline)
	if !done && !timedOut {
		return
	}
	s.gr = nil
	for _, l := range s.lv {
		l.dirty = true
	}
	took := now.Sub(g.since).Round(10 * time.Millisecond)
	if done {
		s.count("gr_completed")
		go s.event("graceful-restart", fmt.Sprintf("graceful restart completed in %s", took))
	} else {
		s.count("gr_timed_out")
		go s.event("graceful-restart", fmt.Sprintf("graceful restart timed out after %s (%d LSPs missing)", took, len(g.pending)))
	}
}

// GRStatus shows graceful restart.
type GRStatus struct {
	Helper     bool          `json:"helper"`
	Restarting *GRRestarting `json:"restarting,omitempty"`
}

type GRRestarting struct {
	Since      string `json:"since"`
	RemainingS int    `json:"remaining_s"`
	Acked      int    `json:"acked"`   // circuit-levels that sent RA
	Synced     int    `json:"synced"`  // and their CSNPs
	Pending    int    `json:"pending"` // LSPs still to receive
}

func (s *session) grStatusLocked(now time.Time) GRStatus {
	st := GRStatus{Helper: s.cfg.GracefulRestart.Helper}
	if g := s.gr; g != nil {
		st.Restarting = &GRRestarting{Since: g.since.Format(time.RFC3339), RemainingS: int(g.deadline.Sub(now).Seconds()),
			Acked: len(g.acked), Synced: len(g.csnp), Pending: len(g.pending)}
	}
	return st
}
