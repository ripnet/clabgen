// Package twamp is a TWAMP Light (RFC 5357 appendix I) and STAMP (RFC 8762,
// TLVs from RFC 8972) session-reflector: test packets to the device's
// addresses on the configured ports are answered with the reflector's
// timestamps, so a router's delay and loss measurements (performance
// monitoring sessions, interface link delay) have a far end. Faults
// (added delay, dropped replies, wrong clocks) test what the router makes
// of them.
package twamp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "twamp-reflector"

// A session with no packets for idleAfter is idle; the oldest idle ones go
// past maxSessions.
const (
	idleAfter   = 10 * time.Second
	maxSessions = 256
)

func init() { protocol.Register(reflectorType{}) }

type Config struct {
	Mode    string   `json:"mode" jsonschema:"enum=stamp,enum=twamp-light,description=stamp: also processes STAMP TLVs (RFC 8972) and answers TWAMP Light too; twamp-light: reflects without looking at TLVs"`
	Ports   []uint16 `json:"ports" jsonschema:"description=UDP ports to answer on (862 is the assigned port; routers make you set it)"`
	Senders []string `json:"senders,omitempty" jsonschema:"description=Only answer senders in these prefixes. Default: any"`
	TTL     uint8    `json:"ttl" jsonschema:"minimum=1,maximum=255,description=TTL / hop limit of replies"`
	DSCP    *uint8   `json:"dscp,omitempty" jsonschema:"minimum=0,maximum=63,description=DSCP of replies. Default: the received one (a Class of Service TLV asking for another wins)"`

	Stateless       bool  `json:"stateless,omitempty" jsonschema:"description=Reply with the sender's sequence number instead of the reflector's own count per session (RFC 8762 4.2)" jsonschema_extras:"x-group=Reflector,x-advanced=true"`
	Synchronized    bool  `json:"synchronized,omitempty" jsonschema_description:"Say the clock is synchronized to an external source (the S bit; Timestamp Information TLV: NTP, else free-running)" jsonschema_extras:"x-group=Reflector,x-advanced=true"`
	ErrorScale      uint8 `json:"error_scale,omitempty" jsonschema:"maximum=63,description=Error estimate: scale (the error is multiplier × 2^-32 × 2^scale s)" jsonschema_extras:"x-group=Reflector,x-advanced=true"`
	ErrorMultiplier uint8 `json:"error_multiplier,omitempty" jsonschema:"maximum=255,description=Error estimate: multiplier. Default 1" jsonschema_extras:"x-group=Reflector,x-advanced=true"`

	DelayUS       uint32  `json:"delay_us,omitempty" jsonschema:"maximum=10000000" jsonschema_description:"Hold each reply this long as added path delay: the receive timestamp is taken as if the packet arrived then, so the sender's round trip and forward delay grow" jsonschema_extras:"x-group=Faults,x-common=true"`
	JitterUS      uint32  `json:"jitter_us,omitempty" jsonschema:"maximum=10000000,description=Add a random 0..jitter to the delay of each reply (replies may overtake each other)" jsonschema_extras:"x-group=Faults,x-common=true"`
	DwellUS       uint32  `json:"dwell_us,omitempty" jsonschema:"maximum=10000000" jsonschema_description:"Hold each reply this long as reflector processing time: both timestamps are true, so a sender that subtracts T3 - T2 sees no change" jsonschema_extras:"x-group=Faults,x-advanced=true"`
	DropPct       float64 `json:"drop_pct,omitempty" jsonschema:"minimum=0,maximum=100,description=Drop this percentage of replies at random" jsonschema_extras:"x-group=Faults,x-common=true"`
	LossDirection string  `json:"loss_direction,omitempty" jsonschema:"enum=backward,enum=forward" jsonschema_description:"Where dropped replies (drop_pct, burst-loss, mute) are lost: backward (default): on the way back, the reflector's sequence number moves on; forward: on the way in, as if never received. Stateful senders tell the two apart" jsonschema_extras:"x-group=Faults,x-common=true"`
	ClockOffsetMS int32   `json:"clock_offset_ms,omitempty" jsonschema:"description=Shift both reflector timestamps: one-way delays come out wrong (or negative) while the round trip stays right" jsonschema_extras:"x-group=Faults,x-advanced=true"`
	BadTimestamps string  `json:"bad_timestamps,omitempty" jsonschema:"enum=,enum=reversed,enum=zero,description=reversed: send T3 before T2; zero: both zero" jsonschema_extras:"x-group=Faults,x-advanced=true"`

	senders []netip.Prefix
}

