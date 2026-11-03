// Package ldp is the Label Distribution Protocol (RFC 5036) for IPv4: link
// and targeted discovery, sessions, downstream-unsolicited labels and liberal
// retention. It is the far end of LSPs: it advertises labels, and the device
// takes traffic for them, but it doesn't switch labels between peers.
package ldp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/prefixset"
	"github.com/ripnet/clabgen/internal/protocol"
)

func init() { protocol.Register(ldpType{}) }

const TypeName = "ldp"

var allRouters = netip.MustParseAddr("224.0.0.2")

type ldpType struct{}

func (ldpType) Name() string { return TypeName }
func (ldpType) Description() string {
	return "LDP router (RFC 5036, IPv4): sessions over link and targeted hellos; advertises labels, keeps those received, takes traffic for its own"
}
func (ldpType) NewConfig() any       { return defaultConfig() }
func (ldpType) EtherTypes() []uint16 { return []uint16{pkt.EtherTypeIPv4} }
func (ldpType) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "advertise", Description: "Advertise labels for more FECs (until withdrawn or restarted)", NewArgs: func() any { return &FECArgs{} }},
		{Name: "withdraw", Description: "Withdraw the labels of FECs (from config or advertised)", NewArgs: func() any { return &FECArgs{} }},
		{Name: "reset", Description: "End sessions with a shutdown notification; they come back", NewArgs: func() any { return &ResetArgs{} }},
		{Name: "flap", Description: "Stay away from peers for a while, ending sessions or going silent", NewArgs: func() any { return &FlapArgs{Seconds: 30, Mode: "notify"} }},
		{Name: "notify", Description: "Send a notification with any status code", NewArgs: func() any { return &NotifyArgs{Code: 10} }},
		{Name: "bindings", Description: "Show the labels received from a peer", NewArgs: func() any { return &BindingsArgs{Limit: 1000} }},
		{Name: "clear", Description: "Reset counters"},
	}
}

// adjacency is one source of hellos from a neighbor.
type adjacency struct {
	targeted bool
	src      netip.Addr
	hold     time.Duration
	expires  time.Time
	since    time.Time
}

// neighbor is an LSR we've heard hellos from, and the session with it.
type neighbor struct {
	lsr       netip.Addr
	transport netip.Addr
	adjs      map[string]*adjacency
	conn      *conn
	dialing   bool
	retryAt   time.Time
	backoff   time.Duration
	flapTill  time.Time
	lastErr   string
	sessions  uint64 // came up so far
}

type session struct {
	env       protocol.Env
	cfg       Config
	rid       netip.Addr
	transport netip.Addr
	id        ID
	answer    *prefixset.Set

	releases []func()
	listener net.Listener
	stop     chan struct{}
	wg       sync.WaitGroup

	mu         sync.Mutex
	neighbors  map[netip.Addr]*neighbor
	fecs       map[netip.Prefix]uint32 // what we advertise
	answered   map[netip.Prefix]bool   // what we've advertised: the device answers for it
	used       map[uint32]func()       // labels taken, with their release
	nextLabel  uint32
	nextMsg    uint32
	silentTill time.Time
	nextLink   time.Time
	nextTarget time.Time
	targeted   map[netip.Addr]bool // peers we send targeted hellos to, listed or asked
	asked      map[netip.Addr]time.Time
	counters   map[string]uint64
	started    time.Time
}

func (ldpType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	ip := env.IP()
	if ip == nil {
		return nil, errors.New("the device has no IP stack")
	}
	rid := netip.MustParseAddr(cfg.RouterID)
	transport := rid
	if cfg.TransportAddress != "" {
		transport = netip.MustParseAddr(cfg.TransportAddress)
	}
	s := &session{env: env, cfg: cfg, rid: rid, transport: transport, id: ID{LSR: rid}, answer: &prefixset.Set{},
		stop: make(chan struct{}), neighbors: map[netip.Addr]*neighbor{}, fecs: map[netip.Prefix]uint32{}, answered: map[netip.Prefix]bool{}, used: map[uint32]func(){},
		nextLabel: cfg.LabelBase, targeted: map[netip.Addr]bool{}, asked: map[netip.Addr]time.Time{}, counters: map[string]uint64{}, started: time.Now()}
	for _, t := range cfg.Targeted {
		s.targeted[netip.MustParseAddr(t.Address)] = true
	}
	s.releases = append(s.releases, ip.AddLocal(rid), ip.AddLocal(transport), ip.AddLocalPrefixes(s.answer))
	bs, err := expandFECs(cfg.FECs, cfg.LabelBase, nil)
	if err != nil {
		s.release()
		return nil, err
	}
	s.mu.Lock()
	s.addFECsLocked(bs)
	s.mu.Unlock()
	l, err := ip.ListenTCP(netip.Addr{}, Port, false, 64)
	if err != nil {
		s.release()
		return nil, fmt.Errorf("listen: %w", err)
	}
	s.listener = l
	s.wg.Add(2)
	go s.accept()
	go s.loop()
	return s, nil
}

