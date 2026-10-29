package cfm

import (
	"encoding/json"
	"strings"
	"time"
)

// Config changes apply in place, keeping the remote MEP table, counters
// and fault state: CCM settings and TLVs, the expected remote MEPs, AIS,
// responders, and Y.1731 sessions (a changed session list restarts the
// sessions only). A new identity (level, MEP ID, MD or MA, CCM interval)
// restarts the MEP.

func (m *mep) Reconfigure(c any) bool {
	nc := *c.(*Config)
	oc := m.c().Config
	if identity(oc) != identity(nc) {
		return false
	}
	now := time.Now()
	var notes []string
	changed := func(name string, a, b any) bool {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		if string(x) == string(y) {
			return false
		}
		notes = append(notes, name)
		return true
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return false
	}
	m.live.Store(newLive(nc))
	if changed("CCM", oc.CCMEnabled, nc.CCMEnabled) {
		m.ccmEnabled = nc.CCMEnabled
	}
	if changed("remote MEPs", oc.RemoteMEPs, nc.RemoteMEPs) {
		// Listed MEPs are expected from now on; ones no longer listed are
		// forgotten (with an empty list, learned ones stay).
		want := map[uint16]bool{}
		for _, id := range nc.RemoteMEPs {
			want[id] = true
		}
		for id, r := range m.rmeps {
			if r.configured && !want[id] {
				if len(want) == 0 {
					r.configured = false
				} else {
					delete(m.rmeps, id)
				}
			}
		}
		for id := range want {
			if r := m.rmeps[id]; r != nil {
				r.configured = true
			} else {
				m.rmeps[id] = &rmep{id: id, configured: true, state: rmepStart, since: now}
			}
		}
	}
	var stale []*session
	if changed("sessions", oc.Sessions, nc.Sessions) {
		stale, m.sessions = m.sessions, nil
	}
	changed("TLVs, priority and responders", []any{oc.CCMPriority, oc.PortStatusTLV, oc.InterfaceStatusTLV, oc.SenderID, oc.OrgTLV, oc.CCMPadding,
		oc.DisableLBR, oc.DisableLTR, oc.DisableDMR, oc.DisableSLR}, []any{nc.CCMPriority, nc.PortStatusTLV, nc.InterfaceStatusTLV, nc.SenderID, nc.OrgTLV,
		nc.CCMPadding, nc.DisableLBR, nc.DisableLTR, nc.DisableDMR, nc.DisableSLR})
	changed("AIS", oc.AIS, nc.AIS)
	changed("ignored MEPs", []any{oc.IgnoreLocalMEPs, oc.IgnoreUnknownMEPs}, []any{nc.IgnoreLocalMEPs, nc.IgnoreUnknownMEPs})
	evs := m.evaluateLocked(now)
	m.mu.Unlock()
	for _, x := range stale {
		close(x.quit)
	}
	if stale != nil {
		m.startSessions()
	}
	m.emit(evs)
	if len(notes) > 0 {
		m.env.Emit("config-changed", "CFM config applied, MEP kept: "+strings.Join(notes, ", "), nil)
	}
	return true
}

// identity is what makes the MEP the MEP it is to its peers.
func identity(c Config) string {
	b, _ := json.Marshal([]any{c.Level, c.MEPID, c.MDFormat, c.MDName, c.MAFormat, c.MAName, c.CCMInterval})
	return string(b)
}
