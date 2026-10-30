package dhcp

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const Type4 = "dhcp"

func init() { protocol.Register(type4{}) }

// RawOption is an option to send as given.
type RawOption struct {
	Code  uint8  `json:"code" jsonschema:"minimum=1,maximum=254,description=Option code"`
	Value string `json:"value" jsonschema_description:"Its value: text, or hex bytes as 0x…"`
}

// Config4 is a DHCPv4 client's config.
type Config4 struct {
	ClientID       string      `json:"client_id,omitempty" jsonschema_description:"Client identifier (option 61): text, or hex as 0x…. Default: hardware type 1 and the device's MAC"`
	Hostname       string      `json:"hostname,omitempty" jsonschema:"description=Host name to send (option 12)"`
	VendorClass    string      `json:"vendor_class,omitempty" jsonschema:"description=Vendor class identifier (option 60)"`
	RequestAddress string      `json:"request_address,omitempty" jsonschema:"description=Address to ask for (option 50)"`
	LeaseTime      uint32      `json:"lease_time,omitempty" jsonschema:"description=Lease time to ask for in seconds (option 51)"`
	Broadcast      bool        `json:"broadcast" jsonschema_description:"Ask for broadcast replies (the BOOTP broadcast flag), for servers and relays that can't unicast to a client without an address"`
	RapidCommit    bool        `json:"rapid_commit" jsonschema_description:"Ask for a two-message exchange (RFC 4039): an Ack straight after the Discover"`
	Gateway        bool        `json:"gateway" jsonschema_description:"Use the lease's router (option 3) as the device's IPv4 gateway (a configured gateway4 wins)"`
	Options        []RawOption `json:"options,omitempty" jsonschema:"description=More options to send in every message"`
	ReleaseOnStop  bool        `json:"release_on_stop" jsonschema:"description=Release the lease when the protocol stops or is deleted"`
}

// parseValue reads text, or hex bytes as 0x….
func parseValue(s string) ([]byte, error) {
	if h, ok := strings.CutPrefix(strings.ToLower(s), "0x"); ok {
		b, err := hex.DecodeString(strings.NewReplacer(":", "", " ", "", "-", "").Replace(h))
		if err != nil {
			return nil, fmt.Errorf("%q: bad hex", s)
		}
		return b, nil
	}
	return []byte(s), nil
}

func (c *Config4) Validate() error {
	if _, err := parseValue(c.ClientID); err != nil {
		return fmt.Errorf("client_id %w", err)
	}
	if c.RequestAddress != "" {
		if a, err := netip.ParseAddr(c.RequestAddress); err != nil || !a.Is4() {
			return fmt.Errorf("request_address %q is not an IPv4 address", c.RequestAddress)
		}
	}
	for _, o := range c.Options {
		if o.Code == 0 || o.Code == 255 {
			return errors.New("option codes are 1-254")
		}
		if _, err := parseValue(o.Value); err != nil {
			return fmt.Errorf("option %d %w", o.Code, err)
		}
	}
	return nil
}

type type4 struct{}

func (type4) Name() string { return Type4 }
func (type4) Description() string {
	return "DHCPv4 client: leases an address and gateway for the device, renews and rebinds it"
}
func (type4) NewConfig() any       { return &Config4{Gateway: true, ReleaseOnStop: true} }
func (type4) EtherTypes() []uint16 { return []uint16{pkt.EtherTypeIPv4} }
func (type4) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "renew", Description: "Renew the lease now (unicast to the server)"},
		{Name: "rebind", Description: "Rebind now (broadcast, to any server)"},
		{Name: "release", Description: "Release the lease and stay without one until restart"},
		{Name: "restart", Description: "Release any lease and start again with a Discover"},
		{Name: "clear", Description: "Reset the message counters"},
	}
}

const (
	stInit       = "init"
	stSelecting  = "selecting"
	stRequesting = "requesting"
	stBound      = "bound"
	stRenewing   = "renewing"
	stRebinding  = "rebinding"
	stReleased   = "released"
)

type cmd struct {
	name string
	done chan error
}

