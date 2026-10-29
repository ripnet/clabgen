package bgp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"

	"github.com/ripnet/clabgen/internal/ipstack"
)

// Session states (RFC 4271 8.2.2).
const (
	stIdle        = "idle"
	stConnect     = "connect"
	stActive      = "active"
	stOpenSent    = "opensent"
	stOpenConfirm = "openconfirm"
	stEstablished = "established"
)

type peer struct {
	s    *session
	cfg  Peer
	addr netip.Addr
	fams []family
	hold int
	ttl  int
	wake chan struct{}
	quit chan struct{} // closed (under s.mu) when the peer leaves the config

	// Under s.mu, like cfg.Passive and cfg.Description: a config change
	// updates them in place.
	retry time.Duration

	// Under s.mu.
	conns    map[*conn]bool
	est      *conn
	state    string
	since    time.Time
	lastErr  string // last error of any connection attempt
	lastDown string // why the last session went down
	downAt   time.Time
	ups      int
	downs    int
	flapTill time.Time
	flapMode string
	lastBad  string // hex of the last message we couldn't parse
	adjIn    map[gb.RouteFamily]map[netip.Prefix]*pathInfo
	adjX     map[gb.RouteFamily]map[string]*rxRoute // VPN families
	eorIn    map[gb.RouteFamily]bool
	counters map[string]uint64
}

func newPeer(s *session, c Peer) *peer {
	p := &peer{s: s, cfg: c, addr: netip.MustParseAddr(c.Address).Unmap(), hold: c.HoldTime, ttl: c.TTL,
		retry: retryOf(c), wake: make(chan struct{}, 1), quit: make(chan struct{}), conns: map[*conn]bool{},
		state: stIdle, since: time.Now(), counters: map[string]uint64{}}
	if p.hold == 0 {
		p.hold = s.cfg.HoldTime
	}
	if p.ttl == 0 && c.RemoteAS != s.cfg.LocalAS {
		p.ttl = 1 // directly connected eBGP
	}
	for _, f := range c.Families {
		fam, _ := familyByName(f)
		p.fams = append(p.fams, fam)
	}
	if len(p.fams) == 0 {
		p.fams = []family{famOfPrefix(netip.PrefixFrom(p.addr, 0))}
	}
	p.resetRIBLocked()
	return p
}

func retryOf(c Peer) time.Duration {
	if c.ConnectRetry == 0 {
		return 5 * time.Second
	}
	return time.Duration(c.ConnectRetry) * time.Second
}

// removedLocked reports whether the peer was taken out of the config.
func (p *peer) removedLocked() bool {
	select {
	case <-p.quit:
		return true
	default:
		return false
	}
}

func (p *peer) ebgp() bool { return p.cfg.RemoteAS != p.s.cfg.LocalAS }

func (p *peer) resetRIBLocked() {
	p.adjIn, p.eorIn = map[gb.RouteFamily]map[netip.Prefix]*pathInfo{}, map[gb.RouteFamily]bool{}
	p.adjX = map[gb.RouteFamily]map[string]*rxRoute{}
}

func (p *peer) established() *conn {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	return p.est
}

// manage dials the peer while there is no session (unless passive).
func (p *peer) manage() {
	defer p.s.wg.Done()
	for {
		p.s.mu.Lock()
		now := time.Now()
		flapping := now.Before(p.flapTill)
		dial := !p.cfg.Passive && p.est == nil && !flapping && !p.hasOutboundLocked()
		if len(p.conns) == 0 && p.state != stIdle && !flapping {
			p.setStateLocked(stActive)
		}
		wait := p.retry
		if flapping {
			wait = time.Until(p.flapTill)
		}
		p.s.mu.Unlock()
		if dial {
			p.dial()
		}
		select {
		case <-p.s.stop:
			return
		case <-p.quit:
			return
		case <-p.wake:
		case <-time.After(wait):
		}
	}
}

func (p *peer) hasOutboundLocked() bool {
	for c := range p.conns {
		if c.outbound {
			return true
		}
	}
	return false
}

// setStateLocked sets the peer's state; while a session is Established,
// other connections' handshakes don't show.
func (p *peer) setStateLocked(st string) {
	if p.est != nil && st != stEstablished {
		return
	}
	if p.state != st {
		p.state, p.since = st, time.Now()
	}
}

