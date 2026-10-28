// Package bgp is an emulated BGP speaker (RFC 4271) on a device: peers
// over the device's TCP (gVisor netstack, see ipstack), IPv4/IPv6 unicast
// (RFC 4760) and the VPN families (vpn.go: VPNv4/v6, EVPN, L2VPN; srpolicy.go:
// SR Policy), 4-byte
// AS numbers, route refresh, End-of-RIB; route
// injection at scale with communities; received routes per peer; actions
// to advertise/withdraw, reset, flap, send NOTIFICATIONs and route
// refreshes. Messages are encoded and decoded with GoBGP's codec.
package bgp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"

	"github.com/ripnet/clabgen/internal/ipstack"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "bgp"

func init() { protocol.Register(bgpType{}) }

type bgpType struct{}

func (bgpType) Name() string { return TypeName }
func (bgpType) Description() string {
	return "BGP speaker: IPv4/IPv6 unicast, VPNv4/v6, EVPN, L2VPN (VPLS/VPWS) and SR Policy over the device's TCP, route injection, received routes, reset/flap/notification actions"
}
func (bgpType) NewConfig() any       { return defaultConfig() }
func (bgpType) EtherTypes() []uint16 { return nil } // TCP arrives through the device's IP stack
func (bgpType) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "advertise", Description: "Advertise routes (until withdrawn or restarted)", NewArgs: func() any { return &RouteArgs{} }},
		{Name: "withdraw", Description: "Withdraw routes (injected or from config)", NewArgs: func() any { return &RouteArgs{} }},
		{Name: "reset", Description: "Reset sessions (Cease / administrative reset); they come back at once", NewArgs: func() any { return &PeerArgs{} }},
		{Name: "flap", Description: "Bring sessions down for a while, then let them come back", NewArgs: func() any { return &FlapArgs{Seconds: 10, Mode: "notify"} }},
		{Name: "notify", Description: "Send a NOTIFICATION with any code/subcode (closes the session)", NewArgs: func() any { return &NotifyArgs{Code: 6, Subcode: 2} }},
		{Name: "route-refresh", Description: "Ask peers to resend their routes", NewArgs: func() any { return &PeerArgs{} }},
		{Name: "rib", Description: "Show routes received from a peer", NewArgs: func() any { return &RIBArgs{Limit: 1000} }},
		{Name: "clear", Description: "Reset counters"},
	}
}

type session struct {
	env protocol.Env
	cfg Config
	rid netip.Addr

	mu       sync.Mutex
	peers    []*peer
	byAddr   map[netip.Addr]*peer
	out      map[netip.Prefix]*tmpl // what we advertise (unicast)
	xout     map[string]*xroute     // VPN families (vpn.go), by key
	tmpls    map[*tmpl]bool         // templates in use
	nextTmpl int
	counters map[string]uint64

	listeners []net.Listener
	stop      chan struct{}
	wg        sync.WaitGroup
}

func (bgpType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	ip := env.IP()
	if ip == nil {
		return nil, errors.New("the device has no IP stack")
	}
	s := &session{env: env, cfg: cfg, byAddr: map[netip.Addr]*peer{}, out: map[netip.Prefix]*tmpl{}, tmpls: map[*tmpl]bool{}, xout: map[string]*xroute{},
		counters: map[string]uint64{}, stop: make(chan struct{})}
	if cfg.RouterID != "" {
		s.rid = netip.MustParseAddr(cfg.RouterID)
	} else {
		for _, p := range ip.Addresses() {
			if p.Addr().Is4() {
				s.rid = p.Addr()
				break
			}
		}
		if !s.rid.IsValid() {
			return nil, errors.New("router_id is needed: the device has no IPv4 address")
		}
	}
	for _, r := range cfg.Routes {
		s.addRouteLocked(r)
		s.addXLocked(s.vpnRoutes(r))
	}
	for _, e := range cfg.EVPN {
		s.addXLocked(s.evpnRoutes(e))
	}
	for _, l := range cfg.L2VPN {
		if x, err := l.expand(); err == nil {
			s.addXLocked([]*xroute{x})
		}
	}
	for _, p := range cfg.SRPolicies {
		if x, err := p.expand(); err == nil {
			s.addXLocked([]*xroute{x})
		}
	}
	for _, v6 := range []bool{false, true} {
		l, err := ip.ListenTCP(netip.Addr{}, 179, v6, 0)
		if err != nil {
			s.closeListeners()
			return nil, fmt.Errorf("listen on port 179: %w", err)
		}
		s.listeners = append(s.listeners, l)
	}
	for _, pc := range cfg.Peers {
		p := newPeer(s, pc)
		s.peers = append(s.peers, p)
		s.byAddr[p.addr] = p
	}
	for _, l := range s.listeners {
		s.wg.Add(1)
		go s.accept(l)
	}
	for _, p := range s.peers {
		s.wg.Add(1)
		go p.manage()
	}
	return s, nil
}

