package ospf

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Neighbor states (RFC 2328 10.1).
const (
	nDown = iota
	nInit
	n2Way
	nExStart
	nExchange
	nLoading
	nFull
)

var nbrStateNames = []string{"down", "init", "2-way", "exstart", "exchange", "loading", "full"}

type nbr struct {
	ifc        *iface
	rid, addr  netip.Addr
	id         netip.Addr // in DR/BDR fields: addr (v2) or rid (v3)
	ifID       uint32     // OSPFv3: its interface ID
	mac        pkt.MAC
	prio       uint8
	dr, bdr    netip.Addr // as declared in its hellos
	options    uint32
	state      int
	since      time.Time
	lastHello  time.Time
	dead       time.Duration
	cryptoSeq  uint64           // OSPFv2: last sequence number (must not decrease)
	seq3       map[uint8]uint64 // OSPFv3: last sequence number per packet type (must increase)
	lastReason string
	mtu        uint16

	// Database exchange.
	master     bool
	ddSeq      uint32
	summary    []LSAHeader // still to describe
	lastSent   []byte      // last DBD body (retransmitted by the master, repeated by the slave)
	sentMore   bool        // the last DBD sent had the M bit
	sentFlags  uint8       // flags of the last DBD sent
	ddAt       time.Time   // when the master retransmits
	lastRxFlag uint8
	lastRxSeq  uint32
	exchanged  bool // an exchange has started (for duplicate detection)

	req   map[LSAKey]LSAHeader // to request
	reqAt time.Time

	rxmt   map[LSAKey]bool // our LSAs it hasn't acknowledged (current instance in s.db)
	rxmtAt time.Time
}

func (n *nbr) stateName() string { return nbrStateNames[n.state] }

// seqOKLocked applies the replay rules to an authenticated packet: OSPFv2
// sequence numbers may not go down (RFC 2328 D.5.2), OSPFv3 ones must go
// up per packet type (RFC 7166 4.6).
func (s *session) seqOKLocked(n *nbr, typ uint8, seq uint64) bool {
	if s.cd.v3 {
		if n.seq3 == nil {
			n.seq3 = map[uint8]uint64{}
		}
		if last, ok := n.seq3[typ]; ok && seq <= last {
			s.count("rx_auth_failures")
			n.ifc.authErr = fmt.Sprintf("%s sequence number %d not above %d", typeNames[typ], seq, last)
			return false
		}
		n.seq3[typ] = seq
		return true
	}
	if seq < n.cryptoSeq {
		s.count("rx_auth_failures")
		n.ifc.authErr = "sequence number went down"
		return false
	}
	n.cryptoSeq = seq
	return true
}

func (s *session) setNbrLocked(n *nbr, st int, now time.Time, why string) {
	if n.state == st {
		return
	}
	old := n.state
	n.state, n.since, n.lastReason = st, now, why
	if st == nFull || old == nFull {
		s.dirty = true // our router/network LSAs list full adjacencies
		if st == nFull {
			n.ifc.ups++
		}
		msg := fmt.Sprintf("neighbor %s (%s) on %s: %s → %s (%s)", n.rid, n.addr, n.ifc.device, nbrStateNames[old], nbrStateNames[st], why)
		data := map[string]any{"neighbor": n.rid.String(), "interface": n.ifc.device, "state": nbrStateNames[st]}
		go s.env.Emit("neighbor", msg, data)
	}
}

// nbrResetLocked drops the adjacency back to a lower state, clearing the
// exchange lists.
func (s *session) nbrResetLocked(n *nbr, st int, now time.Time, why string) {
	n.summary, n.lastSent, n.req, n.rxmt = nil, nil, nil, nil
	n.exchanged = false
	s.setNbrLocked(n, st, now, why)
}

func (s *session) nbrDownLocked(n *nbr, now time.Time, why string) {
	s.nbrResetLocked(n, nDown, now, why)
}

// exStartLocked begins an adjacency: we claim master until the DBD
// exchange says otherwise.
func (s *session) exStartLocked(n *nbr, now time.Time) {
	n.summary, n.lastSent, n.req, n.rxmt = nil, nil, map[LSAKey]LSAHeader{}, map[LSAKey]bool{}
	n.exchanged = false
	n.master = true
	n.ddSeq = uint32(now.UnixNano()>>20) | 1
	s.setNbrLocked(n, nExStart, now, "adjacency")
	s.sendDBDLocked(n, DDInit|DDMore|DDMaster, nil, now)
}

