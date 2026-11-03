package rsvp

import (
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

type tailKey struct {
	sess   SessionID
	sender SenderID
}

// tail is an LSP ending here: path state from upstream, and the label we gave.
type tail struct {
	sess      SessionID
	sender    SenderID
	name      string
	phop      Hop
	style     uint32
	attrFlags uint8
	tspec     TSpec
	rro       []RROHop
	label     uint32
	refreshMs uint32
	expires   time.Time
	nextResv  time.Time
	since     time.Time
	torn      bool      // no reservation now: we tore it, or are refusing for a while
	heldTill  time.Time // answered again at this time, without waiting for a Path
}

func (t *tail) status(now time.Time) TailStatus {
	st := TailStatus{Name: t.name, From: t.sender.From.String(), To: t.sess.To.String(), TunnelID: t.sess.TunnelID, LSPID: t.sender.LSPID,
		Label: int64(t.label), PrevHop: t.phop.Addr.String(), Style: "fixed filter", ExpiresS: t.expires.Sub(now).Seconds(),
		Since: t.since.UTC().Format(time.RFC3339), Torn: t.torn}
	if t.style == StyleSE {
		st.Style = "shared explicit"
	}
	switch {
	case t.attrFlags&AttrNodeProtection != 0:
		st.Protection = "node"
	case t.attrFlags&AttrLocalProtection != 0:
		st.Protection = "link"
	}
	// The Path's RRO has the nearest hop first; read from the head instead.
	for i := len(t.rro) - 1; i >= 0; i-- {
		st.Path = append(st.Path, recorded(t.rro[i:i+1])...)
	}
	return st
}

// refusedTillLocked is when we'll answer LSPs of this session again.
func (s *session) refusedTillLocked(sess SessionID) time.Time {
	if s.refuse[sess].After(s.refuseAll) {
		return s.refuse[sess]
	}
	return s.refuseAll
}

// refusedLocked reports whether we aren't answering an LSP of this session.
func (s *session) refusedLocked(sess SessionID, now time.Time) bool {
	return now.Before(s.refuseAll) || now.Before(s.refuse[sess])
}

func (s *session) rxPathLocked(m *Message, h *pkt.IPv4, now time.Time) []out {
	s.counters["rx_path"]++
	if !m.HasSess || !m.HasHop || !m.HasSndr {
		if m.UnknownCType != 0 {
			s.counters["rx_unsupported"]++
		} else {
			s.counters["rx_errors"]++
		}
		return nil
	}
	ts := TSpec{}
	if m.TSpec != nil {
		ts = *m.TSpec
	}
	reject := func(code uint8, value uint16, why string) []out {
		s.counters["path_refused"]++
		s.event("lsp-refused", fmt.Sprintf("%s: %s", m.Session, why))
		e := ErrorSpec{Node: s.rid, Flags: ErrFlagPathStateRemoved, Code: code, Value: value}
		if o, ok := s.unicast(m.Hop.Addr, PathErrMsg(m.Session, e, m.Sender, ts), "tx_path_err"); ok {
			return []out{o}
		}
		return nil
	}
	if !s.env.IP().IsLocal(m.Session.To) {
		return reject(ErrRouting, ErrValNoRoute, "not for this router, and it doesn't switch labels")
	}
	if !m.HasLReq {
		return reject(ErrRouting, ErrValBadL3PID, "no label request")
	}
	if m.LabelReq != pkt.EtherTypeIPv4 {
		return reject(ErrRouting, ErrValBadL3PID, fmt.Sprintf("label request for L3PID %#x", m.LabelReq))
	}
	if s.cfg.RefuseLSPs {
		return reject(ErrAdmission, 2, "refuse_lsps is on")
	}
	key := tailKey{m.Session, m.Sender}
	t := s.tails[key]
	fresh := t == nil
	if fresh {
		l, err := s.takeLabelLocked()
		if err != nil {
			return reject(ErrRouting, 9, err.Error())
		}
		t = &tail{sess: m.Session, sender: m.Sender, label: l, since: now, torn: s.refusedLocked(m.Session, now)}
		if t.torn {
			t.heldTill = s.refusedTillLocked(m.Session)
		}
		s.tails[key] = t
	}
	changed := fresh || t.phop != m.Hop
	t.phop, t.refreshMs, t.tspec = m.Hop, m.RefreshM, ts
	t.expires = now.Add(stateLife(m.RefreshM))
	t.rro = append(t.rro[:0], m.RRO...)
	t.style = StyleFF
	t.attrFlags = 0
	if m.Attr != nil {
		t.name, t.attrFlags = m.Attr.Name, m.Attr.Flags
		if m.Attr.Flags&AttrSE != 0 {
			t.style = StyleSE
		}
	}
	s.neighborLocked(m.Hop.Addr, now)
	if t.torn && !s.refusedLocked(m.Session, now) {
		t.torn, t.heldTill = false, time.Time{}
		changed = true
	}
	if fresh {
		name := t.name
		if name == "" {
			name = m.Session.String()
		}
		s.event("lsp-up", fmt.Sprintf("LSP %s from %s (LSP ID %d) ends here, label %d", name, m.Sender.From, m.Sender.LSPID, t.label))
	}
	if changed && !t.torn {
		return s.resvGroupLocked(t, now)
	}
	return nil
}

// resvGroupLocked is the Resv for an LSP: shared explicit LSPs of a session
// from one neighbor go in one message, which is their whole reservation.
func (s *session) resvGroupLocked(t *tail, now time.Time) []out {
	group := []*tail{t}
	if t.style == StyleSE {
		group = group[:0]
		for _, x := range s.tails {
			if x.sess == t.sess && x.phop.Addr == t.phop.Addr && x.style == StyleSE && !x.torn {
				group = append(group, x)
			}
		}
		sort.Slice(group, func(i, j int) bool { return group[i].sender.LSPID < group[j].sender.LSPID })
	}
	if len(group) == 0 {
		return nil
	}
	iface, ok := s.ifaceToward(t.phop.Addr)
	if !ok {
		s.counters["tx_no_source"]++
		return nil
	}
	flows := make([]Flow, 0, len(group))
	for _, x := range group {
		x.nextResv = now.Add(s.jitter())
		f := Flow{Sender: x.sender, Label: x.label, HasLbl: true, HasRRO: true}
		hop := RROHop{Addr: iface}
		if x.attrFlags&AttrLabelRecording != 0 {
			hop.Label, hop.HasLabel = x.label, true
		}
		f.RRO = []RROHop{hop}
		flows = append(flows, f)
	}
	r := ResvMsg{Session: t.sess, Hop: Hop{iface, t.phop.LIH}, RefreshMs: s.refreshMs(), Style: t.style, TSpec: t.tspec, Flows: flows}
	o, ok := s.unicast(t.phop.Addr, r.Encode(), "tx_resv")
	if !ok {
		return nil
	}
	return []out{o}
}

func (s *session) tickTailsLocked(now time.Time) []out {
	var msgs []out
	for k, t := range s.tails {
		if now.After(t.expires) {
			s.counters["tails_expired"]++
			if !t.torn {
				msgs = append(msgs, s.resvTearLocked(t)...)
			}
			s.removeTailLocked(k, t, "no refresh from "+t.phop.Addr.String())
			continue
		}
		// A reservation torn for a while comes back when the while is over;
		// one torn without a time waits for the next Path.
		if t.torn && !t.heldTill.IsZero() && !now.Before(t.heldTill) {
			t.torn, t.heldTill = false, time.Time{}
			msgs = append(msgs, s.resvGroupLocked(t, now)...)
			continue
		}
		if !t.torn && !now.Before(t.nextResv) {
			msgs = append(msgs, s.resvGroupLocked(t, now)...)
		}
	}
	return msgs
}

func (s *session) resvTearLocked(t *tail) []out {
	iface, ok := s.ifaceToward(t.phop.Addr)
	if !ok {
		return nil
	}
	if o, ok := s.unicast(t.phop.Addr, ResvTearMsg(t.sess, Hop{iface, t.phop.LIH}, t.style, []SenderID{t.sender}), "tx_resv_tear"); ok {
		return []out{o}
	}
	return nil
}

// tearTailsLocked tears down reservations, keeping the path state: the next
// Path refresh answers them again unless we're refusing.
func (s *session) tearTailsLocked(match func(*tail) bool) []out {
	var msgs []out
	for _, t := range s.tails {
		if match(t) && !t.torn {
			t.torn = true
			msgs = append(msgs, s.resvTearLocked(t)...)
			s.event("lsp-down", fmt.Sprintf("LSP %s from %s (LSP ID %d): reservation torn down", t.displayName(), t.sender.From, t.sender.LSPID))
		}
	}
	return msgs
}

func (t *tail) displayName() string {
	if t.name != "" {
		return t.name
	}
	return t.sess.String()
}

func (s *session) removeTailLocked(k tailKey, t *tail, why string) {
	delete(s.tails, k)
	s.releaseTailLabelLocked(t.label)
	if !t.torn {
		s.event("lsp-down", fmt.Sprintf("LSP %s from %s (LSP ID %d): %s", t.displayName(), t.sender.From, t.sender.LSPID, why))
	}
}

func (s *session) releaseTailLabelLocked(l uint32) {
	// A packet already on its way still gets through.
	time.AfterFunc(oldLabelKept, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, t := range s.tails {
			if t.label == l {
				return
			}
		}
		s.dropLabelLocked(l)
	})
}

