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

const Type6 = "dhcpv6"

func init() { protocol.Register(type6{}) }

// PDAddress is an address in the delegated prefix.
type PDAddress struct {
	Subnet uint64 `json:"subnet" jsonschema_description:"Which /64 of the delegated prefix: 0 is the first (for a prefix longer than /64, 0)"`
	Host   uint64 `json:"host" jsonschema:"minimum=1,description=The interface ID in that /64"`
}

// RawOption6 is a DHCPv6 option to send as given.
type RawOption6 struct {
	Code  uint16 `json:"code" jsonschema:"minimum=1,description=Option code"`
	Value string `json:"value" jsonschema_description:"Its value: text, or hex bytes as 0x…"`
}

// Config6 is a DHCPv6 client's config.
type Config6 struct {
	IANA          bool         `json:"ia_na" jsonschema:"description=Ask for an address (IA_NA)"`
	IAPD          bool         `json:"ia_pd" jsonschema:"description=Ask for a delegated prefix (IA_PD)"`
	PDLength      uint8        `json:"pd_length,omitempty" jsonschema:"maximum=128,description=Prefix length to ask for (a hint: 0 for none)"`
	PDAddresses   []PDAddress  `json:"pd_addresses,omitempty" jsonschema_description:"Addresses in the delegated prefix that the device answers for (ping and flows): hosts behind the CPE to test routing to the prefix"`
	RA            bool         `json:"ra" jsonschema_description:"Solicit and follow router advertisements: they give the default gateway (DHCPv6 doesn't) and the on-link prefix of the leased address"`
	RapidCommit   bool         `json:"rapid_commit" jsonschema_description:"Ask for a two-message exchange: a Reply straight after the Solicit"`
	DUID          string       `json:"duid,omitempty" jsonschema_description:"Client DUID in hex (0x…). Default: a DUID-LL from the device's MAC"`
	Options       []RawOption6 `json:"options,omitempty" jsonschema:"description=More options to send in every message"`
	ReleaseOnStop bool         `json:"release_on_stop" jsonschema:"description=Release the bindings when the protocol stops or is deleted"`
}

func (c *Config6) Validate() error {
	if !c.IANA && !c.IAPD {
		return errors.New("ask for an address (ia_na), a prefix (ia_pd) or both")
	}
	if len(c.PDAddresses) > 0 && !c.IAPD {
		return errors.New("pd_addresses need ia_pd")
	}
	if c.PDLength > 128 {
		return errors.New("pd_length is 0-128")
	}
	for _, a := range c.PDAddresses {
		if a.Host == 0 {
			return errors.New("pd_addresses: host must be at least 1")
		}
	}
	if c.DUID != "" {
		if b, err := parseValue(c.DUID); err != nil || !strings.HasPrefix(strings.ToLower(c.DUID), "0x") || len(b) < 2 {
			return fmt.Errorf("duid %q: want hex bytes as 0x…", c.DUID)
		}
	}
	for _, o := range c.Options {
		if o.Code == 0 {
			return errors.New("option codes start at 1")
		}
		if _, err := parseValue(o.Value); err != nil {
			return fmt.Errorf("option %d %w", o.Code, err)
		}
	}
	return nil
}

type type6 struct{}

func (type6) Name() string { return Type6 }
func (type6) Description() string {
	return "DHCPv6 client: leases an address and a delegated prefix, with the default gateway from router advertisements"
}
func (type6) NewConfig() any       { return &Config6{IANA: true, RA: true, ReleaseOnStop: true} }
func (type6) EtherTypes() []uint16 { return []uint16{pkt.EtherTypeIPv6} }
func (type6) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "renew", Description: "Renew the bindings now"},
		{Name: "rebind", Description: "Rebind now (to any server)"},
		{Name: "release", Description: "Release the bindings and stay without them until restart"},
		{Name: "restart", Description: "Release any bindings and start again with a Solicit"},
		{Name: "clear", Description: "Reset the message counters"},
	}
}

const (
	stSoliciting = "soliciting"
	iaidNA       = 1
	iaidPD       = 1
)

var (
	allRouters = netip.MustParseAddr("ff02::2")
	msgNames6  = map[uint8]string{MsgSolicit: "solicit", MsgAdvertise: "advertise", MsgRequest6: "request", MsgConfirm: "confirm",
		MsgRenew: "renew", MsgRebind: "rebind", MsgReply: "reply", MsgRelease6: "release", MsgDecline6: "decline", MsgReconfigure: "reconfigure"}
)

