package dhcp

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeServer4 = "dhcp-server"

func init() { protocol.Register(server4Type{}) }

// Pool4 is the addresses for one client subnet.
type Pool4 struct {
	Subnet    string      `json:"subnet" jsonschema:"required" jsonschema_description:"The clients' subnet, e.g. 10.1.0.0/24: requests relayed from it (by giaddr, or a relay's link selection) or sent straight to the server on its own subnet lease from this pool"`
	Start     string      `json:"start,omitempty" jsonschema:"description=First address to lease (default: the subnet's first host)"`
	End       string      `json:"end,omitempty" jsonschema:"description=Last address to lease (default: the subnet's last host)"`
	Router    string      `json:"router,omitempty" jsonschema:"description=The clients' router (option 3)"`
	DNS       []string    `json:"dns,omitempty" jsonschema:"description=DNS servers (option 6)"`
	Domain    string      `json:"domain,omitempty" jsonschema:"description=Domain name (option 15)"`
	LeaseTime uint32      `json:"lease_time,omitempty" jsonschema:"description=Lease time in seconds for this pool (default: the server-wide lease time)"`
	Options   []RawOption `json:"options,omitempty" jsonschema:"description=More options to give this pool's clients"`
}

// ServerConfig4 is a DHCPv4 server's config.
type ServerConfig4 struct {
	Pools       []Pool4 `json:"pools" jsonschema:"required,description=Address pools: one per client subnet"`
	LeaseTime   uint32  `json:"lease_time" jsonschema:"minimum=1,description=Lease time in seconds"`
	RenewTime   uint32  `json:"renew_time,omitempty" jsonschema:"description=T1 in seconds (default: half the lease)"`
	RebindTime  uint32  `json:"rebind_time,omitempty" jsonschema:"description=T2 in seconds (default: 7/8 of the lease)"`
	RapidCommit bool    `json:"rapid_commit" jsonschema_description:"Answer a Discover that asks for rapid commit with an Ack (RFC 4039)"`
}

// pool4 is a Pool4 parsed.
type pool4 struct {
	cfg        Pool4
	subnet     netip.Prefix
	start, end netip.Addr
	router     netip.Addr
	dns        []netip.Addr
	extra      []Option4
	lease      uint32
}

