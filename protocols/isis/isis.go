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
	"strings"
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
		{Name: "topology-link", Description: "Take a simulated link down or up, or change its metric", NewArgs: func() any { return &TopoLinkArgs{} }},
		{Name: "topology-node", Description: "Take a simulated router down or up, or set its overload bit", NewArgs: func() any { return &TopoNodeArgs{} }},
		{Name: "lsp", Description: "Show one LSP's full content (neighbors, prefixes, addresses)", NewArgs: func() any { return &LSPArgs{} }},
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
	mts         []uint16 // topologies in common
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
	info    *LSPInfo  // decoded content, cached for status
}

// infoLocked returns the entry's decoded content (cached).
func (e *dbEntry) infoLocked() *LSPInfo {
	if e.info == nil {
		in := e.lsp.Info()
		e.info = &in
	}
	return e.info
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
	// LSPs we originate, per system: us and any simulated routers.
	orig      map[SysID]*origin
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

	mts []uint16 // multi-topology: 0 plus the configured MTs (nil: single topology)

	sendKeys map[string]int // keychain scope → key ID sending now (-1: none)

	adjLabels map[string]map[string]uint32 // SR adjacency labels per originator
	sr        srState                      // what we advertise, for status

	topo     *topology // simulated routers behind us (nil: none)
	flapTill time.Time
	flapMode string
	nextHelo time.Time
	counters map[string]uint64
	events   []string

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
		s.lv[n] = &level{n: n, db: map[LSPID]*dbEntry{}, orig: map[SysID]*origin{}, dirty: true}
	}
	s.adj.state = stDown
	if len(cfg.Topologies) > 0 {
		s.mts = []uint16{MTIPv4Unicast}
		for _, t := range cfg.Topologies {
			s.mts = append(s.mts, mtIDs[t])
		}
	}
	if k := cfg.Topology.Kind; k != "" && k != "none" {
		t, err := buildTopology(cfg.Topology, &s.sys)
		if err != nil {
			return nil, fmt.Errorf("topology: %w", err)
		}
		s.topo = t
	}
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
		// 1500 − LLC (ports are ≥ 1500), leaving room for authentication.
		padTo = 1497 - s.cfg.HelloAuth.tlvLen()
	}
	hold := uint16(s.cfg.HelloInterval * s.cfg.HelloMultiplier)
	return s.cfg.HelloAuth.sign(EncodeP2PHello(s.cfg.circuitType(), s.sys, hold, 1, s.areas, a4, a6, s.mts, tw, padTo))
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
	s.keyRolloverLocked(now)
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
	mts, ok := s.commonMTs(p)
	if !ok {
		s.count("rx_hello_mt_mismatch")
		return
	}
	s.adj.mts = mts
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
	own := s.originates(l.ID.Node().Sys())
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
		// Re-originate past its sequence number, even when it equals ours
		// (someone purged our current copy).
		if o := lv.orig[l.ID.Node().Sys()]; l.ID[6] == 0 && o != nil && int(frag) < o.frags {
			s.bumpOwnLocked(lv, l.ID, l.Seq)
		} else if s.lan != nil && l.ID[6] == pnID && frag == 0 && s.lan[l.Level].isDIS {
			// Our pseudonode (purged, or from before a restart).
			s.bumpOwnLocked(lv, l.ID, l.Seq)
		} else {
			// A fragment we no longer originate: purge it.
			lv.db[l.ID] = &dbEntry{lsp: mustLSP(s.signLSP(purgeRaw(l.Level, l.ID, l.Seq, l.Flags&3))), rxAt: now, own: true, srm: true, purgeAt: now.Add(60 * time.Second)}
		}
		if s.lan == nil {
			out = append(out, s.signSNP(EncodePSNP(l.Level, s.nodeID(), []LSPEntry{{l.Lifetime, l.ID, l.Seq, l.Checksum}})))
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
			out = append(out, s.signSNP(EncodePSNP(l.Level, s.nodeID(), []LSPEntry{{l.Lifetime, l.ID, l.Seq, l.Checksum}})))
		}
	case cmp == 0:
		if e != nil {
			e.srm = false
			if own && e.own && l.Lifetime != 0 && l.Checksum != e.lsp.Checksum {
				// Same sequence, different content: a copy from before a
				// restart. Move past it (ISO 10589 7.3.16.1).
				s.bumpOwnLocked(lv, l.ID, l.Seq)
			}
		}
		if s.lan == nil {
			out = append(out, s.signSNP(EncodePSNP(l.Level, s.nodeID(), []LSPEntry{{l.Lifetime, l.ID, l.Seq, l.Checksum}})))
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
				if e.own && x.Lifetime != 0 && x.Checksum != e.lsp.Checksum && s.originates(x.ID.Node().Sys()) {
					s.bumpOwnLocked(lv, x.ID, x.Seq)
				}
			case 1:
				if s.originates(x.ID.Node().Sys()) {
					// Neighbor has a newer copy of an LSP we originate.
					if o, f := lv.orig[x.ID.Node().Sys()], x.ID[7]; x.ID[6] == 0 && o != nil && int(f) < o.frags {
						s.bumpOwnLocked(lv, x.ID, x.Seq)
					} else if s.lan != nil && x.ID[6] == pnID && s.lan[sn.Level].isDIS {
						s.bumpOwnLocked(lv, x.ID, x.Seq)
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
		s.sendTo(dst, s.signSNP(EncodePSNP(sn.Level, s.nodeID(), reqs)), "psnp")
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
	// Authentication: hellos with the hello key; LSPs (and SNPs, with
	// snp_auth) with the area/domain key.
	var kind string
	switch {
	case p.P2P != nil || p.LAN != nil:
		kind, err = "hello", s.cfg.HelloAuth.verify(p.Raw)
	case p.LSP != nil:
		kind, err = "lsp", s.cfg.LSPAuth.verify(p.Raw)
	case p.SNP != nil && s.cfg.SNPAuth:
		kind, err = "snp", s.cfg.LSPAuth.verify(p.Raw)
	}
	if err != nil {
		s.mu.Lock()
		s.count("rx_auth_failures_" + kind)
		first := s.counters["rx_auth_failures_"+kind] == 1
		s.mu.Unlock()
		if first {
			go s.event("auth-failure", fmt.Sprintf("%s from %s: %v", kind, f.Src, err))
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
		out = append(out, s.signSNP(EncodeCSNP(l.n, s.nodeID(), start, end, es)))
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
			body.Neighbors = []ISReach{{Neighbor: ll.lanID, Metric: s.metric}}
		}
	} else if s.adjUpAt(l.n) {
		var nb NodeID
		copy(nb[:], s.adj.sys[:])
		body.Neighbors = []ISReach{{Neighbor: nb, Metric: s.metric}}
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
	isType := s.isType(l.n)
	if s.topo != nil {
		body.Neighbors = append(body.Neighbors, s.topo.attachedLocked(l.n)...)
	}
	var sids map[netip.Prefix]PrefixSID
	if s.cfg.SegmentRouting.Enabled {
		pfx, sids = srPrefixes(s.cfg.SegmentRouting.NodeSIDs, pfx)
		body.SR = s.srCap(body.RouterID, len(v6g) > 0)
	}
	pfx = s.mtify(&body, pfx)
	var lanNbrs []SysID
	if s.lan != nil {
		for _, a := range s.lan[l.n].adjs {
			if a.state == stUp {
				lanNbrs = append(lanNbrs, a.sys)
			}
		}
		sort.Slice(lanNbrs, func(i, j int) bool { return bytes.Compare(lanNbrs[i][:], lanNbrs[j][:]) < 0 })
	}
	st := &srState{}
	pfx = s.srDecorate("", &body, pfx, sids, lanNbrs, st)
	if s.cfg.SegmentRouting.Enabled && (l.n == 2 || s.cfg.Level == "L1") {
		s.sr = *st
	}
	s.originateSys(l, s.sys, body, pfx, isType, now)
	if s.topo != nil {
		s.topo.originateLocked(s, l, isType, now)
	}
	if s.lan != nil {
		s.pseudonodeLocked(l, isType, now)
	}
	if now.After(l.refreshAt) || l.refreshAt.IsZero() {
		l.refreshAt = now.Add(time.Duration(s.cfg.LSPRefresh) * time.Second)
	}
	l.dirty = false
}

var mtIDs = map[string]uint16{"ipv6-unicast": MTIPv6Unicast, "ipv4-multicast": MTIPv4Multicast, "ipv6-multicast": MTIPv6Multicast}

// commonMTs returns the topologies we share with a hello's sender; a hello
// without TLV 229 (or our own single-topology mode) means MT 0.
func (s *session) commonMTs(p *PDU) ([]uint16, bool) {
	ours, theirs := s.mts, p.MTs
	if len(ours) == 0 {
		ours = []uint16{MTIPv4Unicast}
	}
	if !p.HasMT {
		theirs = []uint16{MTIPv4Unicast}
	}
	var out []uint16
	for _, a := range ours {
		for _, b := range theirs {
			if a == b {
				out = append(out, a)
			}
		}
	}
	return out, len(out) > 0
}

// mtify applies multi-topology to an LSP body (ours or a simulated
// router's): TLV 229, IS reachability per MT, IPv6 prefixes into the IPv6
// unicast topology, and copies of prefixes into multicast topologies.
func (s *session) mtify(body *lspBody, pfx []IPReach) []IPReach {
	if len(s.mts) == 0 {
		return pfx
	}
	body.MTs = s.mts
	body.MTNeighbors = map[uint16][]ISReach{}
	has := map[uint16]bool{}
	for _, m := range s.mts {
		has[m] = true
		if m != MTIPv4Unicast {
			body.MTNeighbors[m] = body.Neighbors
		}
	}
	out := make([]IPReach, 0, len(pfx))
	for _, p := range pfx {
		v6 := p.Prefix.Addr().Is6()
		if v6 && has[MTIPv6Unicast] {
			p.MT = MTIPv6Unicast
		}
		out = append(out, p)
		if !v6 && has[MTIPv4Multicast] {
			c := p
			c.MT = MTIPv4Multicast
			out = append(out, c)
		}
		if v6 && has[MTIPv6Multicast] {
			c := p
			c.MT = MTIPv6Multicast
			out = append(out, c)
		}
	}
	return out
}

func mtNameList(ids []uint16) []string {
	var out []string
	for _, id := range ids {
		out = append(out, MTName(id))
	}
	return out
}

// bumpOwnLocked makes our next copy of id go past seq.
func (s *session) bumpOwnLocked(l *level, id LSPID, seq uint32) {
	if id[6] == 0 {
		if o := l.orig[id.Node().Sys()]; o != nil && o.seq[id[7]] <= seq {
			o.seq[id[7]] = seq
			o.force[id[7]] = true
		}
	} else if s.lan != nil && id[6] == pnID {
		if ll := s.lan[l.n]; ll.pnSeq <= seq {
			ll.pnSeq = seq
			ll.pnForce = true
		}
	}
	l.dirty = true
}

func (s *session) isType(n int) byte {
	if n == 1 && s.cfg.Level != "L1L2" {
		return lspTypeL1
	}
	return lspTypeL1L2
}

// originates reports whether we originate LSPs for sys (us or a
// simulated router).
func (s *session) originates(sys SysID) bool {
	if sys == s.sys {
		return true
	}
	return s.topo != nil && s.topo.bySys[sys] != nil
}

// origin tracks the LSP fragments we originate for one system.
type origin struct {
	seq   map[byte]uint32
	force map[byte]bool // re-originate even if unchanged
	frags int
}

// originateSys (re)builds the LSP fragments of one system we originate:
// body goes in fragment 0 with as many prefixes as fit; the rest follow in
// further fragments. Sequence numbers only change with content (or on
// refresh); fragments no longer needed are purged.
func (s *session) originateSys(l *level, sys SysID, body lspBody, pfx []IPReach, isType byte, now time.Time) {
	o := l.orig[sys]
	if o == nil {
		o = &origin{seq: map[byte]uint32{}, force: map[byte]bool{}}
		l.orig[sys] = o
	}
	life := uint16(s.cfg.LSPLifetime)
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
	refresh := now.After(l.refreshAt)
	for i, fp := range frags {
		b := lspBody{Prefixes: fp}
		if i == 0 {
			b = body
			b.Prefixes = fp
		}
		id := MakeLSPID(sys, 0, byte(i))
		old := l.db[id]
		seq := o.seq[byte(i)]
		raw := s.signLSP(EncodeLSP(l.n, id, seq, life, isType, b))
		// Bump the sequence when the content changed, to refresh, or when a
		// neighbor showed us a newer copy (which raised o.seq).
		if old == nil || !old.own || !old.purgeAt.IsZero() || !sameContent(old.lsp.Raw, raw) || refresh || seq == 0 || old.lsp.Seq != seq || o.force[byte(i)] {
			delete(o.force, byte(i))
			seq++
			o.seq[byte(i)] = seq
			l.db[id] = &dbEntry{lsp: mustLSP(s.signLSP(EncodeLSP(l.n, id, seq, life, isType, b))), rxAt: now, life: life, own: true, srm: s.adjUpAt(l.n)}
		}
	}
	s.purgeFrags(l, sys, o, len(frags), isType, now)
	o.frags = len(frags)
}

// withdrawSys purges every fragment of a system we stop originating.
func (s *session) withdrawSys(l *level, sys SysID, isType byte, now time.Time) {
	if o := l.orig[sys]; o != nil {
		s.purgeFrags(l, sys, o, 0, isType, now)
		o.frags = 0
	}
}

func (s *session) purgeFrags(l *level, sys SysID, o *origin, from int, isType byte, now time.Time) {
	for i := from; i < o.frags; i++ {
		id := MakeLSPID(sys, 0, byte(i))
		if e := l.db[id]; e != nil && e.own && e.purgeAt.IsZero() {
			seq := o.seq[byte(i)] + 1
			o.seq[byte(i)] = seq
			l.db[id] = &dbEntry{lsp: mustLSP(s.signLSP(purgeRaw(l.n, id, seq, isType))), rxAt: now, own: true, srm: true, purgeAt: now.Add(60 * time.Second)}
		}
	}
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
	budget := s.cfg.LSPMTU - len(llc) - s.cfg.LSPAuth.tlvLen() - len(EncodeLSP(level, LSPID{}, 1, 1, isType, body))
	type tlvKey struct {
		v6 bool
		mt uint16
	}
	open := map[tlvKey]int{} // bytes in the current TLV of each kind
	used := 0
	for i, p := range ps {
		k := tlvKey{p.Prefix.Addr().Is6(), p.MT}
		sz := 5
		if k.v6 {
			sz = 6
		}
		sz += (p.Prefix.Bits() + 7) / 8
		if p.SID != nil {
			sz += 1 + 8 // sub-TLV length + Prefix-SID sub-TLV
		}
		hdr := 0 // MT TLVs start with the 2-byte MT ID
		if p.MT != 0 {
			hdr = 2
		}
		add := sz
		if open[k] == 0 || open[k]+sz > 255-hdr {
			add += 2 + hdr // a new TLV
			open[k] = 0
		}
		if used+add > budget {
			if i == 0 && !first {
				return 1 // always make progress
			}
			return i
		}
		used += add
		open[k] += sz
	}
	return len(ps)
}

// signLSP adds LSP authentication (area/domain password) if configured.
func (s *session) signLSP(raw []byte) []byte { return s.cfg.LSPAuth.sign(raw) }

// signSNP authenticates a CSNP/PSNP with the LSP key when snp_auth is on.
func (s *session) signSNP(raw []byte) []byte {
	if !s.cfg.SNPAuth {
		return raw
	}
	return s.cfg.LSPAuth.sign(raw)
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
	Topologies  []string `json:"topologies,omitempty"` // in common with the neighbor
	Priority    uint8    `json:"priority,omitempty"`   // LAN
	DIS         bool     `json:"dis,omitempty"`        // LAN: this neighbor is the DIS
}

// LSPStatus summarises one LSDB entry. Statuses are polled often, so they
// carry counts; the "lsp" action returns an LSP's full content.
type LSPStatus struct {
	Level     int    `json:"level"`
	ID        string `json:"lsp_id"`
	Seq       uint32 `json:"seq"`
	Lifetime  uint16 `json:"lifetime"`
	Checksum  string `json:"checksum"`
	Own       bool   `json:"own,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	Overload  bool   `json:"overload,omitempty"`
	Neighbors int    `json:"neighbors"`
	Prefixes  int    `json:"prefixes"`
}

// LSPDetail is an LSP's full content (the "lsp" action).
type LSPDetail struct {
	Level    int    `json:"level"`
	ID       string `json:"lsp_id"`
	Seq      uint32 `json:"seq"`
	Lifetime uint16 `json:"lifetime"`
	Checksum string `json:"checksum"`
	Own      bool   `json:"own,omitempty"`
	LSPInfo
}

type LSPArgs struct {
	ID    string `json:"id" jsonschema:"description=LSP ID like 0000.0000.0001.00-00 (or hostname.00-00)"`
	Level int    `json:"level,omitempty" jsonschema:"description=1 or 2; default: the first level that has it"`
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
	Topologies  []string          `json:"topologies"` // multi-topology (ipv4-unicast alone: single topology)
	SR          *SRStatus         `json:"segment_routing,omitempty"`
	HelloAuth   string            `json:"hello_auth"` // type only, never the key
	LSPAuth     string            `json:"lsp_auth"`
	Adjacency   AdjStatus         `json:"adjacency"`   // point-to-point (or, on a LAN, the first Up one)
	Adjacencies []AdjStatus       `json:"adjacencies"` // all neighbors
	LAN         []LANStatus       `json:"lan,omitempty"`
	LSDB        []LSPStatus       `json:"lsdb"`
	LSDBTotal   int               `json:"lsdb_total"` // entries; lsdb lists at most maxStatusLSPs
	Topology    *TopoStatus       `json:"topology,omitempty"`
	Counters    map[string]uint64 `json:"counters"`
}

func (s *session) Status() any {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{SystemID: s.sys.String(), Hostname: s.host, Level: s.cfg.Level, Circuit: s.cfg.Circuit,
		Areas: s.cfg.Areas, Metric: s.metric, Overload: s.overload, Flapping: now.Before(s.flapTill), Prefixes: len(s.prefixes),
		LSDB: []LSPStatus{}, Counters: map[string]uint64{}, HelloAuth: authType(s.cfg.HelloAuth), LSPAuth: authType(s.cfg.LSPAuth), Topologies: mtNameList(s.mts)}
	if len(s.mts) == 0 {
		st.Topologies = []string{"ipv4-unicast (single topology)"}
	}
	if sr := s.cfg.SegmentRouting; sr.Enabled {
		st.SR = &SRStatus{SRGBBase: sr.SRGBBase, SRGBSize: sr.SRGBSize, NodeSIDs: s.sr.NodeSIDs, AdjSIDs: s.sr.AdjSIDs}
	}
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
				in := e.infoLocked()
				ls.Hostname, ls.Overload, ls.Neighbors, ls.Prefixes = in.Hostname, in.Overload, len(in.Neighbors), len(in.Prefixes)
				if ls.Hostname != "" {
					names[id.Node().Sys()] = ls.Hostname
				}
			}
			st.LSDBTotal++
			if len(st.LSDB) < maxStatusLSPs {
				st.LSDB = append(st.LSDB, ls)
			}
		}
	}
	if s.topo != nil {
		st.Topology = s.topo.status()
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
		st.Adjacency.Topologies = mtNameList(a.mts)
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

func (s *session) lspDetailLocked(a *LSPArgs) (any, error) {
	now := time.Now()
	for _, n := range s.sortedLevels() {
		if a.Level != 0 && a.Level != n {
			continue
		}
		l := s.lv[n]
		for _, id := range sortedIDs(l.db) {
			e := l.db[id]
			name := id.String()
			if in := e.infoLocked(); in.Hostname != "" {
				name = in.Hostname + id.String()[14:]
			}
			if a.ID != id.String() && a.ID != name {
				continue
			}
			d := LSPDetail{Level: n, ID: id.String(), Seq: e.lsp.Seq, Lifetime: e.remaining(now), Checksum: fmt.Sprintf("%#04x", e.lsp.Checksum), Own: e.own}
			if e.purgeAt.IsZero() || e.remaining(now) > 0 {
				d.LSPInfo = *e.infoLocked()
			}
			return d, nil
		}
	}
	return nil, fmt.Errorf("no LSP %q", a.ID)
}

// Brief is the status without the LSDB rows (protocol.Briefer).
func (s *session) Brief() any {
	st := s.Status().(Status)
	st.LSDB = []LSPStatus{}
	return st
}

// maxStatusLSPs caps the LSDB listed in the status (large emulated
// topologies would make it huge); lsdb_total has the full count.
const maxStatusLSPs = 500

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
	case "lsp":
		return s.lspDetailLocked(args.(*LSPArgs))
	case "topology-link":
		return s.topoLinkLocked(args.(*TopoLinkArgs))
	case "topology-node":
		return s.topoNodeLocked(args.(*TopoNodeArgs))
	case "clear":
		s.counters = map[string]uint64{}
		return map[string]any{"cleared": true}, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

func authType(a Auth) string {
	if !a.enabled() {
		return "none"
	}
	if a.Type == "keychain" {
		now := time.Now()
		send := "none"
		if k, ok := a.sendKey(now); ok {
			send = fmt.Sprintf("%d (%s)", k.ID, k.Type)
		}
		var acc []string
		for _, k := range a.acceptKeys(now) {
			acc = append(acc, fmt.Sprint(k.ID))
		}
		return fmt.Sprintf("keychain: sending key %s, accepting %s", send, strings.Join(acc, ", "))
	}
	return a.Type
}

// keyRolloverLocked notices keychain send-key changes. A new LSP key
// re-signs (and so re-originates, with new sequence numbers) all our LSPs,
// so no neighbor keeps copies signed with a retired key.
func (s *session) keyRolloverLocked(now time.Time) {
	for _, sc := range []struct {
		name string
		a    Auth
	}{{"hello", s.cfg.HelloAuth}, {"lsp", s.cfg.LSPAuth}} {
		if sc.a.Type != "keychain" {
			continue
		}
		id := -1
		if k, ok := sc.a.sendKey(now); ok {
			id = int(k.ID)
		}
		if s.sendKeys == nil {
			s.sendKeys = map[string]int{}
		}
		prev, seen := s.sendKeys[sc.name]
		if seen && prev == id {
			continue
		}
		s.sendKeys[sc.name] = id
		if !seen {
			continue
		}
		if sc.name == "lsp" {
			for _, l := range s.lv {
				l.dirty = true
				for _, o := range l.orig {
					for i := 0; i < o.frags; i++ {
						o.force[byte(i)] = true
					}
				}
			}
			if s.lan != nil {
				for _, ll := range s.lan {
					ll.pnForce = true
				}
			}
		}
		msg := fmt.Sprintf("%s authentication now sends with key %d", sc.name, id)
		if id < 0 {
			msg = sc.name + " authentication has no valid send key: sending unauthenticated"
		}
		s.count("auth_key_rollovers")
		go s.event("auth-key-rollover", msg)
	}
}