// binding is a leased address or prefix, with its lifetimes' ends.
type binding struct {
	prefix               netip.Prefix // an address as /128
	preferred, valid     uint32
	preferredAt, validAt time.Time
}

// ra is what a router advertisement said.
type ra struct {
	router         netip.Addr
	lifetime       time.Duration
	managed, other bool
	onLink         []netip.Prefix
}

type client6 struct {
	env      protocol.Env
	cfg      Config6
	duid     []byte
	extra    []Option6
	ll       netip.Addr
	releaseL func()

	rx    chan any // *Msg6 or ra
	cmds  chan cmd
	stop  chan struct{}
	ended chan struct{}

	// Owned by run.
	xid       uint32
	started   time.Time // of the current exchange (elapsed time option)
	solicited time.Time // of the first Solicit, for the bind time
	retx      time.Time
	bo        backoff
	attempts  int
	collectTo time.Time // end of the first Solicit round
	best      *Msg6
	bestPref  int
	rsLeft    int
	rsAt      time.Time
	routerAt  time.Time // when the router's lifetime ends
	gwRelease func()
	gwRouter  netip.Addr
	naRelease map[netip.Addr]func()
	naPrefix  map[netip.Addr]netip.Prefix
	pdRelease []func()

	mu       sync.Mutex
	state    string
	server   []byte
	addrs    []binding
	prefixes []binding
	pdAddrs  []netip.Addr
	ra       *ra
	dns      []netip.Addr
	domains  []string
	naStatus string
	pdStatus string
	t1, t2   time.Time
	bindTime time.Duration
	counters map[string]uint64
}

func (type6) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config6)
	if env.IP() == nil {
		return nil, errors.New("the device has no IP stack")
	}
	mac := env.Device().MAC()
	cl := &client6{env: env, cfg: cfg, duid: DUIDLL(mac), ll: pkt.LinkLocal(mac),
		rx: make(chan any, 16), cmds: make(chan cmd), stop: make(chan struct{}), ended: make(chan struct{}),
		naRelease: map[netip.Addr]func(){}, naPrefix: map[netip.Addr]netip.Prefix{},
		state: stInit, counters: map[string]uint64{}}
	if cfg.DUID != "" {
		cl.duid, _ = parseValue(cfg.DUID)
	}
	for _, o := range cfg.Options {
		v, _ := parseValue(o.Value)
		cl.extra = append(cl.extra, Option6{o.Code, v})
	}
	// The link-local address DHCPv6 and router solicitations go from, and
	// replies come back to.
	cl.releaseL = env.IP().AddAddress(netip.PrefixFrom(cl.ll, 64), netip.Addr{})
	go cl.run()
	return cl, nil
}

func (c *client6) countKey(k string) {
	c.mu.Lock()
	c.counters[k]++
	c.mu.Unlock()
}

func (c *client6) HandleRx(f *pkt.Frame) {
	if f.EtherType != pkt.EtherTypeIPv6 {
		return
	}
	if r, ok := c.parseRA(f); ok {
		select {
		case c.rx <- r:
		default:
		}
		return
	}
	_, _, b, ok := udpOf(f, Port6Client)
	if !ok {
		return
	}
	m, err := Decode6(b)
	if err != nil || string(m.Get(Opt6ClientID)) != string(c.duid) {
		return
	}
	select {
	case c.rx <- m:
	default:
	}
}

func (c *client6) parseRA(f *pkt.Frame) (ra, bool) {
	h, err := pkt.DecodeIPv6(f.Payload)
	if err != nil || h.NextHeader != pkt.ProtoICMPv6 || h.HopLimit != 255 || !h.Src.IsLinkLocalUnicast() {
		return ra{}, false
	}
	m := h.Payload
	if len(m) < 16 || m[0] != pkt.ICMPv6RouterAdv || m[1] != 0 || !pkt.ICMPv6Valid(h.Src, h.Dst, m) {
		return ra{}, false
	}
	r := ra{router: h.Src, managed: m[5]&0x80 != 0, other: m[5]&0x40 != 0,
		lifetime: time.Duration(binary.BigEndian.Uint16(m[6:])) * time.Second}
	for o := m[16:]; len(o) >= 8; {
		n := int(o[1]) * 8
		if n == 0 || n > len(o) {
			break
		}
		// Prefix information with the on-link flag and a valid lifetime.
		if o[0] == 3 && n == 32 && o[3]&0x80 != 0 && binary.BigEndian.Uint32(o[4:]) != 0 && o[2] <= 128 {
			r.onLink = append(r.onLink, netip.PrefixFrom(netip.AddrFrom16([16]byte(o[16:32])), int(o[2])).Masked())
		}
		o = o[n:]
	}
	return r, true
}

