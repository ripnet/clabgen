// Package ospf is an emulated OSPFv2 (RFC 2328) or OSPFv3 (RFC 5340,
// type ospfv3; the version differences are mostly in packet3.go and
// orig3.go) router in one area, with
// one or more interfaces (iface.go), each point-to-point or broadcast (DR
// and BDR election): neighbor state machine and database exchange
// (nbr.go), flooding with retransmission, ageing and refresh (lsdb.go),
// our router LSA, a network LSA where we are DR, injected prefixes as
// AS-external or stub links, and actions to flap, change costs, set
// max-metric and advertise/withdraw prefixes. It computes no routes.
package ospf

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
	"github.com/ripnet/clabgen/internal/topogen"
)

const (
	TypeName   = "ospf"
	TypeNameV3 = "ospfv3"
)

func init() {
	protocol.Register(ospfType{})
	protocol.Register(ospf3Type{})
}

type ospfType struct{}

func (ospfType) Name() string { return TypeName }
func (ospfType) Description() string {
	return "OSPFv2 router with one or more interfaces (point-to-point or broadcast with DR/BDR): adjacencies, LSDB sync and flooding, router/network LSAs, route injection (external or stub), flap/cost/max-metric actions"
}
func (ospfType) NewConfig() any             { return defaultConfig() }
func (ospfType) EtherTypes() []uint16       { return []uint16{pkt.EtherTypeIPv4} }
func (ospfType) Actions() []protocol.Action { return actions() }

type ospf3Type struct{}

func (ospf3Type) Name() string { return TypeNameV3 }
func (ospf3Type) Description() string {
	return "OSPFv3 (IPv6) router with one or more interfaces (point-to-point or broadcast with DR/BDR): adjacencies, LSDB sync and flooding, router/network/link/intra-area-prefix LSAs, route injection (external or internal), flap/cost/max-metric actions"
}
func (ospf3Type) NewConfig() any             { return defaultConfigV3() }
func (ospf3Type) EtherTypes() []uint16       { return []uint16{pkt.EtherTypeIPv6} }
func (ospf3Type) Actions() []protocol.Action { return actions() }

func actions() []protocol.Action {
	return []protocol.Action{
		{Name: "max-metric", Description: "Set or clear max-metric (stub router, RFC 6987): transit links at 65535", NewArgs: func() any { return &MaxMetricArgs{Set: true} }},
		{Name: "cost", Description: "Change an interface's cost (default: every interface)", NewArgs: func() any { return &CostArgs{Cost: 10} }},
		{Name: "advertise", Description: "Advertise more prefixes (until withdrawn or restarted)", NewArgs: func() any { return &PrefixArgs{} }},
		{Name: "withdraw", Description: "Stop advertising prefixes (injected or from config)", NewArgs: func() any { return &PrefixArgs{} }},
		{Name: "flap", Description: "Bring adjacencies down for a while (one interface or all), then let them come back", NewArgs: func() any { return &FlapArgs{Seconds: 10, Mode: "reset"} }},
		{Name: "topology-link", Description: "Take a simulated link down or up, or change its cost", NewArgs: func() any { return &TopoLinkArgs{} }},
		{Name: "topology-node", Description: "Take a simulated router down or up, or set max-metric on its links", NewArgs: func() any { return &TopoNodeArgs{} }},
		{Name: "bfd-fail", Description: "Stop sending BFD for a while (one interface or all): neighbors with BFD drop us before our dead interval", NewArgs: func() any { return &BFDArgs{Seconds: 10} }},
		{Name: "graceful-restart", Description: "Restart gracefully: announce a grace period, drop all neighbors and the LSDB, resynchronise, then originate again", NewArgs: func() any { return &GRArgs{} }},
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
	Type   string `json:"type" jsonschema_description:"LSA type as the LSDB lists it: router / network / external / nssa, summary / asbr-summary (v2), link / intra-area-prefix / inter-area-prefix (v3)"`
	ID     string `json:"id" jsonschema:"description=Link state ID"`
	AdvRtr string `json:"adv_router,omitempty" jsonschema:"description=Advertising router. Default: the only match"`
	Area   string `json:"area,omitempty" jsonschema_description:"Area ID, or as for AS-scoped LSAs. Default: the only match"`
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
		// Either family here; the session checks its own.
		if _, err := p.expand(false); err != nil {
			if _, err6 := p.expand(true); err6 != nil {
				return err
			}
		}
	}
	return nil
}

