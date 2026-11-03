// Package rsvp is RSVP-TE (RFC 3209) for IPv4. A device is the head of the
// LSPs it lists and the tail of those signalled to it: it takes traffic for
// the labels it hands out, and flows on the device can push the labels it
// receives. It doesn't switch labels between neighbors. Refresh reduction
// (RFC 2961) isn't implemented: neighbors refresh in full.
package rsvp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

func init() { protocol.Register(rsvpType{}) }

const TypeName = "rsvp"

const (
	// A label we've replaced still takes traffic for this long, while the
	// neighbor switches.
	oldLabelKept = 2 * time.Second
	stateFactor  = 5.25 // (K + 0.5) × 1.5 with K = 3 (RFC 2205 section 3.7)
	helloFactor  = 3.5
)

type rsvpType struct{}

func (rsvpType) Name() string { return TypeName }
func (rsvpType) Description() string {
	return "RSVP-TE router (RFC 3209, IPv4): head of the LSPs it lists, tail of those signalled to it; flows can send into its LSPs"
}
func (rsvpType) NewConfig() any       { return defaultConfig() }
func (rsvpType) EtherTypes() []uint16 { return []uint16{pkt.EtherTypeIPv4} }
func (rsvpType) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "signal", Description: "Signal an LSP from here that is shut down or torn down", NewArgs: func() any { return &SelectArgs{} }},
		{Name: "tear", Description: "Tear LSPs down: PathTear from here, ResvTear for those ending here", NewArgs: func() any { return &TearArgs{} }},
		{Name: "resignal", Description: "Move an LSP from here make-before-break: a new path, then the old one goes", NewArgs: func() any { return &ResignalArgs{} }},
		{Name: "error", Description: "Send an error: PathErr for LSPs ending here, ResvErr for those from here", NewArgs: func() any { return &ErrorArgs{Code: 1} }},
		{Name: "relabel", Description: "Hand LSPs ending here new labels", NewArgs: func() any { return &RelabelArgs{} }},
		{Name: "flap", Description: "Stay away from neighbors for a while, tearing LSPs down or going silent", NewArgs: func() any { return &FlapArgs{Seconds: 30, Mode: "tear"} }},
		{Name: "clear", Description: "Reset counters"},
	}
}

// out is a message to send, once its next hop is resolved.
type out struct {
	dst, via, src netip.Addr
	ttl           uint8
	alert         bool
	b             []byte
	counter       string
}

type neighbor struct {
	addr      netip.Addr
	remote    uint32
	up        bool
	heard     time.Time
	upSince   time.Time
	nextHello time.Time
}

type session struct {
	env  protocol.Env
	cfg  Config
	rid  netip.Addr
	inst uint32 // our hello instance

	releases []func()
	stop     chan struct{}
	wg       sync.WaitGroup
	outq     chan out
	evq      chan [2]string
	ipID     atomic.Uint32

	mu         sync.Mutex
	heads      []*head
	tails      map[tailKey]*tail
	neighbors  map[netip.Addr]*neighbor
	labels     map[uint32]func()
	nextLabel  uint32
	silentTill time.Time
	wasSilent  bool
	refuse     map[SessionID]time.Time // tails we don't answer until then
	refuseAll  time.Time
	counters   map[string]uint64
}

func (rsvpType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	ip := env.IP()
	if ip == nil {
		return nil, errors.New("the device has no IP stack")
	}
	rid := netip.MustParseAddr(cfg.RouterID)
	s := &session{env: env, cfg: cfg, rid: rid, inst: rand.Uint32()>>1 + 1, stop: make(chan struct{}), outq: make(chan out, 4096), evq: make(chan [2]string, 1024),
		tails: map[tailKey]*tail{}, neighbors: map[netip.Addr]*neighbor{}, labels: map[uint32]func(){}, nextLabel: cfg.LabelBase,
		refuse: map[SessionID]time.Time{}, counters: map[string]uint64{}}
	s.releases = append(s.releases, ip.AddLocal(rid))
	for i, l := range cfg.LSPs {
		ero, err := eroFrom(l.Hops)
		if err != nil {
			s.releases[0]()
			return nil, err
		}
		s.heads = append(s.heads, &head{cfg: l, sess: SessionID{To: netip.MustParseAddr(l.To), TunnelID: l.tunnelID(i), ExtID: rid},
			ero: ero, down: l.Shutdown, nextID: uint16(rand.Intn(60000)) + 1, state: stateDown})
	}
	s.wg.Add(3)
	go s.events()
	go s.sender()
	go s.loop()
	return s, nil
}

