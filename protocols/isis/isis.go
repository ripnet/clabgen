// Package isis is an emulated IS-IS router (ISO 10589, RFC 1195/5305/5308)
// with one interface, point-to-point (3-way adjacency, RFC 5303) or
// broadcast (LAN adjacencies, DIS election, pseudonode; see lan.go): LSDB
// synchronisation with CSNP/PSNP and flooding, our own LSPs (fragmented as
// needed) carrying the device's addresses and injected prefixes, and
// actions to overload, change metrics, advertise/withdraw prefixes and flap.
package isis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "isis"

func init() { protocol.Register(isisType{}) }

type isisType struct{}

func (isisType) Name() string { return TypeName }
func (isisType) Description() string {
	return "IS-IS router (point-to-point or broadcast/LAN with DIS): adjacencies, LSDB sync, LSP origination with route injection, overload/metric/flap actions"
}
func (isisType) NewConfig() any       { return defaultConfig() }
func (isisType) EtherTypes() []uint16 { return []uint16{pkt.EtherTypeLLC} }
func (isisType) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "overload", Description: "Set or clear the overload bit", NewArgs: func() any { return &OverloadArgs{Set: true} }},
		{Name: "metric", Description: "Change the metric toward the neighbor", NewArgs: func() any { return &MetricArgs{Metric: 10} }},
		{Name: "advertise", Description: "Advertise more prefixes (until withdrawn or restarted)", NewArgs: func() any { return &PrefixArgs{} }},
		{Name: "withdraw", Description: "Stop advertising prefixes (injected or from config)", NewArgs: func() any { return &PrefixArgs{} }},
		{Name: "flap", Description: "Bring the adjacency down for a while, then let it come back", NewArgs: func() any { return &FlapArgs{Seconds: 10, Mode: "reset"} }},
		{Name: "clear", Description: "Reset counters"},
	}
}

type OverloadArgs struct {
	Set bool `json:"set"`
}
type MetricArgs struct {
	Metric uint32 `json:"metric" jsonschema:"minimum=1,maximum=16777214"`
}
type PrefixArgs struct {
	Prefixes []Prefix `json:"prefixes" jsonschema:"description=Prefixes to add or remove; each with an optional count of consecutive prefixes and a metric"`
}
type FlapArgs struct {
	Seconds int    `json:"seconds" jsonschema:"minimum=1,maximum=3600,description=How long the adjacency stays down"`
	Mode    string `json:"mode" jsonschema:"enum=reset,enum=silent,description=reset: keep sending hellos saying Down (the neighbor drops at once); silent: stop hellos (the neighbor's hold timer expires)"`
}

func (a *FlapArgs) Validate() error {
	if a.Mode != "reset" && a.Mode != "silent" {
		return fmt.Errorf("mode must be reset or silent")
	}
	return nil
}

func (a *MetricArgs) Validate() error {
	if a.Metric < 1 || a.Metric > 0xfffffe {
		return fmt.Errorf("metric must be 1-16777214")
	}
	return nil
}

func (a *PrefixArgs) Validate() error {
	if len(a.Prefixes) == 0 {
		return fmt.Errorf("at least one prefix")
	}
	for _, p := range a.Prefixes {
		if _, err := p.expand(); err != nil {
			return err
		}
	}
	return nil
}

// Adjacency states shown in status.
const (
	stDown         = "down"
	stInitializing = "initializing"
	stUp           = "up"
)

type adjacency struct {
	state       string
	sys         SysID
	mac         pkt.MAC
	extCircuit  uint32
	levels      []int // levels the adjacency runs at
	hold        time.Duration
	lastHello   time.Time
	since       time.Time
	areas       []string
	addrs       []string
	threeWay    bool
	transitions int
}

type dbEntry struct {
	lsp     *LSP
	rxAt    time.Time // when the current copy arrived (or was originated)
	life    uint16    // remaining lifetime at rxAt
	own     bool
	srm     bool      // send to the neighbor
	srmAt   time.Time // last sent (for retransmission)
	purgeAt time.Time // zero-age entries are removed after ZeroAgeLifetime
}

func (e *dbEntry) remaining(now time.Time) uint16 {
	el := int(now.Sub(e.rxAt) / time.Second)
	if el >= int(e.life) {
		return 0
	}
	return e.life - uint16(el)
}

type level struct {
	n  int
	db map[LSPID]*dbEntry
	// our fragments: number → seq
	ownSeq    map[byte]uint32
	ownFrags  int
	lastCSNP  time.Time
	dirty     bool // regenerate our LSPs
	refreshAt time.Time
}