func (s *session) sendDBDLocked(n *nbr, flags uint8, hs []LSAHeader, now time.Time) {
	opts := uint32(OptE)
	if s.cd.v3 {
		opts = options3
		if authType(n.ifc.auth) == AuthCrypto {
			opts |= OptAT
		}
	}
	d := DBD{MTU: uint16(s.cfg.MTU), Options: opts, Flags: flags, Seq: n.ddSeq, Headers: hs}
	n.lastSent = s.cd.encodeDBD(d)
	n.sentMore, n.sentFlags = flags&DDMore != 0, flags
	n.ddAt = now.Add(s.rxmt)
	s.queue(n.ifc, n.addr, n.mac, TypeDBD, n.lastSent)
}

// nextChunk takes the next DBD's worth of the summary list.
func (s *session) nextChunk(n *nbr) []LSAHeader {
	room := (s.maxBody(n.ifc) - 8) / lsaHeaderLen
	if room > len(n.summary) {
		room = len(n.summary)
	}
	hs := n.summary[:room]
	n.summary = n.summary[room:]
	return hs
}

// negotiationDoneLocked fills the summary list (RFC 2328 10.3).
func (s *session) negotiationDoneLocked(n *nbr, now time.Time) {
	n.summary = n.summary[:0]
	for k, l := range s.db {
		if k.Link == 0 || k.Link == n.ifc.idx+1 {
			n.summary = append(n.summary, l.headerAt(now))
		}
	}
	s.setNbrLocked(n, nExchange, now, "negotiation done")
}

func (s *session) seqMismatchLocked(n *nbr, now time.Time, why string) {
	s.count("seq_mismatches")
	s.exStartLocked(n, now)
	n.lastReason = "sequence mismatch: " + why
}

// ---- database description (RFC 2328 10.6, 10.8) ----

func (s *session) rxDBDLocked(n *nbr, d *DBD, now time.Time) {
	if !s.cfg.MTUIgnore && int(d.MTU) > s.cfg.MTU {
		s.count("rx_mtu_mismatch")
		n.ifc.authErr = fmt.Sprintf("neighbor %s MTU %d is larger than ours (%d)", n.rid, d.MTU, s.cfg.MTU)
		return
	}
	n.mtu = d.MTU
	dup := n.exchanged && d.Flags == n.lastRxFlag && d.Seq == n.lastRxSeq
	switch n.state {
	case nDown, n2Way:
		return
	case nInit:
		s.twoWayLocked(n, now, true)
		if n.state != nExStart {
			return
		}
		fallthrough
	case nExStart:
		init := uint8(DDInit | DDMore | DDMaster)
		switch {
		case d.Flags&init == init && len(d.Headers) == 0 && s.rid.Less(n.rid):
			n.master, n.ddSeq = false, d.Seq // we are slave
			n.options = d.Options
			s.negotiationDoneLocked(n, now)
			n.exchanged, n.lastRxFlag, n.lastRxSeq = true, d.Flags, d.Seq
			s.slaveReplyLocked(n, d, now)
		case d.Flags&(DDInit|DDMaster) == 0 && d.Seq == n.ddSeq && n.rid.Less(s.rid):
			n.master = true
			n.options = d.Options
			s.negotiationDoneLocked(n, now)
			n.exchanged, n.lastRxFlag, n.lastRxSeq = true, d.Flags, d.Seq
			s.masterAckLocked(n, d, now)
		}
		return
	case nExchange:
		if dup {
			if !n.master {
				s.queue(n.ifc, n.addr, n.mac, TypeDBD, n.lastSent) // slave repeats its last
			}
			return
		}
		if d.Flags&DDMaster != 0 == n.master {
			s.seqMismatchLocked(n, now, "master/slave bit")
			return
		}
		if d.Flags&DDInit != 0 {
			s.seqMismatchLocked(n, now, "init bit")
			return
		}
		if d.Options != n.options {
			s.seqMismatchLocked(n, now, "options changed")
			return
		}
		if n.master && d.Seq != n.ddSeq || !n.master && d.Seq != n.ddSeq+1 {
			s.seqMismatchLocked(n, now, fmt.Sprintf("sequence %d", d.Seq))
			return
		}
		n.lastRxFlag, n.lastRxSeq = d.Flags, d.Seq
		if n.master {
			s.masterAckLocked(n, d, now)
		} else {
			n.ddSeq = d.Seq
			s.slaveReplyLocked(n, d, now)
		}
	case nLoading, nFull:
		if dup {
			if !n.master {
				s.queue(n.ifc, n.addr, n.mac, TypeDBD, n.lastSent)
			}
			return
		}
		s.seqMismatchLocked(n, now, "DBD after the exchange")
	}
}