func (s *session) Stop() {
	s.mu.Lock()
	var msgs []out
	for _, h := range s.heads {
		msgs = append(msgs, s.dropHeadLocked(h, "instance stopped")...)
	}
	msgs = append(msgs, s.tearTailsLocked(func(*tail) bool { return true })...)
	s.mu.Unlock()
	for _, m := range msgs {
		s.enqueue(m)
	}
	// Let the teardowns go out.
	time.Sleep(20 * time.Millisecond)
	close(s.stop)
	s.wg.Wait()
	s.mu.Lock()
	for _, r := range s.labels {
		r()
	}
	s.labels = map[uint32]func(){}
	s.mu.Unlock()
	for _, r := range s.releases {
		r()
	}
}

func (s *session) enqueue(m out) {
	select {
	case s.outq <- m:
	default:
		s.count("tx_dropped")
	}
}

func (s *session) count(k string) {
	s.mu.Lock()
	s.counters[k]++
	s.mu.Unlock()
}

// event queues an event: it may be called holding the lock, and events go out
// in the order they happened.
func (s *session) event(kind, msg string) {
	select {
	case s.evq <- [2]string{kind, msg}:
	default:
	}
}

func (s *session) events() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			for {
				select {
				case e := <-s.evq:
					s.env.Emit(e[0], e[1], nil)
				default:
					return
				}
			}
		case e := <-s.evq:
			s.env.Emit(e[0], e[1], nil)
		}
	}
}

// sender resolves next hops and sends: resolving waits for ARP, which needs
// the receive path free.
func (s *session) sender() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		case m := <-s.outq:
			s.send(m)
		}
	}
}

func (s *session) send(m out) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, mac, err := s.env.IP().ResolveNextHop(ctx, m.via)
	cancel()
	if err != nil {
		s.count("tx_unresolved")
		return
	}
	h := pkt.IPv4{TOS: 0xc0, ID: uint16(s.ipID.Add(1)), TTL: m.ttl, Protocol: ProtoRSVP, Src: m.src, Dst: m.dst, Payload: m.b}
	if m.alert {
		h.Options = []byte{0x94, 4, 0, 0}
	}
	if s.env.Send(mac, pkt.EtherTypeIPv4, h.Encode(), protocol.SendOpts{}) == nil {
		s.count(m.counter)
	} else {
		s.count("tx_errors")
	}
}

// ifaceToward is our address on the way to a.
func (s *session) ifaceToward(a netip.Addr) (netip.Addr, bool) {
	return s.env.IP().Source(a)
}

// unicast builds a message to a neighbor from our address toward it.
func (s *session) unicast(dst netip.Addr, b []byte, counter string) (out, bool) {
	src, ok := s.ifaceToward(dst)
	if !ok {
		return out{}, false
	}
	return out{dst: dst, via: dst, src: src, ttl: 255, b: b, counter: counter}, true
}