func (p *peer) localAddr() netip.Addr {
	if p.cfg.LocalAddress != "" {
		return netip.MustParseAddr(p.cfg.LocalAddress)
	}
	if ip := p.s.env.IP(); ip != nil {
		if a, ok := ip.Source(p.addr); ok {
			return a
		}
	}
	return netip.Addr{}
}

func (p *peer) dial() {
	p.s.mu.Lock()
	p.setStateLocked(stConnect)
	retry := p.retry
	p.s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), min(retry, 10*time.Second)+time.Second)
	defer cancel()
	nc, err := p.s.env.IP().DialTCP(ctx, netip.AddrPortFrom(p.addr, 179), p.localAddr(), p.ttl)
	if err != nil {
		p.s.mu.Lock()
		p.counters["connect_failures"]++
		if p.est == nil && len(p.conns) == 0 {
			p.lastErr = "connect: " + err.Error()
			p.setStateLocked(stActive)
		}
		p.s.mu.Unlock()
		return
	}
	p.attach(nc, true)
}

// attach starts the BGP handshake on a new connection.
func (p *peer) attach(nc net.Conn, outbound bool) {
	c := &conn{p: p, c: nc, outbound: outbound, state: stOpenSent, closed: make(chan struct{}), jobSig: make(chan struct{}, 1)}
	p.s.mu.Lock()
	refuse := p.est != nil || len(p.conns) >= 2 || time.Now().Before(p.flapTill) || p.removedLocked()
	if !refuse {
		p.conns[c] = true
		p.setStateLocked(stOpenSent)
	}
	p.s.mu.Unlock()
	if refuse {
		c.write(serialize(gb.NewBGPNotificationMessage(gb.BGP_ERROR_CEASE, gb.BGP_ERROR_SUB_CONNECTION_REJECTED, nil)))
		nc.Close()
		return
	}
	p.s.wg.Add(1)
	go c.run()
}

// ---- one connection ----

type job struct {
	dump     bool
	refresh  gb.RouteFamily // resend this family (route refresh)
	t        *tmpl
	withdraw bool
	pfx      []netip.Prefix
	xs       []*xroute // VPN families: advertise (or withdraw) these
}

type conn struct {
	p        *peer
	c        net.Conn
	outbound bool

	// Under p.s.mu.
	state    string
	remoteID netip.Addr
	remoteAS uint32
	holdTime int
	fams     []family
	caps     []string
	refresh  bool // peer can do route refresh

	wmu    sync.Mutex
	silent bool // flap: send nothing more
	closed chan struct{}
	once   sync.Once
	why    string // set when we close it (under wmu)

	jmu    sync.Mutex
	jobs   []job
	jobSig chan struct{}
}

func (c *conn) count(k string) {
	c.p.s.mu.Lock()
	c.p.counters[k]++
	c.p.s.mu.Unlock()
}

func (c *conn) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.silent {
		return nil
	}
	c.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.c.Write(b)
	return err
}

func (c *conn) send(m *gb.BGPMessage, kind string) error {
	err := c.write(serialize(m))
	if err == nil {
		c.count("tx_" + kind)
	}
	return err
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.closed)
		c.c.Close()
	})
}

// notify sends a NOTIFICATION and closes.
func (c *conn) notify(code, sub uint8, data []byte) {
	why := "sent " + notifyText(code, sub, data)
	if c.send(gb.NewBGPNotificationMessage(code, sub, data), "notification") == nil {
		c.p.s.mu.Lock()
		c.p.lastErr = why
		c.p.s.mu.Unlock()
	}
	c.wmu.Lock()
	if c.why == "" {
		c.why = why
	}
	c.silent = true // nothing after the NOTIFICATION
	c.wmu.Unlock()
	// Half-close so the peer reads the NOTIFICATION before the connection
	// goes (closing with unread data would reset it).
	if hc, ok := c.c.(interface{ CloseWrite() error }); ok {
		hc.CloseWrite()
		time.AfterFunc(500*time.Millisecond, c.close)
		return
	}
	c.close()
}

// closedWhy is why we closed the connection ("" if we didn't).
func (c *conn) closedWhy() string {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.why
}

