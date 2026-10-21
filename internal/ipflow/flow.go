// Package ipflow is the engine shared by IP traffic flows (ICMP, UDP, …):
// config parts, the send loop with rate pacing, source selection and
// next-hop resolution, IP encapsulation with every header field
// configurable, the payload signature, status and preview. A flow type
// supplies the transport layer (a Transport) and its receive matching.
package ipflow

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/flowrx"
	"github.com/ripnet/clabgen/internal/flowstats"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// Flow states.
const (
	StateIdle      = "idle"
	StateResolving = "resolving"
	StateRunning   = "running"
	StateDone      = "done"
	StateError     = "error"
)

// The signature (see flowrx, which decodes it at receivers).
const SigLen = flowrx.SigLen

var (
	SigMagic       = flowrx.SigMagic
	ParseSignature = flowrx.ParseSignature
)

// Actions every flow offers.
func Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "start", Description: "Start sending (statistics continue from the last run unless cleared)"},
		{Name: "stop", Description: "Stop sending; packets still in flight are counted once their timeout passes"},
		{Name: "clear", Description: "Reset statistics"},
		{Name: "preview", Description: "Show the next packet as hex, without sending it"},
	}
}

// Transport builds a flow's transport layer.
type Transport interface {
	// Message returns the transport message for sequence index n around
	// data (the payload, signature included), and its IP protocol number.
	// src and dst are for pseudo-header checksums.
	Message(n uint64, src, dst netip.Addr, data []byte) (msg []byte, proto uint8)
	// HeaderLen is the transport header size, for the frame size.
	HeaderLen() int
	// Describe summarises packet n's transport header for the preview.
	Describe(n uint64) string
}

// Flow runs one IP flow. Flow types embed it and add HandleRx.
type Flow struct {
	Env    protocol.Env
	Spec   Spec
	Dst    netip.Addr
	FlowID uint32
	// Echo is true when the flow expects replies (and tracks loss/RTT).
	Echo bool
	tp   Transport

	pattern []byte
	release func() // gives back the flow ID claim

	mu      sync.Mutex
	state   string
	lastErr string
	errAt   time.Time
	src     netip.Addr
	nextHop netip.Addr
	dstMAC  pkt.MAC
	haveMAC bool
	n       uint64 // packets sent in total (the sequence index)
	ipID    uint16
	started time.Time
	stopped time.Time
	tracker *flowstats.Tracker
	cancel  context.CancelFunc
	done    chan struct{}
}

// New prepares a flow; call Begin after the embedding type is set up.
func New(env protocol.Env, spec Spec, echo bool, tp Transport) *Flow {
	dst, _ := spec.Dst()
	f := &Flow{Env: env, Spec: spec, Dst: dst, Echo: echo, tp: tp, state: StateIdle,
		FlowID:  binary.BigEndian.Uint32(Hash(env, 2)),
		tracker: flowstats.New(spec.Interval(), spec.TimeoutD())}
	if spec.IPID != nil {
		f.ipID = *spec.IPID
	}
	f.pattern = fill(spec.Payload)
	f.release = func() {}
	if ip := env.IP(); ip != nil {
		f.release = ip.ClaimFlow(f.FlowID)
	}
	return f
}

// Hash returns stable bytes derived from the instance and device, from
// offset off (e.g. for identifiers and ports).
func Hash(env protocol.Env, off int) []byte {
	h := sha256.Sum256([]byte(env.InstanceName() + "/" + env.Device().MAC().String()))
	return h[off:]
}

// Begin starts sending if the config asks for it.
func (f *Flow) Begin() {
	if f.Spec.AutoStart {
		f.Start()
	}
}

func fill(p Payload) []byte {
	b := make([]byte, p.PayloadSize)
	switch p.PayloadPattern {
	case "increment":
		for i := range b {
			b[i] = byte(i)
		}
	case "random":
		rand.Read(b)
	case "text", "hex":
		src := []byte(p.PayloadData)
		if p.PayloadPattern == "hex" {
			src, _ = hex.DecodeString(strings.ReplaceAll(p.PayloadData, " ", ""))
		}
		for i := range b {
			b[i] = src[i%len(src)]
		}
	}
	return b
}

