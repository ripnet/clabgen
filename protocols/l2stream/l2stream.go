// Package l2stream is raw Ethernet traffic with no IP stack: frames of a
// chosen ethertype from one or many source MACs to a destination MAC,
// each carrying the flow signature, for testing L2 services. Receiving
// clabgen devices count them one way (loss, reordering, latency,
// outages); with reply "echo", an l2-echo responder on the far device
// sends them back for round-trip loss, outage and RTT, and pass/fail
// checks.
package l2stream

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/flowrx"
	"github.com/ripnet/clabgen/internal/flowstats"
	"github.com/ripnet/clabgen/internal/ipflow"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "l2"

// DefaultEtherType is IEEE 802 local experimental ethertype 1.
const DefaultEtherType = 0x88b5

// The payload: the flow signature, a kind byte, then the fill pattern.
const (
	kindOneWay  byte = 0
	kindRequest byte = 1 // please echo
	kindReply   byte = 2

	headerLen = flowrx.SigLen + 1
)

func init() { protocol.Register(streamType{}) }

type Config struct {
	DstMAC      string `json:"dst_mac" jsonschema:"description=Destination MAC: the far device's (its Devices tab) for unicast; default broadcast" jsonschema_extras:"x-group=Frames"`
	SrcMAC      string `json:"src_mac,omitempty" jsonschema:"description=First source MAC. Default: the device's" jsonschema_extras:"x-group=Frames"`
	SrcMACCount int    `json:"src_mac_count" jsonschema:"minimum=1,maximum=65536" jsonschema_description:"Cycle through this many source MACs from src_mac (one per frame), e.g. to fill MAC tables" jsonschema_extras:"x-group=Frames"`
	EtherType   uint16 `json:"ethertype" jsonschema:"minimum=1536,maximum=65535" jsonschema_description:"Ethertype (default 0x88b5; not IP, ARP, CFM, LLDP, LACP, PTP or VLAN tags)" jsonschema_extras:"x-group=Frames"`
	PCP         *uint8 `json:"vlan_pcp,omitempty" jsonschema:"minimum=0,maximum=7,description=PCP on every VLAN tag. Default: the device's" jsonschema_extras:"x-group=Frames"`
	FrameSize   int    `json:"frame_size" jsonschema:"minimum=60,maximum=9018" jsonschema_description:"Bytes per frame without the FCS, VLAN tags included (60 is a 64-byte frame on the wire)" jsonschema_extras:"x-group=Frames"`
	Pattern     string `json:"payload_pattern" jsonschema:"enum=increment,enum=zeros,enum=random,description=Fill after the signature" jsonschema_extras:"x-group=Frames,x-advanced=true"`
	Reply       string `json:"reply" jsonschema:"enum=none,enum=echo" jsonschema_description:"echo: an l2-echo responder on the far device sends each frame back, for round-trip loss, outage, RTT and checks" jsonschema_extras:"x-group=Frames"`

	ipflow.Run

	Checks ipflow.Checks `json:"checks" jsonschema:"description=Pass/fail limits on what the stream measures (echo) since it started or was cleared"`
}

func defaultConfig() *Config {
	return &Config{DstMAC: "ff:ff:ff:ff:ff:ff", SrcMACCount: 1, EtherType: DefaultEtherType, FrameSize: 60,
		Pattern: "increment", Reply: "none", Run: ipflow.Run{Mode: "continuous", Rate: 10, Timeout: "1s"}}
}

// reserved ethertypes: other protocols (and the IP stacks) own them.
var reserved = map[uint16]string{pkt.EtherTypeIPv4: "IPv4", pkt.EtherTypeIPv6: "IPv6", pkt.EtherTypeARP: "ARP",
	pkt.EtherTypeCFM: "CFM", 0x88cc: "LLDP", 0x8809: "slow protocols (LACP, OAM)", 0x88f7: "PTP", 0x8100: "802.1Q", 0x88a8: "802.1ad", 0x9100: "QinQ"}

