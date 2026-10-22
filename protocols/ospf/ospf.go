// Package ospf is an emulated OSPFv2 router (RFC 2328) in one area, with
// one or more interfaces (iface.go), each point-to-point or broadcast (DR
// and BDR election): neighbor state machine and database exchange
// (nbr.go), flooding with retransmission, ageing and refresh (lsdb.go),
// our router LSA, a network LSA where we are DR, injected prefixes as
// AS-external or stub links, and actions to flap, change costs, set
// max-metric and advertise/withdraw prefixes. It computes no routes.
package ospf

import (
	"context"
	"fmt"
	"math/rand"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "ospf"

func init() { protocol.Register(ospfType{}) }

type ospfType struct{}

func (ospfType) Name() string { return TypeName }
func (ospfType) Description() string {
	return "OSPFv2 router with one or more interfaces (point-to-point or broadcast with DR/BDR): adjacencies, LSDB sync and flooding, router/network LSAs, route injection (external or stub), flap/cost/max-metric actions"
}
func (ospfType) NewConfig() any       { return defaultConfig() }
func (ospfType) EtherTypes() []uint16 { return []uint16{pkt.EtherTypeIPv4} }
func (ospfType) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "max-metric", Description: "Set or clear max-metric (stub router, RFC 6987): transit links at 65535", NewArgs: func() any { return &MaxMetricArgs{Set: true} }},
		{Name: "cost", Description: "Change an interface's cost (default: every interface)", NewArgs: func() any { return &CostArgs{Cost: 10} }},
		{Name: "advertise", Description: "Advertise more prefixes (until withdrawn or restarted)", NewArgs: func() any { return &PrefixArgs{} }},
		{Name: "withdraw", Description: "Stop advertising prefixes (injected or from config)", NewArgs: func() any { return &PrefixArgs{} }},
		{Name: "flap", Description: "Bring adjacencies down for a while (one interface or all), then let them come back", NewArgs: func() any { return &FlapArgs{Seconds: 10, Mode: "reset"} }},
		{Name: "lsa", Description: "Show one LSA's content", NewArgs: func() any { return &LSAArgs{} }},
		{Name: "clear", Description: "Reset counters"},
	}
}

type MaxMetricArgs struct {
	Set bool `json:"set"`
}
type CostArgs struct {
	Cost      uint16 `json:"cost" jsonschema:"minimum=1,maximum=65535"`
	Interface string `json:"interface,omitempty" jsonschema:"description=Device name; empty: every interface"`
}
type PrefixArgs struct {
	Prefixes []Prefix `json:"prefixes" jsonschema:"description=Prefixes to add or remove; each with an optional count of consecutive prefixes"`
}
type FlapArgs struct {
	Seconds   int    `json:"seconds" jsonschema:"minimum=1,maximum=3600,description=How long adjacencies stay down"`
	Mode      string `json:"mode" jsonschema:"enum=reset,enum=silent,description=reset: hellos without neighbors (the neighbor drops at once); silent: no hellos (its dead timer expires)"`
	Interface string `json:"interface,omitempty" jsonschema:"description=Device name; empty: every interface"`
}
type LSAArgs struct {
	Type   string `json:"type" jsonschema:"enum=router,enum=network,enum=summary,enum=asbr-summary,enum=external,description=LSA type"`
	ID     string `json:"id" jsonschema:"description=Link state ID"`
	AdvRtr string `json:"adv_router,omitempty" jsonschema:"description=Advertising router. Default: the only match"`
}

func (a *CostArgs) Validate() error {
	if a.Cost == 0 {
		return fmt.Errorf("cost must be 1-65535")
	}
	return nil
}