type session struct {
	env      protocol.Env
	cfg      Config
	cd       codec
	rid      netip.Addr
	instance uint8 // OSPFv3

	areas    []*area
	areaByID func(AreaID) *area
	home     *area   // the router's area (first interface, loopbacks, internal prefixes)
	ifArea   []*area // by interface index (from the config)
	topoArea *area

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

	topo    *topology               // simulated routers behind us (nil: none)
	boot    uint64                  // OSPFv3 auth: high half of our sequence numbers
	extIDs  map[netip.Prefix]uint32 // OSPFv3 external, NSSA and inter-area-prefix LSIDs, stable per prefix
	nextExt uint32

	translating map[*area]bool        // NSSAs whose type-7 LSAs we translate
	reasm       map[reasmKey]*partial // IP fragments being reassembled
	gr          *gr                   // our graceful restart in progress

	opqIDs    map[string]uint32 // stable opaque IDs (sr.go)
	opqNext   map[string]uint32
	adjLabels map[string]uint32 // adjacency SID labels by interface and neighbor

	stop chan struct{}
	done chan struct{}
}

// outPkt is a packet to send once mu is released.
type outPkt struct {
	ifc   *iface
	dst   netip.Addr
	mac   pkt.MAC
	typ   uint8
	body  []byte
	bfd   []byte // a BFD control packet instead (bfd.go)
	sport uint16
}

func (ospfType) Start(env protocol.Env, c any) (protocol.Session, error) {
	return start(env, *c.(*Config), codec{}, 0)
}

func (ospf3Type) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := c.(*ConfigV3)
	return start(env, cfg.common(), codec{v3: true}, cfg.InstanceID)
}