func (s *session) release() {
	s.mu.Lock()
	for _, r := range s.used {
		r()
	}
	s.used = map[uint32]func(){}
	s.mu.Unlock()
	for _, r := range s.releases {
		r()
	}
}

func (s *session) Stop() {
	close(s.stop)
	s.listener.Close()
	s.mu.Lock()
	var cs []*conn
	for _, n := range s.neighbors {
		if n.conn != nil {
			cs = append(cs, n.conn)
		}
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.notifyClose(StatusShutdown, "instance stopped")
	}
	s.wg.Wait()
	s.release()
}

func (s *session) msgID() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgIDLocked()
}

func (s *session) msgIDLocked() uint32 {
	s.nextMsg++
	return s.nextMsg
}

func (s *session) count(k string) {
	s.mu.Lock()
	s.counters[k]++
	s.mu.Unlock()
}

func (s *session) event(kind, msg string) { s.env.Emit(kind, msg, nil) }

// ---- FECs ----

// addFECsLocked records bindings, takes their labels at the device and
// updates what it answers for. It returns those new or changed.
func (s *session) addFECsLocked(bs []binding) []binding {
	ip := s.env.IP()
	var changed []binding
	for _, b := range bs {
		if old, ok := s.fecs[b.prefix]; ok {
			if old == b.label {
				continue
			}
			s.dropLabelLocked(old)
		}
		s.fecs[b.prefix] = b.label
		if ip != nil {
			if _, have := s.used[b.label]; !have {
				s.used[b.label] = ip.AddLocalLabel(b.label)
			}
		}
		changed = append(changed, b)
	}
	s.syncAnswerLocked()
	return changed
}

// dropLabelLocked lets go of a label no prefix uses any more.
func (s *session) dropLabelLocked(l uint32) {
	for _, x := range s.fecs {
		if x == l {
			return
		}
	}
	if r, ok := s.used[l]; ok {
		r()
		delete(s.used, l)
	}
}

// syncAnswerLocked makes the device answer for every prefix it has had a
// label for. Withdrawing a label only takes the label away: a router with an
// IGP route falls back to plain IP, and those packets are still welcome.
func (s *session) syncAnswerLocked() {
	for p := range s.fecs {
		s.answered[p] = true
	}
	ps := make([]netip.Prefix, 0, len(s.answered))
	for p := range s.answered {
		ps = append(ps, p)
	}
	s.answer.Update(ps)
}

// allocFECsLocked expands action FECs, giving new labels above what's used.
func (s *session) allocFECsLocked(fecs []FEC) ([]binding, error) {
	used := map[uint32]bool{}
	for l := range s.used {
		used[l] = true
	}
	// A prefix we advertise keeps its label unless the action names another.
	return expandFECs(fecs, s.nextLabel, used)
}

// ---- discovery ----