func (s *session) closeListeners() {
	for _, l := range s.listeners {
		l.Close()
	}
}

func (s *session) accept(l net.Listener) {
	defer s.wg.Done()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		a := ipstack.AddrPortOf(c.RemoteAddr()).Addr().Unmap()
		s.mu.Lock()
		p := s.byAddr[a]
		if p == nil {
			s.counters["rx_unknown_peer_connections"]++
		}
		s.mu.Unlock()
		if p == nil {
			c.Close()
			continue
		}
		p.attach(c, false)
	}
}

func (s *session) Stop() {
	close(s.stop)
	s.closeListeners()
	s.mu.Lock()
	var conns []*conn
	for _, p := range s.peers {
		for c := range p.conns {
			conns = append(conns, c)
		}
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.notify(gb.BGP_ERROR_CEASE, gb.BGP_ERROR_SUB_ADMINISTRATIVE_SHUTDOWN, shutdownData("clabgen instance stopped"))
	}
	s.wg.Wait()
}

func (s *session) HandleRx(*pkt.Frame) {}

func (s *session) event(kind, msg string) { s.env.Emit(kind, msg, nil) }

// ---- advertised routes ----

// addRouteLocked records a route's prefixes with its attributes and
// returns them by family.
func (s *session) addRouteLocked(r Route) (*tmpl, []netip.Prefix) {
	t, err := r.attrs()
	if err != nil || r.RD != "" {
		return nil, nil
	}
	s.nextTmpl++
	t.id = s.nextTmpl
	s.tmpls[t] = true
	ps, _ := r.expand()
	for _, p := range ps {
		if old := s.out[p]; old != nil {
			old.count[famIdx(p)]--
			s.dropTmplLocked(old)
		}
		s.out[p] = t
		t.count[famIdx(p)]++
	}
	return t, ps
}

func famIdx(p netip.Prefix) int {
	if p.Addr().Is4() {
		return 0
	}
	return 1
}

// vpnRoutes are a VPN route's NLRIs (nil if it isn't one).
func (s *session) vpnRoutes(r Route) []*xroute {
	if r.RD == "" {
		return nil
	}
	t, err := r.attrs()
	if err != nil {
		return nil
	}
	xs, _ := r.vpnRoutes(t)
	return xs
}

func (s *session) evpnRoutes(e EVPNRoute) []*xroute {
	xs, _, _ := e.expand(s.rid)
	return xs
}

// addXLocked records routes of the VPN families.
func (s *session) addXLocked(xs []*xroute) {
	for _, x := range xs {
		s.xout[x.key] = x
	}
}

// advertisedLocked counts what peer p gets of family f.
func (s *session) advertisedLocked(p *peer, f family) int {
	if isXFamily(f.rf) {
		n := 0
		for _, x := range s.xout {
			if x.f == f && x.t.forPeer(p.addr) {
				n++
			}
		}
		return n
	}
	i := 0
	if f.v6 {
		i = 1
	}
	n := 0
	for t := range s.tmpls {
		if t.forPeer(p.addr) {
			n += t.count[i]
		}
	}
	return n
}

// dropTmplLocked forgets a template no prefix uses any more.
func (s *session) dropTmplLocked(t *tmpl) {
	if t.count == [2]int{} {
		delete(s.tmpls, t)
	}
}

func famOfPrefix(p netip.Prefix) family {
	if p.Addr().Is4() {
		return famIPv4
	}
	return famIPv6
}

// ---- status ----

type Status struct {
	LocalAS    uint32            `json:"local_as"`
	RouterID   string            `json:"router_id"`
	Advertised map[string]int    `json:"advertised"` // prefixes we inject, per family
	Peers      []PeerStatus      `json:"peers"`
	Counters   map[string]uint64 `json:"counters"`
}

func (s *session) Status() any {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{LocalAS: s.cfg.LocalAS, RouterID: s.rid.String(), Advertised: map[string]int{}, Peers: []PeerStatus{}, Counters: map[string]uint64{}}
	for p := range s.out {
		st.Advertised[famOfPrefix(p).name]++
	}
	for _, x := range s.xout {
		st.Advertised[x.f.name]++
	}
	for k, v := range s.counters {
		st.Counters[k] = v
	}
	for _, p := range s.peers {
		st.Peers = append(st.Peers, p.statusLocked(now))
	}
	return st
}