// headersLocked adds what the neighbor describes and we lack to its
// request list. It reports false on an unknown LSA type.
func (s *session) headersLocked(n *nbr, hs []LSAHeader, now time.Time) bool {
	for _, h := range hs {
		if !s.cd.known(h.Type) {
			return false
		}
		k := s.keyFor(n.ifc, h)
		l := s.db[k]
		if l == nil || Compare(h, l.headerAt(now)) > 0 {
			n.req[k] = h
		}
	}
	return true
}

// keyFor is an LSA's LSDB key as heard on f (link-scoped LSAs are per link).
func (s *session) keyFor(f *iface, h LSAHeader) LSAKey {
	k := h.Key()
	if s.cd.linkScope(h.Type) {
		k.Link = f.idx + 1
	}
	return k
}

// masterAckLocked handles the slave's answer to our current DBD.
func (s *session) masterAckLocked(n *nbr, d *DBD, now time.Time) {
	if !s.headersLocked(n, d.Headers, now) {
		s.seqMismatchLocked(n, now, "unknown LSA type")
		return
	}
	if !n.sentMore && d.Flags&DDMore == 0 && n.lastSent != nil && len(n.summary) == 0 && n.state == nExchange && d.Seq == n.ddSeq && n.exchangedOnce() {
		s.exchangeDoneLocked(n, now)
		return
	}
	n.ddSeq++
	hs := s.nextChunk(n)
	flags := uint8(DDMaster)
	if len(n.summary) > 0 {
		flags |= DDMore
	}
	s.sendDBDLocked(n, flags, hs, now)
}

// exchangedOnce: the master has sent at least one DBD after negotiation
// (the first ack is of the initial empty packet).
func (n *nbr) exchangedOnce() bool { return n.lastSent != nil && n.sentFlags&DDInit == 0 }

// slaveReplyLocked answers the master's DBD with ours (same sequence).
func (s *session) slaveReplyLocked(n *nbr, d *DBD, now time.Time) {
	if !s.headersLocked(n, d.Headers, now) {
		s.seqMismatchLocked(n, now, "unknown LSA type")
		return
	}
	hs := s.nextChunk(n)
	flags := uint8(0)
	if len(n.summary) > 0 {
		flags |= DDMore
	}
	s.sendDBDLocked(n, flags, hs, now)
	if d.Flags&DDMore == 0 && flags&DDMore == 0 {
		s.exchangeDoneLocked(n, now)
	}
}

func (s *session) exchangeDoneLocked(n *nbr, now time.Time) {
	if len(n.req) == 0 {
		s.setNbrLocked(n, nFull, now, "exchange done")
		return
	}
	s.setNbrLocked(n, nLoading, now, "exchange done")
	s.sendLSRLocked(n, now)
}

// ---- requests (RFC 2328 10.7, 10.9) ----

func (s *session) sendLSRLocked(n *nbr, now time.Time) {
	n.reqAt = now.Add(s.rxmt)
	room := s.maxBody(n.ifc) / 12
	var keys []LSAKey
	for k := range n.req {
		if len(keys) == room {
			break
		}
		keys = append(keys, k)
	}
	if len(keys) > 0 {
		s.queue(n.ifc, n.addr, n.mac, TypeLSR, EncodeLSR(keys))
	}
}

func (s *session) rxLSRLocked(n *nbr, keys []LSAKey, now time.Time) {
	if n.state < nExchange {
		return
	}
	var lsas [][]byte
	for _, k := range keys {
		if s.cd.linkScope(k.Type) {
			k.Link = n.ifc.idx + 1
		}
		l := s.db[k]
		if l == nil {
			s.count("bad_ls_requests")
			s.seqMismatchLocked(n, now, "request for an LSA we don't have")
			return
		}
		lsas = append(lsas, l.wire(now))
	}
	s.sendLSUsLocked(n.ifc, n.addr, n.mac, lsas)
}

// sendLSUsLocked packs LSAs into as many updates as needed.
func (s *session) sendLSUsLocked(f *iface, dst netip.Addr, mac pkt.MAC, lsas [][]byte) {
	room := s.maxBody(f) - 4
	var cur [][]byte
	size := 0
	for _, l := range lsas {
		if len(cur) > 0 && size+len(l) > room {
			s.queue(f, dst, mac, TypeLSU, EncodeLSU(cur))
			cur, size = nil, 0
		}
		cur = append(cur, l)
		size += len(l)
	}
	if len(cur) > 0 {
		s.queue(f, dst, mac, TypeLSU, EncodeLSU(cur))
	}
}