type client4 struct {
	env      protocol.Env
	cfg      Config4
	clientID []byte
	extra    []Option4

	rx    chan *Msg4
	cmds  chan cmd
	stop  chan struct{}
	ended chan struct{}

	// Owned by run.
	xid      uint32
	started  time.Time // of the current exchange (secs field)
	retx     time.Time
	bo       backoff
	offer    *Msg4
	release  func()
	attempts int

	mu       sync.Mutex
	state    string
	lease    lease4
	bindTime time.Duration
	lastNak  string
	counters map[string]uint64
}

// lease4 is what a server gave.
type lease4 struct {
	addr    netip.Prefix
	server  netip.Addr
	router  netip.Addr
	dns     []netip.Addr
	domain  string
	secs    uint32
	boundAt time.Time
	t1, t2  time.Time // renewing and rebinding start
	expiry  time.Time
}

func (type4) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config4)
	if env.IP() == nil {
		return nil, errors.New("the device has no IP stack")
	}
	mac := env.Device().MAC()
	cl := &client4{env: env, cfg: cfg, clientID: append([]byte{1}, mac[:]...),
		rx: make(chan *Msg4, 16), cmds: make(chan cmd), stop: make(chan struct{}), ended: make(chan struct{}),
		state: stInit, counters: map[string]uint64{}}
	if cfg.ClientID != "" {
		cl.clientID, _ = parseValue(cfg.ClientID)
	}
	for _, o := range cfg.Options {
		v, _ := parseValue(o.Value)
		cl.extra = append(cl.extra, Option4{o.Code, v})
	}
	go cl.run()
	return cl, nil
}

var msgNames4 = map[uint8]string{MsgDiscover: "discover", MsgOffer: "offer", MsgRequest: "request", MsgDecline: "decline",
	MsgAck: "ack", MsgNak: "nak", MsgRelease: "release", MsgInform: "inform"}

func (c *client4) count(dir string, t uint8) { c.countKey(dir + "_" + msgNames4[t]) }

func (c *client4) countKey(k string) {
	c.mu.Lock()
	c.counters[k]++
	c.mu.Unlock()
}

func (c *client4) HandleRx(f *pkt.Frame) {
	_, _, b, ok := udpOf(f, Port4Client)
	if !ok {
		return
	}
	m, err := Decode4(b)
	if err != nil || m.Op != OpReply || m.CHAddr != c.env.Device().MAC() {
		return
	}
	select {
	case c.rx <- m:
	default:
	}
}

func (c *client4) setState(s string) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
}

func (c *client4) getState() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *client4) run() {
	defer close(c.ended)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	c.discover(time.Now())
	for {
		timer.Reset(time.Until(c.wake()))
		select {
		case <-c.stop:
			if c.cfg.ReleaseOnStop {
				c.sendRelease()
			}
			c.drop()
			return
		case m := <-c.rx:
			c.onMsg(m, time.Now())
		case k := <-c.cmds:
			k.done <- c.onCmd(k.name, time.Now())
		case <-timer.C:
			c.onTimer(time.Now())
		}
	}
}

// wake is when onTimer has something to do.
func (c *client4) wake() time.Time {
	w := time.Now().Add(time.Hour)
	at := func(t time.Time) {
		if !t.IsZero() && t.Before(w) {
			w = t
		}
	}
	l := c.cur()
	switch c.getState() {
	case stInit, stSelecting, stRequesting:
		at(c.retx)
	case stBound:
		at(l.t1)
		at(l.t2)
		at(l.expiry)
	case stRenewing, stRebinding:
		at(c.retx)
		at(l.t2)
		at(l.expiry)
	}
	return w
}

func (c *client4) onTimer(now time.Time) {
	switch st := c.getState(); st {
	case stInit:
		c.discover(now)
	case stSelecting:
		c.send(c.build(MsgDiscover, now), netip.Addr{})
		c.retx = now.Add(c.bo.next())
	case stRequesting:
		if c.attempts++; c.attempts > 4 {
			c.discover(now)
			return
		}
		c.send(c.request(now), netip.Addr{})
		c.retx = now.Add(c.bo.next())
	case stBound, stRenewing, stRebinding:
		l := c.cur()
		switch {
		case !now.Before(l.expiry):
			c.env.Emit("expired", "lease expired: "+l.addr.String(), nil)
			c.drop()
			c.discover(now)
		case !now.Before(l.t2) && st != stRebinding:
			c.rebind(now)
		case !now.Before(l.t1) && st == stBound:
			c.renew(now)
		case st != stBound && !now.Before(c.retx):
			c.retransmitLease(now)
		}
	}
}