func (c *client6) setState(s string) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
}

func (c *client6) getState() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *client6) run() {
	defer close(c.ended)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	now := time.Now()
	if c.cfg.RA {
		c.rsLeft, c.rsAt = 3, now
	}
	c.solicit(now)
	for {
		timer.Reset(time.Until(c.wake()))
		select {
		case <-c.stop:
			if c.cfg.ReleaseOnStop {
				c.sendRelease()
			}
			c.dropAll()
			c.dropGateway()
			c.releaseL()
			return
		case x := <-c.rx:
			now := time.Now()
			switch v := x.(type) {
			case ra:
				c.onRA(v, now)
			case *Msg6:
				c.onMsg(v, now)
			}
		case k := <-c.cmds:
			k.done <- c.onCmd(k.name, time.Now())
		case <-timer.C:
			c.onTimer(time.Now())
		}
	}
}

func (c *client6) wake() time.Time {
	w := time.Now().Add(time.Hour)
	at := func(t time.Time) {
		if !t.IsZero() && t.Before(w) {
			w = t
		}
	}
	if c.rsLeft > 0 {
		at(c.rsAt)
	}
	at(c.routerAt)
	at(c.retx)
	at(c.collectTo)
	c.mu.Lock()
	if c.state == stBound || c.state == stRenewing || c.state == stRebinding {
		if c.state == stBound {
			at(c.t1)
		}
		if c.state != stRebinding {
			at(c.t2)
		}
		for _, b := range append(append([]binding(nil), c.addrs...), c.prefixes...) {
			at(b.validAt)
		}
	}
	c.mu.Unlock()
	return w
}

func (c *client6) onTimer(now time.Time) {
	if c.rsLeft > 0 && !now.Before(c.rsAt) {
		c.sendRS()
		c.rsLeft--
		c.rsAt = now.Add(4 * time.Second)
	}
	if !c.routerAt.IsZero() && !now.Before(c.routerAt) {
		c.env.Emit("router-expired", "router "+c.gwRouter.String()+" expired", nil)
		c.dropGateway()
		c.mu.Lock()
		c.ra = nil
		c.mu.Unlock()
	}
	c.expire(now)
	st := c.getState()
	switch {
	case st == stSoliciting && !c.collectTo.IsZero() && !now.Before(c.collectTo):
		c.collectTo = time.Time{}
		if c.best != nil {
			c.request(now)
		}
	case (st == stSoliciting || st == stInit) && !c.retx.IsZero() && !now.Before(c.retx):
		if st == stInit {
			c.solicit(now)
		} else {
			c.send(c.build(MsgSolicit, now))
			c.retx = now.Add(c.bo.next())
		}
	case st == stRequesting && !now.Before(c.retx):
		if c.attempts++; c.attempts >= 10 {
			c.solicit(now)
			return
		}
		c.send(c.requestMsg(MsgRequest6, now))
		c.retx = now.Add(c.bo.next())
	case st == stBound:
		c.mu.Lock()
		t1, t2 := c.t1, c.t2
		c.mu.Unlock()
		if !now.Before(t2) {
			c.extend(MsgRebind, now)
		} else if !now.Before(t1) {
			c.extend(MsgRenew, now)
		}
	case st == stRenewing || st == stRebinding:
		c.mu.Lock()
		t2 := c.t2
		c.mu.Unlock()
		if st == stRenewing && !now.Before(t2) {
			c.extend(MsgRebind, now)
		} else if !now.Before(c.retx) {
			c.send(c.requestMsg(map[string]uint8{stRenewing: MsgRenew, stRebinding: MsgRebind}[st], now))
			c.retx = now.Add(c.bo.next())
		}
	}
}

