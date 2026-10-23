// Package packet is the packet builder: a traffic flow of frames made of
// layers (Ethernet, VLAN, MPLS, ARP, IPv4/6, UDP, TCP, ICMP, VXLAN, raw
// bytes, payload) with every field settable, fields changed per packet by
// modifiers, or the frames of an imported pcap replayed.
package packet

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/flowstats"
	"github.com/ripnet/clabgen/internal/ipflow"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "packet"

func init() { protocol.Register(packetType{}) }

type Config struct {
	Layers    []Layer    `json:"layers" jsonschema_description:"The packet, outermost header first: the device adds its MAC and VLAN tags to the first Ethernet header (written or not)" jsonschema_extras:"x-summary={kind}"`
	Modifiers []Modifier `json:"modifiers,omitempty" jsonschema:"description=Fields changed from packet to packet" jsonschema_extras:"x-summary={field} {mode}"`
	Pcap      *Pcap      `json:"pcap,omitempty" jsonschema:"description=Replay the frames of a capture instead of building from layers"`
	PCP       *uint8     `json:"vlan_pcp,omitempty" jsonschema:"minimum=0,maximum=7,description=PCP on the device's VLAN tags. Default: the device's"`
	ipflow.Run
}

type Pcap struct {
	Data   string `json:"data" jsonschema:"description=The capture (pcap or pcapng; Ethernet) as base64"`
	Frames string `json:"frames,omitempty" jsonschema_description:"Which frames, 1-based: e.g. 1-10,15. Default: all"`
	SrcMAC string `json:"src_mac,omitempty" jsonschema:"enum=device,enum=keep,description=device (default): send from the device's MAC; keep: the captured one"`
	VLANs  string `json:"vlans,omitempty" jsonschema_description:"replace (default): drop the captured VLAN tags (the device adds its own); keep: keep them inside the device's" jsonschema:"enum=replace,enum=keep"`
}

func (c *Config) Validate() error {
	if err := c.Run.Validate(); err != nil {
		return err
	}
	if c.PCP != nil && *c.PCP > 7 {
		return errors.New("vlan_pcp must be 0-7")
	}
	for _, m := range c.Modifiers {
		if err := m.check(); err != nil {
			return err
		}
	}
	if c.Pcap != nil && c.Pcap.Data != "" {
		if len(c.Modifiers) > 0 {
			return errors.New("modifiers apply to layers, not to a pcap replay")
		}
		_, err := c.Pcap.frames()
		return err
	}
	if err := checkLayers(c.Layers); err != nil {
		return err
	}
	// Everything that doesn't need the device: build with stand-ins.
	t, err := build(c.Layers, buildCtx{source: func(dst netip.Addr) (netip.Addr, bool) {
		if dst.Is4() {
			return netip.MustParseAddr("192.0.2.1"), true
		}
		return netip.MustParseAddr("2001:db8::1"), true
	}})
	if err != nil {
		return err
	}
	_, err = compile(c.Modifiers, t)
	return err
}

// frames decodes the capture and picks the configured frames.
func (p *Pcap) frames() ([][]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(p.Data))
	if err != nil {
		return nil, fmt.Errorf("pcap data: not base64: %w", err)
	}
	all, err := readPcap(raw)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, errors.New("pcap: no frames")
	}
	if p.SrcMAC != "" && p.SrcMAC != "device" && p.SrcMAC != "keep" {
		return nil, errors.New("pcap src_mac: device or keep")
	}
	if p.VLANs != "" && p.VLANs != "replace" && p.VLANs != "keep" {
		return nil, errors.New("pcap vlans: replace or keep")
	}
	if p.Frames == "" {
		return all, nil
	}
	var out [][]byte
	for _, part := range strings.Split(p.Frames, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err1 := strconv.Atoi(lo)
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.Atoi(hi)
		}
		if err1 != nil || err2 != nil || a < 1 || b < a || b > len(all) {
			return nil, fmt.Errorf("pcap frames %q: 1-based indices and ranges up to %d", p.Frames, len(all))
		}
		out = append(out, all[a-1:b]...)
	}
	return out, nil
}