func (c *client4) cur() lease4 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lease
}

func (c *client4) leaseAddr() netip.Prefix { return c.cur().addr }

// discover starts a new exchange.
func (c *client4) discover(now time.Time) {
	c.xid, c.started, c.offer, c.attempts = rand.Uint32(), now, nil, 0
	c.bo = backoff{initial: 4 * time.Second, max: 64 * time.Second}
	c.setState(stSelecting)
	c.send(c.build(MsgDiscover, now), netip.Addr{})
	c.retx = now.Add(c.bo.next())
}

func (c *client4) build(t uint8, now time.Time) *Msg4 {
	m := &Msg4{Op: OpRequest, XID: c.xid, CHAddr: c.env.Device().MAC(), Broadcast: c.cfg.Broadcast}
	if t != MsgRelease && t != MsgDecline {
		m.Secs = uint16(min(now.Sub(c.started)/time.Second, 0xffff))
	}
	m.Add(OptMsgType, []byte{t})
	m.Add(OptClientID, c.clientID)
	if t == MsgDiscover || t == MsgRequest {
		if c.cfg.Hostname != "" {
			m.Add(OptHostname, []byte(c.cfg.Hostname))
		}
		if c.cfg.VendorClass != "" {
			m.Add(OptVendorClass, []byte(c.cfg.VendorClass))
		}
		m.Add(OptParamList, []byte{OptSubnetMask, OptRouter, OptDNS, OptDomainName, OptLeaseTime, OptT1, OptT2})
		m.Add(OptMaxSize, binary.BigEndian.AppendUint16(nil, 1500))
		if c.cfg.LeaseTime > 0 {
			m.Add(OptLeaseTime, binary.BigEndian.AppendUint32(nil, c.cfg.LeaseTime))
		}
		m.Options = append(m.Options, c.extra...)
	}
	if t == MsgDiscover {
		if c.cfg.RequestAddress != "" {
			a := netip.MustParseAddr(c.cfg.RequestAddress).As4()
			m.Add(OptRequestedIP, a[:])
		}
		if c.cfg.RapidCommit {
			m.Add(OptRapidCommit, nil)
		}
	}
	return m
}

// request is the selecting-state Request for c.offer.
func (c *client4) request(now time.Time) *Msg4 {
	m := c.build(MsgRequest, now)
	y := c.offer.YIAddr.As4()
	m.Add(OptRequestedIP, y[:])
	s := c.offer.Addr(OptServerID).As4()
	m.Add(OptServerID, s[:])
	return m
}

// send transmits m: broadcast from 0.0.0.0 (to invalid), or unicast from
// the lease's address.
func (c *client4) send(m *Msg4, to netip.Addr) {
	src, dst, mac := netip.IPv4Unspecified(), netip.AddrFrom4([4]byte{255, 255, 255, 255}), pkt.Broadcast
	if m.CIAddr.IsValid() && !m.CIAddr.IsUnspecified() {
		src = m.CIAddr
	}
	if to.IsValid() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, nh, err := c.env.IP().ResolveNextHop(ctx, to)
		cancel()
		if err != nil {
			c.countKey("tx_errors")
			return
		}
		dst, mac = to, nh
	}
	et, b := udpPacket(src, dst, Port4Client, Port4Server, 64, m.Encode())
	if c.env.Send(mac, et, b, protocol.SendOpts{}) == nil {
		c.count("tx", m.Type())
	}
}