// expire drops bindings whose valid lifetime ended; with none left, it
// starts over.
func (c *client6) expire(now time.Time) {
	c.mu.Lock()
	if c.state != stBound && c.state != stRenewing && c.state != stRebinding {
		c.mu.Unlock()
		return
	}
	keep := func(bs []binding) ([]binding, []binding) {
		var live, gone []binding
		for _, b := range bs {
			if now.Before(b.validAt) {
				live = append(live, b)
			} else {
				gone = append(gone, b)
			}
		}
		return live, gone
	}
	var goneA, goneP []binding
	c.addrs, goneA = keep(c.addrs)
	c.prefixes, goneP = keep(c.prefixes)
	empty := len(c.addrs) == 0 && len(c.prefixes) == 0
	c.mu.Unlock()
	for _, b := range goneA {
		c.env.Emit("expired", "address "+b.prefix.Addr().String()+" expired", nil)
	}
	for _, b := range goneP {
		c.env.Emit("expired", "prefix "+b.prefix.String()+" expired", nil)
	}
	if len(goneA)+len(goneP) > 0 {
		c.apply()
	}
	if empty {
		c.solicit(now)
	}
}

func (c *client6) sendRS() {
	m := []byte{pkt.ICMPv6RouterSol, 0, 0, 0, 0, 0, 0, 0, pkt.NDOptSourceLLAddr, 1}
	mac := c.env.Device().MAC()
	m = append(m, mac[:]...)
	pkt.ICMPv6Checksum(c.ll, allRouters, m)
	h := pkt.IPv6{NextHeader: pkt.ProtoICMPv6, HopLimit: 255, Src: c.ll, Dst: allRouters, Payload: m}
	if c.env.Send(pkt.MulticastMAC(allRouters), pkt.EtherTypeIPv6, h.Encode(), protocol.SendOpts{}) == nil {
		c.countKey("tx_rs")
	}
}

func (c *client6) onRA(r ra, now time.Time) {
	c.countKey("rx_ra")
	if !c.cfg.RA || c.gwRouter.IsValid() && r.router != c.gwRouter {
		return // one router is enough
	}
	c.rsLeft = 0
	c.mu.Lock()
	c.ra = &r
	c.mu.Unlock()
	if r.lifetime == 0 {
		if c.gwRouter.IsValid() {
			c.env.Emit("router-gone", "router "+r.router.String()+" stopped being a default router", nil)
		}
		c.dropGateway()
	} else {
		if !c.gwRouter.IsValid() {
			c.gwRelease = c.env.IP().AddAddress(netip.Prefix{}, r.router)
			c.gwRouter = r.router
			c.env.Emit("router", "default router "+r.router.String(), nil)
		}
		c.routerAt = now.Add(r.lifetime)
	}
	c.apply()
}

func (c *client6) dropGateway() {
	if c.gwRelease != nil {
		c.gwRelease()
	}
	c.gwRelease, c.gwRouter, c.routerAt = nil, netip.Addr{}, time.Time{}
}

// solicit starts a new exchange.
func (c *client6) solicit(now time.Time) {
	c.xid, c.started, c.solicited, c.best, c.bestPref, c.attempts = rand.Uint32()&0xffffff, now, now, nil, -1, 0
	c.bo = backoff{initial: time.Second, max: 120 * time.Second}
	c.setState(stSoliciting)
	c.send(c.build(MsgSolicit, now))
	c.retx = now.Add(c.bo.next())
	c.collectTo = c.retx
}

func (c *client6) build(t uint8, now time.Time) *Msg6 {
	m := &Msg6{Type: t, XID: c.xid}
	m.Add(Opt6ClientID, c.duid)
	el := uint16(min(now.Sub(c.started)/(10*time.Millisecond), 0xffff))
	m.Add(Opt6ElapsedTime, binary.BigEndian.AppendUint16(nil, el))
	if t != MsgRelease6 && t != MsgDecline6 {
		m.Add(Opt6ORO, []byte{0, byte(Opt6DNS), 0, byte(Opt6DomainList)})
	}
	if t == MsgSolicit {
		if c.cfg.IANA {
			m.Options = append(m.Options, IA{Kind: Opt6IANA, IAID: iaidNA}.Encode())
		}
		if c.cfg.IAPD {
			pd := IA{Kind: Opt6IAPD, IAID: iaidPD}
			if c.cfg.PDLength > 0 {
				pd.Prefixes = []IAPrefix{{Prefix: netip.PrefixFrom(netip.IPv6Unspecified(), int(c.cfg.PDLength))}}
			}
			m.Options = append(m.Options, pd.Encode())
		}
		if c.cfg.RapidCommit {
			m.Add(Opt6RapidCommit, nil)
		}
	}
	m.Options = append(m.Options, c.extra...)
	return m
}