type packetType struct{}

func (packetType) Name() string { return TypeName }
func (packetType) Description() string {
	return "Packet builder: frames of any layers with every field settable, modifiers per packet, or a pcap replayed"
}
func (packetType) Category() string     { return protocol.CategoryTraffic }
func (packetType) EtherTypes() []uint16 { return nil }
func (packetType) NewConfig() any {
	return &Config{Layers: []Layer{{Ethernet: &Ethernet{}}, {Payload: &Payload{Size: 46}}}, Run: ipflow.DefaultRun}
}
func (packetType) Actions() []protocol.Action {
	acts := ipflow.Actions()
	for i := range acts {
		if acts[i].Name == "preview" {
			acts[i] = protocol.Action{Name: "preview", Description: "Show a packet as hex and by layer, without sending it", NewArgs: func() any { return &PreviewArgs{} }}
		}
	}
	return acts
}

type PreviewArgs struct {
	Index *uint64 `json:"index,omitempty" jsonschema:"description=Which packet (0-based; modifiers change fields per packet). Default: the next one"`
}

// A frame to send: dst, src, ethertype and what follows (no device tags).
type unit struct {
	dst, src pkt.MAC
	et       uint16
	payload  []byte
}

type stream struct {
	env     protocol.Env
	cfg     Config
	flowID  uint32
	release func()

	mu      sync.Mutex
	state   string
	lastErr string
	errAt   time.Time
	tpl     *template
	mods    []mod
	frames  []unit // pcap replay
	n       uint64
	started time.Time
	stopped time.Time
	tracker *flowstats.Tracker
	cancel  context.CancelFunc
	done    chan struct{}
}

func (packetType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	f := &stream{env: env, cfg: cfg, state: ipflow.StateIdle, release: func() {},
		flowID:  binary.BigEndian.Uint32(ipflow.Hash(env, 10)),
		tracker: flowstats.New(cfg.Interval(), cfg.TimeoutD())}
	if cfg.Pcap != nil && cfg.Pcap.Data != "" {
		fs, err := cfg.Pcap.frames()
		if err != nil {
			return nil, err
		}
		f.frames = units(fs, cfg.Pcap)
	}
	if ip := env.IP(); ip != nil {
		f.release = ip.ClaimFlow(f.flowID)
	}
	if cfg.AutoStart {
		f.start()
	}
	return f, nil
}

// units prepares captured frames for sending.
func units(fs [][]byte, p *Pcap) []unit {
	var out []unit
	for _, b := range fs {
		if len(b) < 14 {
			continue
		}
		u := unit{dst: pkt.MAC(b[0:6]), src: pkt.MAC(b[6:12])}
		off := 12
		if p.VLANs != "keep" {
			for off+4 <= len(b) && pkt.IsTPID(binary.BigEndian.Uint16(b[off:])) {
				off += 4
			}
		}
		u.et = binary.BigEndian.Uint16(b[off:])
		u.payload = b[off+2:]
		if p.SrcMAC != "keep" {
			u.src = pkt.MAC{} // the device's
		}
		out = append(out, u)
	}
	return out
}

func (f *stream) ctx() buildCtx {
	cx := buildCtx{devMAC: f.env.Device().MAC(), source: func(netip.Addr) (netip.Addr, bool) { return netip.Addr{}, false },
		port: 49152 + binary.BigEndian.Uint16(ipflow.Hash(f.env, 14))%16384}
	if ip := f.env.IP(); ip != nil {
		cx.source = ip.Source
	}
	return cx
}