// ---- actions ----

func (s *session) peersNamed(a string) ([]*peer, error) {
	if a == "" {
		return s.peers, nil
	}
	ad, err := netip.ParseAddr(a)
	if err != nil {
		return nil, fmt.Errorf("peer %q: not an address", a)
	}
	p := s.byAddr[ad.Unmap()]
	if p == nil {
		return nil, fmt.Errorf("no peer %s", a)
	}
	return []*peer{p}, nil
}

func (s *session) Action(_ context.Context, name string, args any) (any, error) {
	switch name {
	case "advertise", "withdraw":
		return s.changeRoutes(name == "advertise", args.(*RouteArgs))
	case "reset", "route-refresh":
		s.mu.Lock()
		ps, err := s.peersNamed(args.(*PeerArgs).Peer)
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		n := 0
		for _, p := range ps {
			if c := p.established(); c != nil {
				n++
				if name == "reset" {
					c.notify(gb.BGP_ERROR_CEASE, gb.BGP_ERROR_SUB_ADMINISTRATIVE_RESET, shutdownData("reset by clabgen"))
				} else {
					c.sendRefresh()
				}
			}
		}
		return map[string]any{"sessions": n}, nil
	case "notify":
		a := args.(*NotifyArgs)
		s.mu.Lock()
		ps, err := s.peersNamed(a.Peer)
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		var data []byte
		if a.Code == gb.BGP_ERROR_CEASE && (a.Subcode == gb.BGP_ERROR_SUB_ADMINISTRATIVE_SHUTDOWN || a.Subcode == gb.BGP_ERROR_SUB_ADMINISTRATIVE_RESET) {
			data = shutdownData(a.Message)
		}
		n := 0
		for _, p := range ps {
			if c := p.established(); c != nil {
				c.notify(a.Code, a.Subcode, data)
				n++
			}
		}
		return map[string]any{"sessions": n}, nil
	case "flap":
		a := args.(*FlapArgs)
		s.mu.Lock()
		ps, err := s.peersNamed(a.Peer)
		till := time.Now().Add(time.Duration(a.Seconds) * time.Second)
		var down []*conn
		for _, p := range ps {
			p.flapTill, p.flapMode = till, a.Mode
			for c := range p.conns {
				down = append(down, c)
			}
		}
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		for _, c := range down {
			if a.Mode == "notify" {
				c.notify(gb.BGP_ERROR_CEASE, gb.BGP_ERROR_SUB_ADMINISTRATIVE_SHUTDOWN, shutdownData("flap by clabgen"))
			} else {
				c.goSilent()
			}
		}
		return map[string]any{"down_until": till.Format(time.RFC3339), "sessions": len(down)}, nil
	case "rib":
		return s.rib(args.(*RIBArgs))
	case "clear":
		s.mu.Lock()
		s.counters = map[string]uint64{}
		for _, p := range s.peers {
			p.counters = map[string]uint64{}
		}
		s.mu.Unlock()
		return map[string]any{"cleared": true}, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

// changeRoutes advertises or withdraws routes and tells established peers.
func (s *session) changeRoutes(add bool, a *RouteArgs) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	routes := a.Routes
	n := 0
	// VPN families.
	var xs []*xroute
	for _, r := range routes {
		xs = append(xs, s.vpnRoutes(r)...)
	}
	for _, e := range a.EVPN {
		xs = append(xs, s.evpnRoutes(e)...)
	}
	for _, l := range a.L2VPN {
		if x, err := l.expand(); err == nil {
			xs = append(xs, x)
		}
	}
	for _, p := range a.SRPolicies {
		if x, err := p.expand(); err == nil {
			xs = append(xs, x)
		}
	}
	var xchanged []*xroute
	for _, x := range xs {
		if add {
			s.xout[x.key] = x
			xchanged = append(xchanged, x)
		} else if old := s.xout[x.key]; old != nil {
			delete(s.xout, x.key)
			xchanged = append(xchanged, old)
		}
	}
	n += len(xchanged)
	type batch struct {
		t   *tmpl
		pfx []netip.Prefix
	}
	var adds []batch
	var dels []netip.Prefix
	for _, r := range routes {
		if r.RD != "" {
			continue
		}
		if add {
			t, ps := s.addRouteLocked(r)
			if t != nil {
				adds = append(adds, batch{t, ps})
				n += len(ps)
			}
			continue
		}
		ps, _ := r.expand()
		for _, p := range ps {
			if t, ok := s.out[p]; ok {
				t.count[famIdx(p)]--
				s.dropTmplLocked(t)
				delete(s.out, p)
				dels = append(dels, p)
				n++
			}
		}
	}
	for _, p := range s.peers {
		c := p.est
		if c == nil {
			continue
		}
		for _, b := range adds {
			if b.t.forPeer(p.addr) {
				c.enqueue(job{t: b.t, pfx: b.pfx})
			}
		}
		if len(dels) > 0 {
			c.enqueue(job{withdraw: true, pfx: dels})
		}
		var mine []*xroute
		for _, x := range xchanged {
			if x.t.forPeer(p.addr) {
				mine = append(mine, x)
			}
		}
		if len(mine) > 0 {
			c.enqueue(job{withdraw: !add, xs: mine})
		}
	}
	verb := "withdrawn"
	if add {
		verb = "advertised"
	}
	return map[string]any{verb: n, "total": len(s.out) + len(s.xout)}, nil
}

// RIBRoute is one received route.
type RIBRoute struct {
	Prefix string `json:"prefix"`
	Family string `json:"family"`
	*pathInfo
}

func (s *session) rib(a *RIBArgs) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.peersNamed(a.Peer)
	if err != nil {
		return nil, err
	}
	if a.Peer == "" {
		return nil, errors.New("rib needs a peer")
	}
	p := ps[0]
	var within netip.Prefix
	if a.Prefix != "" {
		within, _ = netip.ParsePrefix(a.Prefix) // else a substring of other families' routes
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 1000
	}
	var out []RIBRoute
	total := 0
	for _, f := range xfamilies {
		if a.Family != "" && a.Family != f.name {
			continue
		}
		keys := make([]string, 0, len(p.adjX[f.rf]))
		for k, r := range p.adjX[f.rf] {
			if a.Prefix == "" || strings.Contains(r.text, a.Prefix) {
				keys = append(keys, k)
			}
		}
		total += len(keys)
		sort.Strings(keys)
		for _, k := range keys {
			if len(out) >= limit {
				break
			}
			r := p.adjX[f.rf][k]
			out = append(out, RIBRoute{Prefix: r.text, Family: f.name, pathInfo: r.info})
		}
	}
	for _, f := range []family{famIPv4, famIPv6} {
		if a.Family != "" && a.Family != f.name || a.Prefix != "" && !within.IsValid() {
			continue
		}
		keys := make([]netip.Prefix, 0, len(p.adjIn[f.rf]))
		for k := range p.adjIn[f.rf] {
			if !within.IsValid() || within.Overlaps(k) && k.Bits() >= within.Bits() {
				keys = append(keys, k)
			}
		}
		total += len(keys)
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].Addr() != keys[j].Addr() {
				return keys[i].Addr().Less(keys[j].Addr())
			}
			return keys[i].Bits() < keys[j].Bits()
		})
		for _, k := range keys {
			if len(out) >= limit {
				break
			}
			out = append(out, RIBRoute{Prefix: k.String(), Family: f.name, pathInfo: p.adjIn[f.rf][k]})
		}
	}
	if out == nil {
		out = []RIBRoute{}
	}
	return map[string]any{"routes": out, "total": total}, nil
}