// requestMsg is a Request (for the best Advertise), Renew, Rebind or
// Release (for the bindings).
func (c *client6) requestMsg(t uint8, now time.Time) *Msg6 {
	m := c.build(t, now)
	c.mu.Lock()
	server := c.server
	addrs, prefixes := c.addrs, c.prefixes
	c.mu.Unlock()
	if t == MsgRequest6 {
		server = c.best.Get(Opt6ServerID)
		addrs, prefixes = nil, nil
		for _, ia := range c.best.IAs() {
			for _, a := range ia.Addrs {
				addrs = append(addrs, binding{prefix: netip.PrefixFrom(a.Addr, 128)})
			}
			for _, p := range ia.Prefixes {
				prefixes = append(prefixes, binding{prefix: p.Prefix})
			}
		}
	}
	if t != MsgRebind {
		m.Add(Opt6ServerID, server)
	}
	if c.cfg.IANA {
		ia := IA{Kind: Opt6IANA, IAID: iaidNA}
		for _, a := range addrs {
			ia.Addrs = append(ia.Addrs, IAAddr{Addr: a.prefix.Addr()})
		}
		m.Options = append(m.Options, ia.Encode())
	}
	if c.cfg.IAPD {
		ia := IA{Kind: Opt6IAPD, IAID: iaidPD}
		for _, p := range prefixes {
			ia.Prefixes = append(ia.Prefixes, IAPrefix{Prefix: p.prefix})
		}
		if len(ia.Prefixes) == 0 && c.cfg.PDLength > 0 {
			ia.Prefixes = []IAPrefix{{Prefix: netip.PrefixFrom(netip.IPv6Unspecified(), int(c.cfg.PDLength))}}
		}
		m.Options = append(m.Options, ia.Encode())
	}
	return m
}

func (c *client6) send(m *Msg6) {
	et, b := udpPacket(c.ll, AllServers, Port6Client, Port6Server, 1, m.Encode())
	if c.env.Send(pkt.MulticastMAC(AllServers), et, b, protocol.SendOpts{}) == nil {
		c.countKey("tx_" + msgNames6[m.Type])
	}
}

func (c *client6) request(now time.Time) {
	c.xid, c.started, c.attempts = rand.Uint32()&0xffffff, now, 0
	c.bo = backoff{initial: time.Second, max: 30 * time.Second}
	c.setState(stRequesting)
	c.send(c.requestMsg(MsgRequest6, now))
	c.retx = now.Add(c.bo.next())
}

func (c *client6) extend(t uint8, now time.Time) {
	c.xid, c.started = rand.Uint32()&0xffffff, now
	c.bo = backoff{initial: 10 * time.Second, max: 600 * time.Second}
	c.setState(map[uint8]string{MsgRenew: stRenewing, MsgRebind: stRebinding}[t])
	c.send(c.requestMsg(t, now))
	c.retx = now.Add(c.bo.next())
}

// usable reports whether an Advertise or Reply has something for us.
func usable(m *Msg6) bool {
	if s := StatusOf(m.Options); s != nil && s.Code != StatusSuccess {
		return false
	}
	for _, ia := range m.IAs() {
		if len(ia.Addrs) > 0 || len(ia.Prefixes) > 0 {
			return true
		}
	}
	return false
}

func (c *client6) onMsg(m *Msg6, now time.Time) {
	c.countKey("rx_" + msgNames6[m.Type])
	if m.XID != c.xid || m.Get(Opt6ServerID) == nil {
		return
	}
	st := c.getState()
	switch {
	case st == stSoliciting && m.Type == MsgAdvertise && usable(m):
		pref := 0
		if p := m.Get(Opt6Preference); len(p) == 1 {
			pref = int(p[0])
		}
		if pref > c.bestPref {
			c.best, c.bestPref = m, pref
		}
		if pref == 255 || c.collectTo.IsZero() {
			c.collectTo = time.Time{}
			c.request(now)
		}
	case st == stSoliciting && m.Type == MsgReply && c.cfg.RapidCommit && m.Has(Opt6RapidCommit),
		(st == stRequesting || st == stRenewing || st == stRebinding) && m.Type == MsgReply:
		c.bind(m, now, st)
	}
}

func iaStatus(ia IA, found bool, what string) string {
	switch {
	case !found:
		return "no " + what + " in the reply"
	case ia.Status != nil && ia.Status.Code != StatusSuccess:
		return strings.TrimSpace(statusName(ia.Status.Code) + " " + ia.Status.Message)
	}
	return ""
}