func (c *conn) goSilent() {
	c.wmu.Lock()
	c.silent = true
	c.wmu.Unlock()
}

func (c *conn) enqueue(j job) {
	c.jmu.Lock()
	c.jobs = append(c.jobs, j)
	c.jmu.Unlock()
	select {
	case c.jobSig <- struct{}{}:
	default:
	}
}

func (c *conn) readMsg(deadline time.Duration) (*gb.BGPMessage, error) {
	m, _, err := c.readMsgL2(deadline)
	return m, err
}

// readMsgL2 reads a message; L2VPN routes in an UPDATE come back decoded
// apart (vpn.go).
func (c *conn) readMsgL2(deadline time.Duration) (*gb.BGPMessage, *l2Update, error) {
	if deadline > 0 {
		c.c.SetReadDeadline(time.Now().Add(deadline))
	} else {
		c.c.SetReadDeadline(time.Time{})
	}
	var h [19]byte
	if _, err := io.ReadFull(c.c, h[:]); err != nil {
		return nil, nil, err
	}
	l := int(binary.BigEndian.Uint16(h[16:]))
	for _, x := range h[:16] {
		if x != 0xff {
			return nil, nil, gb.NewMessageError(gb.BGP_ERROR_MESSAGE_HEADER_ERROR, gb.BGP_ERROR_SUB_CONNECTION_NOT_SYNCHRONIZED, nil, "bad marker")
		}
	}
	if l < 19 || l > maxMsg {
		return nil, nil, gb.NewMessageError(gb.BGP_ERROR_MESSAGE_HEADER_ERROR, gb.BGP_ERROR_SUB_BAD_MESSAGE_LENGTH, h[16:18], fmt.Sprintf("bad length %d", l))
	}
	b := make([]byte, l)
	copy(b, h[:])
	if _, err := io.ReadFull(c.c, b[19:]); err != nil {
		return nil, nil, err
	}
	b, l2 := takeL2VPN(b)
	m, err := gb.ParseBGPMessage(b)
	if err != nil {
		c.p.s.mu.Lock()
		c.p.lastBad = fmt.Sprintf("% x", b[:min(len(b), 512)])
		c.p.s.mu.Unlock()
	}
	return m, l2, err
}