func (c *Config) Validate() error {
	if _, err := pkt.ParseMAC(c.DstMAC); err != nil {
		return fmt.Errorf("dst_mac: %w", err)
	}
	if c.SrcMAC != "" {
		m, err := pkt.ParseMAC(c.SrcMAC)
		if err != nil {
			return fmt.Errorf("src_mac: %w", err)
		}
		if m.IsMulticast() {
			return errors.New("src_mac must be a unicast MAC")
		}
	}
	if c.SrcMACCount < 1 || c.SrcMACCount > 65536 {
		return errors.New("src_mac_count must be 1-65536")
	}
	if c.EtherType < 0x0600 {
		return errors.New("ethertype must be at least 0x0600 (smaller values are lengths)")
	}
	if n, ok := reserved[c.EtherType]; ok {
		return fmt.Errorf("ethertype %#04x is %s; pick another (default 0x88b5)", c.EtherType, n)
	}
	if c.PCP != nil && *c.PCP > 7 {
		return errors.New("vlan_pcp must be 0-7")
	}
	if c.FrameSize < 60 || c.FrameSize > 9018 {
		return errors.New("frame_size must be 60-9018")
	}
	switch c.Pattern {
	case "increment", "zeros", "random":
	default:
		return fmt.Errorf("invalid payload_pattern %q", c.Pattern)
	}
	if c.Reply != "none" && c.Reply != "echo" {
		return errors.New("reply must be none or echo")
	}
	if err := c.Run.Validate(); err != nil {
		return err
	}
	return c.Checks.Validate()
}

type streamType struct{}

func (streamType) Name() string { return TypeName }
func (streamType) Description() string {
	return "Raw L2 stream (no IP): frames of any ethertype from one or many source MACs, counted at clabgen receivers or echoed back by l2-echo"
}
func (streamType) Category() string           { return protocol.CategoryTraffic }
func (streamType) NewConfig() any             { return defaultConfig() }
func (streamType) EtherTypes() []uint16       { return nil }
func (streamType) Actions() []protocol.Action { return ipflow.Actions() }

// EtherTypesFor: echo replies come back with the stream's ethertype.
func (streamType) EtherTypesFor(c any) []uint16 {
	if cfg := c.(*Config); cfg.Reply == "echo" {
		return []uint16{cfg.EtherType}
	}
	return nil
}

type stream struct {
	env    protocol.Env
	cfg    Config
	dst    pkt.MAC
	src    pkt.MAC // first source MAC
	flowID uint32
	echo   bool

	release, unclaim func()

	mu      sync.Mutex
	state   string
	lastErr string
	n       uint64
	started time.Time
	stopped time.Time
	tracker *flowstats.Tracker
	cancel  context.CancelFunc
	done    chan struct{}
}

func (streamType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	f := &stream{env: env, cfg: cfg, echo: cfg.Reply == "echo", state: ipflow.StateIdle,
		flowID:  binary.BigEndian.Uint32(ipflow.Hash(env, 6)),
		tracker: flowstats.New(cfg.Interval(), cfg.TimeoutD()), release: func() {}, unclaim: func() {}}
	f.dst, _ = pkt.ParseMAC(cfg.DstMAC)
	f.src = env.Device().MAC()
	if cfg.SrcMAC != "" {
		f.src, _ = pkt.ParseMAC(cfg.SrcMAC)
	}
	if room := 14 + 4*len(env.Device().VLANs()) + headerLen; cfg.FrameSize < room {
		return nil, fmt.Errorf("frame_size %d is too small for the device's VLAN tags and the signature (at least %d)", cfg.FrameSize, room)
	}
	// A bridge would learn the destination on our side and send it back.
	for i := 0; i < cfg.SrcMACCount && !f.dst.IsMulticast(); i++ {
		if f.srcFor(uint64(i)) == f.dst {
			return nil, fmt.Errorf("dst_mac %s is one of the source MACs (%s and the %d after it)", f.dst, f.src, cfg.SrcMACCount-1)
		}
	}
	if ip := env.IP(); ip != nil {
		f.release = ip.ClaimFlow(f.flowID) // our echoes aren't "received here"
	}
	// Echoes to source MACs other than the device's must reach us.
	if mc, ok := env.(protocol.MACClaimer); ok && f.echo && (f.src != env.Device().MAC() || cfg.SrcMACCount > 1) {
		macs := make([]pkt.MAC, cfg.SrcMACCount)
		for i := range macs {
			macs[i] = f.srcFor(uint64(i))
		}
		f.unclaim = mc.ClaimMACs(macs)
	}
	if cfg.AutoStart {
		f.start()
	}
	return f, nil
}