func parsePools4(c *ServerConfig4) ([]*pool4, error) {
	if len(c.Pools) == 0 {
		return nil, errors.New("at least one pool")
	}
	var out []*pool4
	for i, pc := range c.Pools {
		p := &pool4{cfg: pc, lease: c.LeaseTime}
		if pc.LeaseTime > 0 {
			p.lease = pc.LeaseTime
		}
		var err error
		if p.subnet, err = netip.ParsePrefix(pc.Subnet); err != nil || !p.subnet.Addr().Is4() {
			return nil, fmt.Errorf("pool %d: subnet %q is not an IPv4 prefix", i+1, pc.Subnet)
		}
		p.subnet = p.subnet.Masked()
		p.start, p.end = p.subnet.Addr(), broadcast4(p.subnet)
		if p.subnet.Bits() < 31 {
			p.start, p.end = p.start.Next(), p.end.Prev()
		}
		for _, x := range []struct {
			s   string
			dst *netip.Addr
			n   string
		}{{pc.Start, &p.start, "start"}, {pc.End, &p.end, "end"}, {pc.Router, &p.router, "router"}} {
			if x.s == "" {
				continue
			}
			a, err := netip.ParseAddr(x.s)
			if err != nil || !p.subnet.Contains(a) {
				return nil, fmt.Errorf("pool %s: %s %q is not an address in it", pc.Subnet, x.n, x.s)
			}
			*x.dst = a
		}
		if p.end.Less(p.start) {
			return nil, fmt.Errorf("pool %s: start is after end", pc.Subnet)
		}
		for _, d := range pc.DNS {
			a, err := netip.ParseAddr(d)
			if err != nil || !a.Is4() {
				return nil, fmt.Errorf("pool %s: dns %q is not an IPv4 address", pc.Subnet, d)
			}
			p.dns = append(p.dns, a)
		}
		for _, o := range pc.Options {
			v, err := parseValue(o.Value)
			if err != nil || o.Code == 0 || o.Code == 255 {
				return nil, fmt.Errorf("pool %s: option %d: bad code or value", pc.Subnet, o.Code)
			}
			p.extra = append(p.extra, Option4{o.Code, v})
		}
		for _, q := range out {
			if q.subnet.Overlaps(p.subnet) {
				return nil, fmt.Errorf("pools %s and %s overlap", q.subnet, p.subnet)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

func broadcast4(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(b[:])|(1<<(32-p.Bits())-1))
	return netip.AddrFrom4(b)
}

func (c *ServerConfig4) Validate() error {
	if c.LeaseTime == 0 {
		return errors.New("lease_time must be at least 1")
	}
	if c.RenewTime > 0 && c.RebindTime > 0 && c.RebindTime < c.RenewTime {
		return errors.New("rebind_time is before renew_time")
	}
	_, err := parsePools4(c)
	return err
}

type server4Type struct{}

func (server4Type) Name() string { return TypeServer4 }
func (server4Type) Description() string {
	return "DHCPv4 server: leases from a pool per client subnet, to clients on its link or behind relays"
}
func (server4Type) NewConfig() any             { return &ServerConfig4{LeaseTime: 3600} }
func (server4Type) EtherTypes() []uint16       { return []uint16{pkt.EtherTypeIPv4} }
func (server4Type) Actions() []protocol.Action { return serverActions("leases") }

// lease4s is a lease the server gave (or offered).
type lease4s struct {
	addr     netip.Addr
	pool     *pool4
	client   string // client identifier, or the MAC
	mac      pkt.MAC
	hostname string
	state    string // offered, bound, released, declined
	expires  time.Time
	relay    *relay4
}

func (l *lease4s) active(now time.Time) bool {
	return (l.state == stOffered || l.state == stBound || l.state == stDeclined) && now.Before(l.expires)
}

const (
	stOffered  = "offered"
	stDeclined = "declined"
	stExpired  = "expired"
)

// relay4 is how a relayed request came.
type relay4 struct {
	giaddr netip.Addr
	hops   uint8
	info   []SubOption
	link   netip.Addr // link selection (sub-option 5 or option 118), when given
}

type server4 struct {
	*worker
	env protocol.Env

	mu     sync.Mutex
	cfg    ServerConfig4
	pools  []*pool4
	leases map[netip.Addr]*lease4s
}

func (server4Type) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*ServerConfig4)
	if env.IP() == nil {
		return nil, errors.New("the device has no IP stack")
	}
	pools, err := parsePools4(&cfg)
	if err != nil {
		return nil, err
	}
	s := &server4{worker: newWorker(env), env: env, cfg: cfg, pools: pools, leases: map[netip.Addr]*lease4s{}}
	go s.run(s.handle)
	return s, nil
}

func (s *server4) HandleRx(f *pkt.Frame) {
	src, dst, b, ok := udpOf(f, Port4Server)
	if !ok || dst != netip.AddrFrom4([4]byte{255, 255, 255, 255}) && !ours(s.env.IP(), dst) {
		return
	}
	s.queue(request{src: src, dst: dst, srcMAC: f.Src, data: append([]byte(nil), b...)})
}

// Reconfigure keeps the leases whose addresses are still in a pool.
func (s *server4) Reconfigure(c any) bool {
	cfg := *c.(*ServerConfig4)
	pools, err := parsePools4(&cfg)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg, s.pools = cfg, pools
	for a, l := range s.leases {
		if p := s.poolFor(a); p != nil && !a.Less(p.start) && !p.end.Less(a) {
			l.pool = p
		} else {
			delete(s.leases, a)
		}
	}
	return true
}

// poolFor returns the pool whose subnet holds a. Caller holds mu.
func (s *server4) poolFor(a netip.Addr) *pool4 {
	for _, p := range s.pools {
		if p.subnet.Contains(a) {
			return p
		}
	}
	return nil
}

func clientKey(m *Msg4) string {
	if id := m.Get(OptClientID); len(id) > 0 {
		return hex.EncodeToString(id)
	}
	return "01" + hex.EncodeToString(m.CHAddr[:])
}

var names4 = map[uint8]string{MsgDiscover: "discover", MsgOffer: "offer", MsgRequest: "request", MsgDecline: "decline",
	MsgAck: "ack", MsgNak: "nak", MsgRelease: "release", MsgInform: "inform"}

// selectPool picks the pool for a request: by the relay's link (link
// selection, else giaddr), or for a client on the server's own link, by
// its address or the server's. It is where per-subscriber choices would
// go.
func (s *server4) selectPool(m *Msg4, rel *relay4) *pool4 {
	if rel != nil {
		link := rel.giaddr
		if rel.link.IsValid() {
			link = rel.link
		}
		return s.poolFor(link)
	}
	if !m.CIAddr.IsUnspecified() {
		return s.poolFor(m.CIAddr)
	}
	for _, p := range s.env.IP().Addresses() {
		if p.Addr().Is4() {
			if pl := s.poolFor(p.Addr()); pl != nil {
				return pl
			}
		}
	}
	return nil
}

func relayOf(m *Msg4) *relay4 {
	if m.GIAddr.IsUnspecified() {
		return nil
	}
	r := &relay4{giaddr: m.GIAddr, hops: m.Hops}
	if b := m.Get(OptRelayInfo); b != nil {
		r.info = SubOptions(b)
		for _, so := range r.info {
			if so.Code == SubLinkSel && len(so.Data) == 4 {
				r.link = netip.AddrFrom4([4]byte(so.Data))
			}
		}
	}
	if a := m.Addr(OptSubnetSel); a.IsValid() && !r.link.IsValid() {
		r.link = a
	}
	return r
}

func (s *server4) handle(r request) {
	m, err := Decode4(r.data)
	if err != nil || m.Op != OpRequest {
		return
	}
	t := m.Type()
	s.count("rx_" + names4[t])
	rel := relayOf(m)
	if rel != nil {
		s.count("rx_relayed")
	}
	now := time.Now()
	s.mu.Lock()
	pool := s.selectPool(m, rel)
	if pool == nil {
		s.mu.Unlock()
		s.count("no_pool")
		where := "on the server's link"
		if rel != nil {
			where = "from relay " + rel.giaddr.String()
		}
		s.env.Emit("no-pool", fmt.Sprintf("no pool for a %s %s", names4[t], where), nil)
		return
	}
	reply := s.answer(m, t, pool, rel, now)
	s.mu.Unlock()
	if reply == nil {
		return
	}
	s.send(m, reply, rel, r.srcMAC)
}

// serverID is the address the server answers from: toward the relay, or
// on the client's subnet.
func (s *server4) serverID(pool *pool4, rel *relay4) netip.Addr {
	to := pool.subnet.Addr()
	if rel != nil {
		to = rel.giaddr
	}
	a, _ := s.env.IP().Source(to)
	return a
}

func (s *server4) ourAddress(a netip.Addr) bool { return ours(s.env.IP(), a) }

// answer builds the reply to m, updating the leases. Caller holds mu.
func (s *server4) answer(m *Msg4, t uint8, pool *pool4, rel *relay4, now time.Time) *Msg4 {
	key := clientKey(m)
	sid := s.serverID(pool, rel)
	req := m.Addr(OptRequestedIP)
	switch t {
	case MsgDiscover:
		a, ok := s.allocate(pool, key, req, now)
		if !ok {
			s.count("pool_exhausted")
			s.env.Emit("pool-exhausted", "pool "+pool.subnet.String()+" has no free address", nil)
			return nil
		}
		if s.cfg.RapidCommit && m.Has(OptRapidCommit) {
			s.bind(a, pool, key, m, rel, now)
			r := s.reply(m, MsgAck, a, pool, sid)
			r.Add(OptRapidCommit, nil)
			return r
		}
		s.leases[a] = &lease4s{addr: a, pool: pool, client: key, mac: m.CHAddr, state: stOffered, expires: now.Add(offerHold), relay: rel}
		return s.reply(m, MsgOffer, a, pool, sid)
	case MsgRequest:
		sel := m.Addr(OptServerID)
		switch {
		case sel.IsValid() && !s.ourAddress(sel):
			// The client took another server's offer.
			if l := s.leases[req]; l != nil && l.client == key && l.state == stOffered {
				delete(s.leases, req)
			}
			return nil
		case sel.IsValid(): // selecting
			if l := s.leases[req]; l != nil && l.client == key && l.active(now) && l.state != stDeclined {
				s.bind(req, pool, key, m, rel, now)
				return s.reply(m, MsgAck, req, pool, sid)
			}
			return s.nak(m, "offer not found", sid)
		}
		// Init-reboot (requested address) or renewing/rebinding (ciaddr).
		a := req
		if !m.CIAddr.IsUnspecified() {
			a = m.CIAddr
		}
		if !a.IsValid() || a.IsUnspecified() {
			return nil
		}
		if !pool.subnet.Contains(a) || a.Less(pool.start) || pool.end.Less(a) {
			return s.nak(m, "wrong subnet", sid)
		}
		if l := s.leases[a]; l != nil && l.active(now) && l.client != key {
			return s.nak(m, "address in use", sid)
		}
		if l := s.leases[a]; l != nil && l.client != key && l.state == stDeclined {
			return s.nak(m, "address declined", sid)
		}
		if s.excluded(pool, a) {
			return s.nak(m, "address not available", sid)
		}
		s.bind(a, pool, key, m, rel, now)
		return s.reply(m, MsgAck, a, pool, sid)
	case MsgRelease:
		if l := s.leases[m.CIAddr]; l != nil && l.client == key {
			l.state, l.expires = stReleased, now
			s.env.Emit("released", fmt.Sprintf("%s released %s", m.CHAddr, m.CIAddr), nil)
		}
		return nil
	case MsgDecline:
		if l := s.leases[req]; l != nil && l.client == key {
			l.state, l.expires = stDeclined, now.Add(time.Duration(pool.lease)*time.Second)
			s.env.Emit("declined", fmt.Sprintf("%s declined %s", m.CHAddr, req), nil)
		}
		return nil
	case MsgInform:
		r := s.reply(m, MsgAck, netip.Addr{}, pool, sid)
		r.CIAddr = m.CIAddr
		return r
	}
	return nil
}

// excluded: the pool's router and the server's own addresses aren't leased.
func (s *server4) excluded(pool *pool4, a netip.Addr) bool {
	return a == pool.router || s.ourAddress(a)
}

// allocate picks an address for a client: the one it had, the one it asks
// for, or the first free one.
func (s *server4) allocate(pool *pool4, key string, req netip.Addr, now time.Time) (netip.Addr, bool) {
	free := func(a netip.Addr) bool {
		if !pool.subnet.Contains(a) || a.Less(pool.start) || pool.end.Less(a) || s.excluded(pool, a) {
			return false
		}
		l := s.leases[a]
		return l == nil || l.client == key && l.state != stDeclined || !l.active(now)
	}
	var had []netip.Addr
	for a, l := range s.leases {
		if l.client == key && l.pool == pool && l.state != stDeclined {
			had = append(had, a)
		}
	}
	slices.SortFunc(had, func(x, y netip.Addr) int { return x.Compare(y) })
	for _, a := range had {
		if free(a) {
			return a, true
		}
	}
	if req.IsValid() && free(req) {
		return req, true
	}
	// Never-used addresses first, then ones whose leases ended.
	var reuse netip.Addr
	for a, n := pool.start, 0; n < maxScan; a, n = a.Next(), n+1 {
		if free(a) {
			if s.leases[a] == nil {
				return a, true
			}
			if !reuse.IsValid() {
				reuse = a
			}
		}
		if a == pool.end {
			break
		}
	}
	return reuse, reuse.IsValid()
}

func (s *server4) bind(a netip.Addr, pool *pool4, key string, m *Msg4, rel *relay4, now time.Time) {
	l := s.leases[a]
	fresh := l == nil || l.state != stBound || l.client != key
	if rel == nil && l != nil && l.client == key {
		rel = l.relay // a renewal unicast straight to us: keep how it first came
	}
	l = &lease4s{addr: a, pool: pool, client: key, mac: m.CHAddr, hostname: string(m.Get(OptHostname)), state: stBound,
		expires: now.Add(time.Duration(pool.lease) * time.Second), relay: rel}
	s.leases[a] = l
	if fresh {
		via := ""
		if rel != nil {
			via = " via " + rel.giaddr.String()
		}
		s.env.Emit("bound", fmt.Sprintf("%s to %s%s", a, m.CHAddr, via), map[string]any{"address": a.String(), "mac": m.CHAddr.String()})
	}
}

func (s *server4) times(pool *pool4) (lease, t1, t2 uint32) {
	lease = pool.lease
	t1, t2 = lease/2, uint32(uint64(lease)*7/8)
	if s.cfg.RenewTime > 0 {
		t1 = s.cfg.RenewTime
	}
	if s.cfg.RebindTime > 0 {
		t2 = s.cfg.RebindTime
	}
	return
}

// reply builds an Offer or Ack for address a (invalid for an Inform's Ack).
func (s *server4) reply(m *Msg4, t uint8, a netip.Addr, pool *pool4, sid netip.Addr) *Msg4 {
	r := &Msg4{Op: OpReply, XID: m.XID, Broadcast: m.Broadcast, GIAddr: m.GIAddr, CHAddr: m.CHAddr, YIAddr: a}
	r.Add(OptMsgType, []byte{t})
	r.Add(OptServerID, sid.AsSlice())
	if a.IsValid() {
		lease, t1, t2 := s.times(pool)
		r.Add(OptLeaseTime, binary.BigEndian.AppendUint32(nil, lease))
		r.Add(OptT1, binary.BigEndian.AppendUint32(nil, t1))
		r.Add(OptT2, binary.BigEndian.AppendUint32(nil, t2))
	}
	mask := net4Mask(pool.subnet.Bits())
	r.Add(OptSubnetMask, mask[:])
	if pool.router.IsValid() {
		r.Add(OptRouter, pool.router.AsSlice())
	}
	if len(pool.dns) > 0 {
		var b []byte
		for _, d := range pool.dns {
			b = append(b, d.AsSlice()...)
		}
		r.Add(OptDNS, b)
	}
	if pool.cfg.Domain != "" {
		r.Add(OptDomainName, []byte(pool.cfg.Domain))
	}
	r.Options = append(r.Options, pool.extra...)
	if ri := m.Get(OptRelayInfo); ri != nil {
		r.Add(OptRelayInfo, ri) // echoed (RFC 3046 2.2)
	}
	return r
}

func net4Mask(bits int) [4]byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], ^uint32(0)<<(32-bits))
	return b
}

