// Package isis is an emulated IS-IS router (ISO 10589, RFC 1195/5305/5308)
// with one or more interfaces (circuits; see circuit.go), each
// point-to-point (3-way adjacency, RFC 5303) or broadcast (LAN adjacencies,
// DIS election, pseudonode; see lan.go): LSDB synchronisation with
// CSNP/PSNP and flooding between circuits, our own LSPs (fragmented as
// needed) carrying the interfaces' addresses and injected prefixes, and
// actions to overload, change metrics, advertise/withdraw prefixes and flap.
package isis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
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
	return "IS-IS router with one or more interfaces (point-to-point or broadcast/LAN with DIS): adjacencies, LSDB sync and flooding, LSP origination with route injection, overload/metric/flap actions"
}
func (isisType) NewConfig() any { return defaultConfig() }

// IP frames are for BFD.
func (isisType) EtherTypes() []uint16 {
	return []uint16{pkt.EtherTypeLLC, pkt.EtherTypeIPv4, pkt.EtherTypeIPv6}
}
func (isisType) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "overload", Description: "Set or clear the overload bit", NewArgs: func() any { return &OverloadArgs{Set: true} }},
		{Name: "metric", Description: "Change an interface's metric (default: every interface)", NewArgs: func() any { return &MetricArgs{Metric: 10} }},
		{Name: "advertise", Description: "Advertise more prefixes (until withdrawn or restarted)", NewArgs: func() any { return &PrefixArgs{} }},
		{Name: "withdraw", Description: "Stop advertising prefixes (injected or from config)", NewArgs: func() any { return &PrefixArgs{} }},
		{Name: "flap", Description: "Bring adjacencies down for a while (one interface or all), then let them come back", NewArgs: func() any { return &FlapArgs{Seconds: 10, Mode: "reset"} }},
		{Name: "topology-link", Description: "Take a simulated link down or up, or change its metric", NewArgs: func() any { return &TopoLinkArgs{} }},
		{Name: "topology-node", Description: "Take a simulated router down or up, or set its overload bit", NewArgs: func() any { return &TopoNodeArgs{} }},
		{Name: "graceful-restart", Description: "Restart gracefully (RFC 8706): drop the learned LSDB and resynchronise it with neighbors' help, keeping adjacencies and our LSPs", NewArgs: func() any { return &GRArgs{} }},
		{Name: "bfd-fail", Description: "Silence our BFD sessions for a while (one interface or all): neighbors detect a failure", NewArgs: func() any { return &BFDArgs{Seconds: 5} }},
		{Name: "lsp", Description: "Show one LSP's full content (neighbors, prefixes, addresses)", NewArgs: func() any { return &LSPArgs{} }},
		{Name: "clear", Description: "Reset counters"},
	}
}

type OverloadArgs struct {
	Set bool `json:"set"`
}
type MetricArgs struct {
	Metric    uint32 `json:"metric" jsonschema:"minimum=1,maximum=16777214"`
	Interface string `json:"interface,omitempty" jsonschema:"description=Device name; empty: every interface"`
}
type PrefixArgs struct {
	Prefixes []Prefix `json:"prefixes" jsonschema:"description=Prefixes to add or remove; each with an optional count of consecutive prefixes and a metric"`
}
type FlapArgs struct {
	Seconds   int    `json:"seconds" jsonschema:"minimum=1,maximum=3600,description=How long the adjacency stays down"`
	Mode      string `json:"mode" jsonschema:"enum=reset,enum=silent,description=reset: keep sending hellos saying Down (the neighbor drops at once); silent: stop hellos (the neighbor's hold timer expires)"`
	Interface string `json:"interface,omitempty" jsonschema:"description=Device name; empty: every interface"`
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
	state      string
	sys        SysID
	mac        pkt.MAC
	extCircuit uint32
	levels     []int // levels the adjacency runs at
	hold       time.Duration
	lastHello  time.Time
	since      time.Time
	areas      []string
	addrs      []string
	mts        []uint16 // topologies in common
	threeWay   bool
	nbr        nbrAddrs // its hello addresses and BFD-enabled TLV
	helping    bool     // it is restarting (RR) and we help
	suppress   bool     // it asks us not to advertise the adjacency (SA)
	waitBFD    bool     // up but for BFD (RFC 6213)
}

type dbEntry struct {
	lsp     *LSP
	rxAt    time.Time // when the current copy arrived (or was originated)
	life    uint16    // remaining lifetime at rxAt
	own     bool
	srm     map[int]time.Time // circuits to send it on → last sent (retransmission on p2p)
	purgeAt time.Time         // zero-age entries are removed after ZeroAgeLifetime
	info    *LSPInfo          // decoded content, cached for status
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
	dirty     bool // regenerate our LSPs
	refreshAt time.Time

	gen     uint64 // bumped on every LSDB change
	spfGen  uint64 // gen of the last SPF run
	nextSPF time.Time
	reach   *reach // the last SPF run's result (L1, L1L2)
}