func (c *Config) Validate() error {
	if c.Mode != "stamp" && c.Mode != "twamp-light" {
		return errors.New("mode must be stamp or twamp-light")
	}
	if len(c.Ports) == 0 {
		return errors.New("at least one port")
	}
	for _, p := range c.Ports {
		if p == 0 {
			return errors.New("ports must be 1-65535")
		}
	}
	if c.TTL == 0 {
		return errors.New("ttl must be 1-255")
	}
	if c.DSCP != nil && *c.DSCP > 63 {
		return errors.New("dscp must be 0-63")
	}
	if c.ErrorScale > 63 {
		return errors.New("error_scale must be 0-63")
	}
	if c.DropPct < 0 || c.DropPct > 100 {
		return errors.New("drop_pct must be 0-100")
	}
	if c.LossDirection != "" && c.LossDirection != "backward" && c.LossDirection != "forward" {
		return errors.New("loss_direction must be backward or forward")
	}
	if c.BadTimestamps != "" && c.BadTimestamps != "reversed" && c.BadTimestamps != "zero" {
		return errors.New("bad_timestamps must be reversed, zero or empty")
	}
	c.senders = nil
	for _, s := range c.Senders {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, err2 := netip.ParseAddr(s)
			if err2 != nil {
				return fmt.Errorf("senders: %q is not a prefix or address", s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		c.senders = append(c.senders, p.Masked())
	}
	return nil
}

type BurstArgs struct {
	Count int `json:"count" jsonschema:"minimum=1,maximum=1000000" jsonschema_description:"Replies to drop in a row, in each session"`
}

func (a *BurstArgs) Validate() error {
	if a.Count < 1 {
		return errors.New("count must be at least 1")
	}
	return nil
}

type reflectorType struct{}

func (reflectorType) Name() string { return TypeName }
func (reflectorType) Description() string {
	return "TWAMP Light / STAMP session-reflector: answers a router's delay and loss measurements, with faults"
}
func (reflectorType) NewConfig() any {
	return &Config{Mode: "stamp", Ports: []uint16{862}, TTL: 255}
}
func (reflectorType) EtherTypes() []uint16 { return []uint16{pkt.EtherTypeIPv4, pkt.EtherTypeIPv6} }
func (reflectorType) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "mute", Description: "Stop answering (senders see total loss) until unmute"},
		{Name: "unmute", Description: "Answer again"},
		{Name: "burst-loss", Description: "Drop each current session's next replies in a row", NewArgs: func() any { return &BurstArgs{Count: 10} }},
		{Name: "clear", Description: "Forget the sessions and reset the counters"},
	}
}

type sessionKey struct {
	src, dst netip.AddrPort
	ssid     uint16
}

type sess struct {
	key        sessionKey
	stamp      bool
	ptp        bool
	first      time.Time
	last       time.Time
	idle       bool
	rx, tx     uint64
	dropped    uint64
	burst      int    // replies still to drop (burst-loss)
	seq        uint32 // reflector's next sequence number
	lastSeq    uint32
	seen       bool
	gaps       uint64
	reordered  uint64
	duplicates uint64
	lastTTL    uint8
	lastDSCP   uint8
	dwellUS    float64
	tlvs       map[string]uint64
	tlvLenHdr  bool      // a TLV's length counted its header
	followSeq  uint32    // the last reply's sequence number and T3,
	followT3   time.Time // for a Follow-up Telemetry TLV
}