// shutdownData is an RFC 9003 shutdown communication.
func shutdownData(msg string) []byte {
	if len(msg) > 255 {
		msg = msg[:255]
	}
	return append([]byte{byte(len(msg))}, msg...)
}

// notifyText names a NOTIFICATION.
func notifyText(code, sub uint8, data []byte) string {
	t := gb.NewNotificationErrorCode(code, sub).String()
	if code == gb.BGP_ERROR_CEASE && (sub == gb.BGP_ERROR_SUB_ADMINISTRATIVE_SHUTDOWN || sub == gb.BGP_ERROR_SUB_ADMINISTRATIVE_RESET) && len(data) > 0 && int(data[0]) <= len(data)-1 && data[0] > 0 {
		t += fmt.Sprintf(" (%q)", data[1:1+data[0]])
	}
	return strings.TrimSpace(t)
}

// Health is the peers' state, or how many are established when they
// differ (protocol.Healther).
func (s *session) Health() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.peers) == 0 {
		return "no peers"
	}
	est, first, same := 0, s.peers[0].state, true
	for _, p := range s.peers {
		if p.state == stEstablished {
			est++
		}
		same = same && p.state == first
	}
	if same {
		return first
	}
	return fmt.Sprintf("%d/%d established", est, len(s.peers))
}
