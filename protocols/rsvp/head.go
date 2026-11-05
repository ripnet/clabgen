package rsvp

import (
	"errors"
	"fmt"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// LSP states.
const (
	stateDown      = "down"
	stateSignaling = "signaling"
	stateUp        = "up"
)

// head is an LSP from here.
type head struct {
	cfg  LSP
	sess SessionID
	ero  []EROHop

	down     bool // not to be signalled until the signal action
	nextID   uint16
	retryAt  time.Time
	active   *attempt // the LSP traffic goes into
	pending  *attempt // being signalled: the first, or the next of make-before-break
	state    string
	since    time.Time
	lastErr  string
	repaired bool
	setups   uint64
}

// attempt is one LSP instance (one LSP ID) of a head.
type attempt struct {
	sender      SenderID
	ero         []EROHop
	nextPath    time.Time
	established bool
	label       uint32
	hop         Hop
	rro         []RROHop
	refreshMs   uint32
	resvExpires time.Time
}

func (h *head) tspec() TSpec { return tspecFor(h.cfg.BandwidthMbps) }

func (h *head) status() HeadStatus {
	st := HeadStatus{Name: h.cfg.Name, To: h.sess.To.String(), TunnelID: h.sess.TunnelID, State: h.state, Protection: h.cfg.Protection,
		LastError: h.lastErr, Repaired: h.repaired, Setups: h.setups, Resigning: h.active != nil && h.pending != nil}
	for _, e := range h.ero {
		st.Hops = append(st.Hops, Hop4{Address: e.Addr.String(), Loose: e.Loose})
	}
	a := h.active
	if a == nil {
		a = h.pending
	}
	if a != nil {
		st.LSPID = a.sender.LSPID
	}
	if a != nil && a.established {
		l := int64(a.label)
		st.Label, st.NextHop, st.Path = &l, a.hop.Addr.String(), recorded(a.rro)
	}
	if !h.since.IsZero() {
		st.Since = h.since.UTC().Format(time.RFC3339)
	}
	return st
}

func (s *session) setHeadStateLocked(h *head, state string, now time.Time) {
	if h.state != state {
		h.state, h.since = state, now
	}
}

// pathFor builds an attempt's Path message.
func (s *session) pathFor(h *head, a *attempt) (out, bool) {
	via := h.sess.To
	if len(a.ero) > 0 {
		via = a.ero[0].Addr
	}
	iface, ok := s.ifaceToward(via)
	if !ok {
		s.counters["tx_no_source"]++
		return out{}, false
	}
	flags := AttrSE | AttrLabelRecording
	var frr *FRR
	switch h.cfg.Protection {
	case "link":
		flags |= AttrLocalProtection
	case "node":
		flags |= AttrLocalProtection | AttrNodeProtection
	}
	if flags&AttrLocalProtection != 0 {
		frr = &FRR{Setup: h.cfg.setup(), Hold: h.cfg.HoldPriority, HopLimit: 16, Flags: 2, Bandwidth: h.tspec().Rate}
	}
	m := PathMsg{Session: h.sess, Hop: Hop{iface, 1}, RefreshMs: s.refreshMs(), ERO: a.ero,
		Attr: Attr{Setup: h.cfg.setup(), Hold: h.cfg.HoldPriority, Flags: flags, Name: h.cfg.Name}, FRR: frr,
		Sender: a.sender, TSpec: h.tspec(), RRO: []RROHop{{Addr: iface}}, Record: true}
	return out{dst: h.sess.To, via: via, src: s.rid, ttl: 64, alert: true, b: m.Encode(64), counter: "tx_path"}, true
}

func (s *session) tearAttemptLocked(h *head, a *attempt) []out {
	via := h.sess.To
	if a.established {
		via = a.hop.Addr
	} else if len(a.ero) > 0 {
		via = a.ero[0].Addr
	}
	iface, ok := s.ifaceToward(via)
	if !ok {
		return nil
	}
	return []out{{dst: h.sess.To, via: via, src: s.rid, ttl: 64, alert: true, b: PathTearMsg(h.sess, Hop{iface, 1}, a.sender, h.tspec()), counter: "tx_path_tear"}}
}

func (s *session) newAttemptLocked(h *head, ero []EROHop, now time.Time) *attempt {
	id := h.nextID
	h.nextID++
	if h.nextID == 0 {
		h.nextID = 1
	}
	return &attempt{sender: SenderID{From: s.rid, LSPID: id}, ero: ero, nextPath: now}
}

func (s *session) tickHeadsLocked(now time.Time) []out {
	var msgs []out
	retry := time.Duration(s.cfg.RetryInterval) * time.Second
	for _, h := range s.heads {
		if h.down {
			continue
		}
		if h.active == nil && h.pending == nil && !now.Before(h.retryAt) {
			h.pending = s.newAttemptLocked(h, h.ero, now)
			s.setHeadStateLocked(h, stateSignaling, now)
		}
		for _, a := range []*attempt{h.active, h.pending} {
			if a == nil {
				continue
			}
			if a.established && now.After(a.resvExpires) {
				s.counters["resv_expired"]++
				msgs = append(msgs, s.attemptFailedLocked(h, a, "no Resv refresh from "+a.hop.Addr.String(), now, true)...)
				continue
			}
			if !now.Before(a.nextPath) {
				if a.established {
					a.nextPath = now.Add(s.jitter())
				} else {
					a.nextPath = now.Add(retry)
				}
				if o, ok := s.pathFor(h, a); ok {
					msgs = append(msgs, o)
				}
			}
		}
	}
	return msgs
}

// publishLocked tells the device's flows what an LSP's traffic takes now.
func (s *session) publishLocked(h *head, mac pkt.MAC) {
	a := h.active
	var labels []uint32
	if a.label != LabelImplicitNull {
		labels = []uint32{a.label}
	}
	s.env.IP().SetTunnel(h.cfg.Name, &protocol.Tunnel{Labels: labels, NextHop: a.hop.Addr, MAC: mac, To: h.sess.To, Preference: protocol.PreferenceRSVP})
}

func (s *session) rxResvLocked(m *Message, src pkt.MAC, now time.Time) []out {
	h := s.headFor(m.Session)
	if h == nil || !m.HasHop {
		s.counters["rx_ignored"]++
		return nil
	}
	s.neighborLocked(m.Hop.Addr, now)
	var msgs []out
	for _, f := range m.Flows {
		if !f.HasLbl {
			continue
		}
		var a *attempt
		for _, x := range []*attempt{h.pending, h.active} {
			if x != nil && x.sender == f.Sender {
				a = x
			}
		}
		if a == nil {
			continue
		}
		wasUp := a.established
		old := a.label
		a.established, a.label, a.hop, a.rro, a.refreshMs = true, f.Label, m.Hop, f.RRO, m.RefreshM
		a.resvExpires = now.Add(stateLife(m.RefreshM))
		if a == h.pending {
			// Up: traffic moves here, and the path before it goes.
			prev := h.active
			h.active, h.pending = a, nil
			a.nextPath = now.Add(s.jitter())
			h.setups++
			h.lastErr, h.repaired = "", false
			s.setHeadStateLocked(h, stateUp, now)
			s.publishLocked(h, src)
			if prev != nil {
				msgs = append(msgs, s.tearAttemptLocked(h, prev)...)
				s.event("lsp-resignaled", fmt.Sprintf("LSP %s moved to LSP ID %d, label %d, next hop %s", h.cfg.Name, a.sender.LSPID, f.Label, m.Hop.Addr))
			} else {
				s.event("lsp-up", fmt.Sprintf("LSP %s to %s is up, label %d, next hop %s", h.cfg.Name, h.sess.To, f.Label, m.Hop.Addr))
			}
			continue
		}
		if a == h.active {
			s.publishLocked(h, src)
			if wasUp && old != f.Label {
				s.event("lsp-relabel", fmt.Sprintf("LSP %s: label %d, was %d", h.cfg.Name, f.Label, old))
			}
		}
	}
	return msgs
}

func (s *session) headFor(sess SessionID) *head {
	for _, h := range s.heads {
		if h.sess == sess {
			return h
		}
	}
	return nil
}

// attemptFailedLocked ends an attempt, with a PathTear if tear, and the head
// tries again after the retry interval.
func (s *session) attemptFailedLocked(h *head, a *attempt, why string, now time.Time, tear bool) []out {
	var msgs []out
	if tear {
		msgs = s.tearAttemptLocked(h, a)
	}
	h.lastErr = why
	switch a {
	case h.pending:
		h.pending = nil
		if h.active == nil {
			h.retryAt = now.Add(time.Duration(s.cfg.RetryInterval) * time.Second)
			s.setHeadStateLocked(h, stateSignaling, now)
		}
		s.event("lsp-error", fmt.Sprintf("LSP %s: %s", h.cfg.Name, why))
	case h.active:
		h.active = nil
		s.env.IP().SetTunnel(h.cfg.Name, nil)
		if h.pending == nil {
			h.retryAt = now.Add(time.Duration(s.cfg.RetryInterval) * time.Second)
		}
		s.setHeadStateLocked(h, stateSignaling, now)
		s.event("lsp-down", fmt.Sprintf("LSP %s: %s", h.cfg.Name, why))
	}
	return msgs
}

// dropHeadLocked ends all of a head's LSPs, with PathTears.
func (s *session) dropHeadLocked(h *head, why string) []out {
	var msgs []out
	now := time.Now()
	for _, a := range []*attempt{h.active, h.pending} {
		if a != nil {
			msgs = append(msgs, s.tearAttemptLocked(h, a)...)
		}
	}
	if h.active != nil {
		s.env.IP().SetTunnel(h.cfg.Name, nil)
		s.event("lsp-down", fmt.Sprintf("LSP %s: %s", h.cfg.Name, why))
	}
	h.active, h.pending = nil, nil
	s.setHeadStateLocked(h, stateDown, now)
	return msgs
}

func (s *session) rxResvTearLocked(m *Message, now time.Time) []out {
	h := s.headFor(m.Session)
	if h == nil {
		return nil
	}
	var msgs []out
	for _, f := range m.Flows {
		for _, a := range []*attempt{h.active, h.pending} {
			if a != nil && a.sender == f.Sender {
				msgs = append(msgs, s.attemptFailedLocked(h, a, "ResvTear from "+m.Hop.Addr.String(), now, true)...)
			}
		}
	}
	return msgs
}

func errorText(e *ErrorSpec) string {
	name := map[uint8]string{ErrAdmission: "admission control failure", ErrPolicy: "policy control failure", ErrNoPath: "no path information", ErrNoSender: "no sender information",
		ErrUnknownStyle: "unknown style", ErrPreempted: "preempted", 13: "unknown object class", 14: "unknown object type", ErrRouting: "routing problem", ErrNotify: "notify", 23: "RSVP system error"}[e.Code]
	if name == "" {
		name = "error"
	}
	return fmt.Sprintf("%s (code %d value %d) from %s", name, e.Code, e.Value, e.Node)
}

func (s *session) rxPathErrLocked(m *Message, now time.Time) []out {
	if m.Error == nil || !m.HasSess {
		return nil
	}
	h := s.headFor(m.Session)
	if h == nil {
		s.counters["rx_ignored"]++
		return nil
	}
	why := errorText(m.Error)
	if m.Error.Code == ErrNotify {
		if m.Error.Value == ErrValLocalRepaired {
			h.repaired = true
			s.counters["locally_repaired"]++
			s.event("lsp-repaired", fmt.Sprintf("LSP %s: repaired locally by %s", h.cfg.Name, m.Error.Node))
			return nil
		}
		h.lastErr = why
		s.event("lsp-notify", fmt.Sprintf("LSP %s: %s", h.cfg.Name, why))
		return nil
	}
	var a *attempt
	for _, x := range []*attempt{h.pending, h.active} {
		if x != nil && (!m.HasSndr || x.sender == m.Sender) {
			a = x
			break
		}
	}
	if a == nil {
		return nil
	}
	if a == h.active && m.Error.Flags&ErrFlagPathStateRemoved == 0 {
		// The LSP stands: it's only reported.
		h.lastErr = why
		s.event("lsp-error", fmt.Sprintf("LSP %s: %s", h.cfg.Name, why))
		return nil
	}
	return s.attemptFailedLocked(h, a, why, now, m.Error.Flags&ErrFlagPathStateRemoved == 0)
}

func (s *session) rxResvErrLocked(m *Message) {
	if m.Error != nil {
		s.event("lsp-error", fmt.Sprintf("ResvErr for %s: %s", m.Session, errorText(m.Error)))
	}
}

func (s *session) signal(name string) (any, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.heads {
		if h.cfg.Name == name {
			if h.active != nil || h.pending != nil {
				return nil, fmt.Errorf("LSP %s is already signalled", name)
			}
			h.down, h.retryAt = false, now
			return map[string]any{"signaling": name}, nil
		}
	}
	return nil, fmt.Errorf("no LSP %q from here", name)
}

func (s *session) resignal(a *ResignalArgs) (any, error) {
	now := time.Now()
	ero, err := eroFrom(a.Hops)
	if err != nil {
		return nil, err
	}
	var msgs []out
	s.mu.Lock()
	var h *head
	for _, x := range s.heads {
		if x.cfg.Name == a.LSP {
			h = x
		}
	}
	if h == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("no LSP %q from here", a.LSP)
	}
	if h.down || h.active == nil {
		s.mu.Unlock()
		return nil, errors.New("the LSP isn't up: there is nothing to move")
	}
	if len(ero) > 0 {
		h.ero = ero
	}
	if h.pending != nil {
		msgs = append(msgs, s.tearAttemptLocked(h, h.pending)...)
	}
	h.pending = s.newAttemptLocked(h, h.ero, now)
	id := h.pending.sender.LSPID
	s.mu.Unlock()
	for _, m := range msgs {
		s.enqueue(m)
	}
	return map[string]any{"lsp_id": id}, nil
}