// srcFor is frame n's source MAC: the first one counted up (mod count).
func (f *stream) srcFor(n uint64) pkt.MAC {
	if f.cfg.SrcMACCount <= 1 {
		return f.src
	}
	var b [8]byte
	copy(b[2:], f.src[:])
	v := binary.BigEndian.Uint64(b[:]) + n%uint64(f.cfg.SrcMACCount)
	binary.BigEndian.PutUint64(b[:], v)
	var m pkt.MAC
	copy(m[:], b[2:])
	m[0] &^= 1 // stay unicast
	return m
}

func (f *stream) payloadLen() int { return f.cfg.FrameSize - 14 - 4*len(f.env.Device().VLANs()) }

// build returns frame n's payload (after the ethertype).
func (f *stream) build(n uint64, now time.Time) []byte {
	b := make([]byte, f.payloadLen())
	switch f.cfg.Pattern {
	case "increment":
		for i := headerLen; i < len(b); i++ {
			b[i] = byte(i)
		}
	case "random":
		rand.Read(b[headerLen:])
	}
	copy(b, flowrx.SigMagic[:])
	binary.BigEndian.PutUint32(b[4:], f.flowID)
	binary.BigEndian.PutUint64(b[8:], n)
	binary.BigEndian.PutUint64(b[16:], uint64(now.UnixNano()))
	b[flowrx.SigLen] = kindOneWay
	if f.echo {
		b[flowrx.SigLen] = kindRequest
	}
	return b
}

func (f *stream) opts(n uint64) protocol.SendOpts {
	o := protocol.SendOpts{Src: f.srcFor(n)}
	if f.cfg.PCP != nil {
		o.SetPCP, o.PCP = true, *f.cfg.PCP
	}
	return o
}

func (f *stream) start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		return errors.New("already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel, f.done = cancel, make(chan struct{})
	f.state, f.lastErr, f.started, f.stopped = ipflow.StateRunning, "", time.Now(), time.Time{}
	go f.run(ctx, f.done)
	f.env.Emit("flow-started", fmt.Sprintf("sending to %s at %g fps", f.dst, f.cfg.Rate), nil)
	return nil
}

func (f *stream) halt(final string) {
	f.mu.Lock()
	cancel, done := f.cancel, f.done
	f.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	f.mu.Lock()
	f.cancel, f.state, f.stopped = nil, final, time.Now()
	f.mu.Unlock()
}

func (f *stream) Stop() {
	f.halt(ipflow.StateIdle)
	f.release()
	f.unclaim()
}

// run paces frames like IP flows do (catching up after a stall, skipping
// far behind instead of bursting).
func (f *stream) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	if !f.cfg.Run.Pace(ctx, f.sendOne, func(now time.Time) { f.tracker0().Expire(now, false) }) {
		f.tracker0().Expire(time.Now(), false)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(f.cfg.TimeoutD()):
	}
	f.tracker0().Expire(time.Now(), true)
	f.mu.Lock()
	f.state, f.stopped, f.cancel = ipflow.StateDone, time.Now(), nil
	f.mu.Unlock()
	f.env.Emit("flow-done", "finished", nil)
}

func (f *stream) tracker0() *flowstats.Tracker {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tracker
}