type session struct {
	env  protocol.Env
	cfg  Config
	sys  SysID
	host string
	mac  pkt.MAC

	mu       sync.Mutex
	areas    [][]byte
	metric   uint32
	overload bool
	prefixes map[netip.Prefix]IPReach // configured + advertised
	adj      adjacency                // point-to-point
	lan      map[int]*lanLevel        // broadcast circuits, per level (nil for p2p)
	lv       map[int]*level

	adjTransitions int // LAN adjacency ups and downs
	flapTill       time.Time
	flapMode       string
	nextHelo       time.Time
	counters       map[string]uint64
	events         []string

	stop chan struct{}
	done chan struct{}
}

func (isisType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	s := &session{env: env, cfg: cfg, mac: env.Device().MAC(), metric: cfg.Metric, overload: cfg.Overload,
		prefixes: map[netip.Prefix]IPReach{}, lv: map[int]*level{}, counters: map[string]uint64{},
		stop: make(chan struct{}), done: make(chan struct{})}
	if cfg.SystemID != "" {
		s.sys, _ = ParseSysID(cfg.SystemID)
	} else {
		h := sha256.Sum256([]byte(env.InstanceName() + "/" + s.mac.String()))
		s.sys = SysID{0x02, 0x00, h[0], h[1], h[2], h[3]}
	}
	s.host = cfg.Hostname
	if s.host == "" {
		s.host = env.InstanceName()
	}
	for _, a := range cfg.Areas {
		b, _ := ParseArea(a)
		s.areas = append(s.areas, b)
	}
	for _, p := range cfg.Prefixes {
		ps, _ := p.expand()
		for _, r := range ps {
			s.prefixes[r.Prefix] = r
		}
	}
	for _, n := range cfg.levels() {
		s.lv[n] = &level{n: n, db: map[LSPID]*dbEntry{}, ownSeq: map[byte]uint32{}, dirty: true}
	}
	s.adj.state = stDown
	if cfg.Circuit == "broadcast" {
		s.lan = map[int]*lanLevel{}
		for _, n := range cfg.levels() {
			s.lan[n] = &lanLevel{n: n, adjs: map[pkt.MAC]*lanAdj{}}
			s.electLocked(n, time.Now())
		}
	}
	go s.run()
	return s, nil
}

func (s *session) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

func (s *session) count(k string) { s.counters[k]++ }

func (s *session) event(kind, msg string) {
	s.env.Emit(kind, msg, nil)
}

// ---- sending ----

func (s *session) send(pdu []byte, what string) { s.sendTo(AllISs, pdu, what) }