func refreshOf(ms uint32) time.Duration {
	if ms == 0 {
		return 30 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

func stateLife(ms uint32) time.Duration {
	return time.Duration(float64(refreshOf(ms)) * stateFactor)
}

// jitter is the time to the next refresh: 0.5 to 1.5 × the interval.
func (s *session) jitter() time.Duration {
	r := time.Duration(s.cfg.RefreshInterval) * time.Second
	return r/2 + time.Duration(rand.Int63n(int64(r)))
}

func (s *session) refreshMs() uint32 { return uint32(s.cfg.RefreshInterval) * 1000 }

// ---- receiving ----

// HandleRx takes RSVP packets for us.
func (s *session) HandleRx(f *pkt.Frame) {
	if f.EtherType != pkt.EtherTypeIPv4 || len(f.Payload) < 20 || f.Payload[9] != ProtoRSVP {
		return
	}
	h, err := pkt.DecodeIPv4(f.Payload)
	if err != nil {
		s.count("rx_errors")
		return
	}
	now := time.Now()
	s.mu.Lock()
	silent := now.Before(s.silentTill)
	s.mu.Unlock()
	if silent {
		return
	}
	m, err := Decode(h.Payload)
	if err != nil {
		s.count("rx_errors")
		return
	}
	ip := s.env.IP()
	if m.Type != MsgPath && !ip.IsLocal(h.Dst) {
		return
	}
	var msgs []out
	s.mu.Lock()
	switch m.Type {
	case MsgPath:
		msgs = s.rxPathLocked(m, h, now)
	case MsgPathTear:
		s.counters["rx_path_tear"]++
		msgs = s.rxPathTearLocked(m)
	case MsgResv:
		s.counters["rx_resv"]++
		msgs = s.rxResvLocked(m, f.Src, now)
	case MsgResvTear:
		s.counters["rx_resv_tear"]++
		msgs = s.rxResvTearLocked(m, now)
	case MsgPathErr:
		s.counters["rx_path_err"]++
		msgs = s.rxPathErrLocked(m, now)
	case MsgResvErr:
		s.counters["rx_resv_err"]++
		s.rxResvErrLocked(m)
	case MsgHello:
		s.counters["rx_hello"]++
		msgs = s.rxHelloLocked(m, h.Src, now)
	default:
		s.counters["rx_ignored"]++
	}
	s.mu.Unlock()
	for _, o := range msgs {
		s.enqueue(o)
	}
}

// ---- the timer loop ----

func (s *session) loop() {
	defer s.wg.Done()
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
	var msgs []out
	s.mu.Lock()
	if now.Before(s.silentTill) {
		s.wasSilent = true
		s.mu.Unlock()
		return
	}
	if s.wasSilent {
		// We weren't listening: nothing has really expired on our account.
		s.wasSilent = false
		s.restartTimersLocked(now)
	}
	msgs = append(msgs, s.tickHeadsLocked(now)...)
	msgs = append(msgs, s.tickTailsLocked(now)...)
	msgs = append(msgs, s.tickHellosLocked(now)...)
	s.mu.Unlock()
	for _, m := range msgs {
		s.enqueue(m)
	}
}

func (s *session) restartTimersLocked(now time.Time) {
	for _, t := range s.tails {
		t.expires = now.Add(stateLife(t.refreshMs))
		t.nextResv = now
	}
	for _, h := range s.heads {
		for _, a := range []*attempt{h.active, h.pending} {
			if a != nil {
				a.nextPath = now
				if a.established {
					a.resvExpires = now.Add(stateLife(a.refreshMs))
				}
			}
		}
	}
	for _, n := range s.neighbors {
		n.heard = now
		n.nextHello = now
	}
}

// ---- hellos ----

func (s *session) neighborLocked(a netip.Addr, now time.Time) *neighbor {
	if s.cfg.HelloInterval == 0 {
		return nil
	}
	n := s.neighbors[a]
	if n == nil {
		n = &neighbor{addr: a, nextHello: now}
		s.neighbors[a] = n
	}
	return n
}

func (s *session) rxHelloLocked(m *Message, src netip.Addr, now time.Time) []out {
	if !m.HasHello {
		return nil
	}
	var msgs []out
	if !m.HelloAck {
		if o, ok := s.unicast(src, HelloMsg(true, s.inst, m.SrcInst), "tx_hello_ack"); ok {
			o.ttl = 1
			msgs = append(msgs, o)
		}
	}
	n := s.neighborLocked(src, now)
	if n == nil {
		return msgs
	}
	if n.remote != 0 && m.SrcInst != 0 && n.remote != m.SrcInst {
		msgs = append(msgs, s.neighborLostLocked(n, "restarted", now)...)
	}
	if m.SrcInst != 0 {
		n.remote = m.SrcInst
	}
	n.heard = now
	if !n.up && m.HelloAck {
		n.up, n.upSince = true, now
		s.event("neighbor-up", fmt.Sprintf("hellos answered by %s", src))
	}
	return msgs
}

func (s *session) tickHellosLocked(now time.Time) []out {
	if s.cfg.HelloInterval == 0 {
		return nil
	}
	iv := time.Duration(s.cfg.HelloInterval) * time.Second
	var msgs []out
	for _, n := range s.neighbors {
		if n.up && now.Sub(n.heard) > time.Duration(helloFactor*float64(iv)) {
			msgs = append(msgs, s.neighborLostLocked(n, "hellos stopped", now)...)
		}
		if !now.Before(n.nextHello) {
			n.nextHello = now.Add(iv)
			if o, ok := s.unicast(n.addr, HelloMsg(false, s.inst, n.remote), "tx_hello"); ok {
				o.ttl = 1
				msgs = append(msgs, o)
			}
		}
	}
	return msgs
}

// neighborLostLocked ends what goes through a neighbor.
func (s *session) neighborLostLocked(n *neighbor, why string, now time.Time) []out {
	n.up, n.remote = false, 0
	s.event("neighbor-down", fmt.Sprintf("neighbor %s: %s", n.addr, why))
	var msgs []out
	for k, t := range s.tails {
		if t.phop.Addr == n.addr {
			s.removeTailLocked(k, t, "neighbor "+n.addr.String()+": "+why)
		}
	}
	for _, h := range s.heads {
		for _, a := range []*attempt{h.active, h.pending} {
			if a != nil && a.hop.Addr == n.addr {
				msgs = append(msgs, s.attemptFailedLocked(h, a, "neighbor "+n.addr.String()+": "+why, now, false)...)
			}
		}
	}
	return msgs
}

// ---- labels ----

// takeLabelLocked hands out the label the tail mode says.
func (s *session) takeLabelLocked() (uint32, error) {
	switch s.cfg.TailMode {
	case "implicit-null":
		return LabelImplicitNull, nil
	case "explicit-null":
		if _, have := s.labels[LabelExplicitNull]; !have {
			s.labels[LabelExplicitNull] = s.env.IP().AddLocalLabel(LabelExplicitNull)
		}
		return LabelExplicitNull, nil
	}
	for tries := uint32(0); tries <= MaxLabel; tries++ {
		l := s.nextLabel
		s.nextLabel++
		if s.nextLabel > MaxLabel {
			s.nextLabel = MinLabel
		}
		if _, have := s.labels[l]; !have {
			s.labels[l] = s.env.IP().AddLocalLabel(l)
			return l, nil
		}
	}
	return 0, errors.New("out of labels")
}

// dropLabelLocked lets go of a label, unless shared (the null labels).
func (s *session) dropLabelLocked(l uint32) {
	if l == LabelImplicitNull || l == LabelExplicitNull {
		return
	}
	if r, ok := s.labels[l]; ok {
		r()
		delete(s.labels, l)
	}
}

// ---- status ----

type RecordedHop struct {
	Address string `json:"address"`
	Label   *int64 `json:"label,omitempty"`
	// local protection: "available", "in use"
	Protection string `json:"protection,omitempty"`
}

func recorded(hops []RROHop) []RecordedHop {
	out := make([]RecordedHop, 0, len(hops))
	for _, h := range hops {
		r := RecordedHop{Address: h.Addr.String()}
		if h.HasLabel {
			l := int64(h.Label)
			r.Label = &l
		}
		switch {
		case h.Flags&RRProtectionInUse != 0:
			r.Protection = "in use"
		case h.Flags&RRProtectionAvail != 0:
			r.Protection = "available"
			if h.Flags&RRNodeProt != 0 {
				r.Protection = "available (node)"
			}
		}
		out = append(out, r)
	}
	return out
}

type HeadStatus struct {
	Name       string        `json:"name"`
	To         string        `json:"to"`
	TunnelID   uint16        `json:"tunnel_id"`
	LSPID      uint16        `json:"lsp_id,omitempty"`
	State      string        `json:"state"`
	Label      *int64        `json:"label,omitempty"`
	NextHop    string        `json:"next_hop,omitempty"`
	Protection string        `json:"protection,omitempty"`
	Hops       []Hop4        `json:"hops,omitempty"`
	Path       []RecordedHop `json:"path,omitempty"`
	Since      string        `json:"since,omitempty"`
	LastError  string        `json:"last_error,omitempty"`
	Repaired   bool          `json:"locally_repaired,omitempty"`
	Setups     uint64        `json:"setups"`
	Resigning  bool          `json:"resignaling,omitempty"`
}

type TailStatus struct {
	Name       string        `json:"name"`
	From       string        `json:"from"`
	To         string        `json:"to"`
	TunnelID   uint16        `json:"tunnel_id"`
	LSPID      uint16        `json:"lsp_id"`
	Label      int64         `json:"label"`
	PrevHop    string        `json:"previous_hop"`
	Style      string        `json:"style"`
	Protection string        `json:"protection,omitempty"`
	Path       []RecordedHop `json:"path,omitempty"`
	ExpiresS   float64       `json:"expires_in_s"`
	Since      string        `json:"since"`
	Torn       bool          `json:"torn,omitempty"`
}

type NeighborStatus struct {
	Address string `json:"address"`
	Hellos  string `json:"hellos"`
	Since   string `json:"since,omitempty"`
	Remote  uint32 `json:"instance,omitempty"`
}

type Status struct {
	RouterID  string            `json:"router_id"`
	Heads     []HeadStatus      `json:"heads"`
	Tails     []TailStatus      `json:"tails"`
	Neighbors []NeighborStatus  `json:"neighbors"`
	Counters  map[string]uint64 `json:"counters"`
	SilentS   float64           `json:"silent_remaining_s,omitempty"`
}

func (s *session) Status() any {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{RouterID: s.rid.String(), Heads: []HeadStatus{}, Tails: []TailStatus{}, Neighbors: []NeighborStatus{}, Counters: map[string]uint64{}}
	for k, v := range s.counters {
		st.Counters[k] = v
	}
	if now.Before(s.silentTill) {
		st.SilentS = s.silentTill.Sub(now).Seconds()
	}
	for _, h := range s.heads {
		st.Heads = append(st.Heads, h.status())
	}
	for _, t := range s.tails {
		st.Tails = append(st.Tails, t.status(now))
	}
	sort.Slice(st.Tails, func(i, j int) bool {
		a, b := st.Tails[i], st.Tails[j]
		if a.TunnelID != b.TunnelID {
			return a.TunnelID < b.TunnelID
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.LSPID < b.LSPID
	})
	for _, n := range s.neighbors {
		x := NeighborStatus{Address: n.addr.String(), Hellos: "waiting", Remote: n.remote}
		if n.up {
			x.Hellos, x.Since = "up", n.upSince.UTC().Format(time.RFC3339)
		}
		st.Neighbors = append(st.Neighbors, x)
	}
	sort.Slice(st.Neighbors, func(i, j int) bool { return st.Neighbors[i].Address < st.Neighbors[j].Address })
	return st
}

// Health is one word for group views.
func (s *session) Health() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.heads) == 0 {
		if len(s.tails) == 0 {
			return "no-lsps"
		}
		return "up"
	}
	up := 0
	for _, h := range s.heads {
		if h.state == stateUp {
			up++
		}
	}
	switch up {
	case len(s.heads):
		return "up"
	case 0:
		return "down"
	}
	return "partly-up"
}

// ---- actions ----

func (s *session) Action(_ context.Context, name string, args any) (any, error) {
	switch name {
	case "signal":
		return s.signal(args.(*SelectArgs).LSP)
	case "tear":
		return s.tear(args.(*TearArgs))
	case "resignal":
		return s.resignal(args.(*ResignalArgs))
	case "error":
		return s.sendError(args.(*ErrorArgs))
	case "relabel":
		return s.relabel(args.(*RelabelArgs).LSP)
	case "flap":
		return s.flap(args.(*FlapArgs))
	case "clear":
		s.mu.Lock()
		s.counters = map[string]uint64{}
		s.mu.Unlock()
		return map[string]any{"cleared": true}, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

func (s *session) flap(a *FlapArgs) (any, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	d := time.Duration(a.Seconds) * time.Second
	now := time.Now()
	if a.Mode == "silent" {
		s.mu.Lock()
		s.silentTill = now.Add(d)
		s.mu.Unlock()
		s.event("flap", fmt.Sprintf("silent for %d s", a.Seconds))
		return map[string]any{"silent_s": a.Seconds}, nil
	}
	s.event("flap", fmt.Sprintf("tearing LSPs down for %d s", a.Seconds))
	return s.tear(&TearArgs{Seconds: a.Seconds})
}

func (s *session) tear(a *TearArgs) (any, error) {
	now := time.Now()
	var hold time.Time
	if a.Seconds > 0 {
		hold = now.Add(time.Duration(a.Seconds) * time.Second)
	}
	var msgs []out
	heads, tails := 0, 0
	s.mu.Lock()
	for _, h := range s.heads {
		if a.LSP != "" && h.cfg.Name != a.LSP {
			continue
		}
		heads++
		msgs = append(msgs, s.dropHeadLocked(h, "torn down")...)
		if a.Seconds > 0 {
			h.down, h.retryAt = false, hold
		} else {
			h.down = true
		}
	}
	match := func(t *tail) bool { return a.LSP == "" || t.name == a.LSP }
	for _, t := range s.tails {
		if match(t) {
			tails++
			if a.Seconds > 0 {
				s.refuse[t.sess] = hold
				t.heldTill = hold
			}
		}
	}
	msgs = append(msgs, s.tearTailsLocked(match)...)
	if a.LSP == "" && a.Seconds > 0 {
		s.refuseAll = hold // also LSPs that aren't here yet
	}
	s.mu.Unlock()
	if a.LSP != "" && heads == 0 && tails == 0 {
		return nil, fmt.Errorf("no LSP %q", a.LSP)
	}
	for _, m := range msgs {
		s.enqueue(m)
	}
	return map[string]any{"heads": heads, "tails": tails}, nil
}

func (s *session) sendError(a *ErrorArgs) (any, error) {
	var msgs []out
	sent := 0
	s.mu.Lock()
	e := ErrorSpec{Node: s.rid, Code: a.Code, Value: a.Value}
	for _, t := range s.tails {
		if a.LSP != "" && t.name != a.LSP {
			continue
		}
		x := e
		if a.RemoveState {
			x.Flags = ErrFlagPathStateRemoved
		}
		if o, ok := s.unicast(t.phop.Addr, PathErrMsg(t.sess, x, t.sender, t.tspec), "tx_path_err"); ok {
			msgs = append(msgs, o)
			sent++
		}
	}
	for _, h := range s.heads {
		if (a.LSP != "" && h.cfg.Name != a.LSP) || h.active == nil || !h.active.established {
			continue
		}
		iface, ok := s.ifaceToward(h.active.hop.Addr)
		if !ok {
			continue
		}
		if o, ok := s.unicast(h.active.hop.Addr, ResvErrMsg(h.sess, Hop{iface, 1}, e, StyleSE, h.active.sender, h.tspec()), "tx_resv_err"); ok {
			msgs = append(msgs, o)
			sent++
		}
	}
	s.mu.Unlock()
	if sent == 0 {
		return nil, errors.New("no LSP to send it for")
	}
	for _, m := range msgs {
		s.enqueue(m)
	}
	return map[string]any{"sent": sent}, nil
}

func tspecFor(mbps float64) TSpec {
	r := float32(mbps * 1e6 / 8)
	return TSpec{Rate: r, Bucket: r, Peak: float32(math.Inf(1)), MinPolice: 20, MaxPacket: 1500}
}