func (s *session) rxPathTearLocked(m *Message) []out {
	if !m.HasSess || !m.HasSndr {
		return nil
	}
	k := tailKey{m.Session, m.Sender}
	if t := s.tails[k]; t != nil {
		s.removeTailLocked(k, t, "torn down by "+m.Sender.From.String())
	}
	return nil
}

func (s *session) relabel(name string) (any, error) {
	now := time.Now()
	var msgs []out
	n := 0
	s.mu.Lock()
	seen := map[SessionID]netip.Addr{}
	for _, t := range s.tails {
		if name != "" && t.name != name {
			continue
		}
		l, err := s.takeLabelLocked()
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		old := t.label
		t.label = l
		s.releaseTailLabelLocked(old)
		n++
		s.event("lsp-relabel", fmt.Sprintf("LSP %s from %s (LSP ID %d): label %d, was %d", t.displayName(), t.sender.From, t.sender.LSPID, l, old))
		seen[t.sess] = t.phop.Addr
	}
	for _, t := range s.tails {
		if a, ok := seen[t.sess]; ok && a == t.phop.Addr && !t.torn {
			msgs = append(msgs, s.resvGroupLocked(t, now)...)
			delete(seen, t.sess)
		}
	}
	s.mu.Unlock()
	if n == 0 {
		return nil, fmt.Errorf("no LSP ending here to relabel")
	}
	for _, m := range msgs {
		s.enqueue(m)
	}
	return map[string]any{"relabelled": n}, nil
}