func (s *session) sendTo(dst pkt.MAC, pdu []byte, what string) {
	if err := s.env.Send(dst, pkt.EtherTypeLLC, frame(pdu), protocol.SendOpts{SetPCP: true, PCP: 7}); err != nil {
		s.mu.Lock()
		s.count("tx_errors")
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.count("tx_" + what)
	s.mu.Unlock()
}

func (s *session) addrs() (v4, v6ll, v6glob []netip.Prefix) {
	ip := s.env.IP()
	if ip == nil {
		return
	}
	for _, p := range ip.Addresses() {
		a := p.Addr()
		switch {
		case a.Is4():
			v4 = append(v4, p)
		case a.IsLinkLocalUnicast():
			v6ll = append(v6ll, p)
		default:
			v6glob = append(v6glob, p)
		}
	}
	return v4, v6ll, v6glob
}

// hello builds our point-to-point hello. Caller holds mu.
func (s *session) helloLocked() []byte {
	v4, v6ll, _ := s.addrs()
	var a4, a6 []netip.Addr
	for _, p := range v4 {
		a4 = append(a4, p.Addr())
	}
	for _, p := range v6ll {
		a6 = append(a6, p.Addr())
	}
	tw := ThreeWay{State: AdjDown, ExtCircuit: 1}
	switch s.adj.state {
	case stInitializing:
		tw.State = AdjInitializing
	case stUp:
		tw.State = AdjUp
	}
	if s.adj.state != stDown {
		tw.NeighborSys, tw.NeighborExtCircuit, tw.HasNeighbor = s.adj.sys, s.adj.extCircuit, true
	}
	padTo := 0
	if s.cfg.HelloPadding {
		padTo = 1497 // 1500 − LLC; the port MTU would be better but ports are ≥ 1500
	}
	hold := uint16(s.cfg.HelloInterval * s.cfg.HelloMultiplier)
	return EncodeP2PHello(s.cfg.circuitType(), s.sys, hold, 1, s.areas, a4, a6, tw, padTo)
}

// ---- main loop ----

func (s *session) run() {
	defer close(s.done)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

func (s *session) tick(now time.Time) {
	type pdu struct {
		dst  pkt.MAC
		b    []byte
		kind string
	}
	var out []pdu // sent after unlocking
	add := func(dst pkt.MAC, b []byte, kind string) { out = append(out, pdu{dst, b, kind}) }
	s.mu.Lock()
	flapping := now.Before(s.flapTill)
	if s.lan != nil {
		s.tickLANLocked(now, flapping, add)
	} else {
		// Hold timer.
		if s.adj.state != stDown && now.Sub(s.adj.lastHello) > s.adj.hold {
			s.adjDownLocked(now, "hold time expired")
		}
		// Hellos (jittered by up to 25%). A silent flap sends none; a reset
		// flap keeps sending them with state Down (we ignore the neighbor's).
		if !(flapping && s.flapMode == "silent") && !now.Before(s.nextHelo) {
			add(AllISs, s.helloLocked(), "hello")
			iv := time.Duration(s.cfg.HelloInterval) * time.Second
			s.nextHelo = now.Add(iv - time.Duration(rand.Int63n(int64(iv)/4+1)))
		}
	}
	for _, n := range s.sortedLevels() {
		l := s.lv[n]
		// Our LSPs.
		if l.dirty || now.After(l.refreshAt) {
			s.originateLocked(l, now)
		}
		s.ageLocked(l, now)
		if !s.adjUpAt(n) {
			continue
		}
		if s.lan != nil {
			dst := s.lanDst(n)
			// Only the DIS sends CSNPs on a LAN.
			if s.lan[n].isDIS && now.Sub(l.lastCSNP) >= time.Duration(s.cfg.CSNPInterval)*time.Second {
				for _, c := range s.csnpsLocked(l, now) {
					add(dst, c, "csnp")
				}
				l.lastCSNP = now
			}
			// Flood once to the LAN; the DIS's CSNPs catch losses.
			for _, id := range sortedIDs(l.db) {
				if e := l.db[id]; e.srm {
					add(dst, lspWithLifetime(e.lsp.Raw, e.remaining(now)), "lsp")
					e.srm = false
				}
			}
			continue
		}
		// Periodic CSNP.
		if now.Sub(l.lastCSNP) >= time.Duration(s.cfg.CSNPInterval)*time.Second {
			for _, c := range s.csnpsLocked(l, now) {
				add(AllISs, c, "csnp")
			}
			l.lastCSNP = now
		}
		// Flood / retransmit (every 5 s until acknowledged).
		for _, id := range sortedIDs(l.db) {
			e := l.db[id]
			if e.srm && now.Sub(e.srmAt) >= 5*time.Second {
				add(AllISs, lspWithLifetime(e.lsp.Raw, e.remaining(now)), "lsp")
				e.srmAt = now
			}
		}
	}
	s.mu.Unlock()
	for _, o := range out {
		s.sendTo(o.dst, o.b, o.kind)
	}
}

func (s *session) sortedLevels() []int {
	ls := make([]int, 0, len(s.lv))
	for n := range s.lv {
		ls = append(ls, n)
	}
	sort.Ints(ls)
	return ls
}

func (s *session) adjUpAt(n int) bool {
	if s.lan != nil {
		return s.lanAdjUp(n)
	}
	if s.adj.state != stUp {
		return false
	}
	for _, x := range s.adj.levels {
		if x == n {
			return true
		}
	}
	return false
}

func sortedIDs(db map[LSPID]*dbEntry) []LSPID {
	ids := make([]LSPID, 0, len(db))
	for id := range db {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
	return ids
}

// lspWithLifetime returns a copy of an LSP with its remaining lifetime
// field updated (the checksum doesn't cover it).
func lspWithLifetime(raw []byte, life uint16) []byte {
	b := append([]byte{}, raw...)
	binary.BigEndian.PutUint16(b[lspRawLifeOff:], life)
	return b
}

// ---- adjacency ----

func (s *session) adjDownLocked(now time.Time, why string) {
	if s.lan != nil {
		for _, n := range s.sortedLevels() {
			ll := s.lan[n]
			for m, a := range ll.adjs {
				if a.state == stUp {
					s.adjTransitions++
					go s.event("adjacency-down", fmt.Sprintf("L%d adjacency with %s down: %s", n, a.sys, why))
				}
				delete(ll.adjs, m)
			}
			s.electLocked(n, now)
			s.lv[n].dirty = true
		}
		return
	}
	if s.adj.state == stDown {
		return
	}
	wasUp := s.adj.state == stUp
	nb := s.adj.sys
	s.adj = adjacency{state: stDown, since: now, transitions: s.adj.transitions + 1}
	for _, l := range s.lv {
		l.dirty = true
		for _, e := range l.db {
			e.srm = false
		}
	}
	if wasUp {
		go s.event("adjacency-down", fmt.Sprintf("adjacency with %s down: %s", nb, why))
	}
}

func (s *session) rxHello(f *pkt.Frame, p *PDU) {
	h := p.P2P
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count("rx_hello")
	if now.Before(s.flapTill) {
		return
	}
	// Level compatibility.
	var levels []int
	for _, n := range s.cfg.levels() {
		if h.Circuit&(1<<(n-1)) != 0 {
			levels = append(levels, n)
		}
	}
	if len(levels) == 1 && levels[0] == 1 || len(levels) == 2 {
		// Level 1 needs a common area.
		common := false
		for _, a := range p.Areas {
			for _, b := range s.areas {
				if bytes.Equal(a, b) {
					common = true
				}
			}
		}
		if !common {
			if len(levels) == 1 {
				s.count("rx_hello_area_mismatch")
				return
			}
			levels = []int{2}
		}
	}
	if len(levels) == 0 {
		s.count("rx_hello_level_mismatch")
		return
	}
	if s.adj.state != stDown && s.adj.sys != h.Source {
		s.count("rx_hello_other_neighbor")
		return
	}
	prev := s.adj.state
	s.adj.sys, s.adj.mac, s.adj.levels = h.Source, f.Src, levels
	s.adj.hold = time.Duration(h.Hold) * time.Second
	s.adj.lastHello = now
	s.adj.areas = nil // fresh slices: Status hands these out
	for _, a := range p.Areas {
		s.adj.areas = append(s.adj.areas, AreaString(a))
	}
	s.adj.addrs = nil
	for _, v := range p.TLVs.All(TLVIPAddresses) {
		for ; len(v) >= 4; v = v[4:] {
			s.adj.addrs = append(s.adj.addrs, netip.AddrFrom4([4]byte(v[:4])).String())
		}
	}
	for _, v := range p.TLVs.All(TLVIPv6Addresses) {
		for ; len(v) >= 16; v = v[16:] {
			s.adj.addrs = append(s.adj.addrs, netip.AddrFrom16([16]byte(v[:16])).String())
		}
	}
	next := stUp
	if tw := h.ThreeWay; tw != nil {
		s.adj.threeWay = true
		s.adj.extCircuit = tw.ExtCircuit
		switch {
		case tw.HasNeighbor && tw.NeighborSys != s.sys:
			s.adjDownLocked(now, "neighbor reports a different system")
			return
		case tw.State == AdjDown:
			next = stInitializing
		case tw.State == AdjInitializing || tw.State == AdjUp:
			if !tw.HasNeighbor {
				next = stInitializing
			}
		}
		if prev == stUp && tw.State == AdjDown {
			s.adjDownLocked(now, "neighbor reset the adjacency")
			s.adj.sys, s.adj.mac, s.adj.lastHello, s.adj.hold, s.adj.levels = h.Source, f.Src, now, time.Duration(h.Hold)*time.Second, levels
			s.adj.state = stInitializing
			s.nextHelo = now
			return
		}
	}
	if next != prev {
		s.adj.state = next
		s.adj.since = now
		s.nextHelo = now // answer right away so the 3-way handshake is quick
		if next == stUp {
			s.adj.transitions++
			for _, n := range levels {
				l := s.lv[n]
				l.dirty = true
				l.lastCSNP = time.Time{} // send a CSNP now
				for _, e := range l.db {
					e.srm = true // flood everything we have
				}
			}
			go s.event("adjacency-up", fmt.Sprintf("adjacency with %s up (level %v)", h.Source, levels))
		}
	}
}

// ---- LSDB ----

// newer compares an LSP header against a DB entry (ISO 10589 7.3.16).
func newer(seq uint32, life uint16, csum uint16, e *dbEntry, now time.Time) int {
	switch {
	case seq > e.lsp.Seq:
		return 1
	case seq < e.lsp.Seq:
		return -1
	}
	el := e.remaining(now)
	switch {
	case life == 0 && el != 0:
		return 1
	case life != 0 && el == 0:
		return -1
	}
	return 0
}

func (s *session) rxLSP(p *PDU) {
	l := p.LSP
	now := time.Now()
	var out [][]byte
	s.mu.Lock()
	s.count("rx_lsp")
	lv := s.lv[l.Level]
	if lv == nil || !s.adjUpAt(l.Level) {
		s.mu.Unlock()
		return
	}
	if l.Lifetime != 0 && !fletcherOK(l.Raw[12:]) {
		s.count("rx_lsp_checksum_errors")
		s.mu.Unlock()
		return
	}
	e := lv.db[l.ID]
	own := l.ID.Node().Sys() == s.sys
	cmp := 1
	if e != nil {
		cmp = newer(l.Seq, l.Lifetime, l.Checksum, e, now)
	}
	switch {
	case own && (e == nil || cmp > 0):
		// A copy of our own LSP from before a restart, or a purge of it:
		// take over with a higher sequence number.
		if l.Lifetime == 0 && e == nil {
			break
		}
		frag := l.ID[7]
		if l.ID[6] == 0 && int(frag) < lv.ownFrags {
			if l.Seq >= lv.ownSeq[frag] {
				lv.ownSeq[frag] = l.Seq
			}
			lv.dirty = true
		} else if s.lan != nil && l.ID[6] == pnID && frag == 0 && s.lan[l.Level].isDIS {
			// Our pseudonode from before a restart: take it over.
			if ll := s.lan[l.Level]; l.Seq >= ll.pnSeq {
				ll.pnSeq = l.Seq
			}
			lv.dirty = true
		} else {
			// A fragment we no longer originate: purge it.
			lv.db[l.ID] = &dbEntry{lsp: mustLSP(purgeRaw(l.Level, l.ID, l.Seq, l.Flags&3)), rxAt: now, own: true, srm: true, purgeAt: now.Add(60 * time.Second)}
		}
		if s.lan == nil {
			out = append(out, EncodePSNP(l.Level, s.nodeID(), []LSPEntry{{l.Lifetime, l.ID, l.Seq, l.Checksum}}))
		}
	case cmp > 0:
		raw := append([]byte{}, l.Raw...)
		cp := *l
		cp.Raw = raw
		cp.TLVs, _ = parseTLVs(raw[27:])
		ne := &dbEntry{lsp: &cp, rxAt: now, life: l.Lifetime}
		if l.Lifetime == 0 {
			ne.purgeAt = now.Add(60 * time.Second)
		}
		lv.db[l.ID] = ne
		s.count("lsdb_updates")
		// Acknowledge (p2p: PSNP; on a LAN the DIS's CSNPs do).
		if s.lan == nil {
			out = append(out, EncodePSNP(l.Level, s.nodeID(), []LSPEntry{{l.Lifetime, l.ID, l.Seq, l.Checksum}}))
		}
	case cmp == 0:
		if e != nil {
			e.srm = false
		}
		if s.lan == nil {
			out = append(out, EncodePSNP(l.Level, s.nodeID(), []LSPEntry{{l.Lifetime, l.ID, l.Seq, l.Checksum}}))
		}
	default:
		// Ours is newer: send it.
		e.srm, e.srmAt = true, time.Time{}
	}
	s.mu.Unlock()
	for _, b := range out {
		s.send(b, "psnp")
	}
}

func (s *session) nodeID() NodeID {
	var n NodeID
	copy(n[:], s.sys[:])
	return n
}

func (s *session) rxSNP(p *PDU) {
	sn := p.SNP
	now := time.Now()
	var reqs []LSPEntry
	s.mu.Lock()
	if sn.Complete {
		s.count("rx_csnp")
	} else {
		s.count("rx_psnp")
	}
	lv := s.lv[sn.Level]
	if lv == nil || !s.adjUpAt(sn.Level) {
		s.mu.Unlock()
		return
	}
	listed := map[LSPID]bool{}
	for _, x := range sn.Entries {
		listed[x.ID] = true
		e := lv.db[x.ID]
		switch {
		case e == nil:
			if x.Lifetime != 0 && x.Seq != 0 {
				reqs = append(reqs, LSPEntry{x.Lifetime, x.ID, 0, 0}) // request it
			}
		default:
			switch newer(x.Seq, x.Lifetime, x.Checksum, e, now) {
			case 0:
				e.srm = false // acknowledged / in sync
			case 1:
				if x.ID.Node().Sys() == s.sys {
					// Neighbor has a newer copy of our LSP than we do.
					if f := x.ID[7]; x.ID[6] == 0 && int(f) < lv.ownFrags && x.Seq >= lv.ownSeq[f] {
						lv.ownSeq[f] = x.Seq
						lv.dirty = true
					}
				} else {
					reqs = append(reqs, LSPEntry{x.Lifetime, x.ID, 0, 0})
				}
			case -1:
				e.srm, e.srmAt = true, time.Time{}
			}
		}
	}
	if sn.Complete {
		// LSPs in the range the neighbor doesn't have: send them.
		for id, e := range lv.db {
			if !listed[id] && bytes.Compare(id[:], sn.Start[:]) >= 0 && bytes.Compare(id[:], sn.End[:]) <= 0 && e.remaining(now) > 0 {
				e.srm, e.srmAt = true, time.Time{}
			}
		}
	}
	s.mu.Unlock()
	if len(reqs) > 0 {
		dst := AllISs
		if s.lan != nil {
			dst = s.lanDst(sn.Level)
		}
		s.sendTo(dst, EncodePSNP(sn.Level, s.nodeID(), reqs), "psnp")
	}
}

func (s *session) HandleRx(f *pkt.Frame) {
	if f.Src == s.mac || (f.Dst != AllISs && f.Dst != AllL1ISs && f.Dst != AllL2ISs && f.Dst != s.mac) {
		return
	}
	p, err := Decode(f.Payload)
	if err != nil {
		if err != ErrNotISIS {
			s.mu.Lock()
			s.count("rx_errors")
			s.mu.Unlock()
		}
		return
	}
	switch {
	case p.P2P != nil:
		if s.lan != nil {
			s.mu.Lock()
			s.count("rx_p2p_hello_ignored")
			s.mu.Unlock()
			return
		}
		s.rxHello(f, p)
	case p.LSP != nil:
		s.rxLSP(p)
	case p.SNP != nil:
		s.rxSNP(p)
	case p.LAN != nil:
		if s.lan == nil {
			s.mu.Lock()
			s.count("rx_lan_hello_ignored")
			s.mu.Unlock()
			return
		}
		s.rxLANHello(f, p)
	}
}

// ageLocked expires LSPs whose lifetime ran out and removes old purges.
func (s *session) ageLocked(l *level, now time.Time) {
	for id, e := range l.db {
		if !e.purgeAt.IsZero() {
			if now.After(e.purgeAt) {
				delete(l.db, id)
			}
			continue
		}
		if e.remaining(now) == 0 && !e.own {
			e.purgeAt = now.Add(60 * time.Second)
			s.count("lsp_expired")
		}
	}
}

// csnpsLocked describes the whole LSDB, in CSNPs of up to 90 entries.
func (s *session) csnpsLocked(l *level, now time.Time) [][]byte {
	ids := sortedIDs(l.db)
	var out [][]byte
	start := LSPID{}
	const per = 90
	for i := 0; i < len(ids) || i == 0; i += per {
		j := min(i+per, len(ids))
		var es []LSPEntry
		for _, id := range ids[i:j] {
			e := l.db[id]
			es = append(es, LSPEntry{e.remaining(now), id, e.lsp.Seq, e.lsp.Checksum})
		}
		end := LSPID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		if j < len(ids) {
			end = ids[j-1]
		}
		out = append(out, EncodeCSNP(l.n, s.nodeID(), start, end, es))
		if j < len(ids) {
			start = ids[j]
		}
		if len(ids) == 0 {
			break
		}
	}
	return out
}

// ---- origination ----

// originateLocked (re)builds our LSP fragments for a level.
func (s *session) originateLocked(l *level, now time.Time) {
	v4, _, v6g := s.addrs()
	body := lspBody{Areas: s.areas, Hostname: s.host, Overload: s.overload}
	if len(v4) > 0 {
		body.Protocols = append(body.Protocols, NLPIDIPv4)
		body.RouterID = v4[0].Addr()
	}
	if len(v6g) > 0 {
		body.Protocols = append(body.Protocols, NLPIDIPv6)
	}
	for _, p := range v4 {
		body.V4Addrs = append(body.V4Addrs, p.Addr())
	}
	for _, p := range v6g {
		body.V6Addrs = append(body.V6Addrs, p.Addr())
	}
	if s.lan != nil {
		// On a LAN we point at the pseudonode (once a DIS is up).
		if ll := s.lan[l.n]; s.lanAdjUp(l.n) {
			body.Neighbors = []ISReach{{ll.lanID, s.metric}}
		}
	} else if s.adjUpAt(l.n) {
		var nb NodeID
		copy(nb[:], s.adj.sys[:])
		body.Neighbors = []ISReach{{nb, s.metric}}
	}
	// Connected subnets, then injected prefixes.
	var pfx []IPReach
	seen := map[netip.Prefix]bool{}
	for _, p := range append(v4, v6g...) {
		m := p.Masked()
		if !seen[m] {
			seen[m] = true
			pfx = append(pfx, IPReach{Prefix: m, Metric: s.metric})
		}
	}
	keys := make([]netip.Prefix, 0, len(s.prefixes))
	for k := range s.prefixes {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Addr() != keys[j].Addr() {
			return keys[i].Addr().Less(keys[j].Addr())
		}
		return keys[i].Bits() < keys[j].Bits()
	})
	for _, k := range keys {
		pfx = append(pfx, s.prefixes[k])
	}
	isType := lspTypeL1L2
	if l.n == 1 {
		isType = lspTypeL1
		if s.cfg.Level == "L1L2" {
			isType = lspTypeL1L2
		}
	}
	life := uint16(s.cfg.LSPLifetime)
	// Fragment 0 carries everything but the prefixes that don't fit.
	var frags [][]IPReach
	rest := pfx
	first := body
	for {
		n := s.fitPrefixes(l.n, isType, first, rest, len(frags) == 0)
		frags = append(frags, rest[:n])
		rest = rest[n:]
		if len(rest) == 0 || len(frags) == 256 {
			break
		}
		first = lspBody{}
	}
	built := map[LSPID][]byte{}
	for i, fp := range frags {
		b := lspBody{Prefixes: fp}
		if i == 0 {
			b = body
			b.Prefixes = fp
		}
		id := MakeLSPID(s.sys, 0, byte(i))
		old := l.db[id]
		seq := l.ownSeq[byte(i)]
		raw := EncodeLSP(l.n, id, seq, life, isType, b)
		// Only bump the sequence when the content changed (or to refresh).
		// (or when a neighbor showed us a newer copy, which raised ownSeq).
		if old == nil || !old.own || !sameContent(old.lsp.Raw, raw) || now.After(l.refreshAt) || seq == 0 || old.lsp.Seq != seq {
			seq++
			l.ownSeq[byte(i)] = seq
			raw = EncodeLSP(l.n, id, seq, life, isType, b)
		} else {
			raw = old.lsp.Raw
		}
		built[id] = raw
	}
	// Purge fragments we no longer need.
	for i := len(frags); i < l.ownFrags; i++ {
		id := MakeLSPID(s.sys, 0, byte(i))
		if e := l.db[id]; e != nil && e.own && e.purgeAt.IsZero() {
			seq := l.ownSeq[byte(i)] + 1
			l.ownSeq[byte(i)] = seq
			l.db[id] = &dbEntry{lsp: mustLSP(purgeRaw(l.n, id, seq, isType)), rxAt: now, own: true, srm: true, purgeAt: now.Add(60 * time.Second)}
		}
	}
	l.ownFrags = len(frags)
	for id, raw := range built {
		old := l.db[id]
		if old != nil && old.own && bytes.Equal(old.lsp.Raw, raw) {
			continue
		}
		l.db[id] = &dbEntry{lsp: mustLSP(raw), rxAt: now, life: life, own: true, srm: s.adjUpAt(l.n)}
	}
	if s.lan != nil {
		s.pseudonodeLocked(l, isType, now)
	}
	if now.After(l.refreshAt) || l.refreshAt.IsZero() {
		l.refreshAt = now.Add(time.Duration(s.cfg.LSPRefresh) * time.Second)
	}
	l.dirty = false
}

// sameContent compares two LSPs ignoring lifetime, sequence and checksum.
func sameContent(a, b []byte) bool {
	if len(a) != len(b) || len(a) < 27 {
		return false
	}
	return bytes.Equal(a[:10], b[:10]) && bytes.Equal(a[12:20], b[12:20]) && bytes.Equal(a[26:], b[26:])
}

// fitPrefixes returns how many of ps fit in one fragment with body. It
// mirrors how EncodeLSP packs prefix items into 255-byte TLVs, so it
// doesn't have to re-encode.
func (s *session) fitPrefixes(level int, isType byte, body lspBody, ps []IPReach, first bool) int {
	body.Prefixes = nil
	budget := s.cfg.LSPMTU - len(llc) - len(EncodeLSP(level, LSPID{}, 1, 1, isType, body))
	var open4, open6, used int // bytes in the current v4 / v6 TLV
	for i, p := range ps {
		sz, open := 5, &open4
		if p.Prefix.Addr().Is6() {
			sz, open = 6, &open6
		}
		sz += (p.Prefix.Bits() + 7) / 8
		add := sz
		if *open == 0 || *open+sz > 255 {
			add += 2 // a new TLV
			*open = 0
		}
		if used+add > budget {
			if i == 0 && !first {
				return 1 // always make progress
			}
			return i
		}
		used += add
		*open += sz
	}
	return len(ps)
}

// purgeRaw builds a purge: header only, lifetime 0, checksum 0.
func purgeRaw(level int, id LSPID, seq uint32, isType byte) []byte {
	b := EncodeLSP(level, id, seq, 0, isType, lspBody{})
	b[24], b[25] = 0, 0
	return b
}

func mustLSP(raw []byte) *LSP {
	p, err := Decode(frame(raw))
	if err != nil || p.LSP == nil {
		panic("isis: built an undecodable LSP: " + fmt.Sprint(err))
	}
	l := *p.LSP
	l.Raw = raw
	return &l
}

// ---- status ----

type AdjStatus struct {
	State       string   `json:"state"`
	Neighbor    string   `json:"neighbor,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	MAC         string   `json:"mac,omitempty"`
	Levels      []int    `json:"levels,omitempty"`
	HoldS       float64  `json:"hold_remaining_s,omitempty"`
	Since       string   `json:"since,omitempty"`
	Areas       []string `json:"areas,omitempty"`
	Addresses   []string `json:"addresses,omitempty"`
	ThreeWay    bool     `json:"three_way"`
	Transitions int      `json:"transitions"`
	Priority    uint8    `json:"priority,omitempty"` // LAN
	DIS         bool     `json:"dis,omitempty"`      // LAN: this neighbor is the DIS
}

type LSPStatus struct {
	Level    int    `json:"level"`
	ID       string `json:"lsp_id"`
	Seq      uint32 `json:"seq"`
	Lifetime uint16 `json:"lifetime"`
	Checksum string `json:"checksum"`
	Own      bool   `json:"own,omitempty"`
	LSPInfo
}

type Status struct {
	SystemID    string            `json:"system_id"`
	Hostname    string            `json:"hostname"`
	Level       string            `json:"level"`
	Circuit     string            `json:"circuit"`
	Areas       []string          `json:"areas"`
	Metric      uint32            `json:"metric"`
	Overload    bool              `json:"overload"`
	Flapping    bool              `json:"flapping,omitempty"`
	Prefixes    int               `json:"advertised_prefixes"`
	Adjacency   AdjStatus         `json:"adjacency"`   // point-to-point (or, on a LAN, the first Up one)
	Adjacencies []AdjStatus       `json:"adjacencies"` // all neighbors
	LAN         []LANStatus       `json:"lan,omitempty"`
	LSDB        []LSPStatus       `json:"lsdb"`
	Counters    map[string]uint64 `json:"counters"`
}

func (s *session) Status() any {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{SystemID: s.sys.String(), Hostname: s.host, Level: s.cfg.Level, Circuit: s.cfg.Circuit,
		Areas: s.cfg.Areas, Metric: s.metric, Overload: s.overload, Flapping: now.Before(s.flapTill), Prefixes: len(s.prefixes),
		LSDB: []LSPStatus{}, Counters: map[string]uint64{}}
	for k, v := range s.counters {
		st.Counters[k] = v
	}
	// Hostnames from the LSDB (TLV 137), for naming neighbors.
	names := map[SysID]string{}
	for _, n := range s.sortedLevels() {
		l := s.lv[n]
		for _, id := range sortedIDs(l.db) {
			e := l.db[id]
			ls := LSPStatus{Level: n, ID: id.String(), Seq: e.lsp.Seq, Lifetime: e.remaining(now), Checksum: fmt.Sprintf("%#04x", e.lsp.Checksum), Own: e.own}
			if e.purgeAt.IsZero() || e.remaining(now) > 0 {
				ls.LSPInfo = e.lsp.Info()
				if ls.Hostname != "" {
					names[id.Node().Sys()] = ls.Hostname
				}
			}
			st.LSDB = append(st.LSDB, ls)
		}
	}
	if s.lan != nil {
		st.Adjacencies, st.LAN = s.lanStatusLocked(now)
		st.Adjacency = AdjStatus{State: stDown, Transitions: s.adjTransitions}
		for i := range st.Adjacencies {
			x := &st.Adjacencies[i]
			if sys, err := ParseSysID(x.Neighbor); err == nil {
				x.Hostname = names[sys]
			}
			if x.State == stUp && st.Adjacency.State != stUp {
				st.Adjacency = *x
				st.Adjacency.Transitions = s.adjTransitions
			}
		}
		if st.Adjacencies == nil {
			st.Adjacencies = []AdjStatus{}
		}
		return st
	}
	a := s.adj
	st.Adjacency = AdjStatus{State: a.state, ThreeWay: a.threeWay, Transitions: a.transitions}
	if a.state != stDown {
		st.Adjacency.Neighbor, st.Adjacency.MAC, st.Adjacency.Levels = a.sys.String(), a.mac.String(), a.levels
		st.Adjacency.HoldS = (a.hold - now.Sub(a.lastHello)).Seconds()
		st.Adjacency.Areas, st.Adjacency.Addresses = a.areas, a.addrs
		st.Adjacency.Hostname = names[a.sys]
	}
	if !a.since.IsZero() {
		st.Adjacency.Since = a.since.Format(time.RFC3339)
	}
	st.Adjacencies = []AdjStatus{}
	if a.state != stDown {
		st.Adjacencies = append(st.Adjacencies, st.Adjacency)
	}
	return st
}

// ---- actions ----

func (s *session) Action(_ context.Context, name string, args any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirty := func() {
		for _, l := range s.lv {
			l.dirty = true
		}
	}
	switch name {
	case "overload":
		s.overload = args.(*OverloadArgs).Set
		dirty()
		return map[string]any{"overload": s.overload}, nil
	case "metric":
		s.metric = args.(*MetricArgs).Metric
		dirty()
		return map[string]any{"metric": s.metric}, nil
	case "advertise", "withdraw":
		n := 0
		for _, p := range args.(*PrefixArgs).Prefixes {
			ps, _ := p.expand()
			for _, r := range ps {
				if name == "advertise" {
					s.prefixes[r.Prefix] = r
				} else if _, ok := s.prefixes[r.Prefix]; ok {
					delete(s.prefixes, r.Prefix)
				} else {
					continue
				}
				n++
			}
		}
		dirty()
		return map[string]any{name + "d": n, "advertised_prefixes": len(s.prefixes)}, nil
	case "flap":
		fa := args.(*FlapArgs)
		now := time.Now()
		s.flapTill, s.flapMode = now.Add(time.Duration(fa.Seconds)*time.Second), fa.Mode
		s.nextHelo = now
		s.adjDownLocked(now, "flap action")
		return map[string]any{"down_until": s.flapTill.Format(time.RFC3339)}, nil
	case "clear":
		s.counters = map[string]uint64{}
		return map[string]any{"cleared": true}, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}