// ---- accessors for flow types ----

// Source is the address the flow sends from (invalid until resolved).
func (f *Flow) Source() netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.src
}

// Tracker returns the current statistics tracker ("clear" replaces it).
func (f *Flow) Tracker() *flowstats.Tracker {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tracker
}

// SentCount is the number of packets sent so far (the next index).
func (f *Flow) SentCount() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// SetError records a message shown in the status (e.g. an ICMP error).
func (f *Flow) SetError(msg string) {
	f.mu.Lock()
	f.lastErr, f.errAt = msg, time.Now()
	f.mu.Unlock()
}

// errorShownFor is how long a running flow reports its last error as
// current; after that it moves to last_error.
const errorShownFor = 10 * time.Second

// RxTime is the frame's kernel receive time, or now.
func RxTime(fr *pkt.Frame) time.Time {
	if fr.RxTime.IsZero() {
		return time.Now()
	}
	return fr.RxTime
}

// ---- building packets ----

// build returns the IP packet for index n. Caller holds mu.
func (f *Flow) build(n uint64, now time.Time) ([]byte, uint16) {
	s := &f.Spec
	data := append([]byte(nil), f.pattern...)
	if s.Signature {
		copy(data[0:], SigMagic[:])
		binary.BigEndian.PutUint32(data[4:], f.FlowID)
		binary.BigEndian.PutUint64(data[8:], n)
		binary.BigEndian.PutUint64(data[16:], uint64(now.UnixNano()))
	}
	msg, proto := f.tp.Message(n, f.src, f.Dst, data)
	tos := s.DSCP<<2 | s.ECN&3
	if f.Dst.Is4() {
		h := pkt.IPv4{TOS: tos, ID: f.ipID, TTL: s.TTL, Protocol: proto, Src: f.src, Dst: f.Dst, Payload: msg}
		if s.DF {
			h.Flags = pkt.IPv4DF
		}
		if s.IPID == nil {
			f.ipID++
		}
		b := h.Encode()
		if s.IPChecksum != "correct" {
			binary.BigEndian.PutUint16(b[10:], Checksum(s.IPChecksum, binary.BigEndian.Uint16(b[10:])))
		}
		return b, pkt.EtherTypeIPv4
	}
	h := pkt.IPv6{TrafficClass: tos, FlowLabel: s.FlowLabel, NextHeader: proto, HopLimit: s.TTL, Src: f.src, Dst: f.Dst, Payload: msg}
	return h.Encode(), pkt.EtherTypeIPv6
}

// ---- running ----

// Start begins sending.
func (f *Flow) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		return errors.New("already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel, f.done = cancel, make(chan struct{})
	f.state, f.lastErr, f.started, f.stopped = StateResolving, "", time.Now(), time.Time{}
	go f.run(ctx, cancel, f.done)
	f.Env.Emit("flow-started", fmt.Sprintf("sending to %s at %g pps", f.Dst, f.Spec.Rate), nil)
	return nil
}

func (f *Flow) halt(final string) {
	f.mu.Lock()
	cancel, done := f.cancel, f.done
	f.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	f.mu.Lock()
	f.cancel = nil
	if f.state != StateError {
		f.state = final
	}
	f.stopped = time.Now()
	f.mu.Unlock()
}

// Stop ends the session (protocol.Session).
func (f *Flow) Stop() {
	f.halt(StateIdle)
	f.release()
}

func (f *Flow) setErr(state, msg string) {
	f.mu.Lock()
	changed := f.lastErr != msg
	f.state, f.lastErr, f.errAt = state, msg, time.Now()
	f.mu.Unlock()
	if changed {
		f.Env.Emit("flow-error", msg, nil)
	}
}