type reflector struct {
	env  protocol.Env
	stop chan struct{}
	done chan struct{}

	mu       sync.Mutex
	cfg      Config
	ports    map[uint16]bool
	sessions map[sessionKey]*sess
	muted    bool
	rx, tx   uint64
	dropped  uint64
	refused  uint64
	errs     uint64
	pending  int // replies held by delay or dwell
}

func (reflectorType) Start(env protocol.Env, c any) (protocol.Session, error) {
	r := &reflector{env: env, stop: make(chan struct{}), done: make(chan struct{}), sessions: map[sessionKey]*sess{}}
	r.setConfig(*c.(*Config))
	go r.watch()
	return r, nil
}

func (r *reflector) setConfig(c Config) {
	r.cfg = c
	r.ports = map[uint16]bool{}
	for _, p := range c.Ports {
		r.ports[p] = true
	}
}

// Reconfigure applies every change in place: sessions and counters stay.
func (r *reflector) Reconfigure(c any) bool {
	r.mu.Lock()
	r.setConfig(*c.(*Config))
	r.mu.Unlock()
	return true
}

// watch marks sessions idle and emits the events.
func (r *reflector) watch() {
	defer close(r.done)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-t.C:
			var msgs []string
			r.mu.Lock()
			for _, s := range r.sessions {
				if !s.idle && now.Sub(s.last) > idleAfter {
					s.idle = true
					msgs = append(msgs, fmt.Sprintf("session from %s%s idle: nothing for %s", s.key.src, ssidNote(s), idleAfter))
				}
			}
			r.mu.Unlock()
			for _, m := range msgs {
				r.env.Emit("session-idle", m, nil)
			}
		}
	}
}

func ssidNote(s *sess) string {
	if !s.stamp {
		return ""
	}
	return fmt.Sprintf(" (SSID %d)", s.key.ssid)
}

