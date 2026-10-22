package ospf

import (
	"bytes"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// lsa is one LSDB entry: the LSA as received or originated, with its age
// at rxAt.
type lsa struct {
	key     LSAKey
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
	if h.AdvRtr == s.rid || s.topo != nil && s.topo.byRID[h.AdvRtr] != nil {
		return true
	}
	if h.Type == LSANetwork && !s.cd.v3 {
		for _, f := range s.ifs {
			if a, ok := s.ifAddrLocked(f); ok && a.Addr() == h.ID {
				return true
			}
		}
	}
	return false
}

func (s *session) installLocked(k LSAKey, raw []byte, now time.Time, own bool) *lsa {
	h := s.cd.lsaHeader(raw)
	l := &lsa{key: k, raw: raw, hdr: h, rxAt: now, age: h.Age, own: own, flooded: h.Age >= MaxAge}
	l.hdr.Age = 0
	s.db[k] = l
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
		h := s.cd.lsaHeader(raw)
		if !s.cd.known(h.Type) {
			s.count("rx_lsa_unknown_type")
			continue
		}
		if s.cd.asScope(h.Type) && !f.area.externals() || h.Type == s.cd.nssaType() && f.area.kind != areaNSSA {
			s.count("rx_lsa_wrong_area")
			continue
		}
		k := s.keyFor(f, h)
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
			l := s.installLocked(k, raw, now, false)
			s.count("rx_lsa_new")
			if s.spfInput(h.Type) && s.needsSPFLocked() {
				s.dirty = true // our summaries may change
			}
			if self {
				s.selfOriginatedLocked(l, now)
				s.directAckLocked(n, h)
				// It isn't flooded on, but it meets other neighbors'
				// requests for it (13.3 1b), as after a restart, when our
				// LSAs come back from every neighbor.
				for _, x := range s.allNbrs() {
					if r, ok := x.req[k]; ok && Compare(r, l.headerAt(now)) <= 0 {
						delete(x.req, k)
					}
				}
				continue
			}
			if s.isGrace(h.Type, h.ID) {
				s.rxGraceLocked(f, l, now)
			}
			back := s.floodLocked(l, n, now)
			if !back && !(f.state == ifBackup && n.id != f.dr) {
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
				if f.state == ifBackup && n.id == f.dr {
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
	// Requests can also be met by copies from other neighbors.
	for _, x := range s.allNbrs() {
		s.loadingDoneLocked(x, now)
	}
}

func (s *session) loadingDoneLocked(n *nbr, now time.Time) {
	if n.state == nLoading && len(n.req) == 0 {
		s.setNbrLocked(n, nFull, now, "loading done")
	}
}

func (s *session) directAckLocked(n *nbr, h LSAHeader) {
	s.queue(n.ifc, n.addr, n.mac, TypeAck, s.cd.encodeAck([]LSAHeader{h}))
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
	k := l.key
	back := false
	for _, f := range s.ifs {
		if f.state == ifDown || f.passive || !s.inScope(k, f) {
			continue
		}
		added := false
		for _, n := range f.nbrs {
			if n.state < nExchange || !s.sendsTo(k, n) {
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
			if from.id == f.dr || from.id == f.bdr || f.state == ifBackup {
				continue
			}
			back = true
		}
		for _, d := range s.floodTargets(f) {
			s.sendLSUsLocked(f, d.addr, d.mac, [][]byte{l.wire(now)})
		}
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
			if s.spfInput(k.Type) && s.needsSPFLocked() {
				s.dirty = true
			}
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

// desiredIntraLocked lists our LSAs inside each area: router and network
// LSAs (OSPFv3 also link and intra-area-prefix LSAs) and the emulated
// topology's.
func (s *session) desiredIntraLocked(areas []*area, abr bool, rs routeSet) map[LSAKey]want {
	out := map[LSAKey]want{}
	for _, a := range areas {
		if s.cd.v3 {
			s.area3Locked(out, a, abr, rs)
		} else {
			s.area2Locked(out, a, abr, rs)
			s.opaqueLocked(out, a, rs.sids)
		}
	}
	return out
}

func (s *session) area2Locked(out map[LSAKey]want, a *area, abr bool, rs routeSet) {
	now := time.Now()
	var links []RouterLink
	for _, f := range s.ifs {
		if f.state == ifDown || f.area != a {
			continue
		}
		mine, ok := s.ifAddrLocked(f)
		if !ok {
			continue
		}
		if f.vl != nil {
			for _, n := range f.nbrs {
				if n.adjacent(now) {
					links = append(links, RouterLink{ID: n.rid, Data: mine.Addr(), Type: LinkVirtual, Metric: uint16(min(f.vl.cost, MaxLinkMetric))})
				}
			}
			continue
		}
		cost := f.cost
		transit := cost
		if s.maxMetric {
			transit = MaxLinkMetric
		}
		subnet := RouterLink{ID: mine.Masked().Addr(), Data: MaskOf(mine.Bits()), Type: LinkStub, Metric: cost}
		if f.p2mp {
			subnet = RouterLink{ID: mine.Addr(), Data: MaskOf(32), Type: LinkStub} // RFC 2328 12.4.1.4
		}
		if f.noDR() {
			for _, n := range f.sortedNbrs() {
				if n.adjacent(now) {
					links = append(links, RouterLink{ID: n.rid, Data: mine.Addr(), Type: LinkP2P, Metric: transit})
				}
			}
			links = append(links, subnet)
			continue
		}
		full := 0
		drFull := false
		for _, n := range f.nbrs {
			if n.adjacent(now) {
				full++
				if n.id == f.dr {
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
				if n.adjacent(now) {
					rs = append(rs, n.rid)
				}
			}
			h := LSAHeader{Type: LSANetwork, ID: mine.Addr(), AdvRtr: s.rid}
			s.put(out, a, h, NetworkLSA{Mask: MaskOf(mine.Bits()), Routers: rs}.Encode())
		}
	}
	if a == s.home {
		for _, p := range s.loopbacks {
			links = append(links, RouterLink{ID: p.Masked().Addr(), Data: MaskOf(p.Bits()), Type: LinkStub, Metric: 0})
		}
		for _, r := range s.routes {
			if r.kind == "internal" {
				links = append(links, RouterLink{ID: r.prefix.Addr(), Data: MaskOf(r.prefix.Bits()), Type: LinkStub, Metric: uint16(r.metric)})
			}
		}
	}
	if s.topo != nil && a == s.topoArea {
		links = append(links, s.topo.ourLinks2(s)...)
		s.topo.desiredLocked(s, out, a)
	}
	h := LSAHeader{Type: LSARouter, ID: s.rid, AdvRtr: s.rid}
	s.put(out, a, h, RouterLSA{Flags: s.routerFlagsLocked(a, abr, rs), Links: links}.Encode())
}

// originateLocked brings our LSAs in line with what we should advertise:
// new or changed ones get the next sequence number, ones we no longer
// want are flushed. The areas' own LSAs go first, so that a border
// router's routes (spf.go) include them.
func (s *session) originateLocked(now time.Time) {
	s.dirty = false
	s.genAt = now.Add(time.Second) // MinLSInterval (shortened from 5 s for labs)
	areas := s.activeAreasLocked()
	abr := len(areas) > 1
	rs := s.routeSetLocked()
	w := s.desiredIntraLocked(areas, abr, rs)
	n := s.applyLocked(w, now)
	w2 := s.desiredInterLocked(areas, abr, rs, now)
	n += s.applyLocked(w2, now)
	for k, d := range w2 {
		w[k] = d
	}
	for k, l := range s.db {
		if _, ok := w[k]; ok {
			continue
		}
		if s.gr != nil && s.isGrace(k.Type, k.ID) {
			continue // flushed when the restart is done
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

// applyLocked installs and floods the wanted LSAs that are new or changed
// and reports how many.
func (s *session) applyLocked(w map[LSAKey]want, now time.Time) int {
	n := 0
	for k, d := range w {
		cur := s.db[k]
		if cur != nil && cur.own && !cur.stale && cur.ageAt(now) < MaxAge && cur.hdr.Options == d.hdr.Options && bytes.Equal(cur.body(), d.body) {
			continue
		}
		seq := uint32(InitialSeq)
		if cur != nil {
			seq = cur.hdr.Seq + 1
		}
		h := d.hdr
		h.Seq = seq
		l := s.installLocked(k, s.cd.buildLSA(h, d.body), now, true)
		for _, x := range s.allNbrs() {
			delete(x.rxmt, k)
		}
		s.floodLocked(l, nil, now)
		n++
	}
	return n
}

// spfInput: a change to an LSA of this type can change a border router's
// computed routes (and so its summaries).
func (s *session) spfInput(t uint16) bool {
	if s.cd.v3 {
		switch t {
		case LSA3Router, LSA3Network, LSA3IntraPrefix, LSA3InterPrefix, LSA3InterRouter, LSA3NSSA:
			return true
		}
		return false
	}
	return t >= LSARouter && t <= LSAASBRSummary || t == LSANSSA
}

// ---- status and detail ----

func (s *session) lsaStatus(l *lsa, now time.Time) LSAStatus {
	h := l.hdr
	st := LSAStatus{Type: s.cd.typeName(h.Type), ID: h.ID.String(), AdvRtr: h.AdvRtr.String(), Seq: fmt.Sprintf("%#08x", h.Seq),
		Age: int(l.ageAt(now)), Checksum: fmt.Sprintf("%#04x", h.Checksum), Length: int(h.Length), Own: l.own}
	if a := s.areaOf(l.key); a != nil {
		st.Area = a.id.String()
	}
	if s.cd.v3 {
		st.Summary = summary3(h.Type, l.body())
		if l.key.Link != 0 {
			st.Link = s.ifs[l.key.Link-1].device
		}
	} else {
		st.Summary = l.summary()
	}
	return st
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
		if l.hdr.Type == LSAASBRSummary {
			return fmt.Sprintf("ASBR metric %d", x.Metric)
		}
		return fmt.Sprintf("/%d metric %d", BitsOf(x.Mask), x.Metric)
	case LSAOpaqueLink, LSAOpaqueArea, LSAOpaqueAS:
		sum, _ := opaqueDetail(l.hdr.ID, b)
		return sum
	case LSAExternal, LSANSSA:
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
		if l.hdr.Type == LSANSSA && l.hdr.Options&OptNP != 0 {
			out += " P"
		}
		return out
	}
	return ""
}

// areaMatches: key k is in the named area ("as": AS scope; empty: any).
func (s *session) areaMatches(k LSAKey, name string) bool {
	switch name {
	case "":
		return true
	case "as":
		return k.Area == 0
	}
	id, err := ParseArea(name)
	return err == nil && k.Area != 0 && s.areaOf(k).id == id
}

func (s *session) lsaDetailLocked(a *LSAArgs, now time.Time) (any, error) {
	t, ok := uint16(0), false
	for k := range s.db {
		if s.cd.typeName(k.Type) == a.Type {
			t, ok = k.Type, true
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("no LSAs of type %q", a.Type)
	}
	id, err := netip.ParseAddr(a.ID)
	if err != nil {
		return nil, fmt.Errorf("bad id %q", a.ID)
	}
	var match []*lsa
	for k, l := range s.db {
		if k.Type == t && k.ID == id && (a.AdvRtr == "" || k.AdvRtr.String() == a.AdvRtr) && s.areaMatches(k, a.Area) {
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
		return nil, fmt.Errorf("several matches (advertising routers %s): set adv_router or area", strings.Join(rs, ", "))
	}
	l := match[0]
	out := map[string]any{"header": s.lsaStatus(l, now)}
	b := l.body()
	if s.cd.v3 {
		detail3(t, b, out)
		return out, nil
	}
	out["options"] = fmt.Sprintf("%#02x", l.hdr.Options)
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
	case LSAOpaqueLink, LSAOpaqueArea, LSAOpaqueAS:
		_, d := opaqueDetail(l.hdr.ID, b)
		for k, v := range d {
			out[k] = v
		}
	case LSAExternal, LSANSSA:
		if e, err := DecodeExternalLSA(b); err == nil {
			out["mask"], out["metric"], out["e2"], out["forwarding_address"], out["tag"] = e.Mask.String(), e.Metric, e.E2, e.Forward.String(), e.Tag
		}
	}
	return out, nil
}