func statusName(code uint16) string {
	if n, ok := map[uint16]string{StatusUnspecFail: "UnspecFail", StatusNoAddrsAvail: "NoAddrsAvail", StatusNoBinding: "NoBinding",
		StatusNotOnLink: "NotOnLink", StatusUseMulticast: "UseMulticast", StatusNoPrefixAvail: "NoPrefixAvail"}[code]; ok {
		return n
	}
	return fmt.Sprintf("status %d", code)
}

// bind takes a Reply.
func (c *client6) bind(m *Msg6, now time.Time, from string) {
	if s := StatusOf(m.Options); s != nil && s.Code != StatusSuccess {
		c.env.Emit("refused", strings.TrimSpace("reply: "+statusName(s.Code)+" "+s.Message), nil)
		if from == stRequesting {
			c.setState(stInit)
			c.retx = now.Add(c.bo.next())
		}
		return
	}
	var addrs, prefixes []binding
	var na, pd IA
	var hasNA, hasPD bool
	var t1, t2 uint32
	tmin := func(x, y uint32) uint32 {
		if x == 0 || y != 0 && y < x {
			return y
		}
		return x
	}
	for _, ia := range m.IAs() {
		if ia.Kind == Opt6IANA && ia.IAID == iaidNA && c.cfg.IANA {
			na, hasNA = ia, true
			for _, a := range ia.Addrs {
				if a.Valid > 0 {
					addrs = append(addrs, binding{prefix: netip.PrefixFrom(a.Addr, 128), preferred: a.Preferred, valid: a.Valid,
						preferredAt: now.Add(lifetime(a.Preferred)), validAt: now.Add(lifetime(a.Valid))})
				}
			}
		}
		if ia.Kind == Opt6IAPD && ia.IAID == iaidPD && c.cfg.IAPD {
			pd, hasPD = ia, true
			for _, p := range ia.Prefixes {
				if p.Valid > 0 {
					prefixes = append(prefixes, binding{prefix: p.Prefix, preferred: p.Preferred, valid: p.Valid,
						preferredAt: now.Add(lifetime(p.Preferred)), validAt: now.Add(lifetime(p.Valid))})
				}
			}
		}
		if len(ia.Addrs)+len(ia.Prefixes) > 0 {
			t1, t2 = tmin(t1, ia.T1), tmin(t2, ia.T2)
		}
	}
	if len(addrs)+len(prefixes) == 0 {
		c.env.Emit("refused", "nothing bound: "+strings.Trim(iaStatus(na, hasNA, "address")+"; "+iaStatus(pd, hasPD, "prefix"), "; "), nil)
		if from == stRequesting {
			c.setState(stInit)
			c.retx = now.Add(c.bo.next())
		}
		return
	}
	// Without T1/T2, renew at half and rebind at 0.8 of the shortest
	// preferred lifetime; either way before the shortest valid one ends.
	short, valid := uint32(0), uint32(0)
	for _, b := range append(append([]binding(nil), addrs...), prefixes...) {
		short, valid = tmin(short, b.preferred), tmin(valid, b.valid)
	}
	if short == 0 {
		short = valid
	}
	t1d, t2d := lifetime(t1), lifetime(t2)
	if t1 == 0 {
		t1d = lifetime(short) / 2
	}
	if t2 == 0 {
		t2d = lifetime(short) * 4 / 5
	}
	t1d, t2d = saneTimes(t1d, t2d, lifetime(valid))
	var dns []netip.Addr
	for b := m.Get(Opt6DNS); len(b) >= 16; b = b[16:] {
		dns = append(dns, netip.AddrFrom16([16]byte(b[:16])))
	}
	c.mu.Lock()
	c.server = m.Get(Opt6ServerID)
	c.addrs, c.prefixes, c.dns, c.domains = addrs, prefixes, dns, domainList(m.Get(Opt6DomainList))
	c.naStatus, c.pdStatus = "", ""
	if c.cfg.IANA && len(addrs) == 0 {
		c.naStatus = iaStatus(na, hasNA, "address")
	}
	if c.cfg.IAPD && len(prefixes) == 0 {
		c.pdStatus = iaStatus(pd, hasPD, "prefix")
	}
	c.t1, c.t2 = now.Add(t1d), now.Add(t2d)
	if from != stRenewing && from != stRebinding {
		c.bindTime = now.Sub(c.solicited)
	}
	c.state = stBound
	c.mu.Unlock()
	c.retx = time.Time{}
	var got []string
	for _, b := range addrs {
		got = append(got, b.prefix.Addr().String())
	}
	for _, b := range prefixes {
		got = append(got, b.prefix.String())
	}
	kind := "bound"
	if from == stRenewing || from == stRebinding {
		kind = "renewed"
	}
	c.env.Emit(kind, strings.Join(got, ", "), nil)
	c.apply()
}

