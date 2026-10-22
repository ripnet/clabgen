package ospf

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// lsa is one LSDB entry: the LSA as received or originated, with its age
// at rxAt.
type lsa struct {
	raw     []byte
	hdr     LSAHeader
	rxAt    time.Time
	age     uint16 // at rxAt
	own     bool
	flooded bool // MaxAge copy flooded (for natural ageing)
	stale   bool // ours, due for a refresh
}

func (l *lsa) ageAt(now time.Time) uint16 {
	a := int(l.age) + int(now.Sub(l.rxAt)/time.Second)
	if a > MaxAge || l.age >= MaxAge {
		return MaxAge
	}
	return uint16(a)
}

func (l *lsa) headerAt(now time.Time) LSAHeader {
	h := l.hdr
	h.Age = l.ageAt(now)
	return h
}

// wire is the LSA as sent: aged by the time it has been held plus the
// transmission delay (1 s).
func (l *lsa) wire(now time.Time) []byte {
	b := append([]byte(nil), l.raw...)
	SetAge(b, min(MaxAge, l.ageAt(now)+1))
	return b
}

func (l *lsa) body() []byte { return l.raw[lsaHeaderLen:] }

// isSelf: originated by this router (or a network LSA for one of our
// interface addresses).
func (s *session) isSelfLocked(h LSAHeader) bool {
	if h.AdvRtr == s.rid {
		return true
	}
	if h.Type == LSANetwork {
		for _, f := range s.ifs {
			if a, ok := s.ifAddrLocked(f); ok && a.Addr() == h.ID {
				return true
			}
		}
	}
	return false
}

func (s *session) installLocked(raw []byte, now time.Time, own bool) *lsa {
	h := LSAHeaderOf(raw)
	l := &lsa{raw: raw, hdr: h, rxAt: now, age: h.Age, own: own, flooded: h.Age >= MaxAge}
	l.hdr.Age = 0
	s.db[h.Key()] = l
	return l
}

// ---- receiving updates (RFC 2328 13) ----

func (s *session) rxLSULocked(n *nbr, lsas [][]byte, now time.Time) {
	if n.state < nExchange {
		return
	}
	f := n.ifc
	for _, raw := range lsas {
		if !LSAChecksumOK(raw) {
			s.count("rx_lsa_checksum_errors")
			continue
		}
		h := LSAHeaderOf(raw)
		if !knownType(h.Type) {
			s.count("rx_lsa_unknown_type")
			continue
		}
		k := h.Key()
		db := s.db[k]
		if h.Age >= MaxAge && db == nil && !s.exchangingLocked() {
			s.directAckLocked(n, h)
			continue
		}
		var cmp int
		if db != nil {
			cmp = Compare(h, db.headerAt(now))
		}
		if db == nil || cmp > 0 {
			if db != nil && !db.own && now.Sub(db.rxAt) < time.Second {
				s.count("rx_lsa_min_arrival")
				continue
			}
			if r, ok := n.req[k]; ok && Compare(h, r) >= 0 {
				delete(n.req, k)
			}
			self := s.isSelfLocked(h)
			for _, x := range s.allNbrs() {
				delete(x.rxmt, k)
			}
			l := s.installLocked(raw, now, false)
			s.count("rx_lsa_new")
			if self {
				s.selfOriginatedLocked(l, now)
				s.directAckLocked(n, h)
				continue
			}
			back := s.floodLocked(l, n, now)
			if !back && !(f.state == ifBackup && n.addr != f.dr) {
				s.delayAckLocked(f, h, now)
			}
			continue
		}
		if _, ok := n.req[k]; ok {
			s.count("bad_ls_requests")
			s.seqMismatchLocked(n, now, "update older than requested")
			return
		}
		if cmp == 0 {
			s.count("rx_lsa_duplicate")
			if n.rxmt[k] {
				delete(n.rxmt, k) // implied acknowledgement
				if f.state == ifBackup && n.addr == f.dr {
					s.delayAckLocked(f, h, now)
				}
			} else {
				s.directAckLocked(n, h)
			}
			continue
		}
		// Ours is newer: send it back.
		s.count("rx_lsa_older")
		if db.ageAt(now) >= MaxAge && db.hdr.Seq == MaxSeq {
			continue
		}
		s.sendLSUsLocked(f, n.addr, n.mac, [][]byte{db.wire(now)})
	}
	if n.state == nLoading && len(n.req) == 0 {
		s.setNbrLocked(n, nFull, now, "loading done")
	}
}