func (s *server4) nak(m *Msg4, why string, sid netip.Addr) *Msg4 {
	r := &Msg4{Op: OpReply, XID: m.XID, Broadcast: m.Broadcast, GIAddr: m.GIAddr, CHAddr: m.CHAddr}
	r.Add(OptMsgType, []byte{MsgNak})
	r.Add(OptServerID, sid.AsSlice())
	r.Add(OptMessage, []byte(why))
	if ri := m.Get(OptRelayInfo); ri != nil {
		r.Add(OptRelayInfo, ri)
	}
	s.env.Emit("nak", fmt.Sprintf("NAK to %s: %s", m.CHAddr, why), nil)
	return r
}

// send delivers a reply as RFC 2131 4.1 says: to the relay, or to the
// client by its address, by broadcast, or to its MAC.
func (s *server4) send(m, r *Msg4, rel *relay4, srcMAC pkt.MAC) {
	src := r.Addr(OptServerID)
	dst, dport, mac := netip.AddrFrom4([4]byte{255, 255, 255, 255}), uint16(Port4Client), pkt.Broadcast
	switch {
	case rel != nil:
		dst, dport, mac = rel.giaddr, Port4Server, pkt.MAC{}
	case r.Type() == MsgNak:
	case !m.CIAddr.IsUnspecified():
		dst, mac = m.CIAddr, srcMAC
	case m.Broadcast:
	default:
		dst, mac = r.YIAddr, m.CHAddr
	}
	if err := sendUDP(s.env, src, dst, mac, Port4Server, dport, r.Encode()); err != nil {
		s.count("tx_errors")
		return
	}
	s.count("tx_" + names4[r.Type()])
}