func (a *FlapArgs) Validate() error {
	if a.Mode != "reset" && a.Mode != "silent" {
		return fmt.Errorf("mode must be reset or silent")
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

type session struct {
	env  protocol.Env
	cfg  Config
	rid  netip.Addr
	area netip.Addr

	mu        sync.Mutex
	sendMu    sync.Mutex // keeps packets in sequence-number order (MD5)
	ifs       []*iface   // [0]: the instance's own device
	db        map[LSAKey]*lsa
	routes    map[netip.Prefix]route // configured + advertised
	loopbacks []netip.Prefix
	maxMetric bool
	dirty     bool      // regenerate our LSAs
	genAt     time.Time // earliest next regeneration
	ipID      uint16
	counters  map[string]uint64
	out       []outPkt // queued under mu, sent after
	rxmt      time.Duration
	refresh   time.Duration

	stop chan struct{}
	done chan struct{}
}

// outPkt is a packet to send once mu is released.
type outPkt struct {
	ifc  *iface
	dst  netip.Addr
	mac  pkt.MAC
	typ  uint8
	body []byte
}

func (ospfType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	s := &session{env: env, cfg: cfg, db: map[LSAKey]*lsa{}, routes: map[netip.Prefix]route{}, maxMetric: cfg.MaxMetric,
		counters: map[string]uint64{}, dirty: true, ipID: uint16(rand.Intn(65536)),
		rxmt: time.Duration(cfg.RetransmitInterval) * time.Second, refresh: time.Duration(cfg.LSARefresh) * time.Second,
		stop: make(chan struct{}), done: make(chan struct{})}
	s.area, _ = ParseArea(cfg.Area)
	for _, l := range cfg.Loopbacks {
		p, _ := parseLoopback(l)
		s.loopbacks = append(s.loopbacks, p)
	}
	switch {
	case cfg.RouterID != "":
		s.rid = netip.MustParseAddr(cfg.RouterID)
	case len(s.loopbacks) > 0:
		s.rid = s.loopbacks[0].Addr()
	default:
		if ip := env.IP(); ip != nil {
			for _, p := range ip.Addresses() {
				if p.Addr().Is4() {
					s.rid = p.Addr()
					break
				}
			}
		}
	}
	if !s.rid.IsValid() {
		return nil, fmt.Errorf("no router ID: set router_id, a loopback or an IPv4 address on the device")
	}
	for _, p := range cfg.Prefixes {
		rs, _ := p.expand()
		for _, r := range rs {
			s.routes[r.prefix] = r
		}
	}
	first := Interface{Device: env.Device().Name(), Network: cfg.Network, Cost: cfg.Cost, Passive: cfg.Passive}
	s.ifs = []*iface{s.newIface(0, first)}
	s.ifs[0].mac = env.Device().MAC()
	for i, ic := range cfg.Interfaces {
		s.ifs = append(s.ifs, s.newIface(i+1, ic))
	}
	s.mu.Lock()
	s.addLocalsLocked(s.ifs[0])
	now := time.Now()
	for _, f := range s.ifs {
		if f.attached() {
			s.ifUpLocked(f, now)
		} else {
			f.err = "not attached yet"
		}
	}
	s.mu.Unlock()
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
	for _, f := range s.ifs {
		s.releaseLocalsLocked(f)
		if f.att != nil {
			atts = append(atts, f.att)
			f.att = nil
		}
	}
	s.mu.Unlock()
	for _, a := range atts {
		a.Close()
	}
}

func (s *session) count(k string) { s.counters[k]++ }

func (s *session) event(kind, msg string) { s.env.Emit(kind, msg, nil) }

// ---- main loop ----

const tick = 100 * time.Millisecond

func (s *session) run() {
	defer close(s.done)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.attachLoop(now)
			s.mu.Lock()
			s.timersLocked(now)
			out := s.takeOutLocked()
			s.mu.Unlock()
			s.flush(out)
		}
	}
}

func (s *session) timersLocked(now time.Time) {
	for _, f := range s.ifs {
		if f.state == ifDown {
			continue
		}
		s.ifTimersLocked(f, now)
	}
	s.ageLocked(now)
	if s.dirty && !now.Before(s.genAt) {
		s.originateLocked(now)
	}
}

// ---- sending ----

func (s *session) queue(f *iface, dst netip.Addr, mac pkt.MAC, typ uint8, body []byte) {
	s.out = append(s.out, outPkt{ifc: f, dst: dst, mac: mac, typ: typ, body: body})
}

func (s *session) takeOutLocked() []outPkt {
	out := s.out
	s.out = nil
	return out
}