func (s *session) loop() {
	defer s.wg.Done()
	t := time.NewTicker(200 * time.Millisecond)
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
	s.mu.Lock()
	silent := now.Before(s.silentTill)
	link := !s.cfg.NoLinkHellos && !silent && !now.Before(s.nextLink)
	if link {
		s.nextLink = now.Add(time.Duration(s.cfg.HelloInterval) * time.Second)
	}
	target := !silent && !now.Before(s.nextTarget)
	var peers []netip.Addr
	if target {
		s.nextTarget = now.Add(time.Duration(s.cfg.TargetedHelloInterval) * time.Second)
		for a := range s.targeted {
			peers = append(peers, a)
		}
		for a, until := range s.asked {
			if now.After(until) {
				delete(s.asked, a)
			} else if !s.targeted[a] {
				peers = append(peers, a)
			}
		}
	}
	var dial []*neighbor
	var ending []*conn
	for lsr, n := range s.neighbors {
		for k, a := range n.adjs {
			if now.After(a.expires) {
				delete(n.adjs, k)
				s.counters["adjacencies_expired"]++
				go s.event("adjacency-down", fmt.Sprintf("%s adjacency with %s on %s: hold time expired", adjKind(a), lsr, a.src))
			}
		}
		if len(n.adjs) == 0 {
			if n.conn != nil {
				ending = append(ending, n.conn)
			} else if !n.dialing {
				delete(s.neighbors, lsr)
			}
			continue
		}
		if s.activeFor(n) && n.conn == nil && !n.dialing && !now.Before(n.retryAt) && !now.Before(n.flapTill) {
			n.dialing = true
			dial = append(dial, n)
		}
	}
	s.mu.Unlock()
	if link {
		s.sendHello(allRouters, HelloParams{Hold: uint16(s.cfg.HoldTime)})
	}
	for _, a := range peers {
		s.sendHello(a, HelloParams{Hold: uint16(s.cfg.TargetedHoldTime), Targeted: true, Request: s.isListed(a)})
	}
	for _, c := range ending {
		c.notifyClose(StatusHoldExpired, "no hello adjacency left")
	}
	for _, n := range dial {
		s.wg.Add(1)
		go s.dial(n)
	}
}

func (s *session) isListed(a netip.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targeted[a]
}

func adjKind(a *adjacency) string {
	if a.targeted {
		return "targeted"
	}
	return "link"
}

// activeFor: the side with the higher transport address opens the session
// (section 2.5.2).
func (s *session) activeFor(n *neighbor) bool { return s.transport.Compare(n.transport) > 0 }

// sendHello sends a hello: to all routers on the link with TTL 1, or to a
// peer's address from our transport address.
func (s *session) sendHello(dst netip.Addr, h HelloParams) {
	m := Message{Type: MsgHello, ID: s.msgID(), TLVs: []TLV{h.tlv(), {Type: TLVIPv4Transport, Value: s.transport.AsSlice()}}}
	payload := EncodePDU(s.id, m.Encode())
	var mac pkt.MAC
	var err error
	ttl := uint8(1)
	src := s.linkSource()
	if h.Targeted {
		ttl, src = 64, s.transport
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, mac, err = s.env.IP().ResolveNextHop(ctx, dst)
		cancel()
		if err != nil {
			s.count("tx_unresolved")
			return
		}
	} else {
		mac = pkt.MulticastMAC(dst)
	}
	if !src.IsValid() {
		s.count("tx_no_source")
		return
	}
	b := udpDatagram(payload, src, dst)
	ip := pkt.IPv4{TOS: 0xc0, TTL: ttl, Protocol: pkt.ProtoUDP, Src: src, Dst: dst, Payload: b}
	if s.env.Send(mac, pkt.EtherTypeIPv4, ip.Encode(), protocol.SendOpts{}) == nil {
		s.count(map[bool]string{true: "tx_targeted_hello", false: "tx_link_hello"}[h.Targeted])
	}
}

// udpDatagram builds a datagram from and to port 646.
func udpDatagram(payload []byte, src, dst netip.Addr) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(b[0:], Port)
	binary.BigEndian.PutUint16(b[2:], Port)
	binary.BigEndian.PutUint16(b[4:], uint16(len(b)))
	copy(b[8:], payload)
	cs := pkt.Checksum(b, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(b)))
	if cs == 0 {
		cs = 0xffff
	}
	binary.BigEndian.PutUint16(b[6:], cs)
	return b
}

// linkSource is the device's first IPv4 address, for link hellos.
func (s *session) linkSource() netip.Addr {
	for _, p := range s.env.IP().Addresses() {
		if p.Addr().Is4() {
			return p.Addr()
		}
	}
	return netip.Addr{}
}