func (s *session) directAckLocked(n *nbr, h LSAHeader) {
	s.queue(n.ifc, n.addr, n.mac, TypeAck, EncodeAck([]LSAHeader{h}))
}

func (s *session) allNbrs() []*nbr {
	var out []*nbr
	for _, f := range s.ifs {
		for _, n := range f.nbrs {
			out = append(out, n)
		}
	}
	return out
}

// exchangingLocked: some neighbor is in Exchange or Loading.
func (s *session) exchangingLocked() bool {
	for _, n := range s.allNbrs() {
		if n.state == nExchange || n.state == nLoading {
			return true
		}
	}
	return false
}

// floodLocked sends an LSA out of every interface that needs it (RFC 2328
// 13.3) and reports whether it went back out of the one it came in on.
func (s *session) floodLocked(l *lsa, from *nbr, now time.Time) bool {
	k := l.hdr.Key()
	back := false
	for _, f := range s.ifs {
		if f.state == ifDown || f.passive {
			continue
		}
		added := false
		for _, n := range f.nbrs {
			if n.state < nExchange {
				continue
			}
			if n.state < nFull {
				if r, ok := n.req[k]; ok {
					c := Compare(r, l.headerAt(now))
					if c > 0 {
						continue
					}
					delete(n.req, k)
					if c == 0 {
						continue
					}
				}
			}
			if n == from {
				continue
			}
			if len(n.rxmt) == 0 {
				n.rxmtAt = now.Add(s.rxmt)
			}
			n.rxmt[k] = true
			added = true
		}
		if !added {
			continue
		}
		if from != nil && f == from.ifc {
			if from.addr == f.dr || from.addr == f.bdr || f.state == ifBackup {
				continue
			}
			back = true
		}
		s.sendLSUsLocked(f, f.floodDst(), pkt.MAC{}, [][]byte{l.wire(now)})
	}
	return back
}

// selfOriginatedLocked handles a newer copy of an LSA of ours (RFC 2328
// 13.4), e.g. from before a restart: we re-originate with a higher
// sequence, or flush it if we no longer originate it.
func (s *session) selfOriginatedLocked(l *lsa, now time.Time) {
	s.count("rx_self_originated")
	l.own = true
	s.dirty = true // originate (above its sequence) or flush in the next pass
	s.genAt = now
}

// ---- ageing (RFC 2328 14) ----

func (s *session) ageLocked(now time.Time) {
	exch := s.exchangingLocked()
	for k, l := range s.db {
		age := l.ageAt(now)
		if age < MaxAge {
			if l.own && !l.stale && now.Sub(l.rxAt) >= s.refresh {
				l.stale = true
				s.dirty = true
			}
			continue
		}
		if !l.flooded {
			l.flooded = true
			l.hdr.Age, l.age, l.rxAt = MaxAge, MaxAge, now
			SetAge(l.raw, MaxAge)
			s.floodLocked(l, nil, now)
			continue
		}
		if exch {
			continue
		}
		onList := false
		for _, n := range s.allNbrs() {
			if n.rxmt[k] {
				onList = true
				break
			}
		}
		if !onList {
			delete(s.db, k)
		}
	}
}

// flushLocked prematurely ages one of our LSAs out.
func (s *session) flushLocked(l *lsa, now time.Time) {
	l.own, l.stale, l.flooded = false, false, true
	l.age, l.rxAt = MaxAge, now
	SetAge(l.raw, MaxAge)
	s.floodLocked(l, nil, now)
}

// ---- origination (RFC 2328 12.4) ----

type want struct {
	hdr  LSAHeader
	body []byte
}

