package dhcp

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeServer6 = "dhcpv6-server"

func init() { protocol.Register(server6Type{}) }

// Pool6 is the addresses and delegated prefixes for one client link.
type Pool6 struct {
	Link      string       `json:"link" jsonschema:"required" jsonschema_description:"The clients' link, e.g. 2001:db8:1::/64: requests relayed from it (by the innermost relay's link-address) or sent straight to the server on its own link lease from this pool"`
	Start     string       `json:"start,omitempty" jsonschema_description:"First address to lease (IA_NA). Without start and end, the pool gives no addresses"`
	End       string       `json:"end,omitempty" jsonschema:"description=Last address to lease"`
	PDPrefix  string       `json:"pd_prefix,omitempty" jsonschema_description:"The prefix to delegate from (IA_PD), e.g. 2001:db8:100::/40. Without it, the pool delegates nothing"`
	PDLength  uint8        `json:"pd_length,omitempty" jsonschema:"maximum=128,description=Length of the delegated prefixes"`
	DNS       []string     `json:"dns,omitempty" jsonschema:"description=DNS servers (option 23)"`
	Domains   []string     `json:"domains,omitempty" jsonschema:"description=Domain search list (option 24)"`
	Preferred uint32       `json:"preferred,omitempty" jsonschema:"description=Preferred lifetime in seconds for this pool (default: the server-wide preferred)"`
	Valid     uint32       `json:"valid,omitempty" jsonschema:"description=Valid lifetime in seconds for this pool (default: the server-wide valid)"`
	Options   []RawOption6 `json:"options,omitempty" jsonschema:"description=More options to give this pool's clients"`
}

// ServerConfig6 is a DHCPv6 server's config.
type ServerConfig6 struct {
	Pools       []Pool6 `json:"pools" jsonschema:"required,description=Pools: one per client link"`
	Preferred   uint32  `json:"preferred" jsonschema:"minimum=1,description=Preferred lifetime in seconds"`
	Valid       uint32  `json:"valid" jsonschema:"minimum=1,description=Valid lifetime in seconds"`
	RenewTime   uint32  `json:"renew_time,omitempty" jsonschema:"description=T1 in seconds (default: half the preferred lifetime)"`
	RebindTime  uint32  `json:"rebind_time,omitempty" jsonschema:"description=T2 in seconds (default: 0.8 of the preferred lifetime)"`
	RapidCommit bool    `json:"rapid_commit" jsonschema_description:"Answer a Solicit that asks for rapid commit with a Reply"`
	Preference  uint8   `json:"preference,omitempty" jsonschema:"description=Server preference in Advertise messages (255: clients take it at once)"`
	DUID        string  `json:"duid,omitempty" jsonschema_description:"Server DUID in hex (0x…). Default: a DUID-LL from the device's MAC"`
}

type pool6 struct {
	cfg              Pool6
	link             netip.Prefix
	start, end       netip.Addr
	pd               netip.Prefix
	pdLen            int
	dns              []netip.Addr
	domains          []byte
	extra            []Option6
	preferred, valid uint32
}