func (r *reflector) allowed(a netip.Addr) bool {
	if len(r.cfg.senders) == 0 {
		return true
	}
	for _, p := range r.cfg.senders {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

func (r *reflector) HandleRx(fr *pkt.Frame) {
	if fr.Dst.IsMulticast() {
		return
	}
	var src, dst netip.Addr
	var b []byte
	var ttl, tos uint8
	switch fr.EtherType {
	case pkt.EtherTypeIPv4:
		h, err := pkt.DecodeIPv4(fr.Payload)
		if err != nil || h.Protocol != pkt.ProtoUDP || h.FragOff != 0 || h.Flags&pkt.IPv4MF != 0 {
			return
		}
		src, dst, b, ttl, tos = h.Src, h.Dst, h.Payload, h.TTL, h.TOS
	case pkt.EtherTypeIPv6:
		h, err := pkt.DecodeIPv6(fr.Payload)
		if err != nil || h.NextHeader != pkt.ProtoUDP {
			return
		}
		src, dst, b, ttl, tos = h.Src, h.Dst, h.Payload, h.HopLimit, h.TrafficClass
	default:
		return
	}
	if len(b) < 8 {
		return
	}
	r.mu.Lock()
	ours := r.ports[binary.BigEndian.Uint16(b[2:])]
	r.mu.Unlock()
	if !ours {
		return
	}
	if ip := r.env.IP(); ip == nil || !ip.IsLocalRx(fr, dst) {
		return
	}
	n := int(binary.BigEndian.Uint16(b[4:]))
	if n < 8 || n > len(b) {
		return
	}
	b = b[:n]
	if cs := binary.BigEndian.Uint16(b[6:]); (cs != 0 || dst.Is6()) && pkt.Checksum(b, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(b))) != 0 {
		r.mu.Lock()
		r.errs++
		r.mu.Unlock()
		return
	}
	sport, dport := binary.BigEndian.Uint16(b[0:]), binary.BigEndian.Uint16(b[2:])
	body := b[8:]
	rxTime := fr.RxTime
	if rxTime.IsZero() {
		rxTime = time.Now()
	}

	r.mu.Lock()
	cfg := r.cfg
	req, ok := parseRequest(body, cfg.Mode == "stamp")
	if !ok {
		r.errs++
		r.mu.Unlock()
		return
	}
	if !r.allowed(src) {
		r.refused++
		r.mu.Unlock()
		return
	}
	r.rx++
	key := sessionKey{src: netip.AddrPortFrom(src, sport), dst: netip.AddrPortFrom(dst, dport), ssid: req.ssid}
	s := r.sessions[key]
	var event string
	if s == nil {
		r.evict()
		s = &sess{key: key, first: rxTime, tlvs: map[string]uint64{}}
		r.sessions[key] = s
		if req.stamp {
			event = fmt.Sprintf("STAMP session from %s to %s (SSID %d)", key.src, key.dst, key.ssid)
		} else {
			event = fmt.Sprintf("TWAMP Light session from %s to %s", key.src, key.dst)
		}
	} else if s.idle {
		event = fmt.Sprintf("session from %s%s resumed", key.src, ssidNote(s))
	}
	s.idle = false
	s.last, s.stamp, s.ptp = rxTime, req.stamp, req.ptp
	s.rx++
	s.lastTTL, s.lastDSCP = ttl, tos>>2
	switch {
	case !s.seen:
	case req.seq == s.lastSeq:
		s.duplicates++
	case req.seq < s.lastSeq:
		s.reordered++
	case req.seq > s.lastSeq+1:
		s.gaps += uint64(req.seq - s.lastSeq - 1)
	}
	if !s.seen || req.seq > s.lastSeq {
		s.lastSeq = req.seq
	}
	s.seen = true
	for _, t := range req.tlvs {
		s.tlvs[TLVName(t.typ)]++
		s.tlvLenHdr = s.tlvLenHdr || t.lengthWithHeader
	}
	drop := r.muted
	if !drop && s.burst > 0 {
		s.burst--
		drop = true
	}
	if !drop && cfg.DropPct > 0 && rand.Float64()*100 < cfg.DropPct {
		drop = true
	}
	if drop {
		s.dropped++
		r.dropped++
		if cfg.LossDirection != "forward" {
			s.seq++ // sent and lost on the way back
		}
		r.mu.Unlock()
		if event != "" {
			r.env.Emit("session-new", event, nil)
		}
		return
	}
	hold := time.Duration(cfg.DelayUS) * time.Microsecond
	if cfg.JitterUS > 0 {
		hold += time.Duration(rand.Int64N(int64(cfg.JitterUS)+1)) * time.Microsecond
	}
	seq := req.seq
	if !cfg.Stateless {
		seq = s.seq
		s.seq++
	}
	t2 := rxTime.Add(hold) // path delay: as if it arrived then
	hold += time.Duration(cfg.DwellUS) * time.Microsecond
	if hold > 0 {
		r.pending++
	}
	r.mu.Unlock()
	if event != "" {
		r.env.Emit("session-new", event, nil)
	}

	// What Reply needs of the frame, copied: fr is only valid now.
	reply := &pkt.Frame{Src: fr.Src, Labels: append([]uint32(nil), fr.Labels...)}
	in := replyIn{
		body: append([]byte(nil), body...), req: req, seq: seq, t2: t2,
		src: src, dst: dst, sport: sport, dport: dport, ttl: ttl, tos: tos,
	}
	in.req.t1 = in.body[4:12]
	if req.stamp {
		in.req.tlvs, _ = parseTLVs(in.body)
	}
	if hold <= 0 {
		r.send(reply, s, in)
		return
	}
	time.AfterFunc(hold, func() {
		select {
		case <-r.stop:
			return
		default:
		}
		r.mu.Lock()
		r.pending--
		r.mu.Unlock()
		r.send(reply, s, in)
	})
}

// evict makes room for a new session: the oldest idle one goes first,
// else the least recently heard.
func (r *reflector) evict() {
	if len(r.sessions) < maxSessions {
		return
	}
	var victim *sess
	for _, s := range r.sessions {
		if victim == nil || (s.idle && !victim.idle) || (s.idle == victim.idle && s.last.Before(victim.last)) {
			victim = s
		}
	}
	delete(r.sessions, victim.key)
}

type replyIn struct {
	body         []byte
	req          request
	seq          uint32 // the reflector's
	t2           time.Time
	src, dst     netip.Addr
	sport, dport uint16
	ttl, tos     uint8
}

func (r *reflector) send(fr *pkt.Frame, s *sess, in replyIn) {
	r.mu.Lock()
	cfg := r.cfg
	seq := in.seq
	s.tx++
	r.tx++
	rx, tx := s.rx, s.tx
	followSeq, followT3 := s.followSeq, s.followT3
	r.mu.Unlock()

	n := len(in.body)
	if n < reflectorMin {
		n = reflectorMin
	}
	out := make([]byte, 8+n)
	m := out[8:]
	if len(in.body) > stampBase {
		copy(m[stampBase:], in.body[stampBase:])
	}
	ptp := in.req.ptp
	mult := cfg.ErrorMultiplier
	if mult == 0 {
		mult = 1 // 0 isn't allowed on the wire
	}
	errEst := uint16(cfg.ErrorScale)<<8 | uint16(mult)
	if cfg.Synchronized {
		errEst |= errS
	}
	if ptp {
		errEst |= errZ
	}
	binary.BigEndian.PutUint32(m, seq)
	binary.BigEndian.PutUint16(m[12:], errEst)
	binary.BigEndian.PutUint16(m[14:], in.req.ssid)
	binary.BigEndian.PutUint32(m[24:], in.req.seq)
	copy(m[28:36], in.req.t1)
	binary.BigEndian.PutUint16(m[36:], in.req.err)
	m[40] = in.ttl

	// The reply's DSCP: a Class of Service TLV's request, else config, else
	// what came in.
	dscp := in.tos >> 2
	if cfg.DSCP != nil {
		dscp = *cfg.DSCP
	}
	for _, t := range in.req.tlvs {
		v := m[t.off+4 : t.off+4+len(t.value)]
		known := true
		switch t.typ {
		case tlvExtraPadding:
		case tlvTimestamp:
			if len(v) < 4 {
				m[t.off] |= flagM
				continue
			}
			sync := uint8(5) // local free-running
			if cfg.Synchronized {
				sync = 1 // NTP
			}
			if ptp && cfg.Synchronized {
				sync = 2
			}
			v[0], v[1], v[2], v[3] = sync, 2, sync, 2 // software, local
		case tlvClassOfSvc:
			if len(v) < 4 {
				m[t.off] |= flagM
				continue
			}
			want := v[0] >> 2
			got, ecn := in.tos>>2, in.tos&3
			v[0] = want<<2 | got>>4
			v[1] = (got&0xf)<<4 | ecn<<2 // RP 0: the DSCP asked for is used
			dscp = want
		case tlvDirectMeas:
			if len(v) < 12 {
				m[t.off] |= flagM
				continue
			}
			binary.BigEndian.PutUint32(v[4:], uint32(rx))
			binary.BigEndian.PutUint32(v[8:], uint32(tx))
		case tlvFollowUp:
			if len(v) < 16 {
				m[t.off] |= flagM
				continue
			}
			binary.BigEndian.PutUint32(v, followSeq)
			stamp(v[4:12], followT3, ptp)
			v[12] = 2
		default:
			known = false
		}
		if known {
			m[t.off] &^= flagU
		}
	}

	t2 := in.t2
	t3 := time.Now()
	if t3.Before(t2) {
		t3 = t2
	}
	off := time.Duration(cfg.ClockOffsetMS) * time.Millisecond
	t2, t3 = t2.Add(off), t3.Add(off)
	switch cfg.BadTimestamps {
	case "reversed":
		t2, t3 = t3.Add(time.Millisecond), t2
	case "zero":
		t2, t3 = time.Time{}, time.Time{}
	}
	stamp(m[16:24], t2, ptp)
	stamp(m[4:12], t3, ptp)

	binary.BigEndian.PutUint16(out[0:], in.dport)
	binary.BigEndian.PutUint16(out[2:], in.sport)
	binary.BigEndian.PutUint16(out[4:], uint16(len(out)))
	cs := pkt.Checksum(out, pkt.PseudoSum(in.dst, in.src, pkt.ProtoUDP, len(out)))
	if cs == 0 {
		cs = 0xffff
	}
	binary.BigEndian.PutUint16(out[6:], cs)
	var ipb []byte
	et := pkt.EtherTypeIPv4
	if in.dst.Is4() {
		h := pkt.IPv4{TOS: dscp << 2, TTL: cfg.TTL, Protocol: pkt.ProtoUDP, Src: in.dst, Dst: in.src, Payload: out}
		ipb = h.Encode()
	} else {
		h := pkt.IPv6{TrafficClass: dscp << 2, NextHeader: pkt.ProtoUDP, HopLimit: cfg.TTL, Src: in.dst, Dst: in.src, Payload: out}
		ipb, et = h.Encode(), pkt.EtherTypeIPv6
	}
	err := r.env.IP().Reply(fr, in.src, et, ipb)
	r.mu.Lock()
	if err != nil {
		r.errs++
	}
	s.followSeq, s.followT3 = seq, t3
	if !t3.IsZero() && !t2.IsZero() {
		s.dwellUS = float64(t3.Sub(t2).Microseconds())
	}
	r.mu.Unlock()
}

type SessionStatus struct {
	Sender     string            `json:"sender"`
	Local      string            `json:"local"`
	SSID       uint16            `json:"ssid"`
	Kind       string            `json:"kind"` // stamp or twamp-light
	Timestamps string            `json:"timestamps"`
	State      string            `json:"state"` // active or idle
	First      time.Time         `json:"first"`
	Last       time.Time         `json:"last"`
	LastAgoS   float64           `json:"last_ago_s"`
	Rx         uint64            `json:"rx"`
	Tx         uint64            `json:"tx"`
	Dropped    uint64            `json:"dropped"`
	Burst      int               `json:"burst_remaining,omitempty"`
	Gaps       uint64            `json:"gaps"`
	Reordered  uint64            `json:"reordered"`
	Duplicates uint64            `json:"duplicates"`
	LastSeq    uint32            `json:"last_seq"`
	TTL        uint8             `json:"ttl"`
	DSCP       uint8             `json:"dscp"`
	DwellUS    float64           `json:"dwell_us"`
	TLVs       map[string]uint64 `json:"tlvs,omitempty"`
	// TLVLengthWithHeader: the sender's TLV length counts the TLV header
	// (not RFC 8972; SR OS 26.10 does it). Answered as sent.
	TLVLengthWithHeader bool `json:"tlv_length_with_header,omitempty"`
}

type Status struct {
	Mode     string          `json:"mode"`
	Ports    []uint16        `json:"ports"`
	Muted    bool            `json:"muted"`
	Faults   []string        `json:"faults,omitempty"`
	Active   int             `json:"active"`
	Rx       uint64          `json:"rx"`
	Tx       uint64          `json:"tx"`
	Dropped  uint64          `json:"dropped"`
	Refused  uint64          `json:"refused"`
	Errors   uint64          `json:"errors"`
	Pending  int             `json:"pending"`
	Sessions []SessionStatus `json:"sessions"`
}

func faults(c Config) []string {
	var f []string
	if c.DelayUS > 0 || c.JitterUS > 0 {
		s := fmt.Sprintf("delay %d µs", c.DelayUS)
		if c.JitterUS > 0 {
			s += fmt.Sprintf(" + jitter %d µs", c.JitterUS)
		}
		f = append(f, s)
	}
	if c.DwellUS > 0 {
		f = append(f, fmt.Sprintf("dwell %d µs", c.DwellUS))
	}
	if c.DropPct > 0 {
		f = append(f, fmt.Sprintf("drop %g%%", c.DropPct))
	}
	if c.LossDirection == "forward" {
		f = append(f, "losses forward")
	}
	if c.ClockOffsetMS != 0 {
		f = append(f, fmt.Sprintf("clock offset %d ms", c.ClockOffsetMS))
	}
	if c.BadTimestamps != "" {
		f = append(f, "timestamps "+c.BadTimestamps)
	}
	return f
}

func (r *reflector) Status() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	st := Status{Mode: r.cfg.Mode, Ports: r.cfg.Ports, Muted: r.muted, Faults: faults(r.cfg),
		Rx: r.rx, Tx: r.tx, Dropped: r.dropped, Refused: r.refused, Errors: r.errs, Pending: r.pending, Sessions: []SessionStatus{}}
	for _, s := range r.sessions {
		ss := SessionStatus{
			Sender: s.key.src.String(), Local: s.key.dst.String(), SSID: s.key.ssid, Kind: "twamp-light", Timestamps: "ntp",
			State: "active", First: s.first, Last: s.last, LastAgoS: now.Sub(s.last).Seconds(),
			Rx: s.rx, Tx: s.tx, Dropped: s.dropped, Burst: s.burst, Gaps: s.gaps, Reordered: s.reordered, Duplicates: s.duplicates,
			LastSeq: s.lastSeq, TTL: s.lastTTL, DSCP: s.lastDSCP, DwellUS: s.dwellUS, TLVLengthWithHeader: s.tlvLenHdr,
		}
		if s.stamp {
			ss.Kind = "stamp"
		}
		if s.ptp {
			ss.Timestamps = "ptp"
		}
		if s.idle {
			ss.State = "idle"
		} else {
			st.Active++
		}
		if len(s.tlvs) > 0 {
			ss.TLVs = map[string]uint64{}
			for k, v := range s.tlvs {
				ss.TLVs[k] = v
			}
		}
		st.Sessions = append(st.Sessions, ss)
	}
	sort.Slice(st.Sessions, func(i, j int) bool {
		a, b := st.Sessions[i], st.Sessions[j]
		if a.Sender != b.Sender {
			return a.Sender < b.Sender
		}
		return a.SSID < b.SSID
	})
	return st
}