// desiredLocked lists the LSAs we should originate now.
func (s *session) desiredLocked() map[LSAKey]want {
	out := map[LSAKey]want{}
	var links []RouterLink
	flags := uint8(0)
	for _, f := range s.ifs {
		if f.state == ifDown {
			continue
		}
		mine, ok := s.ifAddrLocked(f)
		if !ok {
			continue
		}
		cost := f.cost
		transit := cost
		if s.maxMetric {
			transit = MaxLinkMetric
		}
		subnet := RouterLink{ID: mine.Masked().Addr(), Data: MaskOf(mine.Bits()), Type: LinkStub, Metric: cost}
		if f.p2p {
			for _, n := range f.sortedNbrs() {
				if n.state == nFull {
					links = append(links, RouterLink{ID: n.rid, Data: mine.Addr(), Type: LinkP2P, Metric: transit})
				}
			}
			links = append(links, subnet)
			continue
		}
		full := 0
		drFull := false
		for _, n := range f.nbrs {
			if n.state == nFull {
				full++
				if n.addr == f.dr {
					drFull = true
				}
			}
		}
		if f.state != ifWaiting && !f.passive && ((f.state == ifDR && full > 0) || drFull) {
			links = append(links, RouterLink{ID: f.dr, Data: mine.Addr(), Type: LinkTransit, Metric: transit})
		} else {
			links = append(links, subnet)
		}
		if f.state == ifDR && full > 0 {
			rs := []netip.Addr{s.rid}
			for _, n := range f.sortedNbrs() {
				if n.state == nFull {
					rs = append(rs, n.rid)
				}
			}
			h := LSAHeader{Options: OptE, Type: LSANetwork, ID: mine.Addr(), AdvRtr: s.rid}
			out[h.Key()] = want{h, NetworkLSA{Mask: MaskOf(mine.Bits()), Routers: rs}.Encode()}
		}
	}
	for _, p := range s.loopbacks {
		links = append(links, RouterLink{ID: p.Masked().Addr(), Data: MaskOf(p.Bits()), Type: LinkStub, Metric: 0})
	}
	var ext []route
	for _, r := range s.routes {
		if r.kind == "internal" {
			links = append(links, RouterLink{ID: r.prefix.Addr(), Data: MaskOf(r.prefix.Bits()), Type: LinkStub, Metric: uint16(r.metric)})
		} else {
			ext = append(ext, r)
		}
	}
	if len(ext) > 0 {
		flags |= RouterE
	}
	sort.Slice(ext, func(i, j int) bool {
		if ext[i].prefix.Addr() != ext[j].prefix.Addr() {
			return ext[i].prefix.Addr().Less(ext[j].prefix.Addr())
		}
		return ext[i].prefix.Bits() < ext[j].prefix.Bits()
	})
	for _, r := range ext {
		id := r.prefix.Addr()
		h := LSAHeader{Options: OptE, Type: LSAExternal, ID: id, AdvRtr: s.rid}
		if _, taken := out[h.Key()]; taken {
			// Same network, longer mask: the host bits tell them apart
			// (RFC 2328 appendix E).
			v := binary.BigEndian.Uint32(id.AsSlice()) | ^binary.BigEndian.Uint32(MaskOf(r.prefix.Bits()).AsSlice())
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], v)
			h.ID = netip.AddrFrom4(b)
		}
		out[h.Key()] = want{h, ExternalLSA{Mask: MaskOf(r.prefix.Bits()), E2: r.kind == "external-2", Metric: r.metric, Forward: r.forward, Tag: r.tag}.Encode()}
	}
	h := LSAHeader{Options: OptE, Type: LSARouter, ID: s.rid, AdvRtr: s.rid}
	out[h.Key()] = want{h, RouterLSA{Flags: flags, Links: links}.Encode()}
	return out
}

// originateLocked brings our LSAs in line with what we should advertise:
// new or changed ones get the next sequence number, ones we no longer
// want are flushed.
func (s *session) originateLocked(now time.Time) {
	s.dirty = false
	s.genAt = now.Add(time.Second) // MinLSInterval (shortened from 5 s for labs)
	w := s.desiredLocked()
	n := 0
	for k, d := range w {
		cur := s.db[k]
		if cur != nil && cur.own && !cur.stale && cur.ageAt(now) < MaxAge && bytes.Equal(cur.body(), d.body) {
			continue
		}
		seq := uint32(InitialSeq)
		if cur != nil {
			seq = cur.hdr.Seq + 1
		}
		h := d.hdr
		h.Seq = seq
		l := s.installLocked(BuildLSA(h, d.body), now, true)
		for _, x := range s.allNbrs() {
			delete(x.rxmt, k)
		}
		s.floodLocked(l, nil, now)
		n++
	}
	for k, l := range s.db {
		if _, ok := w[k]; ok {
			continue
		}
		if l.own || (s.isSelfLocked(l.hdr) && l.ageAt(now) < MaxAge) {
			s.flushLocked(l, now)
			n++
		}
	}
	if n > 0 {
		s.counters["lsa_originated"] += uint64(n)
	}
}

// ---- status and detail ----

func (l *lsa) status(now time.Time) LSAStatus {
	h := l.hdr
	return LSAStatus{Type: LSATypeName(h.Type), ID: h.ID.String(), AdvRtr: h.AdvRtr.String(), Seq: fmt.Sprintf("%#08x", h.Seq),
		Age: int(l.ageAt(now)), Checksum: fmt.Sprintf("%#04x", h.Checksum), Length: int(h.Length), Own: l.own, Summary: l.summary()}
}