func (c *conn) run() {
	p, s := c.p, c.p.s
	defer s.wg.Done()
	reason := c.handshake()
	if reason == "" {
		reason = c.established()
	}
	c.close()
	s.mu.Lock()
	delete(p.conns, c)
	wasUp := p.est == c
	if wasUp {
		p.est = nil
		p.downs++
		p.lastDown, p.downAt = reason, time.Now()
		p.resetRIBLocked()
	}
	if reason != "" && (wasUp || p.est == nil) {
		p.lastErr = reason
	}
	if p.est == nil && len(p.conns) == 0 {
		p.setStateLocked(stIdle)
	}
	s.mu.Unlock()
	if wasUp {
		go s.event("peer-down", fmt.Sprintf("BGP peer %s down: %s", p.addr, reason))
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// handshake runs OpenSent / OpenConfirm. It returns why it failed ("" when
// Established).
func (c *conn) handshake() string {
	p, s := c.p, c.p.s
	as2 := s.cfg.LocalAS
	if as2 > 65535 {
		as2 = gb.AS_TRANS
	}
	caps := []gb.ParameterCapabilityInterface{}
	for _, f := range p.fams {
		caps = append(caps, gb.NewCapMultiProtocol(f.rf))
	}
	caps = append(caps, gb.NewCapRouteRefresh(), gb.NewCapFourOctetASNumber(s.cfg.LocalAS))
	open := gb.NewBGPOpenMessage(uint16(as2), uint16(p.hold), s.rid.String(), []gb.OptionParameterInterface{gb.NewOptionParameterCapability(caps)})
	if err := c.send(open, "open"); err != nil {
		return "send OPEN: " + err.Error()
	}
	gotOpen := false
	for {
		wait := 4 * time.Minute // RFC 4271: a large hold time until OPEN
		if gotOpen && c.holdTime > 0 {
			wait = time.Duration(c.holdTime) * time.Second
		}
		m, err := c.readMsg(wait)
		if err != nil {
			return c.readError(err)
		}
		switch b := m.Body.(type) {
		case *gb.BGPOpen:
			c.count("rx_open")
			if gotOpen {
				c.notify(gb.BGP_ERROR_FSM_ERROR, 0, nil)
				return "second OPEN"
			}
			if code, sub, why := c.checkOpen(b); why != "" {
				c.notify(code, sub, nil)
				return why
			}
			gotOpen = true
			if lose := c.resolveCollision(); lose {
				c.notify(gb.BGP_ERROR_CEASE, gb.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil)
				return "connection collision (the other connection won)"
			}
			if err := c.send(gb.NewBGPKeepAliveMessage(), "keepalive"); err != nil {
				return "send KEEPALIVE: " + err.Error()
			}
		case *gb.BGPKeepAlive:
			c.count("rx_keepalive")
			if !gotOpen {
				c.notify(gb.BGP_ERROR_FSM_ERROR, 0, nil)
				return "KEEPALIVE before OPEN"
			}
			s.mu.Lock()
			lost := p.est != nil
			if !lost {
				p.est, c.state = c, stEstablished
				p.ups++
				p.lastErr = ""
				p.setStateLocked(stEstablished)
			}
			var others []*conn
			for o := range p.conns {
				if o != c {
					others = append(others, o)
				}
			}
			s.mu.Unlock()
			if lost {
				c.notify(gb.BGP_ERROR_CEASE, gb.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil)
				return "connection collision"
			}
			for _, o := range others {
				o.notify(gb.BGP_ERROR_CEASE, gb.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil)
			}
			return ""
		case *gb.BGPNotification:
			c.count("rx_notification")
			return "received " + notifyText(b.ErrorCode, b.ErrorSubcode, b.Data)
		default:
			c.notify(gb.BGP_ERROR_FSM_ERROR, 0, nil)
			return fmt.Sprintf("unexpected message type %d before Established", m.Header.Type)
		}
	}
}

func (c *conn) readError(err error) string {
	if why := c.closedWhy(); why != "" {
		return why
	}
	var me *gb.MessageError
	switch {
	case errors.As(err, &me):
		c.notify(me.TypeCode, me.SubTypeCode, me.Data)
		return "malformed message: " + me.Message
	case isTimeout(err):
		c.notify(gb.BGP_ERROR_HOLD_TIMER_EXPIRED, 0, nil)
		return "hold timer expired"
	case errors.Is(err, io.EOF):
		return "connection closed by the peer"
	}
	select {
	case <-c.closed:
		return "" // we closed it (reason already recorded)
	default:
	}
	return err.Error()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// checkOpen validates the peer's OPEN and negotiates (RFC 4271 6.2).
func (c *conn) checkOpen(o *gb.BGPOpen) (uint8, uint8, string) {
	p, s := c.p, c.p.s
	as := uint32(o.MyAS)
	theirs := map[gb.RouteFamily]bool{}
	var caps []string
	refresh, hasMP := false, false
	for _, op := range o.OptParams {
		oc, ok := op.(*gb.OptionParameterCapability)
		if !ok {
			continue
		}
		for _, x := range oc.Capability {
			caps = append(caps, x.Code().String())
			switch v := x.(type) {
			case *gb.CapMultiProtocol:
				theirs[v.CapValue], hasMP = true, true
			case *gb.CapFourOctetASNumber:
				as = v.CapValue
			case *gb.CapRouteRefresh, *gb.CapRouteRefreshCisco:
				refresh = true
			}
		}
	}
	if !hasMP {
		theirs[gb.RF_IPv4_UC] = true
	}
	if as != p.cfg.RemoteAS {
		return gb.BGP_ERROR_OPEN_MESSAGE_ERROR, gb.BGP_ERROR_SUB_BAD_PEER_AS, fmt.Sprintf("peer AS %d, expected %d", as, p.cfg.RemoteAS)
	}
	if o.HoldTime == 1 || o.HoldTime == 2 {
		return gb.BGP_ERROR_OPEN_MESSAGE_ERROR, gb.BGP_ERROR_SUB_UNACCEPTABLE_HOLD_TIME, fmt.Sprintf("unacceptable hold time %d", o.HoldTime)
	}
	id, _ := netip.AddrFromSlice(o.ID.To4())
	if !id.IsValid() || id.IsUnspecified() || (!p.ebgp() && id == s.rid) {
		return gb.BGP_ERROR_OPEN_MESSAGE_ERROR, gb.BGP_ERROR_SUB_BAD_BGP_IDENTIFIER, fmt.Sprintf("bad BGP identifier %s", o.ID)
	}
	hold := min(p.hold, int(o.HoldTime))
	var fams []family
	for _, f := range p.fams {
		if theirs[f.rf] {
			fams = append(fams, f)
		}
	}
	sort.Strings(caps)
	s.mu.Lock()
	c.state, c.remoteID, c.remoteAS, c.holdTime, c.fams, c.caps, c.refresh = stOpenConfirm, id, as, hold, fams, caps, refresh
	p.setStateLocked(stOpenConfirm)
	s.mu.Unlock()
	return 0, 0, ""
}

// resolveCollision decides between two connections to the peer (RFC 4271
// 6.8) as soon as c has the peer's OPEN: the one opened by the higher BGP
// ID stays. Deciding before the other connection's OPEN arrives (both ends
// already have both connections) keeps the ends from going Established on
// different ones. It reports whether c loses (and closes the other one if
// c wins).
func (c *conn) resolveCollision() bool {
	p, s := c.p, c.p.s
	s.mu.Lock()
	if p.est != nil {
		s.mu.Unlock()
		return true
	}
	var other *conn
	for o := range p.conns {
		if o != c {
			other = o
		}
	}
	keepOutbound := s.rid.Compare(c.remoteID) > 0
	s.mu.Unlock()
	if other == nil {
		return false
	}
	if c.outbound == keepOutbound {
		other.notify(gb.BGP_ERROR_CEASE, gb.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil)
		return false
	}
	return true
}

// established runs the session until it ends; it returns why.
func (c *conn) established() string {
	p, s := c.p, c.p.s
	go s.event("peer-up", fmt.Sprintf("BGP peer %s (AS %d, %s) up: %d families", p.addr, c.remoteAS, c.remoteID, len(c.fams)))
	if c.holdTime > 0 {
		go c.keepalives(time.Duration(c.holdTime) * time.Second / 3)
	}
	go c.sender()
	c.enqueue(job{dump: true})
	for {
		hold := time.Duration(c.holdTime) * time.Second
		m, l2, err := c.readMsgL2(hold)
		if err != nil {
			return c.readError(err)
		}
		switch b := m.Body.(type) {
		case *gb.BGPKeepAlive:
			c.count("rx_keepalive")
		case *gb.BGPUpdate:
			c.count("rx_update")
			c.rxUpdate(b, l2)
		case *gb.BGPRouteRefresh:
			c.count("rx_route_refresh")
			c.enqueue(job{refresh: gb.AfiSafiToRouteFamily(b.AFI, b.SAFI)})
		case *gb.BGPNotification:
			c.count("rx_notification")
			return "received " + notifyText(b.ErrorCode, b.ErrorSubcode, b.Data)
		case *gb.BGPOpen:
			c.notify(gb.BGP_ERROR_FSM_ERROR, 0, nil)
			return "OPEN while Established"
		}
	}
}

func (c *conn) keepalives(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			if c.send(gb.NewBGPKeepAliveMessage(), "keepalive") != nil {
				return
			}
		}
	}
}

func (c *conn) sendRefresh() {
	c.p.s.mu.Lock()
	fams, ok := c.fams, c.refresh
	c.p.s.mu.Unlock()
	if !ok {
		return
	}
	for _, f := range fams {
		afi, safi := gb.RouteFamilyToAfiSafi(f.rf)
		c.send(gb.NewBGPRouteRefreshMessage(afi, 0, safi), "route_refresh")
	}
}

// ---- receiving routes ----

func toPrefix(a gb.AddrPrefixInterface) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(a.String())
	return p.Masked(), err == nil
}