func (c *client4) onMsg(m *Msg4, now time.Time) {
	if m.XID != c.xid {
		return
	}
	t := m.Type()
	c.count("rx", t)
	st := c.getState()
	switch {
	case st == stSelecting && t == MsgOffer && m.Addr(OptServerID).IsValid() && m.YIAddr.Is4() && !m.YIAddr.IsUnspecified():
		c.offer = m
		c.setState(stRequesting)
		c.bo = backoff{initial: 4 * time.Second, max: 64 * time.Second}
		c.send(c.request(now), netip.Addr{})
		c.retx = now.Add(c.bo.next())
	case st == stSelecting && t == MsgAck && c.cfg.RapidCommit && m.Has(OptRapidCommit),
		(st == stRequesting || st == stRenewing || st == stRebinding) && t == MsgAck:
		c.bind(m, now, st)
	case (st == stRequesting || st == stRenewing || st == stRebinding) && t == MsgNak:
		msg := string(m.Get(OptMessage))
		c.mu.Lock()
		c.lastNak = msg
		c.mu.Unlock()
		c.env.Emit("nak", strings.TrimSpace("NAK from "+m.Addr(OptServerID).String()+" "+msg), nil)
		c.drop()
		c.setState(stInit)
		c.retx = now.Add(time.Second)
	}
}

func classful(a netip.Addr) int {
	switch x := a.As4()[0]; {
	case x < 128:
		return 8
	case x < 192:
		return 16
	}
	return 24
}

// bind takes an Ack.
func (c *client4) bind(m *Msg4, now time.Time, from string) {
	bits := classful(m.YIAddr)
	if mask := m.Addr(OptSubnetMask); mask.IsValid() {
		x := mask.As4()
		bits = 0
		for v := binary.BigEndian.Uint32(x[:]); v&0x80000000 != 0; v <<= 1 {
			bits++
		}
	}
	l := lease4{addr: netip.PrefixFrom(m.YIAddr, bits), server: m.Addr(OptServerID), dns: m.Addrs(OptDNS),
		domain: string(m.Get(OptDomainName)), boundAt: now}
	if !l.server.IsValid() {
		l.server = c.leaseServer()
	}
	if rs := m.Addrs(OptRouter); len(rs) > 0 {
		l.router = rs[0]
	}
	l.secs, _ = m.Uint32(OptLeaseTime)
	if l.secs == 0 {
		l.secs = 3600
	}
	d := lifetime(l.secs)
	t1, t2 := d/2, d*7/8
	if v, ok := m.Uint32(OptT1); ok {
		t1 = lifetime(v)
	}
	if v, ok := m.Uint32(OptT2); ok {
		t2 = lifetime(v)
	}
	l.t1, l.t2, l.expiry = now.Add(t1), now.Add(t2), now.Add(d)
	old := c.cur()
	c.mu.Lock()
	c.lease = l
	if from != stRenewing && from != stRebinding {
		c.bindTime = now.Sub(c.started)
	}
	c.state = stBound
	c.mu.Unlock()
	if old.addr != l.addr || old.router != l.router || c.release == nil {
		if c.release != nil {
			c.release()
		}
		gw := netip.Addr{}
		if c.cfg.Gateway {
			gw = l.router
		}
		c.release = c.env.IP().AddAddress(l.addr, gw)
		c.env.Emit("bound", fmt.Sprintf("%s from %s, lease %ds", l.addr, l.server, l.secs), map[string]any{"address": l.addr.String(), "server": l.server.String()})
	} else {
		c.env.Emit("renewed", fmt.Sprintf("%s, lease %ds", l.addr, l.secs), nil)
	}
}

func (c *client4) leaseServer() netip.Addr { return c.cur().server }

// drop removes the lease's address from the device.
func (c *client4) drop() {
	if c.release != nil {
		c.release()
		c.release = nil
	}
	c.mu.Lock()
	c.lease = lease4{}
	c.mu.Unlock()
}

func (c *client4) renew(now time.Time) {
	c.xid, c.started = rand.Uint32(), now
	c.setState(stRenewing)
	c.bo = backoff{initial: 4 * time.Second, max: 64 * time.Second}
	c.retransmitLease(now)
}

func (c *client4) rebind(now time.Time) {
	c.xid, c.started = rand.Uint32(), now
	c.setState(stRebinding)
	c.bo = backoff{initial: 4 * time.Second, max: 64 * time.Second}
	c.retransmitLease(now)
}