func (l *lsa) summary() string {
	b := l.body()
	switch l.hdr.Type {
	case LSARouter:
		r, err := DecodeRouterLSA(b)
		if err != nil {
			return err.Error()
		}
		c := map[uint8]int{}
		for _, x := range r.Links {
			c[x.Type]++
		}
		var parts []string
		for _, t := range []uint8{LinkP2P, LinkTransit, LinkStub, LinkVirtual} {
			if c[t] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", c[t], linkTypeNames[t]))
			}
		}
		if r.Flags&RouterE != 0 {
			parts = append(parts, "ASBR")
		}
		if r.Flags&RouterB != 0 {
			parts = append(parts, "ABR")
		}
		return strings.Join(parts, ", ")
	case LSANetwork:
		n, err := DecodeNetworkLSA(b)
		if err != nil {
			return err.Error()
		}
		return fmt.Sprintf("/%d, %d routers", BitsOf(n.Mask), len(n.Routers))
	case LSASummary, LSAASBRSummary:
		x, err := DecodeSummaryLSA(b)
		if err != nil {
			return err.Error()
		}
		return fmt.Sprintf("/%d metric %d", BitsOf(x.Mask), x.Metric)
	case LSAExternal:
		e, err := DecodeExternalLSA(b)
		if err != nil {
			return err.Error()
		}
		t := "E1"
		if e.E2 {
			t = "E2"
		}
		out := fmt.Sprintf("/%d %s metric %d", BitsOf(e.Mask), t, e.Metric)
		if e.Tag != 0 {
			out += fmt.Sprintf(" tag %d", e.Tag)
		}
		if e.Forward != zero4 {
			out += " fwd " + e.Forward.String()
		}
		return out
	}
	return ""
}

var typeByName = map[string]uint8{"router": LSARouter, "network": LSANetwork, "summary": LSASummary, "asbr-summary": LSAASBRSummary, "external": LSAExternal}

func (s *session) lsaDetailLocked(a *LSAArgs, now time.Time) (any, error) {
	t, ok := typeByName[a.Type]
	if !ok {
		return nil, fmt.Errorf("unknown LSA type %q", a.Type)
	}
	id, err := netip.ParseAddr(a.ID)
	if err != nil {
		return nil, fmt.Errorf("bad id %q", a.ID)
	}
	var match []*lsa
	for k, l := range s.db {
		if k.Type == t && k.ID == id && (a.AdvRtr == "" || k.AdvRtr.String() == a.AdvRtr) {
			match = append(match, l)
		}
	}
	if len(match) == 0 {
		return nil, fmt.Errorf("no %s LSA %s", a.Type, a.ID)
	}
	if len(match) > 1 {
		var rs []string
		for _, l := range match {
			rs = append(rs, l.hdr.AdvRtr.String())
		}
		return nil, fmt.Errorf("several advertising routers (%s): set adv_router", strings.Join(rs, ", "))
	}
	l := match[0]
	out := map[string]any{"header": l.status(now), "options": fmt.Sprintf("%#02x", l.hdr.Options)}
	b := l.body()
	switch t {
	case LSARouter:
		if r, err := DecodeRouterLSA(b); err == nil {
			var links []map[string]any
			for _, x := range r.Links {
				links = append(links, map[string]any{"type": linkTypeNames[x.Type], "id": x.ID.String(), "data": x.Data.String(), "metric": x.Metric})
			}
			out["flags"] = map[string]bool{"asbr": r.Flags&RouterE != 0, "abr": r.Flags&RouterB != 0, "virtual": r.Flags&RouterV != 0}
			out["links"] = links
		}
	case LSANetwork:
		if n, err := DecodeNetworkLSA(b); err == nil {
			var rs []string
			for _, r := range n.Routers {
				rs = append(rs, r.String())
			}
			out["mask"], out["attached_routers"] = n.Mask.String(), rs
		}
	case LSASummary, LSAASBRSummary:
		if x, err := DecodeSummaryLSA(b); err == nil {
			out["mask"], out["metric"] = x.Mask.String(), x.Metric
		}
	case LSAExternal:
		if e, err := DecodeExternalLSA(b); err == nil {
			out["mask"], out["metric"], out["e2"], out["forwarding_address"], out["tag"] = e.Mask.String(), e.Metric, e.E2, e.Forward.String(), e.Tag
		}
	}
	return out, nil
}