func (c *conn) rxUpdate(u *gb.BGPUpdate, l2 *l2Update) {
	p, s := c.p, c.p.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if l2 != nil {
		info := summarize(u.PathAttributes)
		m := p.adjX[gb.RF_VPLS]
		if m == nil {
			m = map[string]*rxRoute{}
			p.adjX[gb.RF_VPLS] = m
		}
		for _, n := range l2.unreach {
			delete(m, n.key)
		}
		in := info.withNextHop(l2.nextHop)
		for _, n := range l2.reach {
			if info.hasAS(s.cfg.LocalAS) {
				delete(m, n.key)
				p.counters["rx_as_path_loops"]++
				continue
			}
			m[n.key] = &rxRoute{text: n.text, info: in}
		}
		if len(l2.reach) == 0 && len(l2.unreach) == 0 {
			p.eorIn[gb.RF_VPLS] = true // an empty MP_UNREACH: End-of-RIB
		}
	}
	if eor, rf := u.IsEndOfRib(); eor && l2 == nil {
		p.eorIn[rf] = true
		return
	}
	rib := func(rf gb.RouteFamily) map[netip.Prefix]*pathInfo {
		m := p.adjIn[rf]
		if m == nil {
			m = map[netip.Prefix]*pathInfo{}
			p.adjIn[rf] = m
		}
		return m
	}
	xrib := func(rf gb.RouteFamily) map[string]*rxRoute {
		m := p.adjX[rf]
		if m == nil {
			m = map[string]*rxRoute{}
			p.adjX[rf] = m
		}
		return m
	}
	for _, w := range u.WithdrawnRoutes {
		if x, ok := toPrefix(w); ok {
			delete(rib(gb.RF_IPv4_UC), x)
		}
	}
	var info *pathInfo
	if len(u.NLRI) > 0 || len(u.PathAttributes) > 0 {
		info = summarize(u.PathAttributes)
	}
	// Our AS in the path: a loop (RFC 4271 9.1.2); treat as withdrawn.
	loop := info != nil && info.hasAS(s.cfg.LocalAS)
	store := func(rf gb.RouteFamily, x netip.Prefix, in *pathInfo) {
		if loop {
			delete(rib(rf), x)
			p.counters["rx_as_path_loops"]++
			return
		}
		rib(rf)[x] = in
	}
	for _, n := range u.NLRI {
		if x, ok := toPrefix(n); ok {
			store(gb.RF_IPv4_UC, x, info)
		}
	}
	for _, a := range u.PathAttributes {
		switch m := a.(type) {
		case *gb.PathAttributeMpReachNLRI:
			rf := gb.AfiSafiToRouteFamily(m.AFI, m.SAFI)
			nh := m.Nexthop.String()
			if m.LinkLocalNexthop != nil {
				nh += " " + m.LinkLocalNexthop.String()
			}
			in := info.withNextHop(nh)
			if isXFamily(rf) {
				vxlan := strings.Contains(strings.Join(in.ExtCommunities, " "), "VXLAN")
				for _, n := range m.Value {
					if loop {
						delete(xrib(rf), xKey(n))
						p.counters["rx_as_path_loops"]++
						continue
					}
					xrib(rf)[xKey(n)] = &rxRoute{text: rxText(n, vxlan), info: in}
				}
				continue
			}
			for _, n := range m.Value {
				if x, ok := toPrefix(n); ok {
					store(rf, x, in)
				}
			}
		case *gb.PathAttributeMpUnreachNLRI:
			rf := gb.AfiSafiToRouteFamily(m.AFI, m.SAFI)
			if isXFamily(rf) {
				for _, n := range m.Value {
					delete(xrib(rf), xKey(n))
				}
				continue
			}
			for _, n := range m.Value {
				if x, ok := toPrefix(n); ok {
					delete(rib(rf), x)
				}
			}
		}
	}
}