// Health: muted, or how many sessions are active.
func (r *reflector) Health() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.muted {
		return "muted"
	}
	n := 0
	for _, s := range r.sessions {
		if !s.idle {
			n++
		}
	}
	switch n {
	case 0:
		return "no sessions"
	case 1:
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

func (r *reflector) Action(_ context.Context, name string, args any) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch name {
	case "mute":
		r.muted = true
		go r.env.Emit("muted", "reflector muted: no replies", nil)
	case "unmute":
		r.muted = false
		go r.env.Emit("unmuted", "reflector answering again", nil)
	case "burst-loss":
		a := args.(*BurstArgs)
		n := 0
		for _, s := range r.sessions {
			if !s.idle {
				s.burst += a.Count
				n++
			}
		}
		if n == 0 {
			return nil, errors.New("no active session")
		}
		go r.env.Emit("burst-loss", fmt.Sprintf("dropping the next %d replies of %d sessions", a.Count, n), nil)
		return map[string]any{"sessions": n}, nil
	case "clear":
		r.sessions = map[sessionKey]*sess{}
		r.rx, r.tx, r.dropped, r.refused, r.errs = 0, 0, 0, 0, 0
		return map[string]any{"cleared": true}, nil
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{"muted": r.muted}, nil
}

func (r *reflector) Stop() {
	close(r.stop)
	<-r.done
}