// prepare picks the source address and resolves the destination MAC.
func (f *Flow) prepare(ctx context.Context) error {
	ip := f.Env.IP()
	var src netip.Addr
	if f.Spec.Source != "" {
		src = netip.MustParseAddr(f.Spec.Source)
	} else {
		var ok bool
		if ip != nil {
			src, ok = ip.Source(f.Dst)
		}
		if !ok {
			return fmt.Errorf("device %s has no IPv%d address", f.Env.Device().Name(), map[bool]int{true: 4, false: 6}[f.Dst.Is4()])
		}
	}
	f.mu.Lock()
	f.src = src
	f.mu.Unlock()
	if f.Spec.DstMAC != "" {
		m, _ := pkt.ParseMAC(f.Spec.DstMAC)
		f.mu.Lock()
		f.dstMAC, f.haveMAC = m, true
		f.mu.Unlock()
		return nil
	}
	return f.resolve(ctx)
}

func (f *Flow) resolve(ctx context.Context) error {
	ip := f.Env.IP()
	if ip == nil {
		return errors.New("device has no IP stack")
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	nh, mac, err := ip.ResolveNextHop(rctx, f.Dst)
	if err != nil {
		return err
	}
	f.mu.Lock()
	old, had := f.dstMAC, f.haveMAC
	f.nextHop, f.dstMAC, f.haveMAC = nh, mac, true
	f.mu.Unlock()
	if had && old != mac {
		f.Env.Emit("flow-next-hop-changed", fmt.Sprintf("next hop %s moved from %s to %s", nh, old, mac), nil)
	}
	return nil
}

func (f *Flow) run(ctx context.Context, cancel context.CancelFunc, done chan struct{}) {
	defer close(done)
	defer cancel() // also stops the next-hop follower
	for {
		err := f.prepare(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		f.setErr(StateResolving, err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	f.mu.Lock()
	f.state, f.lastErr = StateRunning, ""
	f.mu.Unlock()

	// Keep following the next hop (e.g. a gateway that fails over).
	if f.Spec.DstMAC == "" {
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := f.resolve(ctx); err != nil && ctx.Err() == nil {
						f.SetError("next hop: " + err.Error())
					}
				}
			}
		}()
	}

	iv := f.Spec.Interval()
	tick := iv
	if tick < time.Millisecond {
		tick = time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	begin := time.Now()
	var sent uint64
	var end time.Time
	if f.Spec.Mode == "duration" {
		end = begin.Add(f.Spec.DurationD())
	}
	countDone := func() bool { return f.Spec.Mode == "count" && sent >= f.Spec.Count }
	f.sendOne()
	sent++
	finished := countDone()
	for !finished {
		select {
		case <-ctx.Done():
			f.Tracker().Expire(time.Now(), false)
			return
		case now := <-t.C:
			due := uint64(now.Sub(begin)/iv) + 1
			if due > sent+1000 { // far behind (e.g. the host stalled): skip ahead instead of bursting
				sent = due - 1
			}
			for sent < due && !finished {
				if !end.IsZero() && !now.Before(end) {
					finished = true
					break
				}
				f.sendOne()
				sent++
				finished = countDone()
			}
			f.Tracker().Expire(now, false)
		}
	}
	// Wait for the last replies, then settle.
	select {
	case <-ctx.Done():
	case <-time.After(f.Spec.TimeoutD()):
	}
	f.Tracker().Expire(time.Now(), true)
	f.mu.Lock()
	f.state = StateDone
	f.stopped = time.Now()
	f.cancel = nil
	f.mu.Unlock()
	f.Env.Emit("flow-done", "finished", nil)
}

func (f *Flow) sendOne() {
	now := time.Now()
	f.mu.Lock()
	n := f.n
	f.n++
	b, et := f.build(n, now)
	mac := f.dstMAC
	tr := f.tracker
	f.mu.Unlock()
	opt := protocol.SendOpts{}
	if f.Spec.PCP != nil {
		opt = protocol.SendOpts{SetPCP: true, PCP: *f.Spec.PCP}
	}
	// Record before sending: on a fast link the reply can beat us otherwise.
	if f.Echo {
		tr.Sent(n, now)
	}
	if err := f.Env.Send(mac, et, b, opt); err != nil {
		tr.TxError(n, now, err)
		return
	}
	if !f.Echo {
		tr.SentNoReply(now)
	}
}

// ---- status and actions ----

// Base is the status every IP flow reports.
type Base struct {
	State       string  `json:"state"`
	Error       string  `json:"error,omitempty"`
	LastError   string  `json:"last_error,omitempty"`
	LastErrorS  float64 `json:"last_error_age_s,omitempty"`
	Destination string  `json:"destination"`
	Source      string  `json:"source,omitempty"`
	NextHop     string  `json:"next_hop,omitempty"`
	DstMAC      string  `json:"dst_mac,omitempty"`
	Kind        string  `json:"kind"`
	Rate        float64 `json:"rate"`
	Mode        string  `json:"mode"`
	FlowID      string  `json:"flow_id"`
	FrameSize   int     `json:"frame_size"` // bytes on the wire without FCS
	Elapsed     float64 `json:"elapsed_s"`
	TrackReply  bool    `json:"tracks_replies"`
	flowstats.Stats
}

// BaseStatus returns the shared status; the flow type sets Kind and may
// add fields.
func (f *Flow) BaseStatus() Base {
	now := time.Now()
	st := f.Tracker().Snapshot(now, 300)
	f.mu.Lock()
	defer f.mu.Unlock()
	s := Base{State: f.state, Error: f.lastErr, Destination: f.Dst.String(), Rate: f.Spec.Rate, Mode: f.Spec.Mode,
		FlowID: FlowIDString(f.FlowID), TrackReply: f.Echo, Stats: st}
	if f.state != StateRunning {
		s.OutageCurrentMs = 0 // a stopped flow isn't in an outage now
	}
	if f.lastErr != "" && f.state == StateRunning && now.Sub(f.errAt) > errorShownFor {
		s.Error, s.LastError, s.LastErrorS = "", f.lastErr, now.Sub(f.errAt).Round(time.Second).Seconds()
	}
	if f.src.IsValid() {
		s.Source = f.src.String()
	}
	if f.nextHop.IsValid() {
		s.NextHop = f.nextHop.String()
	}
	if f.haveMAC {
		s.DstMAC = f.dstMAC.String()
	}
	hdr := 20
	if f.Dst.Is6() {
		hdr = 40
	}
	s.FrameSize = 14 + 4*len(f.Env.Device().VLANs()) + hdr + f.tp.HeaderLen() + f.Spec.PayloadSize
	if s.FrameSize < 60 {
		s.FrameSize = 60
	}
	if !f.started.IsZero() {
		end := now
		if !f.stopped.IsZero() {
			end = f.stopped
		}
		s.Elapsed = end.Sub(f.started).Seconds()
	}
	return s
}

// FlowIDString formats a flow ID the way statuses and receivers show it.
func FlowIDString(id uint32) string { return fmt.Sprintf("%08x", id) }

type PreviewResult struct {
	Hex     string `json:"hex"`
	Length  int    `json:"length"`
	Summary string `json:"summary"`
}

// Action handles the shared actions.
func (f *Flow) Action(ctx context.Context, name string, _ any) (any, error) {
	switch name {
	case "start":
		if err := f.Start(); err != nil {
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
		f.halt(StateIdle)
		// Replies may still be in flight; settle them once the timeout passes.
		tr := f.Tracker()
		time.AfterFunc(f.Spec.TimeoutD(), func() { tr.Expire(time.Now(), false) })
		f.Env.Emit("flow-stopped", "stopped", nil)
		return map[string]any{"stopped": true}, nil
	case "clear":
		f.mu.Lock()
		f.tracker = flowstats.New(f.Spec.Interval(), f.Spec.TimeoutD())
		f.mu.Unlock()
		return map[string]any{"cleared": true}, nil
	case "preview":
		return f.preview(ctx)
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

func (f *Flow) preview(ctx context.Context) (any, error) {
	if !f.Source().IsValid() {
		// Best effort: a preview of an idle flow still shows the real source.
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		f.prepare(pctx)
		cancel()
	}
	f.mu.Lock()
	ipID := f.ipID
	n := f.n
	b, et := f.build(n, time.Now())
	f.ipID = ipID // a preview doesn't consume an IP ID
	mac, have, src := f.dstMAC, f.haveMAC, f.src
	f.mu.Unlock()
	if !src.IsValid() {
		return nil, fmt.Errorf("no source address for %s on device %s", f.Dst, f.Env.Device().Name())
	}
	d := f.Env.Device()
	tags := d.VLANs()
	if f.Spec.PCP != nil {
		tags = append([]pkt.VLANTag(nil), tags...)
		for i := range tags {
			tags[i].PCP = *f.Spec.PCP
		}
	}
	fr := pkt.Frame{Dst: mac, Src: d.MAC(), Tags: tags, EtherType: et, Payload: b}
	enc := fr.Encode()
	dst := mac.String()
	if !have {
		dst = "unresolved"
	}
	return PreviewResult{Hex: hex.EncodeToString(enc), Length: len(enc),
		Summary: fmt.Sprintf("%s > %s, %s > %s, %s, %d data bytes", d.MAC(), dst, src, f.Dst, f.tp.Describe(n), f.Spec.PayloadSize)}, nil
}

// ---- ICMP errors ----

var errNames4 = map[uint8]string{3: "destination unreachable", 11: "time exceeded", 12: "parameter problem", 5: "redirect"}
var errNames6 = map[uint8]string{1: "destination unreachable", 2: "packet too big", 3: "time exceeded", 4: "parameter problem"}

// ICMPError is an ICMP error message that quotes a packet.
type ICMPError struct {
	Name     string // e.g. "time exceeded"
	Code     uint8
	From     netip.Addr
	InnerSrc netip.Addr
	InnerDst netip.Addr
	Proto    uint8
	Quoted   []byte // the quoted transport header (at least 8 bytes)
}

func (e ICMPError) String() string {
	return fmt.Sprintf("%s (code %d) from %s", e.Name, e.Code, e.From)
}

// ParseICMPError decodes an ICMP (v4) or ICMPv6 error message m received
// from `from`.
func ParseICMPError(m []byte, v4 bool, from netip.Addr) (ICMPError, bool) {
	if len(m) < 8 {
		return ICMPError{}, false
	}
	names := errNames4
	if !v4 {
		names = errNames6
	}
	name := names[m[0]]
	if name == "" {
		return ICMPError{}, false
	}
	e := ICMPError{Name: name, Code: m[1], From: from}
	inner := m[8:]
	if v4 {
		if len(inner) < 28 || inner[0]>>4 != 4 {
			return e, false
		}
		ihl := int(inner[0]&0x0f) * 4
		if ihl < 20 || len(inner) < ihl+8 {
			return e, false
		}
		e.Proto = inner[9]
		e.InnerSrc, e.InnerDst = netip.AddrFrom4([4]byte(inner[12:16])), netip.AddrFrom4([4]byte(inner[16:20]))
		e.Quoted = inner[ihl:]
		return e, true
	}
	if len(inner) < 48 || inner[0]>>4 != 6 {
		return e, false
	}
	e.Proto = inner[6]
	e.InnerSrc, e.InnerDst = netip.AddrFrom16([16]byte(inner[8:24])), netip.AddrFrom16([16]byte(inner[24:40]))
	e.Quoted = inner[40:]
	return e, true
}

// DecodeIP decodes an IPv4 or IPv6 frame addressed to dst carrying proto.
// It returns the source, TTL/hop limit and transport payload; ok is false
// for anything else (including bad IPv4 header checksums).
func DecodeIP(fr *pkt.Frame, dst netip.Addr, proto uint8) (src netip.Addr, ttl uint8, payload []byte, v4, ok bool) {
	switch fr.EtherType {
	case pkt.EtherTypeIPv4:
		h, err := pkt.DecodeIPv4(fr.Payload)
		if err != nil || h.Protocol != proto || h.Dst != dst || h.FragOff != 0 || h.Flags&pkt.IPv4MF != 0 {
			return
		}
		return h.Src, h.TTL, h.Payload, true, true
	case pkt.EtherTypeIPv6:
		h, err := pkt.DecodeIPv6(fr.Payload)
		if err != nil || h.NextHeader != proto || h.Dst != dst {
			return
		}
		return h.Src, h.HopLimit, h.Payload, false, true
	}
	return
}