// apply puts the bindings on the device: the addresses (with the on-link
// prefix an RA gave, else as /128) and the delegated prefixes' addresses.
func (c *client6) apply() {
	c.mu.Lock()
	addrs := append([]binding(nil), c.addrs...)
	prefixes := append([]binding(nil), c.prefixes...)
	var onLink []netip.Prefix
	if c.ra != nil {
		onLink = c.ra.onLink
	}
	c.mu.Unlock()
	want := map[netip.Addr]netip.Prefix{}
	for _, b := range addrs {
		a, p := b.prefix.Addr(), b.prefix
		for _, o := range onLink {
			if o.Contains(a) {
				p = netip.PrefixFrom(a, o.Bits())
			}
		}
		want[a] = p
	}
	for a, rel := range c.naRelease {
		if want[a] != c.naPrefix[a] {
			rel()
			delete(c.naRelease, a)
			delete(c.naPrefix, a)
		}
	}
	for a, p := range want {
		if c.naRelease[a] == nil {
			c.naRelease[a], c.naPrefix[a] = c.env.IP().AddAddress(p, netip.Addr{}), p
		}
	}
	var pdAddrs []netip.Addr
	for _, b := range prefixes {
		for _, x := range c.cfg.PDAddresses {
			if a, ok := pdAddress(b.prefix, x); ok {
				pdAddrs = append(pdAddrs, a)
			}
		}
	}
	c.mu.Lock()
	same := fmt.Sprint(pdAddrs) == fmt.Sprint(c.pdAddrs)
	c.pdAddrs = pdAddrs
	c.mu.Unlock()
	if !same {
		for _, rel := range c.pdRelease {
			rel()
		}
		c.pdRelease = nil
		for _, a := range pdAddrs {
			c.pdRelease = append(c.pdRelease, c.env.IP().AddLocal(a))
		}
	}
}

// pdAddress is subnet x.Subnet's host x.Host in a delegated prefix.
func pdAddress(p netip.Prefix, x PDAddress) (netip.Addr, bool) {
	b := p.Masked().Addr().As16()
	hi, lo := binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
	if p.Bits() <= 64 {
		if p.Bits() > 0 && 64-p.Bits() < 64 && x.Subnet>>(64-p.Bits()) != 0 {
			return netip.Addr{}, false
		}
		hi |= x.Subnet
		lo |= x.Host
	} else {
		if x.Subnet != 0 || 128-p.Bits() < 64 && x.Host>>(128-p.Bits()) != 0 {
			return netip.Addr{}, false
		}
		lo |= x.Host
	}
	binary.BigEndian.PutUint64(b[:8], hi)
	binary.BigEndian.PutUint64(b[8:], lo)
	return netip.AddrFrom16(b), true
}

// domainList decodes a list of DNS names in wire format.
func domainList(b []byte) []string {
	var out []string
	var labels []string
	for len(b) > 0 {
		n := int(b[0])
		if n == 0 {
			if len(labels) > 0 {
				out = append(out, strings.Join(labels, "."))
			}
			labels, b = nil, b[1:]
			continue
		}
		if n > 63 || len(b) < 1+n {
			break
		}
		labels, b = append(labels, string(b[1:1+n])), b[1+n:]
	}
	return out
}

// dropAll removes the bindings from the device.
func (c *client6) dropAll() {
	c.mu.Lock()
	c.addrs, c.prefixes, c.server = nil, nil, nil
	c.mu.Unlock()
	c.apply()
}

func (c *client6) sendRelease() {
	c.mu.Lock()
	n := len(c.addrs) + len(c.prefixes)
	c.mu.Unlock()
	if n == 0 {
		return
	}
	c.xid, c.started = rand.Uint32()&0xffffff, time.Now()
	c.send(c.requestMsg(MsgRelease6, time.Now()))
	c.env.Emit("released", "released the bindings", nil)
}