// flush sends queued packets: OSPF header and authentication, then IP.
func (s *session) flush(out []outPkt) {
	if len(out) == 0 {
		return
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	for _, o := range out {
		s.mu.Lock()
		f := o.ifc
		src, ok := s.ifAddrLocked(f)
		send := s.env.Send
		if f.idx != 0 {
			if f.att == nil {
				ok = false
			} else {
				send = f.att.Send
			}
		}
		f.cryptoSeq++
		seq := f.cryptoSeq
		s.ipID++
		id := s.ipID
		s.mu.Unlock()
		if !ok {
			continue
		}
		p := sign(encodeHeader(o.typ, s.rid, s.area, o.body), f.auth, seq)
		ip := pkt.IPv4{TOS: 0xc0, ID: id, TTL: 1, Protocol: ProtoOSPF, Src: src.Addr(), Dst: o.dst, Payload: p}
		mac := o.mac
		if o.dst.IsMulticast() {
			mac = pkt.MulticastMAC(o.dst)
		}
		err := send(mac, pkt.EtherTypeIPv4, ip.Encode(), protocol.SendOpts{SetPCP: true, PCP: 6})
		s.mu.Lock()
		if err != nil {
			s.count("tx_errors")
		} else {
			s.count("tx_" + typeNames[o.typ])
		}
		s.mu.Unlock()
	}
}

// maxBody is the room for a packet body on an interface.
func (s *session) maxBody(f *iface) int {
	n := s.cfg.MTU - 20 - headerLen
	if authType(f.auth) == AuthCrypto {
		n -= 16
	}
	return n
}

// ---- receiving ----

func (s *session) HandleRx(fr *pkt.Frame) { s.rx(s.ifs[0], fr) }

func (s *session) rx(f *iface, fr *pkt.Frame) {
	ip, err := pkt.DecodeIPv4(fr.Payload)
	if ip == nil || err != nil || ip.Protocol != ProtoOSPF {
		return
	}
	if ip.Flags&pkt.IPv4MF != 0 || ip.FragOff != 0 {
		s.mu.Lock()
		s.count("rx_fragments")
		s.mu.Unlock()
		return
	}
	h, err := DecodeHeader(ip.Payload)
	s.mu.Lock()
	defer func() {
		out := s.takeOutLocked()
		s.mu.Unlock()
		s.flush(out)
	}()
	if f.state == ifDown || f.passive {
		return
	}
	mine, ok := s.ifAddrLocked(f)
	if !ok || ip.Src == mine.Addr() {
		return
	}
	switch {
	case ip.Dst == AllSPFRouters, ip.Dst == mine.Addr():
	case ip.Dst == AllDRouters:
		if f.state != ifDR && f.state != ifBackup {
			return
		}
	default:
		return
	}
	if err != nil {
		s.count("rx_errors")
		return
	}
	if h.AreaID != s.area {
		s.count("rx_area_mismatch")
		return
	}
	if h.RouterID == s.rid {
		s.count("rx_own_router_id")
		return
	}
	if !f.p2p && !mine.Contains(ip.Src) {
		s.count("rx_wrong_subnet")
		return
	}
	seq, crypto, err := verify(h, f.auth)
	if err != nil {
		s.count("rx_auth_failures")
		f.authErr = err.Error()
		return
	}
	now := time.Now()
	if h.Type == TypeHello {
		hello, err := DecodeHello(h.Body)
		if err != nil {
			s.count("rx_errors")
			return
		}
		s.count("rx_hello")
		s.rxHelloLocked(f, h, ip.Src, fr.Src, hello, seq, crypto, now)
		return
	}
	n := f.nbrs[h.RouterID]
	if n == nil {
		s.count("rx_unknown_neighbor")
		return
	}
	if crypto {
		if seq < n.cryptoSeq {
			s.count("rx_auth_failures")
			f.authErr = "sequence number went down"
			return
		}
		n.cryptoSeq = seq
	}
	s.count("rx_" + typeNames[h.Type])
	switch h.Type {
	case TypeDBD:
		if d, err := DecodeDBD(h.Body); err == nil {
			s.rxDBDLocked(n, d, now)
		}
	case TypeLSR:
		if keys, err := DecodeLSR(h.Body); err == nil {
			s.rxLSRLocked(n, keys, now)
		}
	case TypeLSU:
		if lsas, err := DecodeLSU(h.Body); err == nil {
			s.rxLSULocked(n, lsas, now)
		} else {
			s.count("rx_errors")
		}
	case TypeAck:
		if hs, err := DecodeAck(h.Body); err == nil {
			s.rxAckLocked(n, hs)
		}
	}
}

// ---- status ----

type Status struct {
	RouterID   string            `json:"router_id"`
	Area       string            `json:"area"`
	MaxMetric  bool              `json:"max_metric,omitempty"`
	ASBR       bool              `json:"asbr,omitempty"`
	Auth       string            `json:"auth"` // the first interface's; type only
	Loopbacks  []string          `json:"loopbacks,omitempty"`
	Prefixes   int               `json:"advertised_prefixes"`
	Interfaces []IfStatus        `json:"interfaces"`
	Neighbors  []NbrStatus       `json:"neighbors"`
	LSDB       []LSAStatus       `json:"lsdb"`
	LSDBTotal  int               `json:"lsdb_total"`
	LSDBTypes  map[string]int    `json:"lsdb_types"`
	Counters   map[string]uint64 `json:"counters"`
}

type LSAStatus struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	AdvRtr   string `json:"adv_router"`
	Seq      string `json:"seq"`
	Age      int    `json:"age"`
	Checksum string `json:"checksum"`
	Length   int    `json:"length"`
	Own      bool   `json:"own,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

// maxStatusLSAs caps the LSDB listed in the status; lsdb_total has the
// full count.
const maxStatusLSAs = 500

func (s *session) Status() any {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{RouterID: s.rid.String(), Area: s.area.String(), MaxMetric: s.maxMetric, Auth: s.ifs[0].auth.Type,
		Prefixes: len(s.routes), LSDB: []LSAStatus{}, LSDBTotal: len(s.db), LSDBTypes: map[string]int{},
		Counters: map[string]uint64{}, Interfaces: []IfStatus{}, Neighbors: []NbrStatus{}}
	if st.Auth == "" {
		st.Auth = "none"
	}
	for _, p := range s.loopbacks {
		st.Loopbacks = append(st.Loopbacks, p.String())
	}
	for k, v := range s.counters {
		st.Counters[k] = v
	}
	for _, f := range s.ifs {
		st.Interfaces = append(st.Interfaces, s.ifStatusLocked(f, now))
		for _, n := range f.sortedNbrs() {
			st.Neighbors = append(st.Neighbors, n.status(now))
		}
	}
	keys := make([]LSAKey, 0, len(s.db))
	for k, l := range s.db {
		keys = append(keys, k)
		st.LSDBTypes[LSATypeName(k.Type)]++
		if l.own && k.Type == LSAExternal {
			st.ASBR = true
		}
	}
	sortKeys(keys)
	for i, k := range keys {
		if i == maxStatusLSAs {
			break
		}
		st.LSDB = append(st.LSDB, s.db[k].status(now))
	}
	return st
}

// Brief is the status without the LSDB rows (protocol.Briefer).
func (s *session) Brief() any {
	st := s.Status().(Status)
	st.LSDB = []LSAStatus{}
	return st
}

// Health is full or down, or how many active interfaces have a full
// adjacency when there are several (protocol.Healther).
func (s *session) Health() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, up := 0, 0
	for _, f := range s.ifs {
		if f.passive {
			continue
		}
		n++
		for _, nb := range f.nbrs {
			if nb.state == nFull {
				up++
				break
			}
		}
	}
	switch {
	case n == 0:
		return "passive"
	case n == 1 && up == 1:
		return "full"
	case n == 1:
		return "down"
	}
	return fmt.Sprintf("%d/%d full", up, n)
}

func sortKeys(keys []LSAKey) {
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.AdvRtr != b.AdvRtr {
			return a.AdvRtr.Less(b.AdvRtr)
		}
		return a.ID.Less(b.ID)
	})
}

// ---- actions ----

func (s *session) Action(_ context.Context, name string, args any) (any, error) {
	s.mu.Lock()
	defer func() {
		out := s.takeOutLocked()
		s.mu.Unlock()
		s.flush(out)
	}()
	now := time.Now()
	switch name {
	case "clear":
		s.counters = map[string]uint64{}
		for _, f := range s.ifs {
			f.authErr = ""
		}
		return map[string]any{"cleared": true}, nil
	case "max-metric":
		a := args.(*MaxMetricArgs)
		s.maxMetric = a.Set
		s.dirty = true
		go s.event("max-metric", fmt.Sprintf("max-metric %v", a.Set))
		return map[string]any{"max_metric": a.Set}, nil
	case "cost":
		a := args.(*CostArgs)
		fs, err := s.ifsNamed(a.Interface)
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			f.cost = a.Cost
		}
		s.dirty = true
		return map[string]any{"cost": a.Cost, "interfaces": len(fs)}, nil
	case "advertise", "withdraw":
		a := args.(*PrefixArgs)
		n := 0
		for _, p := range a.Prefixes {
			rs, _ := p.expand()
			for _, r := range rs {
				_, had := s.routes[r.prefix]
				if name == "advertise" {
					s.routes[r.prefix] = r
					n++
				} else if had {
					delete(s.routes, r.prefix)
					n++
				}
			}
		}
		if len(s.routes) > maxPrefixes {
			return nil, fmt.Errorf("at most %d prefixes", maxPrefixes)
		}
		s.dirty = true
		key := "advertised"
		if name == "withdraw" {
			key = "withdrawn"
		}
		return map[string]any{key: n, "total": len(s.routes)}, nil
	case "flap":
		a := args.(*FlapArgs)
		fs, err := s.ifsNamed(a.Interface)
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			f.flapTill, f.flapMode = now.Add(time.Duration(a.Seconds)*time.Second), a.Mode
			for _, n := range f.nbrs {
				s.nbrDownLocked(n, now, "flap")
			}
			if a.Mode == "reset" {
				s.helloLocked(f, now)
			}
		}
		go s.event("flap", fmt.Sprintf("flap %s for %ds", a.Mode, a.Seconds))
		return map[string]any{"interfaces": len(fs), "until": now.Add(time.Duration(a.Seconds) * time.Second).Format(time.RFC3339)}, nil
	case "lsa":
		return s.lsaDetailLocked(args.(*LSAArgs), now)
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

func (s *session) ifsNamed(name string) ([]*iface, error) {
	if name == "" {
		return s.ifs, nil
	}
	for _, f := range s.ifs {
		if f.device == name {
			return []*iface{f}, nil
		}
	}
	return nil, fmt.Errorf("no interface %q", name)
}