// prepare resolves the destination MAC when the layers leave it to the
// device, then builds the template.
func (f *stream) prepare(ctx context.Context, cx buildCtx) (*template, []mod, string, error) {
	ls := f.cfg.Layers
	note := ""
	if (ls[0].Ethernet == nil || ls[0].Ethernet.Dst == "") && f.env.IP() != nil {
		if dst, ok := firstIPDst(ls); ok {
			nh, m, err := f.env.IP().ResolveNextHop(ctx, dst)
			if err != nil {
				return nil, nil, "", resolveError{fmt.Errorf("resolving the next hop for %s: %w (or set the Ethernet dst)", dst, err)}
			}
			cx.dstMAC = m
			if nh != dst {
				note = "via " + nh.String()
			}
		}
	}
	t, err := build(ls, cx)
	if err != nil {
		return nil, nil, "", err
	}
	mods, err := compile(f.cfg.Modifiers, t)
	return t, mods, note, err
}

// resolveError: the next hop didn't resolve (a preview shows the packet anyway).
type resolveError struct{ error }

func (f *stream) start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		return errors.New("already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel, f.done = cancel, make(chan struct{})
	f.state, f.lastErr, f.started, f.stopped = ipflow.StateResolving, "", time.Now(), time.Time{}
	go f.run(ctx, f.done)
	f.env.Emit("flow-started", fmt.Sprintf("sending at %g pps", f.cfg.Rate), nil)
	return nil
}

func (f *stream) halt(final string) {
	f.mu.Lock()
	cancel, done := f.cancel, f.done
	f.cancel = nil
	f.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	f.mu.Lock()
	f.state, f.stopped = final, time.Now()
	f.mu.Unlock()
}

// HandleRx: the stream only sends (receivers count it by its signature).
func (f *stream) HandleRx(*pkt.Frame) {}

func (f *stream) Stop() {
	f.halt(ipflow.StateIdle)
	f.release()
}

func (f *stream) setErr(msg string) {
	f.mu.Lock()
	f.lastErr, f.errAt = msg, time.Now()
	f.mu.Unlock()
}