func (s *server4) Action(_ context.Context, name string, args any) (any, error) {
	err := s.action(name, args, func(a string) error {
		addr, err := netip.ParseAddr(a)
		if err != nil {
			return fmt.Errorf("%q is not an address", a)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.leases[addr] == nil {
			return fmt.Errorf("no lease for %s", addr)
		}
		delete(s.leases, addr)
		return nil
	}, func() {
		s.mu.Lock()
		s.leases = map[netip.Addr]*lease4s{}
		s.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	return s.Brief(), nil
}

// PoolStatus4 is one pool's use.
type PoolStatus4 struct {
	Subnet  string `json:"subnet"`
	Range   string `json:"range"`
	Size    string `json:"size"`
	Bound   int    `json:"bound"`
	Offered int    `json:"offered"`
}

// Relay4Status is how a lease's requests were relayed.
type Relay4Status struct {
	GIAddr        string   `json:"giaddr"`
	Hops          uint8    `json:"hops"`
	CircuitID     string   `json:"circuit_id,omitempty"`
	RemoteID      string   `json:"remote_id,omitempty"`
	LinkSelection string   `json:"link_selection,omitempty"`
	SubscriberID  string   `json:"subscriber_id,omitempty"`
	Other         []string `json:"other_suboptions,omitempty"`
}

// Lease4Status is one lease.
type Lease4Status struct {
	Address   string        `json:"address"`
	State     string        `json:"state"`
	MAC       string        `json:"mac"`
	ClientID  string        `json:"client_id"`
	Hostname  string        `json:"hostname,omitempty"`
	ExpiresIn float64       `json:"expires_in_s"`
	Relay     *Relay4Status `json:"relay,omitempty"`
}

// ServerStatus4 is a DHCPv4 server's status.
type ServerStatus4 struct {
	Paused   bool              `json:"paused,omitempty"`
	PausedS  float64           `json:"paused_for_s,omitempty"`
	Pools    []PoolStatus4     `json:"pools"`
	Leases   []Lease4Status    `json:"leases,omitempty"`
	Counters map[string]uint64 `json:"counters"`
}

func (s *server4) status(withLeases bool) ServerStatus4 {
	st := ServerStatus4{}
	st.Counters, st.PausedS, st.Paused = s.snapshot()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pools {
		ps := PoolStatus4{Subnet: p.subnet.String(), Range: p.start.String() + "-" + p.end.String(), Size: rangeSize(p.start, p.end).String()}
		for _, l := range s.leases {
			if l.pool == p && l.active(now) {
				switch l.state {
				case stBound:
					ps.Bound++
				case stOffered:
					ps.Offered++
				}
			}
		}
		st.Pools = append(st.Pools, ps)
	}
	if !withLeases {
		return st
	}
	for _, l := range s.leases {
		ls := Lease4Status{Address: l.addr.String(), State: l.state, MAC: l.mac.String(), ClientID: l.client, Hostname: l.hostname,
			ExpiresIn: max(0, secs(l.expires.Sub(now)))}
		if l.state == stBound && !now.Before(l.expires) {
			ls.State = stExpired
		}
		if r := l.relay; r != nil {
			rs := &Relay4Status{GIAddr: r.giaddr.String(), Hops: r.hops}
			for _, so := range r.info {
				switch so.Code {
				case SubCircuitID:
					rs.CircuitID = optText(so.Data)
				case SubRemoteID:
					rs.RemoteID = optText(so.Data)
				case SubLinkSel:
					if len(so.Data) == 4 {
						rs.LinkSelection = netip.AddrFrom4([4]byte(so.Data)).String()
					}
				case SubSubscriberID:
					rs.SubscriberID = optText(so.Data)
				default:
					rs.Other = append(rs.Other, fmt.Sprintf("%d=%s", so.Code, optText(so.Data)))
				}
			}
			if rs.LinkSelection == "" && r.link.IsValid() {
				rs.LinkSelection = r.link.String()
			}
			ls.Relay = rs
		}
		st.Leases = append(st.Leases, ls)
	}
	sort.Slice(st.Leases, func(i, j int) bool {
		a, b := netip.MustParseAddr(st.Leases[i].Address), netip.MustParseAddr(st.Leases[j].Address)
		return a.Less(b)
	})
	return st
}

func (s *server4) Status() any { return s.status(true) }

// Brief implements protocol.Briefer: the pools without the leases.
func (s *server4) Brief() any { return s.status(false) }