func parsePools6(c *ServerConfig6) ([]*pool6, error) {
	if len(c.Pools) == 0 {
		return nil, errors.New("at least one pool")
	}
	var out []*pool6
	for i, pc := range c.Pools {
		p := &pool6{cfg: pc, preferred: c.Preferred, valid: c.Valid}
		if pc.Preferred > 0 {
			p.preferred = pc.Preferred
		}
		if pc.Valid > 0 {
			p.valid = pc.Valid
		}
		var err error
		if p.link, err = netip.ParsePrefix(pc.Link); err != nil || !p.link.Addr().Is6() {
			return nil, fmt.Errorf("pool %d: link %q is not an IPv6 prefix", i+1, pc.Link)
		}
		p.link = p.link.Masked()
		if (pc.Start == "") != (pc.End == "") {
			return nil, fmt.Errorf("pool %s: give both start and end, or neither", pc.Link)
		}
		if pc.Start != "" {
			if p.start, err = netip.ParseAddr(pc.Start); err != nil || !p.link.Contains(p.start) {
				return nil, fmt.Errorf("pool %s: start %q is not an address in it", pc.Link, pc.Start)
			}
			if p.end, err = netip.ParseAddr(pc.End); err != nil || !p.link.Contains(p.end) {
				return nil, fmt.Errorf("pool %s: end %q is not an address in it", pc.Link, pc.End)
			}
			if p.end.Less(p.start) {
				return nil, fmt.Errorf("pool %s: start is after end", pc.Link)
			}
		}
		if pc.PDPrefix != "" {
			if p.pd, err = netip.ParsePrefix(pc.PDPrefix); err != nil || !p.pd.Addr().Is6() {
				return nil, fmt.Errorf("pool %s: pd_prefix %q is not an IPv6 prefix", pc.Link, pc.PDPrefix)
			}
			p.pd = p.pd.Masked()
			p.pdLen = int(pc.PDLength)
			if p.pdLen == 0 {
				p.pdLen = 56
			}
			if p.pdLen < p.pd.Bits() || p.pdLen > 128 {
				return nil, fmt.Errorf("pool %s: pd_length %d doesn't fit in %s", pc.Link, p.pdLen, p.pd)
			}
		}
		if !p.start.IsValid() && !p.pd.IsValid() {
			return nil, fmt.Errorf("pool %s gives neither addresses (start, end) nor prefixes (pd_prefix)", pc.Link)
		}
		for _, d := range pc.DNS {
			a, err := netip.ParseAddr(d)
			if err != nil || !a.Is6() {
				return nil, fmt.Errorf("pool %s: dns %q is not an IPv6 address", pc.Link, d)
			}
			p.dns = append(p.dns, a)
		}
		for _, d := range pc.Domains {
			for _, l := range strings.Split(strings.Trim(d, "."), ".") {
				if l == "" || len(l) > 63 {
					return nil, fmt.Errorf("pool %s: domain %q", pc.Link, d)
				}
				p.domains = append(append(p.domains, byte(len(l))), l...)
			}
			p.domains = append(p.domains, 0)
		}
		for _, o := range pc.Options {
			v, err := parseValue(o.Value)
			if err != nil || o.Code == 0 {
				return nil, fmt.Errorf("pool %s: option %d: bad code or value", pc.Link, o.Code)
			}
			p.extra = append(p.extra, Option6{o.Code, v})
		}
		for _, q := range out {
			if q.link.Overlaps(p.link) {
				return nil, fmt.Errorf("pools %s and %s overlap", q.link, p.link)
			}
			if q.pd.IsValid() && p.pd.IsValid() && q.pd.Overlaps(p.pd) {
				return nil, fmt.Errorf("pools %s and %s delegate from overlapping prefixes", q.link, p.link)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

func (c *ServerConfig6) Validate() error {
	if c.Preferred == 0 || c.Valid == 0 || c.Preferred > c.Valid {
		return errors.New("preferred and valid lifetimes must be at least 1, preferred no longer than valid")
	}
	if c.DUID != "" {
		if b, err := parseValue(c.DUID); err != nil || !strings.HasPrefix(strings.ToLower(c.DUID), "0x") || len(b) < 2 {
			return fmt.Errorf("duid %q: want hex bytes as 0x…", c.DUID)
		}
	}
	_, err := parsePools6(c)
	return err
}

type server6Type struct{}

func (server6Type) Name() string { return TypeServer6 }
func (server6Type) Description() string {
	return "DHCPv6 server: addresses and delegated prefixes from a pool per client link, to clients on its link or behind relays"
}
func (server6Type) NewConfig() any             { return &ServerConfig6{Preferred: 3600, Valid: 7200} }
func (server6Type) EtherTypes() []uint16       { return []uint16{pkt.EtherTypeIPv6} }
func (server6Type) Actions() []protocol.Action { return serverActions("bindings") }

// binding6 is an address (IA_NA) or prefix (IA_PD) given to a client.
type binding6 struct {
	prefix      netip.Prefix // an address as /128
	pool        *pool6
	duid        string
	iaid        uint32
	kind        uint16
	state       string
	preferredAt time.Time
	expires     time.Time
	relays      []Relay6Hop
}

func (b *binding6) active(now time.Time) bool {
	return (b.state == stOffered || b.state == stBound || b.state == stDeclined) && now.Before(b.expires)
}

// Relay6Hop is one relay a client's request passed, as received.
type Relay6Hop struct {
	Link         string   `json:"link_address"`
	Peer         string   `json:"peer_address"`
	Hops         uint8    `json:"hop_count"`
	InterfaceID  string   `json:"interface_id,omitempty"`
	RemoteID     string   `json:"remote_id,omitempty"`
	SubscriberID string   `json:"subscriber_id,omitempty"`
	ClientLLAddr string   `json:"client_link_layer_address,omitempty"`
	Other        []string `json:"other_options,omitempty"`
}

func hopsOf(relays []*Relay6) []Relay6Hop {
	var out []Relay6Hop
	for _, r := range relays {
		h := Relay6Hop{Link: r.Link.String(), Peer: r.Peer.String(), Hops: r.Hops}
		for _, o := range r.Options {
			switch o.Code {
			case Opt6RelayMsg:
			case Opt6InterfaceID:
				h.InterfaceID = optText(o.Data)
			case Opt6RemoteID:
				if len(o.Data) > 4 {
					h.RemoteID = fmt.Sprintf("%d:%s", binary.BigEndian.Uint32(o.Data), optText(o.Data[4:]))
				}
			case Opt6SubscriberID:
				h.SubscriberID = optText(o.Data)
			case Opt6ClientLLAddr:
				if len(o.Data) == 8 && binary.BigEndian.Uint16(o.Data) == 1 {
					h.ClientLLAddr = pkt.MAC(o.Data[2:]).String() // Ethernet
				} else {
					h.ClientLLAddr = optText(o.Data)
				}
			default:
				h.Other = append(h.Other, fmt.Sprintf("%d=%s", o.Code, optText(o.Data)))
			}
		}
		out = append(out, h)
	}
	return out
}

type server6 struct {
	*worker
	env  protocol.Env
	duid []byte

	mu       sync.Mutex
	cfg      ServerConfig6
	pools    []*pool6
	bindings map[netip.Prefix]*binding6
}

func (server6Type) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*ServerConfig6)
	if env.IP() == nil {
		return nil, errors.New("the device has no IP stack")
	}
	pools, err := parsePools6(&cfg)
	if err != nil {
		return nil, err
	}
	s := &server6{worker: newWorker(env), env: env, duid: DUIDLL(env.Device().MAC()), cfg: cfg, pools: pools, bindings: map[netip.Prefix]*binding6{}}
	if cfg.DUID != "" {
		s.duid, _ = parseValue(cfg.DUID)
	}
	go s.run(s.handle)
	return s, nil
}

func (s *server6) HandleRx(f *pkt.Frame) {
	src, dst, b, ok := udpOf(f, Port6Server)
	if !ok || dst != AllServers && !ours(s.env.IP(), dst) {
		return
	}
	s.queue(request{src: src, dst: dst, srcMAC: f.Src, data: append([]byte(nil), b...)})
}

// Reconfigure keeps the bindings that are still in a pool.
func (s *server6) Reconfigure(c any) bool {
	cfg := *c.(*ServerConfig6)
	pools, err := parsePools6(&cfg)
	if err != nil || cfg.DUID != s.cfg.DUID {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg, s.pools = cfg, pools
	for k, b := range s.bindings {
		b.pool = nil
		for _, p := range pools {
			if b.kind == Opt6IANA && p.start.IsValid() && !b.prefix.Addr().Less(p.start) && !p.end.Less(b.prefix.Addr()) ||
				b.kind == Opt6IAPD && p.pd.IsValid() && p.pd.Contains(b.prefix.Addr()) && b.prefix.Bits() == p.pdLen {
				b.pool = p
			}
		}
		if b.pool == nil {
			delete(s.bindings, k)
		}
	}
	return true
}

// selectPool picks the pool by the innermost relay's link-address, or for
// a client on the server's own link, by the server's addresses.
func (s *server6) selectPool(relays []*Relay6) *pool6 {
	var links []netip.Addr
	if len(relays) > 0 {
		links = []netip.Addr{relays[len(relays)-1].Link}
	} else {
		for _, p := range s.env.IP().Addresses() {
			if p.Addr().Is6() && !p.Addr().IsLinkLocalUnicast() {
				links = append(links, p.Addr())
			}
		}
	}
	for _, a := range links {
		for _, p := range s.pools {
			if p.link.Contains(a) {
				return p
			}
		}
	}
	return nil
}

var names6 = map[uint8]string{MsgSolicit: "solicit", MsgAdvertise: "advertise", MsgRequest6: "request", MsgConfirm: "confirm",
	MsgRenew: "renew", MsgRebind: "rebind", MsgReply: "reply", MsgRelease6: "release", MsgDecline6: "decline", MsgInfoRequest: "information_request"}

func (s *server6) handle(r request) {
	relays, m, err := Unwrap6(r.data)
	if err != nil || len(relays) > 0 && relays[0].Type != MsgRelayForw {
		return
	}
	s.count("rx_" + names6[m.Type])
	if len(relays) > 0 {
		s.count("rx_relayed")
	}
	sid := m.Get(Opt6ServerID)
	needSID := m.Type == MsgRequest6 || m.Type == MsgRenew || m.Type == MsgRelease6 || m.Type == MsgDecline6
	if sid != nil && string(sid) != string(s.duid) || needSID && sid == nil {
		return // for another server, or missing ours (RFC 8415 16)
	}
	now := time.Now()
	s.mu.Lock()
	pool := s.selectPool(relays)
	if pool == nil {
		s.mu.Unlock()
		s.count("no_pool")
		where := "on the server's link"
		if len(relays) > 0 {
			where = "for link " + relays[len(relays)-1].Link.String()
		}
		s.env.Emit("no-pool", fmt.Sprintf("no pool for a %s %s", names6[m.Type], where), nil)
		return
	}
	reply := s.answer(m, pool, relays, now)
	s.mu.Unlock()
	if reply == nil {
		return
	}
	data := Wrap6(relays, reply.Encode())
	var err2 error
	if len(relays) > 0 {
		src, _ := s.env.IP().Source(r.src)
		err2 = sendUDP(s.env, src, r.src, pkt.MAC{}, Port6Server, Port6Server, data)
	} else {
		src, _ := s.env.IP().Source(r.src)
		err2 = sendUDP(s.env, src, r.src, r.srcMAC, Port6Server, Port6Client, data)
	}
	if err2 != nil {
		s.count("tx_errors")
		return
	}
	s.count("tx_" + names6[reply.Type])
}

// answer builds the reply, updating the bindings. Caller holds mu.
func (s *server6) answer(m *Msg6, pool *pool6, relays []*Relay6, now time.Time) *Msg6 {
	duid := string(m.Get(Opt6ClientID))
	if duid == "" && m.Type != MsgInfoRequest {
		return nil
	}
	reply := &Msg6{Type: MsgReply, XID: m.XID}
	if duid != "" {
		reply.Add(Opt6ClientID, []byte(duid))
	}
	reply.Add(Opt6ServerID, s.duid)
	hops := hopsOf(relays)
	switch m.Type {
	case MsgSolicit:
		commit := s.cfg.RapidCommit && m.Has(Opt6RapidCommit)
		if commit {
			reply.Add(Opt6RapidCommit, nil)
		} else {
			reply.Type = MsgAdvertise
			if s.cfg.Preference > 0 {
				reply.Add(Opt6Preference, []byte{s.cfg.Preference})
			}
		}
		state := stOffered
		if commit {
			state = stBound
		}
		for _, ia := range m.IAs() {
			reply.Options = append(reply.Options, s.lease(ia, duid, pool, hops, state, now).Encode())
		}
	case MsgRequest6:
		for _, ia := range m.IAs() {
			reply.Options = append(reply.Options, s.lease(ia, duid, pool, hops, stBound, now).Encode())
		}
	case MsgRenew, MsgRebind:
		for _, ia := range m.IAs() {
			reply.Options = append(reply.Options, s.extend(ia, duid, pool, hops, now).Encode())
		}
	case MsgRelease6, MsgDecline6:
		state, verb := stReleased, "released"
		if m.Type == MsgDecline6 {
			state, verb = stDeclined, "declined"
		}
		for _, ia := range m.IAs() {
			for _, b := range s.ofIA(duid, ia) {
				b.state = state
				b.expires = now
				if state == stDeclined {
					b.expires = now.Add(time.Duration(pool.valid) * time.Second)
				}
				s.env.Emit(verb, fmt.Sprintf("%s %s %s", shortDUID(duid), verb, b.prefix), nil)
			}
		}
		reply.Add(Opt6StatusCode, Status6{StatusSuccess, verb}.Encode())
		return reply
	case MsgConfirm:
		st := Status6{StatusSuccess, "on link"}
		for _, ia := range m.IAs() {
			for _, a := range ia.Addrs {
				if !pool.link.Contains(a.Addr) {
					st = Status6{StatusNotOnLink, a.Addr.String() + " is not on this link"}
				}
			}
		}
		reply.Add(Opt6StatusCode, st.Encode())
		return reply
	case MsgInfoRequest:
	default:
		return nil
	}
	s.addOptions(reply, pool)
	return reply
}

func shortDUID(d string) string { return hex.EncodeToString([]byte(d)) }

func (s *server6) addOptions(r *Msg6, pool *pool6) {
	if len(pool.dns) > 0 {
		var b []byte
		for _, d := range pool.dns {
			b = append(b, d.AsSlice()...)
		}
		r.Add(Opt6DNS, b)
	}
	if len(pool.domains) > 0 {
		r.Add(Opt6DomainList, pool.domains)
	}
	r.Options = append(r.Options, pool.extra...)
}

// ofIA returns a client's bindings for an IA.
func (s *server6) ofIA(duid string, ia IA) []*binding6 {
	var out []*binding6
	for _, b := range s.bindings {
		if b.duid == duid && b.iaid == ia.IAID && b.kind == ia.Kind {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].prefix.Addr().Less(out[j].prefix.Addr()) })
	return out
}

func (s *server6) times(pool *pool6) (t1, t2 uint32) {
	t1, t2 = pool.preferred/2, uint32(uint64(pool.preferred)*4/5)
	if s.cfg.RenewTime > 0 {
		t1 = s.cfg.RenewTime
	}
	if s.cfg.RebindTime > 0 {
		t2 = s.cfg.RebindTime
	}
	return
}

// iaReply is an IA in a reply carrying one binding.
func (s *server6) iaReply(ia IA, b *binding6) IA {
	out := IA{Kind: ia.Kind, IAID: ia.IAID}
	out.T1, out.T2 = s.times(b.pool)
	if ia.Kind == Opt6IANA {
		out.Addrs = []IAAddr{{b.prefix.Addr(), b.pool.preferred, b.pool.valid}}
	} else {
		out.Prefixes = []IAPrefix{{b.prefix, b.pool.preferred, b.pool.valid}}
	}
	return out
}

func noneLeft(ia IA, why string) IA {
	code := StatusNoAddrsAvail
	if ia.Kind == Opt6IAPD {
		code = StatusNoPrefixAvail
	}
	return IA{Kind: ia.Kind, IAID: ia.IAID, Status: &Status6{code, why}}
}

// lease gives (offers or binds) an address or prefix for an IA: the one
// the client has, the one it asks for, or the first free one.
func (s *server6) lease(ia IA, duid string, pool *pool6, hops []Relay6Hop, state string, now time.Time) IA {
	if ia.Kind == Opt6IANA && !pool.start.IsValid() || ia.Kind == Opt6IAPD && !pool.pd.IsValid() {
		return noneLeft(ia, "this link's pool doesn't give these")
	}
	var b *binding6
	for _, x := range s.ofIA(duid, ia) {
		if x.pool == pool && x.state != stDeclined {
			b = x
			break
		}
	}
	if b == nil {
		var want []netip.Prefix
		for _, a := range ia.Addrs {
			want = append(want, netip.PrefixFrom(a.Addr, 128))
		}
		for _, p := range ia.Prefixes {
			if p.Prefix.Addr().IsUnspecified() {
				continue // a length hint; pd_length decides
			}
			want = append(want, p.Prefix)
		}
		p, ok := s.allocate(pool, ia.Kind, want, now)
		if !ok {
			s.count("pool_exhausted")
			s.env.Emit("pool-exhausted", fmt.Sprintf("pool %s has no free %s", pool.link, map[uint16]string{Opt6IANA: "address", Opt6IAPD: "prefix"}[ia.Kind]), nil)
			return noneLeft(ia, "none left")
		}
		b = &binding6{prefix: p, pool: pool, duid: duid, iaid: ia.IAID, kind: ia.Kind}
		if old := s.bindings[p]; old != nil && old.duid != duid {
			delete(s.bindings, p)
		}
		s.bindings[p] = b
	}
	fresh := b.state != stBound
	s.set(b, state, hops, now)
	if fresh && state == stBound {
		s.env.Emit("bound", fmt.Sprintf("%s to %s", b.prefix, shortDUID(duid)), map[string]any{"prefix": b.prefix.String()})
	}
	return s.iaReply(ia, b)
}

func (s *server6) set(b *binding6, state string, hops []Relay6Hop, now time.Time) {
	b.state, b.relays = state, hops
	if state == stOffered {
		b.preferredAt, b.expires = now.Add(offerHold), now.Add(offerHold)
		return
	}
	b.preferredAt = now.Add(time.Duration(b.pool.preferred) * time.Second)
	b.expires = now.Add(time.Duration(b.pool.valid) * time.Second)
}

// extend renews or rebinds a client's IA (RFC 8415 18.3.4, 18.3.5).
func (s *server6) extend(ia IA, duid string, pool *pool6, hops []Relay6Hop, now time.Time) IA {
	for _, b := range s.ofIA(duid, ia) {
		if b.state == stBound || b.state == stOffered {
			if b.pool != pool {
				break // the client moved: it gets NoBinding and starts over
			}
			s.set(b, stBound, hops, now)
			return s.iaReply(ia, b)
		}
	}
	return IA{Kind: ia.Kind, IAID: ia.IAID, Status: &Status6{StatusNoBinding, "no binding"}}
}

func (s *server6) allocate(pool *pool6, kind uint16, want []netip.Prefix, now time.Time) (netip.Prefix, bool) {
	free := func(p netip.Prefix) bool {
		if kind == Opt6IANA && (p.Addr().Less(pool.start) || pool.end.Less(p.Addr()) || ours(s.env.IP(), p.Addr())) {
			return false
		}
		if kind == Opt6IAPD && (p.Bits() != pool.pdLen || !pool.pd.Contains(p.Addr()) || p.Masked() != p) {
			return false
		}
		b := s.bindings[p]
		return b == nil || !b.active(now)
	}
	for _, p := range want {
		if free(p) {
			return p, true
		}
	}
	var reuse netip.Prefix
	try := func(p netip.Prefix) bool {
		if free(p) {
			if s.bindings[p] == nil {
				return true
			}
			if !reuse.IsValid() {
				reuse = p
			}
		}
		return false
	}
	if kind == Opt6IANA {
		size := rangeSize(pool.start, pool.end)
		for i := int64(0); i < maxScan && big.NewInt(i).Cmp(size) < 0; i++ {
			a, _ := nth(pool.start, big.NewInt(i))
			if p := netip.PrefixFrom(a, 128); try(p) {
				return p, true
			}
		}
	} else {
		step := new(big.Int).Lsh(big.NewInt(1), uint(128-pool.pdLen))
		count := new(big.Int).Lsh(big.NewInt(1), uint(pool.pdLen-pool.pd.Bits()))
		for i := int64(0); i < maxScan && big.NewInt(i).Cmp(count) < 0; i++ {
			a, _ := nth(pool.pd.Addr(), new(big.Int).Mul(step, big.NewInt(i)))
			if p := netip.PrefixFrom(a, pool.pdLen); try(p) {
				return p, true
			}
		}
	}
	return reuse, reuse.IsValid()
}

func (s *server6) Action(_ context.Context, name string, args any) (any, error) {
	err := s.action(name, args, func(a string) error {
		p, err := netip.ParsePrefix(a)
		if err != nil {
			x, err2 := netip.ParseAddr(a)
			if err2 != nil {
				return fmt.Errorf("%q is not an address or prefix", a)
			}
			p = netip.PrefixFrom(x, 128)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.bindings[p] == nil {
			return fmt.Errorf("no binding for %s", a)
		}
		delete(s.bindings, p)
		return nil
	}, func() {
		s.mu.Lock()
		s.bindings = map[netip.Prefix]*binding6{}
		s.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	return s.Brief(), nil
}

// PoolStatus6 is one pool's use.
type PoolStatus6 struct {
	Link      string `json:"link"`
	Addresses string `json:"addresses,omitempty"`
	Bound     int    `json:"addresses_bound"`
	Prefixes  string `json:"prefixes,omitempty"`
	Delegated int    `json:"prefixes_bound"`
}

// Binding6Status is one binding.
type Binding6Status struct {
	Prefix      string      `json:"prefix"`
	Kind        string      `json:"kind"`
	State       string      `json:"state"`
	DUID        string      `json:"duid"`
	IAID        uint32      `json:"iaid"`
	PreferredIn float64     `json:"preferred_in_s"`
	ValidIn     float64     `json:"valid_in_s"`
	Relays      []Relay6Hop `json:"relays,omitempty"`
}

// ServerStatus6 is a DHCPv6 server's status.
type ServerStatus6 struct {
	DUID     string            `json:"duid"`
	Paused   bool              `json:"paused,omitempty"`
	PausedS  float64           `json:"paused_for_s,omitempty"`
	Pools    []PoolStatus6     `json:"pools"`
	Bindings []Binding6Status  `json:"bindings,omitempty"`
	Counters map[string]uint64 `json:"counters"`
}

func (s *server6) status(withBindings bool) ServerStatus6 {
	st := ServerStatus6{DUID: hex.EncodeToString(s.duid)}
	st.Counters, st.PausedS, st.Paused = s.snapshot()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pools {
		ps := PoolStatus6{Link: p.link.String()}
		if p.start.IsValid() {
			ps.Addresses = p.start.String() + "-" + p.end.String()
		}
		if p.pd.IsValid() {
			ps.Prefixes = fmt.Sprintf("%s by /%d", p.pd, p.pdLen)
		}
		for _, b := range s.bindings {
			if b.pool == p && b.state == stBound && b.active(now) {
				if b.kind == Opt6IANA {
					ps.Bound++
				} else {
					ps.Delegated++
				}
			}
		}
		st.Pools = append(st.Pools, ps)
	}
	if !withBindings {
		return st
	}
	for _, b := range s.bindings {
		bs := Binding6Status{Prefix: b.prefix.String(), Kind: "address", State: b.state, DUID: shortDUID(b.duid), IAID: b.iaid,
			PreferredIn: max(0, secs(b.preferredAt.Sub(now))), ValidIn: max(0, secs(b.expires.Sub(now))), Relays: b.relays}
		if b.kind == Opt6IAPD {
			bs.Kind = "prefix"
		} else {
			bs.Prefix = b.prefix.Addr().String()
		}
		if b.state == stBound && !now.Before(b.expires) {
			bs.State = stExpired
		}
		st.Bindings = append(st.Bindings, bs)
	}
	sort.Slice(st.Bindings, func(i, j int) bool {
		if st.Bindings[i].Kind != st.Bindings[j].Kind {
			return st.Bindings[i].Kind == "address"
		}
		return st.Bindings[i].Prefix < st.Bindings[j].Prefix
	})
	return st
}

func (s *server6) Status() any { return s.status(true) }

// Brief implements protocol.Briefer: the pools without the bindings.
func (s *server6) Brief() any { return s.status(false) }