// HandleRx takes hellos: UDP to port 646 at all routers or at us.
func (s *session) HandleRx(fr *pkt.Frame) {
	if fr.EtherType != pkt.EtherTypeIPv4 {
		return
	}
	h, err := pkt.DecodeIPv4(fr.Payload)
	if err != nil || h.Protocol != pkt.ProtoUDP || h.FragOff != 0 || h.Flags&pkt.IPv4MF != 0 || len(h.Payload) < 8 {
		return
	}
	if binary.BigEndian.Uint16(h.Payload[2:]) != Port {
		return
	}
	if h.Dst != allRouters && !s.env.IP().IsLocal(h.Dst) {
		return
	}
	n := int(binary.BigEndian.Uint16(h.Payload[4:]))
	if n < 8 || n > len(h.Payload) {
		return
	}
	s.mu.Lock()
	silent := time.Now().Before(s.silentTill)
	s.mu.Unlock()
	if silent {
		return // a silent flap hears nothing either: its adjacencies lapse
	}
	p, err := DecodePDU(h.Payload[8:n])
	if err != nil {
		s.count("rx_bad_hello")
		return
	}
	for i := range p.Messages {
		if p.Messages[i].Type == MsgHello {
			s.rxHello(p.ID, h.Src, h.Dst == allRouters, &p.Messages[i])
		}
	}
}

func (s *session) rxHello(id ID, src netip.Addr, multicast bool, m *Message) {
	t := m.tlv(TLVCommonHello)
	if t == nil {
		s.count("rx_bad_hello")
		return
	}
	hp, err := decodeHelloParams(t.Value)
	if err != nil || hp.Targeted == multicast {
		s.count("rx_bad_hello")
		return
	}
	if id.LSR == s.rid {
		return // our own, looped back
	}
	hold := time.Duration(hp.Hold) * time.Second
	if hp.Hold == 0 {
		hold = 15 * time.Second
		if hp.Targeted {
			hold = 45 * time.Second
		}
	}
	if hp.Hold == 0xffff {
		hold = 365 * 24 * time.Hour
	}
	transport := src
	if x := m.tlv(TLVIPv4Transport); x != nil && len(x.Value) == 4 {
		transport = netip.AddrFrom4([4]byte(x.Value))
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if hp.Targeted && !s.targeted[src] {
		if !s.cfg.AcceptTargeted || !hp.Request {
			s.counters["rx_targeted_refused"]++
			return
		}
		// Asked to answer: keep sending while it keeps asking.
		s.asked[src] = now.Add(hold)
	}
	s.counters["rx_hello"]++
	n := s.neighbors[id.LSR]
	if n == nil {
		n = &neighbor{lsr: id.LSR, adjs: map[string]*adjacency{}}
		s.neighbors[id.LSR] = n
	}
	n.transport = transport
	k := fmt.Sprintf("%v/%s", hp.Targeted, src)
	a := n.adjs[k]
	if a == nil {
		a = &adjacency{targeted: hp.Targeted, src: src, since: now}
		n.adjs[k] = a
		go s.event("adjacency-up", fmt.Sprintf("%s adjacency with %s from %s", adjKind(a), id.LSR, src))
	}
	a.hold, a.expires = hold, now.Add(hold)
}

// ---- status ----

type AdjacencyStatus struct {
	Kind   string  `json:"kind"`
	Source string  `json:"source"`
	HoldS  float64 `json:"hold_remaining_s"`
	Since  string  `json:"since"`
}

type PeerStatus struct {
	LSR         string            `json:"lsr"`
	Transport   string            `json:"transport"`
	State       string            `json:"state"`
	Role        string            `json:"role"`
	Adjacencies []AdjacencyStatus `json:"adjacencies"`
	KeepAlive   int               `json:"keepalive_s,omitempty"`
	Up          string            `json:"up_since,omitempty"`
	Addresses   []string          `json:"addresses,omitempty"`
	Received    int               `json:"received"`
	LastError   string            `json:"last_error,omitempty"`
	Sessions    uint64            `json:"sessions"`
	Counters    map[string]uint64 `json:"counters,omitempty"`
}

type Status struct {
	RouterID   string            `json:"router_id"`
	Transport  string            `json:"transport"`
	Advertised int               `json:"advertised"`
	Peers      []PeerStatus      `json:"peers"`
	Counters   map[string]uint64 `json:"counters"`
	SilentS    float64           `json:"silent_remaining_s,omitempty"`
}

func (s *session) Status() any {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{RouterID: s.rid.String(), Transport: s.transport.String(), Advertised: len(s.fecs), Peers: []PeerStatus{}, Counters: map[string]uint64{}}
	for k, v := range s.counters {
		st.Counters[k] = v
	}
	if now.Before(s.silentTill) {
		st.SilentS = s.silentTill.Sub(now).Seconds()
	}
	for _, n := range s.neighbors {
		p := PeerStatus{LSR: n.lsr.String(), Transport: n.transport.String(), State: "discovered", Role: "passive", Adjacencies: []AdjacencyStatus{}, LastError: n.lastErr, Sessions: n.sessions}
		if s.activeFor(n) {
			p.Role = "active"
		}
		if now.Before(n.flapTill) {
			p.State = "flapped"
		}
		for _, a := range n.adjs {
			p.Adjacencies = append(p.Adjacencies, AdjacencyStatus{Kind: adjKind(a), Source: a.src.String(), HoldS: a.expires.Sub(now).Seconds(), Since: a.since.Format(time.RFC3339)})
		}
		sort.Slice(p.Adjacencies, func(i, j int) bool {
			return p.Adjacencies[i].Kind+p.Adjacencies[i].Source < p.Adjacencies[j].Kind+p.Adjacencies[j].Source
		})
		if c := n.conn; c != nil {
			p.State, p.KeepAlive = c.state, c.keepalive
			if !c.upSince.IsZero() {
				p.Up = c.upSince.Format(time.RFC3339)
			}
			for a := range c.addresses {
				p.Addresses = append(p.Addresses, a.String())
			}
			sort.Strings(p.Addresses)
			p.Received = len(c.lib)
			p.Counters = map[string]uint64{}
			for k, v := range c.counters {
				p.Counters[k] = v
			}
		}
		st.Peers = append(st.Peers, p)
	}
	sort.Slice(st.Peers, func(i, j int) bool { return st.Peers[i].LSR < st.Peers[j].LSR })
	return st
}

// Health is a word for group views.
func (s *session) Health() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.neighbors) == 0 {
		return "no neighbors"
	}
	up := 0
	for _, n := range s.neighbors {
		if n.conn != nil && n.conn.state == stOperational {
			up++
		}
	}
	switch up {
	case len(s.neighbors):
		return "operational"
	case 0:
		return "down"
	}
	return "partial"
}