func start(env protocol.Env, cfg Config, cd codec, instance uint8) (protocol.Session, error) {
	s := &session{env: env, cfg: cfg, cd: cd, instance: instance, extIDs: map[netip.Prefix]uint32{}, boot: uint64(time.Now().Unix()),
		translating: map[*area]bool{}, opqIDs: map[string]uint32{}, opqNext: map[string]uint32{}, adjLabels: map[string]uint32{},
		db: map[LSAKey]*lsa{}, routes: map[netip.Prefix]route{}, maxMetric: cfg.MaxMetric,
		counters: map[string]uint64{}, dirty: true, ipID: uint16(rand.Intn(65536)),
		rxmt: time.Duration(cfg.RetransmitInterval) * time.Second, refresh: time.Duration(cfg.LSARefresh) * time.Second,
		stop: make(chan struct{}), done: make(chan struct{})}
	if err := s.buildAreas(); err != nil {
		return nil, err
	}
	for _, l := range cfg.Loopbacks {
		p, _ := parseLoopback(l, cd.v3)
		s.loopbacks = append(s.loopbacks, p)
	}
	switch {
	case cfg.RouterID != "":
		s.rid = netip.MustParseAddr(cfg.RouterID)
	case len(s.loopbacks) > 0 && !cd.v3:
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
		rs, _ := p.expand(cd.v3)
		for _, r := range rs {
			s.routes[r.prefix] = r
		}
	}
	t, err := s.newTopology(cfg)
	if err != nil {
		return nil, err
	}
	s.topo = t
	first := Interface{Device: env.Device().Name(), Network: cfg.Network, Cost: cfg.Cost, Passive: cfg.Passive, Neighbors: cfg.Neighbors}
	s.ifs = []*iface{s.newIface(0, first)}
	s.ifs[0].mac = env.Device().MAC()
	for i, ic := range cfg.Interfaces {
		s.ifs = append(s.ifs, s.newIface(i+1, ic))
	}
	if err := s.addVirtualLinks(s.areaByID); err != nil {
		return nil, err
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

// newTopology builds the simulated routers behind us (nil: none).
func (s *session) newTopology(cfg Config) (*topology, error) {
	if k := cfg.Topology.Kind; k == "" || k == "none" {
		return nil, nil
	}
	t, err := buildTopology(cfg.Topology, s.rid, s.cd.v3)
	if err != nil {
		return nil, fmt.Errorf("topology: %w", err)
	}
	return t, nil
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
	s.grTimersLocked(now)
	if s.dirty && !now.Before(s.genAt) && (s.gr == nil || s.gr.phase != "restarting") {
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
		src, ok := s.ifSourceLocked(f)
		send := s.env.Send
		ttl := uint8(1)
		mac := o.mac
		via := f
		if f.vl != nil {
			// A virtual link: through our first hop, routed.
			via, mac, ttl = f.vl.via, f.vl.nhMAC, 64
			if via == nil {
				ok = false
			}
		}
		if ok && via.idx != 0 {
			if via.att == nil {
				ok = false
			} else {
				send = via.att.Send
			}
		}
		if o.bfd != nil {
			src, ok := s.ifSourceLocked(f)
			send := s.env.Send
			if f.idx != 0 && f.att != nil {
				send = f.att.Send
			} else if f.idx != 0 {
				ok = false
			}
			var et uint16
			var frame []byte
			if ok {
				et, frame = s.bfdFrame(src, o.dst, o.sport, o.bfd)
			}
			if ok && o.mac != (pkt.MAC{}) {
				s.count("tx_bfd")
			} else {
				s.count("tx_bfd_dropped")
			}
			s.mu.Unlock()
			if ok && o.mac != (pkt.MAC{}) {
				send(o.mac, et, frame, protocol.SendOpts{SetPCP: true, PCP: 6})
			}
			continue
		}
		f.cryptoSeq++
		seq := f.cryptoSeq
		auth := f.auth.sending(time.Now(), s.cd.v3)
		ipf := s.ifIP(f)
		seq3 := s.boot<<32 | uint64(seq)
		s.ipID++
		id := s.ipID
		s.mu.Unlock()
		if !ok {
			continue
		}
		if o.dst.IsMulticast() {
			mac = pkt.MulticastMAC(o.dst)
		} else if mac == (pkt.MAC{}) && ipf != nil {
			// A configured neighbor not heard from yet (non-broadcast).
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			_, m, err := ipf.ResolveNextHop(ctx, o.dst)
			cancel()
			if err != nil {
				s.mu.Lock()
				s.count("tx_unresolved")
				s.mu.Unlock()
				continue
			}
			mac = m
		}
		var err error
		if s.cd.v3 {
			p := s.cd.encodeHeader(o.typ, s.rid, f.area.id, s.instance, o.body)
			if authType(auth) == AuthCrypto {
				p = append(p, trailer3(p, src, auth, seq3)...) // checksum stays zero
			} else {
				binary.BigEndian.PutUint16(p[12:], pkt.Checksum(p, pkt.PseudoSum(src, o.dst, ProtoOSPF, len(p))))
			}
			ip := pkt.IPv6{TrafficClass: 0xc0, NextHeader: ProtoOSPF, HopLimit: ttl, Src: src, Dst: o.dst, Payload: p}
			frames := [][]byte{ip.Encode()}
			if 40+len(p) > s.cfg.MTU {
				frames = fragment6(ip, s.cfg.MTU, uint32(s.boot)<<16|uint32(id))
			}
			for _, fr := range frames {
				if e := send(mac, pkt.EtherTypeIPv6, fr, protocol.SendOpts{SetPCP: true, PCP: 6}); e != nil {
					err = e
				}
			}
			s.countFrags(len(frames))
		} else {
			p := sign(encodeHeader(o.typ, s.rid, f.area.id, o.body), auth, seq)
			ip := pkt.IPv4{TOS: 0xc0, ID: id, TTL: ttl, Protocol: ProtoOSPF, Src: src, Dst: o.dst, Payload: p}
			frames := [][]byte{ip.Encode()}
			if 20+len(p) > s.cfg.MTU {
				frames = fragment4(ip, s.cfg.MTU)
			}
			for _, fr := range frames {
				if e := send(mac, pkt.EtherTypeIPv4, fr, protocol.SendOpts{SetPCP: true, PCP: 6}); e != nil {
					err = e
				}
			}
			s.countFrags(len(frames))
		}
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
	if s.cd.v3 {
		n = s.cfg.MTU - 40 - header3Len
	}
	if f.auth.crypto() {
		n -= f.auth.maxDigest()
		if s.cd.v3 {
			n -= at3Header
		}
	}
	return n
}

// ---- receiving ----

func (s *session) HandleRx(fr *pkt.Frame) { s.rx(s.ifs[0], fr) }

// ipPacket is the IP layer of a received packet.
type ipPacket struct {
	src, dst netip.Addr
	payload  []byte
}

func (s *session) decodeIP(fr *pkt.Frame) (ipPacket, bool) {
	if s.cd.v3 {
		ip, err := pkt.DecodeIPv6(fr.Payload)
		if ip == nil || err != nil {
			return ipPacket{}, false
		}
		if ip.NextHeader == ipv6FragHeader && len(ip.Payload) >= 8 && ip.Payload[0] == ProtoOSPF {
			h := ip.Payload
			fo := binary.BigEndian.Uint16(h[2:])
			k := reasmKey{ip.Src, ip.Dst, binary.BigEndian.Uint32(h[4:])}
			whole, ok := s.reassemble(k, int(fo>>3)*8, fo&1 != 0, h[8:], time.Now())
			return ipPacket{ip.Src, ip.Dst, whole}, ok
		}
		if ip.NextHeader != ProtoOSPF {
			return ipPacket{}, false
		}
		return ipPacket{ip.Src, ip.Dst, ip.Payload}, true
	}
	ip, err := pkt.DecodeIPv4(fr.Payload)
	if ip == nil || err != nil || ip.Protocol != ProtoOSPF {
		return ipPacket{}, false
	}
	if ip.Flags&pkt.IPv4MF != 0 || ip.FragOff != 0 {
		k := reasmKey{ip.Src, ip.Dst, uint32(ip.ID)}
		whole, ok := s.reassemble(k, int(ip.FragOff)*8, ip.Flags&pkt.IPv4MF != 0, ip.Payload, time.Now())
		return ipPacket{ip.Src, ip.Dst, whole}, ok
	}
	return ipPacket{ip.Src, ip.Dst, ip.Payload}, true
}

func (s *session) countFrags(n int) {
	if n > 1 {
		s.mu.Lock()
		s.count("tx_fragmented")
		s.mu.Unlock()
	}
}

func (s *session) rx(f *iface, fr *pkt.Frame) {
	if f.bfd && s.rxBFD(f, fr) {
		return
	}
	ip, ok := s.decodeIP(fr)
	if !ok {
		return
	}
	h, err := s.cd.decodeHeader(ip.payload)
	s.mu.Lock()
	defer func() {
		out := s.takeOutLocked()
		s.mu.Unlock()
		s.flush(out)
	}()
	if f.state == ifDown || f.passive {
		return
	}
	// A virtual link's packets come in on the transit interface, unicast
	// to our address on it, for the backbone.
	if err == nil && h.AreaID != f.area.id {
		if vf := s.virtualFor(f, h.AreaID, h.RouterID); vf != nil && ip.dst == vf.vl.src {
			f = vf
		}
	}
	mine, ok := s.ifSourceLocked(f)
	if !ok || ip.src == mine {
		return
	}
	switch {
	case ip.dst == s.allSPF(), ip.dst == mine:
	case ip.dst == s.allDR():
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
	if s.cd.v3 && h.InstanceID != s.instance {
		s.count("rx_other_instance")
		return
	}
	if h.AreaID != f.area.id {
		s.count("rx_area_mismatch")
		f.authErr = fmt.Sprintf("packet from %s for area %s, the interface is in %s", ip.src, h.AreaID, f.area.id)
		return
	}
	if h.RouterID == s.rid {
		s.count("rx_own_router_id")
		return
	}
	now := time.Now()
	var seq uint64
	var crypto bool
	if s.cd.v3 {
		if f.auth.crypto() {
			var a Auth
			if len(h.Trailer) >= at3Header {
				a, err = f.auth.accepting(binary.BigEndian.Uint16(h.Trailer[6:]), now, true)
			}
			if err == nil {
				seq, err = verify3(h, ip.src, a)
			}
			if err != nil {
				s.count("rx_auth_failures")
				f.authErr = err.Error()
				return
			}
			crypto = true
		} else if pkt.Checksum(h.Packet, pkt.PseudoSum(ip.src, ip.dst, ProtoOSPF, len(h.Packet))) != 0 {
			s.count("rx_checksum_errors")
			return
		}
	} else {
		if p, ok := s.ifAddrLocked(f); !f.p2p && ok && !p.Contains(ip.src) {
			s.count("rx_wrong_subnet")
			return
		}
		var seq2 uint32
		a, err := f.auth.accepting(uint16(h.Auth[2]), now, false)
		if err == nil {
			seq2, crypto, err = verify(h, a)
		}
		seq = uint64(seq2)
		if err != nil {
			s.count("rx_auth_failures")
			f.authErr = err.Error()
			return
		}
	}
	if h.Type == TypeHello {
		hello, err := s.cd.decodeHello(h.Body)
		if err != nil {
			s.count("rx_errors")
			return
		}
		if crypto && s.cd.v3 && hello.Options&OptAT == 0 {
			s.count("rx_auth_failures")
			f.authErr = "hello without the AT bit"
			return
		}
		s.count("rx_hello")
		s.rxHelloLocked(f, h, ip.src, fr.Src, hello, seq, crypto, now)
		return
	}
	n := f.nbrs[h.RouterID]
	if n == nil {
		s.count("rx_unknown_neighbor")
		return
	}
	if crypto && !s.seqOKLocked(n, h.Type, seq) {
		return
	}
	s.count("rx_" + typeNames[h.Type])
	switch h.Type {
	case TypeDBD:
		if d, err := s.cd.decodeDBD(h.Body); err == nil {
			if crypto && s.cd.v3 && d.Options&OptAT == 0 {
				s.count("rx_auth_failures")
				f.authErr = "DBD without the AT bit"
				return
			}
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
		if hs, err := s.cd.decodeAck(h.Body); err == nil {
			s.rxAckLocked(n, hs)
		}
	}
}

// ---- status ----

type Status struct {
	RouterID   string            `json:"router_id"`
	Area       string            `json:"area"` // the router's
	Areas      []AreaStatus      `json:"areas"`
	ABR        bool              `json:"abr,omitempty"`
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
	Topology   *topogen.Status   `json:"topology,omitempty"`
	SR         *SRStatus         `json:"segment_routing,omitempty"`
	GR         GRStatus          `json:"graceful_restart"`
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
	Area     string `json:"area,omitempty"` // area-scoped: the area
	Link     string `json:"link,omitempty"` // link-scoped: the interface it belongs to
	Summary  string `json:"summary,omitempty"`
}

// maxStatusLSAs caps the LSDB listed in the status; lsdb_total has the
// full count.
const maxStatusLSAs = 500

func (s *session) Status() any {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{RouterID: s.rid.String(), Area: s.home.id.String(), Areas: s.areaStatusLocked(), ABR: s.abrLocked(), MaxMetric: s.maxMetric, Auth: s.ifs[0].auth.describe(now),
		Prefixes: len(s.routes), LSDB: []LSAStatus{}, LSDBTotal: len(s.db), LSDBTypes: map[string]int{},
		Counters: map[string]uint64{}, Interfaces: []IfStatus{}, Neighbors: []NbrStatus{}}
	if st.Auth == "" {
		st.Auth = "none"
	}
	for _, p := range s.loopbacks {
		st.Loopbacks = append(st.Loopbacks, p.String())
	}
	if s.topo != nil {
		st.Topology = s.topo.status()
	}
	st.SR = s.srStatusLocked()
	st.GR = s.grStatusLocked(now)
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
		st.LSDBTypes[s.cd.typeName(k.Type)]++
		if l.own && (k.Type == LSAExternal && !s.cd.v3 || k.Type == LSA3External && s.cd.v3) {
			st.ASBR = true
		}
	}
	sortKeys(keys)
	for i, k := range keys {
		if i == maxStatusLSAs {
			break
		}
		st.LSDB = append(st.LSDB, s.lsaStatus(s.db[k], now))
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
		if a.Area != b.Area {
			return a.Area < b.Area
		}
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
			rs, err := p.expand(s.cd.v3)
			if err != nil {
				return nil, err
			}
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
	case "graceful-restart":
		return s.startGRLocked(args.(*GRArgs), now)
	case "bfd-fail":
		a := args.(*BFDArgs)
		fs, err := s.ifsNamed(a.Interface)
		if err != nil {
			return nil, err
		}
		n := 0
		for _, f := range fs {
			if f.bfd {
				f.bfdSilentTill = now.Add(time.Duration(a.Seconds) * time.Second)
				n++
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("BFD is not enabled on these interfaces")
		}
		return map[string]any{"interfaces": n, "until": now.Add(time.Duration(a.Seconds) * time.Second).Format(time.RFC3339)}, nil
	case "topology-link":
		return s.topoLinkLocked(args.(*TopoLinkArgs))
	case "topology-node":
		return s.topoNodeLocked(args.(*TopoNodeArgs))
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