func (f *stream) sendOne() {
	now := time.Now()
	f.mu.Lock()
	n := f.n
	f.n++
	tr := f.tracker
	f.mu.Unlock()
	b := f.build(n, now)
	if f.echo {
		tr.Sent(n, now) // before sending: the echo can beat us otherwise
	}
	if err := f.env.Send(f.dst, f.cfg.EtherType, b, f.opts(n)); err != nil {
		tr.TxError(n, now, err)
		return
	}
	if !f.echo {
		tr.SentNoReply(now)
	}
}

// HandleRx takes our echoes back.
func (f *stream) HandleRx(fr *pkt.Frame) {
	if !f.echo || fr.EtherType != f.cfg.EtherType || len(fr.Payload) < headerLen || fr.Payload[flowrx.SigLen] != kindReply {
		return
	}
	id, seq, sent, ok := flowrx.ParseSignature(fr.Payload)
	if !ok || id != f.flowID {
		return
	}
	f.tracker0().Received(seq, ipflow.RxTime(fr), sent, nil)
}

// ---- status and actions ----

func (f *stream) Status() any {
	now := time.Now()
	tr := f.tracker0()
	st := tr.Snapshot(now, 300)
	f.mu.Lock()
	defer f.mu.Unlock()
	kind := fmt.Sprintf("%#04x", f.cfg.EtherType)
	if f.echo {
		kind += " echo"
	}
	src := f.src.String()
	if f.cfg.SrcMACCount > 1 {
		src = fmt.Sprintf("%s ×%d", f.src, f.cfg.SrcMACCount)
	}
	s := ipflow.Base{State: f.state, Error: f.lastErr, Destination: f.dst.String(), Source: src, Kind: kind,
		Rate: f.cfg.Rate, Mode: f.cfg.Mode, FlowID: ipflow.FlowIDString(f.flowID), FrameSize: f.cfg.FrameSize, TrackReply: f.echo, Stats: st}
	if f.state != ipflow.StateRunning {
		s.OutageCurrentMs = 0
	}
	s.Verdict = f.cfg.Checks.Evaluate(s.Stats, f.echo)
	if !f.started.IsZero() {
		e := now
		if !f.stopped.IsZero() {
			e = f.stopped
		}
		s.Elapsed = e.Sub(f.started).Seconds()
	}
	return s
}

func (f *stream) Action(_ context.Context, name string, _ any) (any, error) {
	switch name {
	case "start":
		if err := f.start(); err != nil {
			return nil, err
		}
		return map[string]any{"started": true}, nil
	case "stop":
		f.mu.Lock()
		running := f.cancel != nil
		f.mu.Unlock()
		if !running {
			return nil, errors.New("not running")
		}
		f.halt(ipflow.StateIdle)
		tr := f.tracker0()
		time.AfterFunc(f.cfg.TimeoutD(), func() { tr.Expire(time.Now(), false) })
		f.env.Emit("flow-stopped", "stopped", nil)
		return map[string]any{"stopped": true}, nil
	case "clear":
		f.mu.Lock()
		f.tracker = flowstats.New(f.cfg.Interval(), f.cfg.TimeoutD())
		f.mu.Unlock()
		return map[string]any{"cleared": true}, nil
	case "preview":
		f.mu.Lock()
		n := f.n
		f.mu.Unlock()
		d := f.env.Device()
		tags := d.VLANs()
		if f.cfg.PCP != nil {
			tags = append([]pkt.VLANTag(nil), tags...)
			for i := range tags {
				tags[i].PCP = *f.cfg.PCP
			}
		}
		src := f.srcFor(n)
		fr := pkt.Frame{Dst: f.dst, Src: src, Tags: tags, EtherType: f.cfg.EtherType, Payload: f.build(n, time.Now())}
		enc := fr.Encode()
		return ipflow.PreviewResult{Hex: hex.EncodeToString(enc), Length: len(enc),
			Summary: fmt.Sprintf("%s > %s, ethertype %#04x, frame %d bytes (+4 FCS), seq %d", src, f.dst, f.cfg.EtherType, len(enc), n)}, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

// Health is the stream's state (protocol.Healther).
func (f *stream) Health() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}