func (c *client6) onCmd(name string, now time.Time) error {
	st := c.getState()
	bound := st == stBound || st == stRenewing || st == stRebinding
	switch name {
	case "renew", "rebind":
		if !bound {
			return fmt.Errorf("nothing to %s (%s)", name, st)
		}
		c.extend(map[string]uint8{"renew": MsgRenew, "rebind": MsgRebind}[name], now)
	case "release":
		if !bound {
			return fmt.Errorf("nothing to release (%s)", st)
		}
		c.sendRelease()
		c.dropAll()
		c.setState(stReleased)
		c.retx, c.collectTo = time.Time{}, time.Time{}
	case "restart":
		if bound {
			c.sendRelease()
		}
		c.dropAll()
		c.solicit(now)
	case "clear":
		c.mu.Lock()
		c.counters = map[string]uint64{}
		c.mu.Unlock()
	default:
		return fmt.Errorf("unknown action %q", name)
	}
	return nil
}

func (c *client6) Action(ctx context.Context, name string, _ any) (any, error) {
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

// Binding6 is a leased address or prefix in status.
type Binding6 struct {
	Prefix      string  `json:"prefix"`
	PreferredIn float64 `json:"preferred_in_s"`
	ValidIn     float64 `json:"valid_in_s"`
}

// RAStatus is the router advertisement followed.
type RAStatus struct {
	Router  string   `json:"router"`
	Managed bool     `json:"managed"`
	Other   bool     `json:"other"`
	OnLink  []string `json:"on_link,omitempty"`
}

// Status6Info is a DHCPv6 client's status.
type Status6Info struct {
	State       string            `json:"state"`
	Server      string            `json:"server_duid,omitempty"`
	Addresses   []Binding6        `json:"addresses,omitempty"`
	Prefixes    []Binding6        `json:"prefixes,omitempty"`
	PDAddresses []string          `json:"pd_addresses,omitempty"`
	NAStatus    string            `json:"ia_na_status,omitempty"`
	PDStatus    string            `json:"ia_pd_status,omitempty"`
	Gateway     string            `json:"gateway,omitempty"`
	RA          *RAStatus         `json:"ra,omitempty"`
	DNS         []string          `json:"dns,omitempty"`
	Domains     []string          `json:"domains,omitempty"`
	RenewIn     float64           `json:"renew_in_s,omitempty"`
	RebindIn    float64           `json:"rebind_in_s,omitempty"`
	BindTime    float64           `json:"bind_time_s,omitempty"`
	Counters    map[string]uint64 `json:"counters"`
}

func (c *client6) Status() any {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	st := Status6Info{State: c.state, NAStatus: c.naStatus, PDStatus: c.pdStatus, Domains: c.domains, Counters: map[string]uint64{}}
	for k, v := range c.counters {
		st.Counters[k] = v
	}
	if c.server != nil {
		st.Server = hex.EncodeToString(c.server)
	}
	b6 := func(b binding) Binding6 {
		p := b.prefix.String()
		if b.prefix.Bits() == 128 {
			p = b.prefix.Addr().String()
		}
		return Binding6{p, max(0, secs(b.preferredAt.Sub(now))), max(0, secs(b.validAt.Sub(now)))}
	}
	for _, b := range c.addrs {
		st.Addresses = append(st.Addresses, b6(b))
	}
	for _, b := range c.prefixes {
		st.Prefixes = append(st.Prefixes, b6(b))
	}
	for _, a := range c.pdAddrs {
		st.PDAddresses = append(st.PDAddresses, a.String())
	}
	for _, d := range c.dns {
		st.DNS = append(st.DNS, d.String())
	}
	if r := c.ra; r != nil {
		st.RA = &RAStatus{Router: r.router.String(), Managed: r.managed, Other: r.other}
		for _, p := range r.onLink {
			st.RA.OnLink = append(st.RA.OnLink, p.String())
		}
		if r.lifetime > 0 {
			st.Gateway = r.router.String()
		}
	}
	if len(c.addrs)+len(c.prefixes) > 0 {
		st.BindTime = secs(c.bindTime)
		st.RenewIn, st.RebindIn = max(0, secs(c.t1.Sub(now))), max(0, secs(c.t2.Sub(now)))
	}
	return st
}

// Health implements protocol.Healther: bound (everything asked for),
// partial (an address or a prefix missing), waiting or released.
func (c *client6) Health() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case stBound, stRenewing, stRebinding:
		if c.cfg.IANA && len(c.addrs) == 0 || c.cfg.IAPD && len(c.prefixes) == 0 {
			return "partial"
		}
		return "bound"
	case stReleased:
		return stReleased
	}
	return "waiting"
}

func (c *client6) Stop() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	<-c.ended
}