type session struct {
	env  protocol.Env
	cfg  Config
	sys  SysID
	host string

	mu        sync.Mutex
	areas     [][]byte
	overload  bool
	olUntil   time.Time // overload on startup
	gr        *grState  // our graceful restart in progress
	ipID      uint16
	prefixes  map[netip.Prefix]IPReach // configured + advertised
	loopbacks []netip.Prefix
	circs     []*circuit // [0]: the instance's own device
	lv        map[int]*level

	mts []uint16 // multi-topology: 0 plus the configured MTs (nil: single topology)

	sendKeys map[string]int // keychain scope → key ID sending now (-1: none)

	adjLabels map[string]map[string]uint32 // SR adjacency labels per originator
	endXFuncs map[string]map[string]uint16 // SRv6 End.X functions per originator
	sr        srState                      // what we advertise, for status

	topo     *topology // simulated routers behind us (nil: none)
	counters map[string]uint64
	events   []string

	stop chan struct{}
	done chan struct{}
}

func (isisType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	s := &session{env: env, cfg: cfg, overload: cfg.Overload, olUntil: time.Now().Add(time.Duration(cfg.OverloadOnStartup) * time.Second),
		prefixes: map[netip.Prefix]IPReach{}, lv: map[int]*level{}, counters: map[string]uint64{},
		stop: make(chan struct{}), done: make(chan struct{})}
	if cfg.SystemID != "" {
		s.sys, _ = ParseSysID(cfg.SystemID)
	} else {
		h := sha256.Sum256([]byte(env.InstanceName() + "/" + env.Device().MAC().String()))
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
	for _, l := range cfg.Loopbacks {
		p, _ := parseLoopback(l)
		s.loopbacks = append(s.loopbacks, p)
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
		if b := cfg.SRv6.TopologyLocatorBase; b != "" {
			loc := netip.MustParsePrefix(b).Masked()
			t.locators = map[string]netip.Prefix{}
			for i, n := range t.nodes {
				if i > 0 {
					var ok bool
					if loc, ok = nextPrefix(loc); !ok {
						return nil, errors.New("srv6: topology_locator_base runs out of locators")
					}
				}
				t.locators[n.name] = loc
			}
		}
	}
	first := Interface{Circuit: cfg.Circuit, Metric: cfg.Metric, Passive: cfg.Passive, TEMetric: cfg.TEMetric, DelayUS: cfg.DelayUS, AdminGroups: cfg.AdminGroups}
	s.circs = []*circuit{s.newCircuit(0, env.Device().Name(), first)}
	s.circs[0].mac = env.Device().MAC()
	s.addLocalsLocked(s.circs[0])
	for i, ic := range cfg.Interfaces {
		s.circs = append(s.circs, s.newCircuit(i+1, ic.Device, ic))
	}
	for _, c := range s.circs {
		if !c.attached() {
			c.attErr = "not attached yet"
		}
		for _, n := range cfg.levels() {
			if c.lan {
				s.electLocked(c, n, time.Now())
			}
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
	var atts []protocol.Attachment
	s.mu.Lock()
	for _, c := range s.circs {
		s.releaseLocalsLocked(c)
		if c.att != nil {
			atts = append(atts, c.att)
			c.att = nil
		}
	}
	s.mu.Unlock()
	for _, a := range atts {
		a.Close() // engine call: without mu
	}
}

func (s *session) count(k string) { s.counters[k]++ }

func (s *session) event(kind, msg string) {
	s.env.Emit(kind, msg, nil)
}

// ---- sending ----

func (s *session) sendTo(c *circuit, dst pkt.MAC, pdu []byte, what string) {
	s.sendFrame(c, dst, pkt.EtherTypeLLC, frame(pdu), what)
}

// sendFrame sends a payload of ethertype et on a circuit.
func (s *session) sendFrame(c *circuit, dst pkt.MAC, et uint16, payload []byte, what string) {
	send := s.env.Send
	if c.idx != 0 {
		s.mu.Lock()
		att := c.att
		s.mu.Unlock()
		if att == nil {
			return
		}
		send = att.Send
	}
	if err := send(dst, et, payload, protocol.SendOpts{SetPCP: true, PCP: 7}); err != nil {
		s.mu.Lock()
		s.count("tx_errors")
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.count("tx_" + what)
	s.mu.Unlock()
}

func (s *session) addrs(c *circuit) (v4, v6ll, v6glob []netip.Prefix) {
	ip := s.circuitIP(c)
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

// hello builds our point-to-point hello for a circuit. Caller holds mu.
func (s *session) helloLocked(c *circuit) []byte {
	v4, v6ll, _ := s.addrs(c)
	var a4, a6 []netip.Addr
	for _, p := range v4 {
		a4 = append(a4, p.Addr())
	}
	for _, p := range v6ll {
		a6 = append(a6, p.Addr())
	}
	tw := ThreeWay{State: AdjDown, ExtCircuit: uint32(c.idx + 1)}
	switch c.adj.state {
	case stInitializing:
		tw.State = AdjInitializing
	case stUp:
		tw.State = AdjUp
	}
	if c.adj.state != stDown {
		tw.NeighborSys, tw.NeighborExtCircuit, tw.HasNeighbor = c.adj.sys, c.adj.extCircuit, true
	}
	padTo := 0
	if s.cfg.HelloPadding {
		// 1500 − LLC (ports are ≥ 1500), leaving room for authentication.
		padTo = 1497 - c.helloAuth.tlvLen()
	}
	hold := uint16(s.cfg.HelloInterval * s.cfg.HelloMultiplier)
	if s.gr != nil {
		hold = s.gr.hold // neighbors hold us that long (RFC 8706 3.2.1)
	}
	extra := append(s.restartTLV(c, 0, time.Now()), s.bfdHello(c)...)
	return c.helloAuth.sign(EncodeP2PHello(s.cfg.circuitType(), s.sys, hold, byte(c.idx+1), s.areas, a4, a6, s.mts, tw, padTo, extra...))
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
			if len(s.circs) > 1 {
				s.attachLoop(now)
			}
			s.tick(now)
		}
	}
}

func (s *session) tick(now time.Time) {
	type pdu struct {
		c    *circuit
		dst  pkt.MAC
		b    []byte
		kind string
		et   uint16 // IP (BFD); 0: an IS-IS PDU
	}
	var out []pdu // sent after unlocking
	add := func(c *circuit, dst pkt.MAC, b []byte, kind string) { out = append(out, pdu{c, dst, b, kind, 0}) }
	addIP := func(c *circuit, dst pkt.MAC, et uint16, b []byte) { out = append(out, pdu{c, dst, b, "bfd", et}) }
	s.mu.Lock()
	s.keyRolloverLocked(now)
	if !s.olUntil.IsZero() && !now.Before(s.olUntil) {
		s.olUntil = time.Time{}
		if s.cfg.OverloadOnStartup > 0 {
			s.dirtyLocked()
			go s.event("overload", "overload on startup ended")
		}
	}
	s.grTickLocked(now)
	s.spfTickLocked(now)
	for _, c := range s.circs {
		s.bfdTickLocked(c, now, addIP)
		if !c.attached() || c.passive {
			continue
		}
		flapping := now.Before(c.flapTill)
		if c.lan {
			s.tickLANLocked(c, now, flapping, add)
			continue
		}
		// Hold timer.
		if c.adj.state != stDown && now.Sub(c.adj.lastHello) > c.adj.hold {
			s.adjDownLocked(c, now, "hold time expired")
		}
		// Hellos (jittered by up to 25%). A silent flap sends none; a reset
		// flap keeps sending them with state Down (we ignore the neighbor's).
		if !(flapping && c.flapMode == "silent") && !now.Before(c.nextHelo) {
			add(c, AllISs, s.helloLocked(c), "hello")
			iv := time.Duration(s.cfg.HelloInterval) * time.Second
			c.nextHelo = now.Add(iv - time.Duration(rand.Int63n(int64(iv)/4+1)))
		}
	}
	csnpEvery := time.Duration(s.cfg.CSNPInterval) * time.Second
	for _, n := range s.sortedLevels() {
		l := s.lv[n]
		// Our LSPs (held while we restart gracefully).
		if s.gr == nil && (l.dirty || now.After(l.refreshAt)) {
			s.originateLocked(l, now)
		}
		s.ageLocked(l, now)
		var csnps [][]byte // the same on every circuit
		ids := sortedIDs(l.db)
		for _, c := range s.circs {
			if !c.adjUpAt(n) {
				continue
			}
			if c.lan {
				dst, ll := s.lanDst(n), c.lans[n]
				// Only the DIS sends CSNPs on a LAN.
				if ll.isDIS && now.Sub(ll.lastCSNP) >= csnpEvery {
					if csnps == nil {
						csnps = s.csnpsLocked(l, now)
					}
					for _, b := range csnps {
						add(c, dst, b, "csnp")
					}
					ll.lastCSNP = now
				}
				// Flood once to the LAN; the DIS's CSNPs catch losses.
				for _, id := range ids {
					e := l.db[id]
					if _, ok := e.srm[c.idx]; ok {
						add(c, dst, lspWithLifetime(e.lsp.Raw, e.remaining(now)), "lsp")
						delete(e.srm, c.idx)
					}
				}
				continue
			}
			// Periodic CSNP.
			if now.Sub(c.lastCSNP[n]) >= csnpEvery {
				if csnps == nil {
					csnps = s.csnpsLocked(l, now)
				}
				for _, b := range csnps {
					add(c, AllISs, b, "csnp")
				}
				c.lastCSNP[n] = now
			}
			// Flood / retransmit (every 5 s until acknowledged).
			for _, id := range ids {
				e := l.db[id]
				if at, ok := e.srm[c.idx]; ok && now.Sub(at) >= 5*time.Second {
					add(c, AllISs, lspWithLifetime(e.lsp.Raw, e.remaining(now)), "lsp")
					e.srm[c.idx] = now
				}
			}
		}
	}
	s.mu.Unlock()
	for _, o := range out {
		if o.et != 0 {
			s.sendFrame(o.c, o.dst, o.et, o.b, o.kind)
			continue
		}
		s.sendTo(o.c, o.dst, o.b, o.kind)
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

// adjUpAt reports whether the circuit has an Up adjacency at level n.
func (c *circuit) adjUpAt(n int) bool {
	if !c.attached() {
		return false
	}
	if c.lan {
		return c.lanAdjUp(n)
	}
	if c.adj.state != stUp {
		return false
	}
	for _, x := range c.adj.levels {
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

func (s *session) adjDownLocked(c *circuit, now time.Time, why string) {
	if c.lan {
		for _, n := range s.sortedLevels() {
			ll := c.lans[n]
			for m, a := range ll.adjs {
				if a.state == stUp {
					c.transitions++
					go s.event("adjacency-down", fmt.Sprintf("%s L%d adjacency with %s down: %s", c.device, n, a.sys, why))
				}
				delete(ll.adjs, m)
			}
			s.electLocked(c, n, now)
			s.lv[n].dirty = true
		}
		return
	}
	if c.adj.state == stDown {
		return
	}
	wasUp := c.adj.state == stUp
	nb := c.adj.sys
	c.adj = adjacency{state: stDown, since: now}
	c.transitions++
	for _, l := range s.lv {
		l.dirty = true
		for _, e := range l.db {
			delete(e.srm, c.idx)
		}
	}
	if wasUp {
		go s.event("adjacency-down", fmt.Sprintf("%s adjacency with %s down: %s", c.device, nb, why))
	}
}

func (s *session) rxHello(c *circuit, f *pkt.Frame, p *PDU) {
	h := p.P2P
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count("rx_hello")
	if now.Before(c.flapTill) {
		return
	}
	if h.Source == s.sys {
		s.count("rx_hello_duplicate_system_id")
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
	c.adj.mts = mts
	c.adj.nbr = parseNbrAddrs(p.TLVs)
	if c.adj.state != stDown && c.adj.sys != h.Source {
		s.count("rx_hello_other_neighbor")
		return
	}
	prev := c.adj.state
	c.adj.sys, c.adj.mac, c.adj.levels = h.Source, f.Src, levels
	c.adj.hold = time.Duration(h.Hold) * time.Second
	c.adj.lastHello = now
	c.adj.areas = nil // fresh slices: Status hands these out
	for _, a := range p.Areas {
		c.adj.areas = append(c.adj.areas, AreaString(a))
	}
	c.adj.addrs = nil
	for _, v := range p.TLVs.All(TLVIPAddresses) {
		for ; len(v) >= 4; v = v[4:] {
			c.adj.addrs = append(c.adj.addrs, netip.AddrFrom4([4]byte(v[:4])).String())
		}
	}
	for _, v := range p.TLVs.All(TLVIPv6Addresses) {
		for ; len(v) >= 16; v = v[16:] {
			c.adj.addrs = append(c.adj.addrs, netip.AddrFrom16([16]byte(v[:16])).String())
		}
	}
	// Graceful restart: a restarting neighbor keeps its adjacency; its
	// acknowledgement of our restart.
	r, hasR := parseRestart(p.TLVs)
	keep := s.helpLocked(c, levels, r, hasR, prev == stUp, &c.adj.helping, &c.adj.suppress, h.Source, now)
	s.grAckLocked(c, levels, r, hasR, now)
	next := stUp
	if tw := h.ThreeWay; tw != nil {
		c.adj.threeWay = true
		c.adj.extCircuit = tw.ExtCircuit
		switch {
		case tw.HasNeighbor && tw.NeighborSys != s.sys:
			s.adjDownLocked(c, now, "neighbor reports a different system")
			return
		case tw.State == AdjDown:
			next = stInitializing
		case tw.State == AdjInitializing || tw.State == AdjUp:
			if !tw.HasNeighbor {
				next = stInitializing
			}
		}
		if keep {
			next = stUp
		}
		if prev == stUp && tw.State == AdjDown && !keep {
			s.adjDownLocked(c, now, "neighbor reset the adjacency")
			c.adj.sys, c.adj.mac, c.adj.lastHello, c.adj.hold, c.adj.levels = h.Source, f.Src, now, time.Duration(h.Hold)*time.Second, levels
			c.adj.state = stInitializing
			c.nextHelo = now
			return
		}
	}
	// RFC 6213: with both sides sending the BFD-enabled TLV, up waits for BFD.
	c.adj.waitBFD = next == stUp && prev != stUp && s.bfdHolds(c, c.adj.nbr) && !c.bfdUp(f.Src)
	if c.adj.waitBFD {
		next = stInitializing
		s.count("adj_waiting_bfd")
	}
	if next != prev {
		c.adj.state = next
		c.adj.since = now
		c.nextHelo = now // answer right away so the 3-way handshake is quick
		if next == stUp {
			c.transitions++
			for _, n := range levels {
				l := s.lv[n]
				l.dirty = true
				c.lastCSNP[n] = time.Time{} // send a CSNP now
				for _, e := range l.db {
					e.flag(c.idx) // send everything we have
				}
			}
			go s.event("adjacency-up", fmt.Sprintf("%s adjacency with %s up (level %v)", c.device, h.Source, levels))
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

func (s *session) rxLSP(c *circuit, p *PDU) {
	l := p.LSP
	now := time.Now()
	var out [][]byte
	s.mu.Lock()
	s.count("rx_lsp")
	lv := s.lv[l.Level]
	if lv == nil || !c.adjUpAt(l.Level) {
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
		} else if l.ID[6] != 0 && frag == 0 && s.pnCircuit(l.ID[6], l.Level) != nil {
			// Our pseudonode (purged, or from before a restart).
			s.bumpOwnLocked(lv, l.ID, l.Seq)
		} else {
			// A fragment we no longer originate: purge it.
			lv.db[l.ID] = &dbEntry{lsp: mustLSP(s.signLSP(purgeRaw(l.Level, l.ID, l.Seq, l.Flags&3))), rxAt: now, own: true, srm: s.allSRM(), purgeAt: now.Add(60 * time.Second)}
			lv.gen++
		}
		if !c.lan {
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
		lv.gen++
		if g := s.gr; g != nil {
			if want, ok := g.pending[l.ID]; ok && l.Seq >= want {
				delete(g.pending, l.ID)
			}
		}
		s.floodLocked(ne, c.idx)
		s.count("lsdb_updates")
		// Acknowledge (p2p: PSNP; on a LAN the DIS's CSNPs do).
		if !c.lan {
			out = append(out, s.signSNP(EncodePSNP(l.Level, s.nodeID(), []LSPEntry{{l.Lifetime, l.ID, l.Seq, l.Checksum}})))
		}
	case cmp == 0:
		if e != nil {
			delete(e.srm, c.idx)
			if own && e.own && l.Lifetime != 0 && l.Checksum != e.lsp.Checksum {
				// Same sequence, different content: a copy from before a
				// restart. Move past it (ISO 10589 7.3.16.1).
				s.bumpOwnLocked(lv, l.ID, l.Seq)
			}
		}
		if !c.lan {
			out = append(out, s.signSNP(EncodePSNP(l.Level, s.nodeID(), []LSPEntry{{l.Lifetime, l.ID, l.Seq, l.Checksum}})))
		}
	default:
		// Ours is newer: send it.
		e.flag(c.idx)
	}
	s.mu.Unlock()
	for _, b := range out {
		s.sendTo(c, AllISs, b, "psnp")
	}
}

func (s *session) nodeID() NodeID {
	var n NodeID
	copy(n[:], s.sys[:])
	return n
}

func (s *session) rxSNP(c *circuit, p *PDU) {
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
	if lv == nil || !c.adjUpAt(sn.Level) {
		s.mu.Unlock()
		return
	}
	s.grCSNPLocked(c, sn)
	want := func(x LSPEntry) {
		reqs = append(reqs, LSPEntry{x.Lifetime, x.ID, 0, 0})
		if g := s.gr; g != nil && x.Lifetime != 0 {
			g.pending[x.ID] = x.Seq
		}
	}
	listed := map[LSPID]bool{}
	for _, x := range sn.Entries {
		listed[x.ID] = true
		e := lv.db[x.ID]
		switch {
		case e == nil:
			if x.Lifetime != 0 && x.Seq != 0 {
				want(x) // request it
			}
		default:
			switch newer(x.Seq, x.Lifetime, x.Checksum, e, now) {
			case 0:
				delete(e.srm, c.idx) // acknowledged / in sync
				if e.own && x.Lifetime != 0 && x.Checksum != e.lsp.Checksum && s.originates(x.ID.Node().Sys()) {
					s.bumpOwnLocked(lv, x.ID, x.Seq)
				}
			case 1:
				if s.originates(x.ID.Node().Sys()) {
					// Neighbor has a newer copy of an LSP we originate.
					if o, f := lv.orig[x.ID.Node().Sys()], x.ID[7]; x.ID[6] == 0 && o != nil && int(f) < o.frags {
						s.bumpOwnLocked(lv, x.ID, x.Seq)
					} else if x.ID[6] != 0 && s.pnCircuit(x.ID[6], sn.Level) != nil {
						s.bumpOwnLocked(lv, x.ID, x.Seq)
					}
				} else {
					want(x)
				}
			case -1:
				e.flag(c.idx)
			}
		}
	}
	if sn.Complete {
		// LSPs in the range the neighbor doesn't have: send them.
		for id, e := range lv.db {
			if !listed[id] && bytes.Compare(id[:], sn.Start[:]) >= 0 && bytes.Compare(id[:], sn.End[:]) <= 0 && e.remaining(now) > 0 {
				e.flag(c.idx)
			}
		}
	}
	s.mu.Unlock()
	if len(reqs) > 0 {
		dst := AllISs
		if c.lan {
			dst = s.lanDst(sn.Level)
		}
		s.sendTo(c, dst, s.signSNP(EncodePSNP(sn.Level, s.nodeID(), reqs)), "psnp")
	}
}

func (s *session) HandleRx(f *pkt.Frame) { s.rx(s.circs[0], f) }

// rx handles a frame received on a circuit.
func (s *session) rx(c *circuit, f *pkt.Frame) {
	s.mu.Lock()
	ok := c.attached() && !c.passive
	mac := c.mac
	s.mu.Unlock()
	if !ok || f.Src == mac {
		return
	}
	if f.EtherType != pkt.EtherTypeLLC {
		if f.Dst == mac {
			s.rxBFD(c, f)
		}
		return
	}
	if f.Dst != AllISs && f.Dst != AllL1ISs && f.Dst != AllL2ISs && f.Dst != mac {
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
		kind, err = "hello", c.helloAuth.verify(p.Raw)
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
			go s.event("auth-failure", fmt.Sprintf("%s from %s on %s: %v", kind, f.Src, c.device, err))
		}
		return
	}
	switch {
	case p.P2P != nil:
		if c.lan {
			s.mu.Lock()
			s.count("rx_p2p_hello_ignored")
			s.mu.Unlock()
			return
		}
		s.rxHello(c, f, p)
	case p.LSP != nil:
		s.rxLSP(c, p)
	case p.SNP != nil:
		s.rxSNP(c, p)
	case p.LAN != nil:
		if !c.lan {
			s.mu.Lock()
			s.count("rx_lan_hello_ignored")
			s.mu.Unlock()
			return
		}
		s.rxLANHello(c, f, p)
	}
}

// ageLocked expires LSPs whose lifetime ran out and removes old purges.
func (s *session) ageLocked(l *level, now time.Time) {
	for id, e := range l.db {
		if !e.purgeAt.IsZero() {
			if now.After(e.purgeAt) {
				delete(l.db, id)
				l.gen++
			}
			continue
		}
		if e.remaining(now) == 0 && !e.own {
			e.purgeAt = now.Add(60 * time.Second)
			l.gen++
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
	v4, v6g, connMetric := s.interfaceAddrs()
	body := lspBody{Areas: s.areas, Hostname: s.host, Overload: s.overload || now.Before(s.olUntil)}
	if l.n == 1 {
		body.Attached = s.attachedLocked()
	}
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
	// A neighbor per point-to-point adjacency; on a LAN, the pseudonode
	// (once a DIS is up), with the Up neighbors for LAN-Adj-SIDs.
	lanNbrs := map[NodeID][]SysID{}
	for _, c := range s.circs {
		if !c.adjUpAt(l.n) {
			continue
		}
		if !c.lan {
			if c.adj.suppress {
				continue // the neighbor is restarting and asked (SA)
			}
			var nb NodeID
			copy(nb[:], c.adj.sys[:])
			body.Neighbors = append(body.Neighbors, ISReach{Neighbor: nb, Metric: c.metric, Attrs: s.circuitAttrs(c)})
			continue
		}
		ll := c.lans[l.n]
		body.Neighbors = append(body.Neighbors, ISReach{Neighbor: ll.lanID, Metric: c.metric, Attrs: s.circuitAttrs(c)})
		var up []SysID
		for _, a := range ll.adjs {
			if a.state == stUp {
				up = append(up, a.sys)
			}
		}
		sort.Slice(up, func(i, j int) bool { return bytes.Compare(up[i][:], up[j][:]) < 0 })
		lanNbrs[ll.lanID] = up
	}
	// Connected subnets, then injected prefixes.
	var pfx []IPReach
	seen := map[netip.Prefix]bool{}
	for _, p := range append(v4, v6g...) {
		m := p.Masked()
		if !seen[m] {
			seen[m] = true
			pfx = append(pfx, IPReach{Prefix: m, Metric: connMetric[m]})
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
		seen[k] = true
	}
	// L1L2: prefixes from the other level.
	inter, interSIDs := s.interLevelLocked(l.n, seen)
	pfx = append(pfx, inter...)
	isType := s.isType(l.n)
	if s.topo != nil {
		body.Neighbors = append(body.Neighbors, s.topo.attachedLocked(s)...)
	}
	var sids map[netip.Prefix][]PrefixSID
	if s.cfg.SegmentRouting.Enabled {
		pfx, sids = srPrefixes(s.cfg.SegmentRouting.NodeSIDs, pfx)
		for p, x := range interSIDs {
			if _, ours := sids[p]; !ours {
				sids[p] = x
			}
		}
		body.Bindings = s.bindings()
	}
	body.SR = s.srCap(body.RouterID, len(v6g) > 0, true)
	st := &srState{}
	body.Locators = s.ourLocators()
	pfx = locatorPrefixes(body.Locators, pfx, st)
	pfx = s.mtify(&body, pfx)
	pfx = s.srDecorate("", &body, pfx, sids, lanNbrs, st)
	s.endXDecorate("", &body, lanNbrs, st)
	if (s.cfg.SegmentRouting.Enabled || s.cfg.SRv6.Enabled) && (l.n == 2 || s.cfg.Level == "L1") {
		s.sr = *st
	}
	s.originateSys(l, s.sys, body, pfx, isType, now)
	if s.topo != nil {
		s.topo.originateLocked(s, l, isType, now)
	}
	for _, c := range s.circs {
		if c.lan {
			s.pseudonodeLocked(c, l, isType, now)
		}
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
	} else if c := s.pnCircuit(id[6], l.n); c != nil {
		if ll := c.lans[l.n]; ll.pnSeq <= seq {
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
			l.db[id] = &dbEntry{lsp: mustLSP(s.signLSP(EncodeLSP(l.n, id, seq, life, isType, b))), rxAt: now, life: life, own: true, srm: s.allSRM()}
			l.gen++
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
			l.db[id] = &dbEntry{lsp: mustLSP(s.signLSP(purgeRaw(l.n, id, seq, isType))), rxAt: now, own: true, srm: s.allSRM(), purgeAt: now.Add(60 * time.Second)}
			l.gen++
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
		if len(p.SIDs) > 0 {
			sz += 1 + 8*len(p.SIDs) // sub-TLV length + Prefix-SID sub-TLVs
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
	Interface   string   `json:"interface,omitempty"`
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

	BFD        []BFDPeerStatus `json:"bfd,omitempty"`
	WaitingBFD bool            `json:"waiting_for_bfd,omitempty"` // RFC 6213
	Restarting bool            `json:"restarting,omitempty"`      // the neighbor restarts (we help)
	Suppressed bool            `json:"suppressed,omitempty"`      // left out of our LSPs (SA)
}

// InterLevelStatus is what an L1L2 router carries between levels, and the
// attached routers an L1 router reaches.
type InterLevelStatus struct {
	Attached        bool     `json:"attached"`         // ATT in our level-1 LSPs
	ToL2            int      `json:"to_l2"`            // level-1 prefixes (and summaries) in our level-2 LSPs
	LeakedToL1      int      `json:"leaked_to_l1"`     // level-2 prefixes leaked into level 1
	AttachedRouters []string `json:"attached_routers"` // L1: reachable L1L2 routers setting ATT
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
	OverloadEnd string            `json:"overload_on_startup_until,omitempty"`
	GR          GRStatus          `json:"graceful_restart"`
	BFD         bool              `json:"bfd"` // on any interface
	InterLevel  *InterLevelStatus `json:"inter_level,omitempty"`
	Flapping    bool              `json:"flapping,omitempty"`
	Prefixes    int               `json:"advertised_prefixes"`
	Loopbacks   []string          `json:"loopbacks,omitempty"`
	Topologies  []string          `json:"topologies"` // multi-topology (ipv4-unicast alone: single topology)
	SR          *SRStatus         `json:"segment_routing,omitempty"`
	HelloAuth   string            `json:"hello_auth"` // the first interface's; type only, never the key
	LSPAuth     string            `json:"lsp_auth"`
	Interfaces  []IfStatus        `json:"interfaces"`
	Adjacency   AdjStatus         `json:"adjacency"`   // the first interface's (if Up, else the first Up one)
	Adjacencies []AdjStatus       `json:"adjacencies"` // all neighbors on all interfaces
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
	c0 := s.circs[0]
	st := Status{SystemID: s.sys.String(), Hostname: s.host, Level: s.cfg.Level, Circuit: c0.circuitName(),
		Areas: s.cfg.Areas, Metric: c0.metric, Overload: s.overload || now.Before(s.olUntil), Flapping: now.Before(c0.flapTill), Prefixes: len(s.prefixes),
		GR:   s.grStatusLocked(now),
		LSDB: []LSPStatus{}, Counters: map[string]uint64{}, HelloAuth: authType(c0.helloAuth), LSPAuth: authType(s.cfg.LSPAuth), Topologies: mtNameList(s.mts)}
	if len(s.mts) == 0 {
		st.Topologies = []string{"ipv4-unicast (single topology)"}
	}
	if now.Before(s.olUntil) {
		st.OverloadEnd = s.olUntil.Format(time.RFC3339)
	}
	for _, c := range s.circs {
		st.BFD = st.BFD || c.bfd
	}
	switch s.cfg.Level {
	case "L1L2":
		to2, _ := s.interLevelLocked(2, nil)
		to1, _ := s.interLevelLocked(1, nil)
		st.InterLevel = &InterLevelStatus{Attached: s.attachedLocked(), ToL2: len(to2), LeakedToL1: len(to1), AttachedRouters: []string{}}
	case "L1":
		st.InterLevel = &InterLevelStatus{AttachedRouters: []string{}}
		if r := s.lv[1].reach; r != nil {
			for _, x := range r.attRtrs {
				st.InterLevel.AttachedRouters = append(st.InterLevel.AttachedRouters, x.String())
			}
		}
	}
	for _, p := range s.loopbacks {
		st.Loopbacks = append(st.Loopbacks, p.String())
	}
	if sr := s.cfg.SegmentRouting; sr.Enabled || s.cfg.SRv6.Enabled {
		st.SR = &SRStatus{Enabled: sr.Enabled, NodeSIDs: s.sr.NodeSIDs, AdjSIDs: s.sr.AdjSIDs, SRv6SIDs: s.sr.SRv6SIDs}
		if sr.Enabled {
			st.SR.SRGBBase, st.SR.SRGBSize = sr.SRGBBase, sr.SRGBSize
			if sr.SRLBSize > 0 {
				st.SR.SRLB = &SRGBInfo{Base: sr.SRLBBase, Size: sr.SRLBSize}
			}
			for _, b := range s.bindings() {
				st.SR.Mappings = append(st.SR.Mappings, BindingInfo{Prefix: b.Prefix.String(), Range: b.Range, Index: b.Index, Algo: b.Algo, MT: b.MT})
			}
		}
		for _, f := range s.cfg.FlexAlgos {
			fi := FADInfo{Algorithm: f.Algorithm, MetricType: f.MetricType, Priority: f.Priority,
				ExcludeAny: groupMask(f.ExcludeAny), IncludeAny: groupMask(f.IncludeAny), IncludeAll: groupMask(f.IncludeAll)}
			if fi.MetricType == "" {
				fi.MetricType = "igp"
			}
			if fi.Priority == 0 {
				fi.Priority = 128
			}
			fi.ParticipateOnly = f.ParticipateOnly
			st.SR.FlexAlgos = append(st.SR.FlexAlgos, fi)
		}
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
	st.Adjacencies, st.Interfaces = []AdjStatus{}, []IfStatus{}
	st.Adjacency = AdjStatus{Interface: c0.device, State: stDown}
	total := 0
	for _, c := range s.circs {
		st.Interfaces = append(st.Interfaces, s.ifStatusLocked(c, now))
		total += c.transitions
		if c.lan {
			adjs, lans := s.lanStatusLocked(c, now)
			st.Adjacencies = append(st.Adjacencies, adjs...)
			st.LAN = append(st.LAN, lans...)
			continue
		}
		a := c.adj
		as := AdjStatus{Interface: c.device, State: a.state, ThreeWay: a.threeWay, Transitions: c.transitions}
		if a.state != stDown {
			as.Neighbor, as.MAC, as.Levels = a.sys.String(), a.mac.String(), a.levels
			as.HoldS = (a.hold - now.Sub(a.lastHello)).Seconds()
			as.Areas, as.Addresses = a.areas, a.addrs
			as.Topologies = mtNameList(a.mts)
			as.Hostname = names[a.sys]
			as.BFD, as.WaitingBFD, as.Restarting, as.Suppressed = c.bfdStatus(a.mac), a.waitBFD, a.helping, a.suppress
		}
		if !a.since.IsZero() {
			as.Since = a.since.Format(time.RFC3339)
		}
		if c.idx == 0 {
			st.Adjacency = as
		}
		if a.state != stDown {
			st.Adjacencies = append(st.Adjacencies, as)
		}
	}
	for i := range st.Adjacencies {
		x := &st.Adjacencies[i]
		if sys, err := ParseSysID(x.Neighbor); err == nil {
			x.Hostname = names[sys]
		}
		if x.State == stUp && st.Adjacency.State != stUp {
			st.Adjacency = *x
		}
	}
	st.Adjacency.Transitions = total
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
	dirty := s.dirtyLocked
	switch name {
	case "overload":
		s.overload = args.(*OverloadArgs).Set
		dirty()
		return map[string]any{"overload": s.overload}, nil
	case "metric":
		ma := args.(*MetricArgs)
		cs, err := s.circuitsNamed(ma.Interface)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			c.metric = ma.Metric
		}
		dirty()
		return map[string]any{"metric": ma.Metric, "interfaces": len(cs)}, nil
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
		cs, err := s.circuitsNamed(fa.Interface)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		till := now.Add(time.Duration(fa.Seconds) * time.Second)
		for _, c := range cs {
			c.flapTill, c.flapMode, c.nextHelo = till, fa.Mode, now
			for _, ll := range c.lans {
				ll.nextHello = now
			}
			s.adjDownLocked(c, now, "flap action")
		}
		return map[string]any{"down_until": till.Format(time.RFC3339), "interfaces": len(cs)}, nil
	case "lsp":
		return s.lspDetailLocked(args.(*LSPArgs))
	case "graceful-restart":
		return s.grStartLocked(args.(*GRArgs), time.Now())
	case "bfd-fail":
		ba := args.(*BFDArgs)
		cs, err := s.circuitsNamed(ba.Interface)
		if err != nil {
			return nil, err
		}
		till := time.Now().Add(time.Duration(ba.Seconds) * time.Second)
		n := 0
		for _, c := range cs {
			if c.bfd {
				c.bfdSilentTill = till
				n += len(c.bfdPeers)
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("no BFD sessions")
		}
		return map[string]any{"silent_until": till.Format(time.RFC3339), "sessions": n}, nil
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
	type scope struct {
		name string
		a    Auth
	}
	scopes := []scope{{"hello", s.circs[0].helloAuth}, {"lsp", s.cfg.LSPAuth}}
	for _, c := range s.circs[1:] {
		scopes = append(scopes, scope{"hello on " + c.device, c.helloAuth})
	}
	for _, sc := range scopes {
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
			for _, c := range s.circs {
				for _, ll := range c.lans {
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

// Health is up or down, or how many non-passive interfaces have an Up
// adjacency when there are several (protocol.Healther).
func (s *session) Health() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, up := 0, 0
	for _, c := range s.circs {
		if c.passive {
			continue
		}
		n++
		if c.attached() && (c.lan && (c.lanAdjUp(1) || c.lanAdjUp(2)) || !c.lan && c.adj.state == stUp) {
			up++
		}
	}
	switch {
	case n == 0:
		return "passive"
	case n == 1 && up == 1:
		return "up"
	case n == 1:
		return "down"
	}
	return fmt.Sprintf("%d/%d up", up, n)
}