// ---- actions ----

// BindingEntry is one label received.
type BindingEntry struct {
	Prefix string `json:"prefix"`
	Label  uint32 `json:"label"`
}

func (s *session) Action(_ context.Context, name string, args any) (any, error) {
	switch name {
	case "advertise":
		return s.changeFECs(true, args.(*FECArgs).FECs)
	case "withdraw":
		return s.changeFECs(false, args.(*FECArgs).FECs)
	case "reset":
		cs, err := s.conns(args.(*ResetArgs).Peer)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			c.notifyClose(StatusShutdown, "reset")
		}
		return map[string]any{"reset": len(cs)}, nil
	case "flap":
		return s.flap(args.(*FlapArgs))
	case "notify":
		a := args.(*NotifyArgs)
		cs, err := s.conns(a.Peer)
		if err != nil {
			return nil, err
		}
		code := a.Code &^ 0xc0000000
		if a.Fatal {
			code |= 0x80000000
		}
		for _, c := range cs {
			if a.Fatal {
				c.notifyClose(code, "notify action")
			} else {
				c.notify(code, 0, 0)
			}
		}
		return map[string]any{"sent": len(cs), "status": StatusText(code)}, nil
	case "bindings":
		return s.bindings(args.(*BindingsArgs))
	case "clear":
		s.mu.Lock()
		s.counters = map[string]uint64{}
		for _, n := range s.neighbors {
			if n.conn != nil {
				n.conn.counters = map[string]uint64{}
			}
		}
		s.mu.Unlock()
		return map[string]any{"cleared": true}, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

func (s *session) conns(peer string) ([]*conn, error) {
	var want netip.Addr
	if peer != "" {
		a, err := netip.ParseAddr(peer)
		if err != nil {
			return nil, fmt.Errorf("peer %q is not an LSR ID", peer)
		}
		want = a
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*conn
	found := !want.IsValid()
	for lsr, n := range s.neighbors {
		if want.IsValid() && lsr != want {
			continue
		}
		found = true
		if n.conn != nil {
			out = append(out, n.conn)
		}
	}
	if !found {
		return nil, fmt.Errorf("no neighbor %s", peer)
	}
	return out, nil
}

func (s *session) changeFECs(add bool, fecs []FEC) (any, error) {
	s.mu.Lock()
	bs, err := s.allocFECsLocked(fecs)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var changed []binding
	if add {
		changed = s.addFECsLocked(bs)
		for _, b := range changed {
			if b.label >= s.nextLabel {
				s.nextLabel = b.label + 1
			}
		}
	} else {
		for _, b := range bs {
			if l, ok := s.fecs[b.prefix]; ok {
				delete(s.fecs, b.prefix)
				s.dropLabelLocked(l)
				changed = append(changed, binding{b.prefix, l})
			}
		}
	}
	var cs []*conn
	for _, n := range s.neighbors {
		if n.conn != nil && n.conn.state == stOperational {
			cs = append(cs, n.conn)
		}
	}
	total := len(s.fecs)
	s.mu.Unlock()
	for _, c := range cs {
		c.sendBindings(changed, !add)
	}
	verb := map[bool]string{true: "advertised", false: "withdrawn"}[add]
	return map[string]any{verb: len(changed), "total": total}, nil
}

func (s *session) flap(a *FlapArgs) (any, error) {
	cs, err := s.conns(a.Peer)
	if err != nil {
		return nil, err
	}
	till := time.Now().Add(time.Duration(a.Seconds) * time.Second)
	s.mu.Lock()
	if a.Mode == "silent" {
		s.silentTill = till
		s.nextLink, s.nextTarget = till, till
	}
	for lsr, n := range s.neighbors {
		if a.Peer == "" || lsr.String() == a.Peer {
			n.flapTill = till
		}
	}
	s.mu.Unlock()
	for _, c := range cs {
		if a.Mode == "silent" {
			c.setSilent(till)
		} else {
			c.notifyClose(StatusShutdown, "flap")
		}
	}
	return map[string]any{"flapped": len(cs), "seconds": a.Seconds, "mode": a.Mode}, nil
}

func (s *session) bindings(a *BindingsArgs) (any, error) {
	want, err := netip.ParseAddr(a.Peer)
	if err != nil {
		return nil, fmt.Errorf("peer %q is not an LSR ID", a.Peer)
	}
	var within netip.Prefix
	if a.Prefix != "" {
		if within, err = netip.ParsePrefix(a.Prefix); err != nil {
			return nil, fmt.Errorf("prefix %q", a.Prefix)
		}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 1000
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.neighbors[want]
	if n == nil {
		return nil, fmt.Errorf("no neighbor %s", a.Peer)
	}
	out := []BindingEntry{}
	total := 0
	if n.conn != nil {
		keys := make([]netip.Prefix, 0, len(n.conn.lib))
		for p := range n.conn.lib {
			if !within.IsValid() || within.Overlaps(p) && p.Bits() >= within.Bits() {
				keys = append(keys, p)
			}
		}
		total = len(keys)
		slices.SortFunc(keys, func(x, y netip.Prefix) int {
			if c := x.Addr().Compare(y.Addr()); c != 0 {
				return c
			}
			return x.Bits() - y.Bits()
		})
		for _, p := range keys {
			if len(out) >= limit {
				break
			}
			out = append(out, BindingEntry{Prefix: p.String(), Label: n.conn.lib[p]})
		}
	}
	return map[string]any{"bindings": out, "total": total}, nil
}

// ---- accepting ----

func (s *session) accept() {
	defer s.wg.Done()
	for {
		nc, err := s.listener.Accept()
		if err != nil {
			return
		}
		c := newConn(s, nil, nc)
		s.wg.Add(1)
		go c.run()
	}
}

func (s *session) dial(n *neighbor) {
	defer s.wg.Done()
	s.mu.Lock()
	remote := n.transport
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nc, err := s.env.IP().DialTCP(ctx, netip.AddrPortFrom(remote, Port), s.transport, 64)
	if err != nil {
		s.mu.Lock()
		n.dialing = false
		n.lastErr = "connect: " + err.Error()
		s.retryLocked(n)
		s.counters["connect_failures"]++
		s.mu.Unlock()
		return
	}
	c := newConn(s, n, nc)
	s.mu.Lock()
	n.dialing = false
	if n.conn != nil {
		s.mu.Unlock()
		nc.Close()
		return
	}
	n.conn = c
	s.wg.Add(1) // run's own, as an accepted connection's
	s.mu.Unlock()
	c.run()
}

// retryLocked backs off session attempts: 15 s doubling to 2 min (section
// 2.5.3), but sooner at first so a lab comes up quickly.
func (s *session) retryLocked(n *neighbor) {
	if n.backoff == 0 {
		n.backoff = 2 * time.Second
	} else {
		n.backoff = min(n.backoff*2, 2*time.Minute)
	}
	n.retryAt = time.Now().Add(n.backoff)
}