// retransmitLease sends the Request of the renewing (unicast) or
// rebinding (broadcast) state.
func (c *client4) retransmitLease(now time.Time) {
	m := c.build(MsgRequest, now)
	m.CIAddr = c.leaseAddr().Addr()
	to := netip.Addr{}
	if c.getState() == stRenewing {
		to = c.leaseServer()
	}
	c.send(m, to)
	c.retx = now.Add(c.bo.next())
}

func (c *client4) sendRelease() {
	l := c.leaseAddr()
	if !l.IsValid() {
		return
	}
	c.xid = rand.Uint32()
	m := c.build(MsgRelease, time.Now())
	m.CIAddr = l.Addr()
	s := c.leaseServer().As4()
	m.Add(OptServerID, s[:])
	c.send(m, c.leaseServer())
	c.env.Emit("released", "released "+l.String(), nil)
}

func (c *client4) onCmd(name string, now time.Time) error {
	st := c.getState()
	bound := st == stBound || st == stRenewing || st == stRebinding
	switch name {
	case "renew", "rebind":
		if !bound {
			return fmt.Errorf("no lease to %s (%s)", name, st)
		}
		if name == "renew" {
			c.renew(now)
		} else {
			c.rebind(now)
		}
	case "release":
		if !bound {
			return fmt.Errorf("no lease to release (%s)", st)
		}
		c.sendRelease()
		c.drop()
		c.setState(stReleased)
	case "restart":
		if bound {
			c.sendRelease()
		}
		c.drop()
		c.discover(now)
	case "clear":
		c.mu.Lock()
		c.counters = map[string]uint64{}
		c.mu.Unlock()
	default:
		return fmt.Errorf("unknown action %q", name)
	}
	return nil
}

func (c *client4) Action(ctx context.Context, name string, _ any) (any, error) {
	k := cmd{name, make(chan error, 1)}
	select {
	case c.cmds <- k:
	case <-c.ended:
		return nil, errors.New("stopped")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := <-k.done; err != nil {
		return nil, err
	}
	return c.Status(), nil
}

// Status4 is a DHCPv4 client's status.
type Status4 struct {
	State     string            `json:"state"`
	Address   string            `json:"address,omitempty"`
	Gateway   string            `json:"gateway,omitempty"`
	Server    string            `json:"server,omitempty"`
	DNS       []string          `json:"dns,omitempty"`
	Domain    string            `json:"domain,omitempty"`
	LeaseS    uint32            `json:"lease_s,omitempty"`
	RenewIn   float64           `json:"renew_in_s,omitempty"`
	RebindIn  float64           `json:"rebind_in_s,omitempty"`
	ExpiresIn float64           `json:"expires_in_s,omitempty"`
	BindTime  float64           `json:"bind_time_s,omitempty"`
	LastNak   string            `json:"last_nak,omitempty"`
	Counters  map[string]uint64 `json:"counters"`
}

func (c *client4) Status() any {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status4{State: c.state, LastNak: c.lastNak, Counters: map[string]uint64{}}
	for k, v := range c.counters {
		st.Counters[k] = v
	}
	if l := c.lease; l.addr.IsValid() {
		now := time.Now()
		st.Address, st.Server, st.Domain, st.LeaseS = l.addr.String(), l.server.String(), l.domain, l.secs
		if l.router.IsValid() {
			st.Gateway = l.router.String()
		}
		for _, d := range l.dns {
			st.DNS = append(st.DNS, d.String())
		}
		st.BindTime = secs(c.bindTime)
		st.RenewIn, st.RebindIn, st.ExpiresIn = max(0, secs(l.t1.Sub(now))), max(0, secs(l.t2.Sub(now))), max(0, secs(l.expiry.Sub(now)))
	}
	return st
}

// Health implements protocol.Healther.
func (c *client4) Health() string {
	switch s := c.getState(); s {
	case stBound, stRenewing, stRebinding:
		return "bound"
	case stReleased:
		return s
	}
	return "waiting"
}

func (c *client4) Stop() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	<-c.ended
}