// ---- sending routes ----

// sender works through the connection's jobs in order.
func (c *conn) sender() {
	for {
		select {
		case <-c.closed:
			return
		case <-c.jobSig:
		}
		for {
			c.jmu.Lock()
			if len(c.jobs) == 0 {
				c.jmu.Unlock()
				break
			}
			j := c.jobs[0]
			c.jobs = c.jobs[1:]
			c.jmu.Unlock()
			if err := c.do(j); err != nil {
				c.close()
				return
			}
		}
	}
}

// nextHops are our next hops toward the peer: the session's local address
// for its family, the device's for the other.
func (c *conn) nextHops() (v4, v6 netip.Addr) {
	local := ipstack.AddrPortOf(c.c.LocalAddr()).Addr().Unmap()
	if local.Is4() {
		v4 = local
	} else if !local.IsLinkLocalUnicast() {
		v6 = local
	}
	if ip := c.p.s.env.IP(); ip != nil {
		for _, a := range ip.Addresses() {
			x := a.Addr()
			if x.Is4() && !v4.IsValid() {
				v4 = x
			}
			if x.Is6() && !x.IsLinkLocalUnicast() && !v6.IsValid() {
				v6 = x
			}
		}
	}
	return v4, v6
}

func (c *conn) do(j job) error {
	p, s := c.p, c.p.s
	s.mu.Lock()
	fams := map[gb.RouteFamily]family{}
	for _, f := range c.fams {
		fams[f.rf] = f
	}
	// What to send, grouped by template and family.
	type key struct {
		t *tmpl
		f family
	}
	groups := map[key][]netip.Prefix{}
	xgroups := map[key][][]byte{}
	xwithdraw := map[family][][]byte{}
	var withdraw map[family][]netip.Prefix
	var eor []family
	addX := func(x *xroute) {
		if _, ok := fams[x.f.rf]; ok && x.t.forPeer(p.addr) {
			xgroups[key{x.t, x.f}] = append(xgroups[key{x.t, x.f}], x.nlri)
		}
	}
	switch {
	case j.xs != nil:
		for _, x := range j.xs {
			if _, ok := fams[x.f.rf]; !ok {
				continue
			}
			if j.withdraw {
				xwithdraw[x.f] = append(xwithdraw[x.f], x.nlri)
			} else if s.xout[x.key] == x {
				addX(x)
			}
		}
	case j.dump || j.refresh != 0:
		for _, x := range s.xout {
			if j.refresh == 0 || x.f.rf == j.refresh {
				addX(x)
			}
		}
		for x, t := range s.out {
			f := famOfPrefix(x)
			if _, ok := fams[f.rf]; !ok || !t.forPeer(p.addr) || (j.refresh != 0 && f.rf != j.refresh) {
				continue
			}
			groups[key{t, f}] = append(groups[key{t, f}], x)
		}
		if j.dump {
			for _, f := range c.fams {
				eor = append(eor, f)
			}
		}
	case j.withdraw:
		withdraw = map[family][]netip.Prefix{}
		for _, x := range j.pfx {
			if f := famOfPrefix(x); fams[f.rf] == f {
				withdraw[f] = append(withdraw[f], x)
			}
		}
	default:
		for _, x := range j.pfx {
			if f := famOfPrefix(x); fams[f.rf] == f && s.out[x] == j.t {
				groups[key{j.t, f}] = append(groups[key{j.t, f}], x)
			}
		}
	}
	localAS, ebgp := s.cfg.LocalAS, p.ebgp()
	s.mu.Unlock()
	v4, v6 := c.nextHops()
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		return keys[a].t.id < keys[b].t.id || keys[a].t.id == keys[b].t.id && !keys[a].f.v6
	})
	for _, k := range keys {
		pfx := groups[k]
		sortPrefixes(pfx)
		nh := k.t.nextHop
		if !nh.IsValid() {
			nh = v4
			if k.f.v6 {
				nh = v6
			}
		}
		if !nh.IsValid() {
			c.count("tx_skipped_no_next_hop")
			continue
		}
		msgs, err := buildUpdates(k.f, k.t.pathAttrs(localAS, ebgp), nh, netip.Addr{}, pfx)
		if err != nil {
			c.count("tx_encode_errors")
			continue
		}
		for _, m := range msgs {
			if err := c.write(m); err != nil {
				return err
			}
			c.count("tx_update")
		}
	}
	xkeys := make([]key, 0, len(xgroups))
	for k := range xgroups {
		xkeys = append(xkeys, k)
	}
	sort.Slice(xkeys, func(a, b int) bool {
		return xkeys[a].t.id < xkeys[b].t.id || xkeys[a].t.id == xkeys[b].t.id && xkeys[a].f.name < xkeys[b].f.name
	})
	for _, k := range xkeys {
		nh := k.t.nextHop
		if !nh.IsValid() {
			nh = v4
			if !nh.IsValid() {
				nh = v6
			}
		}
		if !nh.IsValid() {
			c.count("tx_skipped_no_next_hop")
			continue
		}
		msgs, err := buildMPUpdates(k.f, k.t.pathAttrs(localAS, ebgp), xNextHop(k.f, nh), xgroups[k])
		if err != nil {
			c.count("tx_encode_errors")
			continue
		}
		for _, m := range msgs {
			if err := c.write(m); err != nil {
				return err
			}
			c.count("tx_update")
		}
	}
	for f, nl := range xwithdraw {
		for _, m := range buildMPWithdraws(f, nl) {
			if err := c.write(m); err != nil {
				return err
			}
			c.count("tx_update")
		}
	}
	for f, pfx := range withdraw {
		sortPrefixes(pfx)
		for _, m := range buildWithdraws(f, pfx) {
			if err := c.write(m); err != nil {
				return err
			}
			c.count("tx_update")
		}
	}
	for _, f := range eor {
		if err := c.write(endOfRIB(f)); err != nil {
			return err
		}
		c.count("tx_update")
	}
	return nil
}