func (f *stream) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	if f.frames == nil {
		for {
			t, mods, _, err := f.prepare(ctx, f.ctx())
			if err == nil {
				f.mu.Lock()
				f.tpl, f.mods = t, mods
				f.mu.Unlock()
				break
			}
			f.setErr(err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
	f.mu.Lock()
	f.state, f.lastErr = ipflow.StateRunning, ""
	f.mu.Unlock()
	if !f.cfg.Run.Pace(ctx, f.sendOne, func(now time.Time) { f.tracker0().Expire(now, false) }) {
		return
	}
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

// packetAt makes packet n. Caller holds mu.
func (f *stream) packetAt(n uint64, now time.Time) unit {
	if f.frames != nil {
		return f.frames[n%uint64(len(f.frames))]
	}
	return f.tpl.packet(f.mods, n, f.flowID, now)
}

func (t *template) packet(mods []mod, n uint64, flowID uint32, now time.Time) unit {
	b := append([]byte(nil), t.frame...)
	if t.idField != nil {
		t.idField.set(b, new(big.Int).SetUint64(n))
	}
	for _, m := range mods {
		m.f.set(b, m.value(n))
	}
	if t.sigOff >= 0 {
		s := b[t.sigOff:]
		copy(s, ipflow.SigMagic[:])
		binary.BigEndian.PutUint32(s[4:], flowID)
		binary.BigEndian.PutUint64(s[8:], n)
		binary.BigEndian.PutUint64(s[16:], uint64(now.UnixNano()))
	}
	t.finish(b)
	return unit{dst: pkt.MAC(b[0:6]), src: pkt.MAC(b[6:12]), et: binary.BigEndian.Uint16(b[12:]), payload: b[14:]}
}

func (f *stream) sendOne() {
	now := time.Now()
	f.mu.Lock()
	n := f.n
	f.n++
	u := f.packetAt(n, now)
	tr := f.tracker
	f.mu.Unlock()
	opt := protocol.SendOpts{Src: u.src}
	if f.cfg.PCP != nil {
		opt.SetPCP, opt.PCP = true, *f.cfg.PCP
	}
	if err := f.env.Send(u.dst, u.et, u.payload, opt); err != nil {
		tr.TxError(n, now, err)
		return
	}
	tr.SentNoReply(now)
}

// ---- status ----

func (f *stream) Status() any {
	now := time.Now()
	st := f.tracker0().Snapshot(now, 300)
	f.mu.Lock()
	defer f.mu.Unlock()
	s := ipflow.Base{State: f.state, Rate: f.cfg.Rate, Mode: f.cfg.Mode, FlowID: ipflow.FlowIDString(f.flowID), Stats: st}
	if f.state != ipflow.StateResolving && f.lastErr != "" && now.Sub(f.errAt) < time.Minute {
		s.LastError, s.LastErrorS = f.lastErr, now.Sub(f.errAt).Seconds()
	} else if f.state == ipflow.StateResolving {
		s.Error = f.lastErr
	}
	if f.frames != nil {
		s.Kind = fmt.Sprintf("pcap · %d frames", len(f.frames))
		s.Destination = f.frames[0].dst.String()
	} else {
		s.Kind, s.Destination, s.Source = describe(f.cfg.Layers)
		if f.tpl != nil {
			s.FrameSize = max(len(f.tpl.frame)+4*len(f.env.Device().VLANs()), 60)
			s.DstMAC = pkt.MAC(f.tpl.frame[0:6]).String()
			// The addresses as built (defaults filled in), before modifiers.
			for _, r := range f.tpl.regions {
				if r.Kind == "ipv4" || r.Kind == "ipv6" {
					for _, fd := range r.Fields {
						switch fd.Name {
						case "src":
							s.Source = fd.format(f.tpl.frame)
						case "dst":
							s.Destination = fd.format(f.tpl.frame)
						}
					}
					break
				}
			}
		}
	}
	if !f.started.IsZero() {
		e := now
		if !f.stopped.IsZero() {
			e = f.stopped
		}
		s.Elapsed = e.Sub(f.started).Seconds()
	}
	if f.state != ipflow.StateRunning {
		s.OutageCurrentMs = 0
	}
	return s
}

// describe names the packet: "Ethernet/IPv4/UDP" and its endpoints.
func describe(ls []Layer) (kind, dst, src string) {
	var names []string
	for _, l := range ls {
		k := l.kind()
		names = append(names, titles[k])
		switch {
		case l.IPv4 != nil && dst == "":
			dst, src = l.IPv4.Dst, l.IPv4.Src
		case l.IPv6 != nil && dst == "":
			dst, src = l.IPv6.Dst, l.IPv6.Src
		case l.ARP != nil && dst == "":
			dst = l.ARP.TargetIP
		}
	}
	if ls[0].Ethernet == nil {
		names = append([]string{"Ethernet"}, names...)
	}
	if dst == "" {
		dst = "ff:ff:ff:ff:ff:ff"
		if e := ls[0].Ethernet; e != nil && e.Dst != "" {
			dst = e.Dst
		}
	}
	return strings.Join(names, "/"), dst, src
}

func (f *stream) Health() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// ---- actions ----

func (f *stream) Action(ctx context.Context, name string, args any) (any, error) {
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
		f.env.Emit("flow-stopped", "stopped", nil)
		return map[string]any{"stopped": true}, nil
	case "clear":
		f.mu.Lock()
		f.tracker = flowstats.New(f.cfg.Interval(), f.cfg.TimeoutD())
		f.mu.Unlock()
		return map[string]any{"cleared": true}, nil
	case "preview":
		var idx *uint64
		if a, ok := args.(*PreviewArgs); ok && a != nil {
			idx = a.Index
		}
		f.mu.Lock()
		n := f.n
		f.mu.Unlock()
		if idx != nil {
			n = *idx
		}
		return preview(ctx, f, n)
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

// Preview shows what a config would send (the packet editor's live view).
func (packetType) Preview(ctx context.Context, env protocol.Env, c any, args map[string]any) (any, error) {
	cfg := *c.(*Config)
	f := &stream{env: env, cfg: cfg, flowID: binary.BigEndian.Uint32(ipflow.Hash(env, 10))}
	if cfg.Pcap != nil && cfg.Pcap.Data != "" {
		fs, err := cfg.Pcap.frames()
		if err != nil {
			return nil, err
		}
		f.frames = units(fs, cfg.Pcap)
		return previewPcap(f, fs, index(args))
	}
	return preview(ctx, f, index(args))
}

func index(args map[string]any) uint64 {
	switch v := args["index"].(type) {
	case float64:
		if v >= 0 {
			return uint64(v)
		}
	case string:
		n, _ := strconv.ParseUint(v, 10, 64)
		return n
	}
	return 0
}

// PreviewResult is a packet as sent, with its layers and fields located.
type PreviewResult struct {
	Hex     string         `json:"hex"`
	Length  int            `json:"length"`
	Summary string         `json:"summary"`
	Index   uint64         `json:"index"`
	Note    string         `json:"note,omitempty"`
	Layers  []PreviewLayer `json:"layers"`
	Frames  []PreviewFrame `json:"frames,omitempty"`  // pcap: the frames
	Decoded []Layer        `json:"decoded,omitempty"` // pcap: this frame as layers
}

type PreviewLayer struct {
	Layer  int            `json:"layer"` // config index; -1: implicit Ethernet, -2: the device's VLAN tags
	Title  string         `json:"title"`
	Offset int            `json:"offset"`
	Length int            `json:"length"`
	Fields []PreviewField `json:"fields"`
}

type PreviewField struct {
	Name   string `json:"name"`
	Offset int    `json:"bit_offset"` // in the frame
	Bits   int    `json:"bits"`
	Value  string `json:"value"`
}

type PreviewFrame struct {
	Index   int    `json:"index"`
	Length  int    `json:"length"`
	Summary string `json:"summary"`
}

func preview(ctx context.Context, f *stream, n uint64) (any, error) {
	cx := f.ctx()
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	t, mods, note, err := f.prepare(pctx, cx)
	cancel()
	if re := (resolveError{}); errors.As(err, &re) {
		// Show the packet anyway, toward broadcast.
		note = "next hop unresolved: " + err.Error()
		t, err = build(f.cfg.Layers, cx)
		if err == nil {
			mods, err = compile(f.cfg.Modifiers, t)
		}
	}
	if err != nil {
		return nil, err
	}
	u := t.packet(mods, n, f.flowID, time.Now())
	frame := u.frameBytes(f)
	r := PreviewResult{Hex: hex.EncodeToString(frame), Length: len(frame), Index: n, Note: note}
	r.Layers = locate(t.regions, frame, len(f.env.Device().VLANs()))
	r.Summary = summary(r.Layers, len(frame))
	return r, nil
}

// frameBytes is the frame as it goes on the wire: with the device's tags.
func (u unit) frameBytes(f *stream) []byte {
	src := u.src
	if src.IsZero() {
		src = f.env.Device().MAC()
	}
	tags := append([]pkt.VLANTag(nil), f.env.Device().VLANs()...)
	if f.cfg.PCP != nil {
		for i := range tags {
			tags[i].PCP = *f.cfg.PCP
		}
	}
	fr := pkt.Frame{Dst: u.dst, Src: src, Tags: tags, EtherType: u.et, Payload: u.payload}
	return fr.Encode()
}

// locate maps template regions onto the sent frame (device tags shift
// everything after the source MAC).
func locate(rs []region, frame []byte, tags int) []PreviewLayer {
	shift := func(bytePos int) int {
		if bytePos >= 12 {
			return bytePos + 4*tags
		}
		return bytePos
	}
	var out []PreviewLayer
	for i, r := range rs {
		pl := PreviewLayer{Layer: r.Layer, Title: r.Title, Offset: shift(r.Off), Length: r.Len}
		if i == 0 && tags > 0 {
			pl.Length = 12
		}
		for _, fd := range r.Fields {
			g := fd
			g.Pos = shift(fd.Pos/8)*8 + fd.Pos%8
			if (g.Pos+g.Bits+7)/8 > len(frame) {
				continue
			}
			pl.Fields = append(pl.Fields, PreviewField{Name: fd.Name, Offset: g.Pos, Bits: g.Bits, Value: g.format(frame)})
		}
		if i == 0 && tags > 0 {
			et := pl.Fields[len(pl.Fields)-1]
			pl.Fields = pl.Fields[:len(pl.Fields)-1]
			out = append(out, pl, PreviewLayer{Layer: -2, Title: fmt.Sprintf("Device VLAN tags (%d)", tags), Offset: 12, Length: 4 * tags})
			out = append(out, PreviewLayer{Layer: r.Layer, Title: "EtherType", Offset: 12 + 4*tags, Length: 2, Fields: []PreviewField{et}})
			continue
		}
		out = append(out, pl)
	}
	return out
}

func summary(ls []PreviewLayer, n int) string {
	var parts []string
	for _, l := range ls {
		v := map[string]string{}
		for _, f := range l.Fields {
			v[f.Name] = f.Value
		}
		switch l.Title {
		case "Ethernet":
			parts = append(parts, fmt.Sprintf("%s > %s", v["src"], v["dst"]))
		case "VLAN":
			parts = append(parts, "VLAN "+v["vid"])
		case "MPLS":
			parts = append(parts, "MPLS "+v["label"])
		case "ARP":
			parts = append(parts, fmt.Sprintf("ARP %s > %s", v["sender_ip"], v["target_ip"]))
		case "IPv4", "IPv6":
			parts = append(parts, fmt.Sprintf("%s %s > %s", l.Title, v["src"], v["dst"]))
		case "UDP", "TCP":
			parts = append(parts, fmt.Sprintf("%s %s > %s", l.Title, v["src_port"], v["dst_port"]))
		case "ICMP", "ICMPv6":
			parts = append(parts, fmt.Sprintf("%s type %s code %s", l.Title, v["type"], v["code"]))
		case "VXLAN":
			parts = append(parts, "VXLAN "+v["vni"])
		}
	}
	return fmt.Sprintf("%s, %d bytes", strings.Join(parts, ", "), n)
}

// previewPcap shows a captured frame as it would be sent, decoded.
func previewPcap(f *stream, fs [][]byte, n uint64) (any, error) {
	i := int(n % uint64(len(f.frames)))
	frame := f.frames[i].frameBytes(f)
	dec := decode(fs[i])
	if f.cfg.Pcap.VLANs != "keep" {
		// As a replay would: the device's tags replace the captured ones.
		for len(dec) > 1 && dec[1].VLAN != nil {
			dec = append(dec[:1], dec[2:]...)
		}
	}
	r := PreviewResult{Hex: hex.EncodeToString(frame), Length: len(frame), Index: uint64(i), Decoded: dec}
	// Fields are located by building the sent frame's decoded layers: the same layout.
	r.Summary = fmt.Sprintf("%d bytes", len(frame))
	if t, err := build(decode(frame), f.ctx()); err == nil {
		r.Layers = locate(t.regions, frame, 0)
		r.Summary = summary(r.Layers, len(frame))
	}
	for j, b := range fs {
		if j == 1000 {
			break
		}
		s := fmt.Sprintf("%d bytes", len(b))
		if t, err := build(decode(b), f.ctx()); err == nil {
			s = summary(locate(t.regions, b, 0), len(b))
		}
		r.Frames = append(r.Frames, PreviewFrame{Index: j, Length: len(b), Summary: s})
	}
	return r, nil
}