// ---- acknowledgements (RFC 2328 13.5, 13.7) ----

func (s *session) rxAckLocked(n *nbr, hs []LSAHeader) {
	if n.state < nExchange {
		return
	}
	now := time.Now()
	for _, h := range hs {
		k := s.keyFor(n.ifc, h)
		if !n.rxmt[k] {
			continue
		}
		if l := s.db[k]; l == nil || Compare(h, l.headerAt(now)) == 0 {
			delete(n.rxmt, k)
		}
	}
}

// delayAckLocked queues an acknowledgement, sent within a second.
func (s *session) delayAckLocked(f *iface, h LSAHeader, now time.Time) {
	if len(f.acks) == 0 {
		f.ackAt = now.Add(time.Second)
	}
	f.acks = append(f.acks, h)
}

// ackDst is where an interface's multicast acks and floods go: everyone
// from the DR/BDR and on point-to-point, the DR/BDR otherwise.
func (s *session) floodDst(f *iface) netip.Addr {
	if f.p2p || f.state == ifDR || f.state == ifBackup {
		return s.allSPF()
	}
	return s.allDR()
}

func (s *session) sendAcksLocked(f *iface) {
	room := s.maxBody(f) / lsaHeaderLen
	for len(f.acks) > 0 {
		n := min(room, len(f.acks))
		s.queue(f, s.floodDst(f), pkt.MAC{}, TypeAck, s.cd.encodeAck(f.acks[:n]))
		f.acks = f.acks[n:]
	}
	f.acks = nil
}

// ---- timers ----

func (s *session) nbrTimersLocked(n *nbr, now time.Time) {
	switch n.state {
	case nExStart, nExchange:
		if n.master && n.lastSent != nil && !now.Before(n.ddAt) {
			n.ddAt = now.Add(s.rxmt)
			s.count("dbd_retransmits")
			s.queue(n.ifc, n.addr, n.mac, TypeDBD, n.lastSent)
		}
	}
	if (n.state == nExchange || n.state == nLoading) && len(n.req) > 0 && !now.Before(n.reqAt) {
		s.sendLSRLocked(n, now)
	}
	s.loadingDoneLocked(n, now)
	if n.state >= nExchange && len(n.rxmt) > 0 && !now.Before(n.rxmtAt) {
		n.rxmtAt = now.Add(s.rxmt)
		var lsas [][]byte
		for k := range n.rxmt {
			if l := s.db[k]; l != nil {
				lsas = append(lsas, l.wire(now))
			} else {
				delete(n.rxmt, k)
			}
		}
		if len(lsas) > 0 {
			s.counters["lsa_retransmits"] += uint64(len(lsas))
			s.sendLSUsLocked(n.ifc, n.addr, n.mac, lsas)
		}
	}
}

// ---- status ----

type NbrStatus struct {
	Interface  string  `json:"interface"`
	RouterID   string  `json:"router_id"`
	Address    string  `json:"address"`
	State      string  `json:"state"`
	Priority   uint8   `json:"priority"`
	DR         string  `json:"dr,omitempty"`
	BDR        string  `json:"bdr,omitempty"`
	Role       string  `json:"role,omitempty"` // dr / bdr on broadcast
	DeadIn     float64 `json:"dead_in_s"`
	Since      string  `json:"since"`
	Reason     string  `json:"reason,omitempty"`
	Requests   int     `json:"requests,omitempty"`
	Retransmit int     `json:"retransmit,omitempty"`
	MTU        uint16  `json:"mtu,omitempty"`
}

func (n *nbr) status(now time.Time) NbrStatus {
	st := NbrStatus{Interface: n.ifc.device, RouterID: n.rid.String(), Address: n.addr.String(), State: n.stateName(), Priority: n.prio,
		DeadIn: max(0, (n.dead - now.Sub(n.lastHello)).Seconds()), Since: n.since.Format(time.RFC3339), Reason: n.lastReason,
		Requests: len(n.req), Retransmit: len(n.rxmt), MTU: n.mtu}
	if !n.ifc.p2p {
		if n.dr != zero4 {
			st.DR = n.dr.String()
		}
		if n.bdr != zero4 {
			st.BDR = n.bdr.String()
		}
		switch n.id {
		case n.ifc.dr:
			st.Role = "dr"
		case n.ifc.bdr:
			st.Role = "bdr"
		}
	}
	return st
}