func sortPrefixes(ps []netip.Prefix) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Addr() != ps[j].Addr() {
			return ps[i].Addr().Less(ps[j].Addr())
		}
		return ps[i].Bits() < ps[j].Bits()
	})
}

// ---- status ----

type PeerStatus struct {
	Address       string            `json:"address"`
	RemoteAS      uint32            `json:"remote_as"`
	Description   string            `json:"description,omitempty"`
	Type          string            `json:"type"` // ebgp or ibgp
	State         string            `json:"state"`
	Since         string            `json:"since"`
	RemoteID      string            `json:"remote_id,omitempty"`
	LocalAddress  string            `json:"local_address,omitempty"`
	HoldTime      int               `json:"hold_time,omitempty"` // negotiated
	Families      []string          `json:"families"`            // negotiated (configured when down)
	Capabilities  []string          `json:"capabilities,omitempty"`
	Received      map[string]int    `json:"received"`
	Advertised    map[string]int    `json:"advertised"`
	EndOfRIB      []string          `json:"end_of_rib_received,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
	LastDown      string            `json:"last_down,omitempty"` // why the last session ended
	LastDownAt    string            `json:"last_down_at,omitempty"`
	LastMalformed string            `json:"last_malformed,omitempty"` // hex, first 512 bytes
	Ups           int               `json:"ups"`
	Downs         int               `json:"downs"`
	Flapping      bool              `json:"flapping,omitempty"`
	Counters      map[string]uint64 `json:"counters"`
}

func (p *peer) statusLocked(now time.Time) PeerStatus {
	st := PeerStatus{Address: p.addr.String(), RemoteAS: p.cfg.RemoteAS, Description: p.cfg.Description, Type: "ibgp", State: p.state,
		Since: p.since.Format(time.RFC3339), LastError: p.lastErr, Ups: p.ups, Downs: p.downs, Flapping: now.Before(p.flapTill),
		Received: map[string]int{}, Advertised: map[string]int{}, Counters: map[string]uint64{}, LastDown: p.lastDown, LastMalformed: p.lastBad}
	if !p.downAt.IsZero() {
		st.LastDownAt = p.downAt.Format(time.RFC3339)
	}
	if p.ebgp() {
		st.Type = "ebgp"
	}
	fams := p.fams
	if c := p.est; c != nil {
		fams = c.fams
		st.RemoteID, st.HoldTime, st.Capabilities = c.remoteID.String(), c.holdTime, c.caps
		st.LocalAddress = ipstack.AddrPortOf(c.c.LocalAddr()).Addr().Unmap().String()
	}
	for _, f := range fams {
		st.Families = append(st.Families, f.name)
		st.Received[f.name] = len(p.adjIn[f.rf]) + len(p.adjX[f.rf])
		if p.est != nil {
			st.Advertised[f.name] = p.s.advertisedLocked(p, f)
		}
		if p.eorIn[f.rf] {
			st.EndOfRIB = append(st.EndOfRIB, f.name)
		}
	}
	if st.Families == nil {
		st.Families = []string{}
	}
	for k, v := range p.counters {
		st.Counters[k] = v
	}
	return st
}
