// Package core owns devices and protocol instances and dispatches received
// frames to them. It is independent of the real port implementation so it
// can be exercised in tests with in-memory ports.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/events"
	"github.com/ripnet/clabgen/internal/flowrx"
	"github.com/ripnet/clabgen/internal/ipstack"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// PortIO is the engine's view of the physical ports.
type PortIO interface {
	Send(port string, frame []byte) error
	PortUp(port string) bool
	SetAdmin(port string, up bool) error
	Desired() map[string]bool
}

type DeviceSpec struct {
	Name  string        `json:"name"`
	Port  string        `json:"port"`
	MAC   string        `json:"mac,omitempty"` // empty: derived, stable per host+name
	VLANs []pkt.VLANTag `json:"vlans,omitempty"`
	// IP addresses in CIDR form (IPv4 and IPv6) and optional gateways.
	Addresses []string `json:"addresses,omitempty"`
	Gateway4  string   `json:"gateway4,omitempty"`
	Gateway6  string   `json:"gateway6,omitempty"`

	MACDerived bool   `json:"-"` // set by Devices when MAC was not configured
	Group      string `json:"-"` // set by Devices for group members
}

// sameL2 reports whether two specs put the device on the same wire
// identity, so an update can keep its protocols running.
func sameL2(a, b DeviceSpec) bool {
	if a.Port != b.Port || len(a.VLANs) != len(b.VLANs) {
		return false
	}
	for i := range a.VLANs {
		if a.VLANs[i] != b.VLANs[i] {
			return false
		}
	}
	return true
}

type ProtocolSpec struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Device string          `json:"device"`
	Config json.RawMessage `json:"config,omitempty"`
	// Disabled instances keep their config but don't run.
	Disabled bool `json:"disabled,omitempty"`
}

const (
	StateRunning  = "running"
	StateWaiting  = "waiting"
	StateError    = "error"
	StateDisabled = "disabled"
)

type ProtocolView struct {
	ProtocolSpec
	Group  string // set for group members
	State  string
	Error  string
	Status any
}

// device is one emulated endpoint. name, port, mac and vlans never change
// (an L2 change replaces the device); spec is guarded by Engine.mu since
// IP-only updates change it in place.
type device struct {
	name  string
	port  string
	mac   pkt.MAC
	vlans []pkt.VLANTag
	spec  DeviceSpec
	ip    *ipstack.Stack
	insts []*instance
	taps  []*tap // sessions of other devices attached here
	group string
	e     *Engine
}

func (d *device) Name() string         { return d.name }
func (d *device) Port() string         { return d.port }
func (d *device) MAC() pkt.MAC         { return d.mac }
func (d *device) VLANs() []pkt.VLANTag { return d.vlans }
func (d *device) PortUp() bool         { return d.e.ports.PortUp(d.port) }

func (e *Engine) newDevice(s DeviceSpec, mac pkt.MAC, cfg ipstack.Config) *device {
	d := &device{name: s.Name, port: s.Port, mac: mac, vlans: s.VLANs, spec: s, e: e}
	send := func(dst pkt.MAC, et uint16, payload []byte) error {
		f := pkt.Frame{Dst: dst, Src: mac, Tags: d.vlans, EtherType: et, Payload: payload}
		return e.transmit(d, f.Encode())
	}
	emit := func(kind, msg string, data map[string]any) { e.publish("device/"+d.name, kind, msg, data) }
	d.ip = ipstack.New(mac, cfg, send, emit)
	return d
}

// announce sends gratuitous ARP / unsolicited NAs if the port is up.
func (d *device) announce() {
	if d.ip.Enabled() && d.PortUp() {
		d.ip.Announce()
	}
}

type instance struct {
	spec  ProtocolSpec
	typ   protocol.Type
	cfg   any
	sess  protocol.Session
	dev   *device
	state string
	err   string
	ets   map[uint16]bool
	taps  []*tap // devices this instance attached to (Env.Attach)
	group string
}

type Engine struct {
	ports PortIO
	bus   *events.Bus
	log   *slog.Logger
	host  string

	mu      sync.RWMutex
	devices map[string]*device
	byPort  map[string][]*device
	insts   map[string]*instance
	groups  map[string]*group
	gmu     sync.Mutex // serialises group changes

	// Frames a device sends are also delivered to the other devices on the
	// same port, like hosts on one segment (the port only sends them out).
	local chan localFrame
}

type localFrame struct {
	port string
	b    []byte
	ts   time.Time
	from *device
}

func New(ports PortIO, bus *events.Bus, log *slog.Logger) *Engine {
	host, _ := os.Hostname()
	e := &Engine{ports: ports, bus: bus, log: log, host: host,
		devices: map[string]*device{}, byPort: map[string][]*device{}, insts: map[string]*instance{}, groups: map[string]*group{},
		local: make(chan localFrame, 4096)}
	go func() {
		// One worker keeps local frames in order.
		for lf := range e.local {
			e.receive(lf.port, lf.b, lf.ts, lf.from)
		}
	}()
	return e
}

// transmit sends a device's frame out of its port and to the other
// devices on that port.
func (e *Engine) transmit(d *device, frame []byte) error {
	err := e.ports.Send(d.port, frame)
	e.mu.RLock()
	peers := len(e.byPort[d.port]) > 1
	e.mu.RUnlock()
	if peers && e.ports.PortUp(d.port) {
		select {
		case e.local <- localFrame{d.port, frame, time.Now(), d}:
		default: // overloaded: drop, as a congested segment would
		}
	}
	return err
}

func (e *Engine) Events() *events.Bus { return e.bus }

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,62}$`)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// Receive dispatches one frame from a port. It returns false if nothing
// consumed it.
func (e *Engine) Receive(port string, b []byte, ts time.Time) bool {
	return e.receive(port, b, ts, nil)
}

// receive dispatches a frame to the port's devices, except `from` (the
// sender of a locally delivered frame).
func (e *Engine) receive(port string, b []byte, ts time.Time, from *device) bool {
	f, err := pkt.Decode(b)
	if err != nil {
		return false
	}
	f.Port, f.RxTime = port, ts
	var targets []protocol.Session
	var tapRx []func(*pkt.Frame)
	var stacks []*ipstack.Stack
	isIP := f.EtherType == pkt.EtherTypeARP || f.EtherType == pkt.EtherTypeIPv4 || f.EtherType == pkt.EtherTypeIPv6
	e.mu.RLock()
	for _, d := range e.byPort[port] {
		if d == from {
			continue
		}
		if f.Dst != d.mac && !f.Dst.IsMulticast() {
			continue
		}
		if !pkt.SameVLANs(d.vlans, f.Tags) {
			continue
		}
		if isIP {
			stacks = append(stacks, d.ip)
		}
		for _, in := range d.insts {
			if in.sess != nil && in.ets[f.EtherType] {
				targets = append(targets, in.sess)
			}
		}
		for _, t := range d.taps {
			if t.in.ets[f.EtherType] {
				tapRx = append(tapRx, t.rx)
			}
		}
	}
	e.mu.RUnlock()
	used := false
	for _, s := range stacks {
		if s.HandleRx(f) {
			used = true
		}
	}
	for _, s := range targets {
		s.HandleRx(f)
	}
	for _, rx := range tapRx {
		rx(f)
	}
	return used || len(targets) > 0 || len(tapRx) > 0
}

func (e *Engine) deriveMAC(name string) pkt.MAC {
	h := sha256.Sum256([]byte(e.host + "/" + name))
	var m pkt.MAC
	copy(m[:], h[:6])
	m[0] = (m[0] &^ 0x01) | 0x02 // unicast, locally administered
	return m
}

// ---- devices ----

// checkDevice validates a spec, resolves its MAC and parses its IP config.
// The returned spec has the IP fields normalised.
func (e *Engine) checkDevice(s DeviceSpec) (DeviceSpec, pkt.MAC, ipstack.Config, error) {
	var none ipstack.Config
	if !nameRE.MatchString(s.Name) {
		return s, pkt.MAC{}, none, fmt.Errorf("invalid device name %q", s.Name)
	}
	if s.Port == "" {
		return s, pkt.MAC{}, none, errors.New("device port is required")
	}
	if len(s.VLANs) > 4 {
		return s, pkt.MAC{}, none, errors.New("at most 4 VLAN tags")
	}
	tags := make([]pkt.VLANTag, len(s.VLANs))
	for i, t := range s.VLANs {
		if t.VID > 4095 || t.PCP > 7 {
			return s, pkt.MAC{}, none, fmt.Errorf("invalid VLAN tag %+v", t)
		}
		if t.TPID != 0 && !pkt.IsTPID(t.TPID) {
			return s, pkt.MAC{}, none, fmt.Errorf("unsupported TPID %#04x (use 0x8100, 0x88a8 or 0x9100)", t.TPID)
		}
		if t.TPID == pkt.TPIDDot1Q {
			t.TPID = 0 // the default; keeps specs comparable and exports short
		}
		tags[i] = t
	}
	if len(tags) == 0 {
		tags = nil
	}
	s.VLANs = tags
	cfg, err := ipstack.ParseConfig(s.Addresses, s.Gateway4, s.Gateway6)
	if err != nil {
		return s, pkt.MAC{}, none, err
	}
	s.Addresses, s.Gateway4, s.Gateway6 = cfg.Strings()
	if s.MAC == "" {
		return s, e.deriveMAC(s.Name), cfg, nil
	}
	m, err := pkt.ParseMAC(s.MAC)
	if err != nil {
		return s, pkt.MAC{}, none, err
	}
	if m.IsMulticast() || m.IsZero() {
		return s, pkt.MAC{}, none, fmt.Errorf("device MAC %s must be a non-zero unicast address", m)
	}
	return s, m, cfg, nil
}

func (e *Engine) CreateDevice(s DeviceSpec) (DeviceSpec, error) { return e.createDevice(s, "") }

// memberErr refuses a change to a group member from outside its group.
func memberErr(kind, name, member, by string) error {
	if member == by {
		return nil
	}
	if member != "" {
		return fmt.Errorf("%s %s belongs to group %s: change the group", kind, name, member)
	}
	return fmt.Errorf("%s %s is not in group %s", kind, name, by)
}

func (e *Engine) createDevice(s DeviceSpec, group string) (DeviceSpec, error) {
	s, mac, cfg, err := e.checkDevice(s)
	if err != nil {
		return s, err
	}
	e.mu.Lock()
	if _, dup := e.devices[s.Name]; dup {
		e.mu.Unlock()
		return s, fmt.Errorf("device %s: %w", s.Name, ErrExists)
	}
	d := e.newDevice(s, mac, cfg)
	d.group = group
	e.devices[s.Name] = d
	e.byPort[s.Port] = append(e.byPort[s.Port], d)
	var waiting []*instance
	for _, in := range e.insts {
		if in.spec.Device == s.Name && !in.spec.Disabled {
			waiting = append(waiting, in)
		}
	}
	e.mu.Unlock()
	for _, in := range waiting {
		e.startInstance(in, d)
	}
	d.announce()
	e.publish("device/"+s.Name, "device-created", "device created", nil)
	return s, nil
}

// UpdateDevice changes a device's port, MAC, VLAN stack or IP config. A
// change to the port, MAC or VLANs restarts the device's protocols, since
// their peers' state (learned MACs, sequence numbers) no longer applies; an
// IP-only change is applied in place.
func (e *Engine) UpdateDevice(s DeviceSpec) (DeviceSpec, error) { return e.updateDevice(s, "") }

func (e *Engine) updateDevice(s DeviceSpec, group string) (DeviceSpec, error) {
	s, mac, cfg, err := e.checkDevice(s)
	if err != nil {
		return s, err
	}
	e.mu.Lock()
	old, ok := e.devices[s.Name]
	if !ok {
		e.mu.Unlock()
		return s, fmt.Errorf("device %s: %w", s.Name, ErrNotFound)
	}
	if err := memberErr("device", s.Name, old.group, group); err != nil {
		e.mu.Unlock()
		return s, err
	}
	if old.mac == mac && sameL2(old.spec, s) {
		old.spec = s
		e.mu.Unlock()
		old.ip.SetConfig(cfg)
		old.announce()
		e.publish("device/"+s.Name, "device-updated", fmt.Sprintf("device updated: addresses %s", addrString(s)), map[string]any{"restarted": float64(0)})
		return s, nil
	}
	e.unindex(old)
	old.ip.Close()
	e.closeDeviceTapsLocked(old)
	d := e.newDevice(s, mac, cfg)
	d.group = group
	e.devices[s.Name] = d
	e.byPort[s.Port] = append(e.byPort[s.Port], d)
	insts := old.insts
	old.insts = nil
	var stop []protocol.Session
	for _, in := range insts {
		if in.sess != nil {
			stop = append(stop, in.sess)
		}
		in.sess, in.dev, in.state, in.err = nil, nil, StateWaiting, "restarting after device change"
		e.closeInstanceTapsLocked(in)
	}
	// Instances that failed to start on the old device get another try.
	for _, in := range e.insts {
		if in.spec.Device == s.Name && in.state == StateError {
			insts = append(insts, in)
		}
	}
	e.mu.Unlock()
	for _, x := range stop {
		x.Stop()
	}
	for _, in := range insts {
		e.startInstance(in, d)
	}
	d.announce()
	e.publish("device/"+s.Name, "device-updated", fmt.Sprintf("device updated: port %s, mac %s, vlans %s", s.Port, mac, vlanString(s.VLANs)),
		map[string]any{"restarted": float64(len(insts))})
	return s, nil
}

func addrString(s DeviceSpec) string {
	if len(s.Addresses) == 0 {
		return "none"
	}
	out := strings.Join(s.Addresses, ", ")
	for _, g := range []string{s.Gateway4, s.Gateway6} {
		if g != "" {
			out += ", gw " + g
		}
	}
	return out
}

func vlanString(t []pkt.VLANTag) string {
	if len(t) == 0 {
		return "untagged"
	}
	parts := make([]string, len(t))
	for i, v := range t {
		parts[i] = fmt.Sprintf("%d", v.VID)
		if v.TPID != 0 && v.TPID != pkt.TPIDDot1Q {
			parts[i] = fmt.Sprintf("%#04x:%d", v.TPID, v.VID)
		}
	}
	return strings.Join(parts, "/")
}

// unindex removes d from the per-port dispatch index. Caller holds mu.
func (e *Engine) unindex(d *device) {
	list := e.byPort[d.port]
	for i, x := range list {
		if x == d {
			e.byPort[d.port] = append(list[:i:i], list[i+1:]...)
			return
		}
	}
}

func (e *Engine) DeleteDevice(name string) error { return e.deleteDevice(name, "") }

func (e *Engine) deleteDevice(name, group string) error {
	e.mu.Lock()
	d, ok := e.devices[name]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("device %s: %w", name, ErrNotFound)
	}
	if err := memberErr("device", name, d.group, group); err != nil {
		e.mu.Unlock()
		return err
	}
	delete(e.devices, name)
	e.unindex(d)
	d.ip.Close()
	e.closeDeviceTapsLocked(d)
	insts := d.insts
	d.insts = nil
	var stop []protocol.Session
	for _, in := range insts {
		if in.sess != nil {
			stop = append(stop, in.sess)
		}
		in.sess, in.dev, in.state, in.err = nil, nil, StateWaiting, "device does not exist"
		e.closeInstanceTapsLocked(in)
	}
	e.mu.Unlock()
	for _, s := range stop {
		s.Stop()
	}
	e.publish("device/"+name, "device-deleted", "device deleted", nil)
	return nil
}

func (e *Engine) Devices() []DeviceSpec {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]DeviceSpec, 0, len(e.devices))
	for _, d := range e.devices {
		s := d.spec
		s.MACDerived = s.MAC == ""
		s.MAC = d.mac.String()
		s.Group = d.group
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---- protocols ----

func (e *Engine) CreateProtocol(s ProtocolSpec) (ProtocolView, error) { return e.createProtocol(s, "") }

func (e *Engine) createProtocol(s ProtocolSpec, group string) (ProtocolView, error) {
	if !nameRE.MatchString(s.Name) {
		return ProtocolView{}, fmt.Errorf("invalid protocol name %q", s.Name)
	}
	t, ok := protocol.Lookup(s.Type)
	if !ok {
		return ProtocolView{}, fmt.Errorf("unknown protocol type %q", s.Type)
	}
	cfg := t.NewConfig()
	if err := protocol.Decode(cfg, s.Config); err != nil {
		return ProtocolView{}, err
	}
	s.Config, _ = json.Marshal(cfg) // normalised, defaults filled in
	in := &instance{spec: s, typ: t, cfg: cfg, state: StateWaiting, err: "device does not exist", ets: map[uint16]bool{}, group: group}
	if s.Disabled {
		in.state, in.err = StateDisabled, ""
	}
	for _, et := range t.EtherTypes() {
		in.ets[et] = true
	}
	e.mu.Lock()
	if _, dup := e.insts[s.Name]; dup {
		e.mu.Unlock()
		return ProtocolView{}, fmt.Errorf("protocol %s: %w", s.Name, ErrExists)
	}
	e.insts[s.Name] = in
	d := e.devices[s.Device]
	e.mu.Unlock()
	if d != nil && !s.Disabled {
		e.startInstance(in, d)
	}
	return e.GetProtocol(s.Name)
}

func (e *Engine) startInstance(in *instance, d *device) {
	env := &env{e: e, in: in, dev: d, log: e.log.With("protocol", in.spec.Name)}
	sess, err := in.typ.Start(env, in.cfg)
	e.mu.Lock()
	if e.insts[in.spec.Name] != in || in.spec.Disabled {
		// Deleted or disabled while starting.
		e.mu.Unlock()
		if sess != nil {
			sess.Stop()
		}
		return
	}
	defer e.mu.Unlock()
	if err != nil {
		in.state, in.err = StateError, err.Error()
		e.log.Error("protocol start failed", "protocol", in.spec.Name, "err", err)
		return
	}
	in.sess, in.dev, in.state, in.err = sess, d, StateRunning, ""
	d.insts = append(d.insts, in)
}

func (e *Engine) UpdateProtocol(name string, raw json.RawMessage) (ProtocolView, error) {
	return e.updateProtocol(name, raw, "")
}

func (e *Engine) updateProtocol(name string, raw json.RawMessage, group string) (ProtocolView, error) {
	e.mu.RLock()
	in, ok := e.insts[name]
	e.mu.RUnlock()
	if !ok {
		return ProtocolView{}, fmt.Errorf("protocol %s: %w", name, ErrNotFound)
	}
	if err := memberErr("protocol", name, in.group, group); err != nil {
		return ProtocolView{}, err
	}
	cfg := in.typ.NewConfig()
	if err := protocol.Decode(cfg, raw); err != nil {
		return ProtocolView{}, err
	}
	spec := in.spec
	spec.Config, _ = json.Marshal(cfg)
	if err := e.deleteProtocol(name, group); err != nil {
		return ProtocolView{}, err
	}
	return e.createProtocol(spec, group)
}

func (e *Engine) DeleteProtocol(name string) error { return e.deleteProtocol(name, "") }

func (e *Engine) deleteProtocol(name, group string) error {
	e.mu.Lock()
	in, ok := e.insts[name]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("protocol %s: %w", name, ErrNotFound)
	}
	if err := memberErr("protocol", name, in.group, group); err != nil {
		e.mu.Unlock()
		return err
	}
	delete(e.insts, name)
	sess := e.detachLocked(in)
	e.mu.Unlock()
	if sess != nil {
		sess.Stop()
	}
	return nil
}

// detachLocked takes in off its device and returns its session to stop.
func (e *Engine) detachLocked(in *instance) protocol.Session {
	if in.dev != nil {
		l := in.dev.insts
		for i, x := range l {
			if x == in {
				in.dev.insts = append(l[:i:i], l[i+1:]...)
				break
			}
		}
	}
	sess := in.sess
	in.sess, in.dev = nil, nil
	e.closeInstanceTapsLocked(in)
	return sess
}

// SetProtocolDisabled stops an instance and keeps it with its config
// (disabled), or starts it again. Group members follow their group
// (SetGroupDisabled).
func (e *Engine) SetProtocolDisabled(name string, disabled bool) (ProtocolView, error) {
	return e.setProtocolDisabled(name, disabled, "")
}

func (e *Engine) setProtocolDisabled(name string, disabled bool, group string) (ProtocolView, error) {
	e.mu.Lock()
	in, ok := e.insts[name]
	if !ok {
		e.mu.Unlock()
		return ProtocolView{}, fmt.Errorf("protocol %s: %w", name, ErrNotFound)
	}
	if err := memberErr("protocol", name, in.group, group); err != nil {
		e.mu.Unlock()
		return ProtocolView{}, err
	}
	if in.spec.Disabled == disabled {
		e.mu.Unlock()
		return e.GetProtocol(name)
	}
	in.spec.Disabled = disabled
	var sess protocol.Session
	var d *device
	if disabled {
		sess = e.detachLocked(in)
		in.state, in.err = StateDisabled, ""
	} else {
		in.state, in.err = StateWaiting, "device does not exist"
		d = e.devices[in.spec.Device]
	}
	e.mu.Unlock()
	if sess != nil {
		sess.Stop()
	}
	if d != nil {
		e.startInstance(in, d)
	}
	if disabled {
		e.publish("protocol/"+name, "protocol-disabled", "protocol disabled", nil)
	} else {
		e.publish("protocol/"+name, "protocol-enabled", "protocol enabled", nil)
	}
	return e.GetProtocol(name)
}

func (e *Engine) GetProtocol(name string) (ProtocolView, error) {
	e.mu.RLock()
	in, ok := e.insts[name]
	e.mu.RUnlock()
	if !ok {
		return ProtocolView{}, fmt.Errorf("protocol %s: %w", name, ErrNotFound)
	}
	return e.view(in, true, false), nil
}

func (e *Engine) view(in *instance, withStatus, brief bool) ProtocolView {
	e.mu.RLock()
	v := ProtocolView{ProtocolSpec: in.spec, Group: in.group, State: in.state, Error: in.err}
	sess := in.sess
	e.mu.RUnlock()
	if withStatus && sess != nil {
		if b, ok := sess.(protocol.Briefer); ok && brief {
			v.Status = b.Brief()
		} else {
			v.Status = sess.Status()
		}
	}
	return v
}

func (e *Engine) Protocols(withStatus bool) []ProtocolView {
	return e.ProtocolList(withStatus, false, false)
}

// ProtocolList lists instances; with brief, sessions that offer a compact
// status (protocol.Briefer) report that instead; noMembers leaves out
// group members.
func (e *Engine) ProtocolList(withStatus, brief, noMembers bool) []ProtocolView {
	e.mu.RLock()
	list := make([]*instance, 0, len(e.insts))
	for _, in := range e.insts {
		if !noMembers || in.group == "" {
			list = append(list, in)
		}
	}
	e.mu.RUnlock()
	out := make([]ProtocolView, 0, len(list))
	for _, in := range list {
		out = append(out, e.view(in, withStatus, brief))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (e *Engine) RunAction(ctx context.Context, name, action string, rawArgs json.RawMessage) (any, error) {
	e.mu.RLock()
	in, ok := e.insts[name]
	var sess protocol.Session
	if ok {
		sess = in.sess
	}
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("protocol %s: %w", name, ErrNotFound)
	}
	if sess == nil {
		return nil, fmt.Errorf("protocol %s is not running (%s)", name, in.err)
	}
	a, ok := protocol.FindAction(in.typ, action)
	if !ok {
		return nil, fmt.Errorf("protocol type %s has no action %q", in.typ.Name(), action)
	}
	var args any
	if a.NewArgs != nil {
		args = a.NewArgs()
		if err := protocol.Decode(args, rawArgs); err != nil {
			return nil, err
		}
	}
	return sess.Action(ctx, action, args)
}

func (e *Engine) publish(source, kind, msg string, data map[string]any) {
	e.bus.Publish(events.Event{Source: source, Kind: kind, Message: msg, Data: data})
}

// PublishPortEvent lets the port layer report link changes. When a port
// comes up its devices announce their addresses.
func (e *Engine) PublishPortEvent(port, kind, msg string) {
	e.publish("port/"+port, kind, msg, nil)
	if kind == "port-oper" && msg == "oper up" {
		e.mu.RLock()
		devs := append([]*device(nil), e.byPort[port]...)
		e.mu.RUnlock()
		for _, d := range devs {
			d.announce()
		}
	}
}

// ---- device IP ----

func (e *Engine) deviceStack(name string) (*ipstack.Stack, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	d, ok := e.devices[name]
	if !ok {
		return nil, fmt.Errorf("device %s: %w", name, ErrNotFound)
	}
	return d.ip, nil
}

// DeviceIP returns a device's effective addresses, neighbors and counters.
func (e *Engine) DeviceIP(name string) (ipstack.Status, error) {
	s, err := e.deviceStack(name)
	if err != nil {
		return ipstack.Status{}, err
	}
	return s.Status(), nil
}

// ResolveNeighbor resolves an IP on the device's link with ARP or ND. An
// off-link address resolves its gateway; the next hop used is returned.
func (e *Engine) ResolveNeighbor(ctx context.Context, name string, ip netip.Addr) (nextHop netip.Addr, mac pkt.MAC, cached bool, err error) {
	s, err := e.deviceStack(name)
	if err != nil {
		return
	}
	if !s.Enabled() {
		err = fmt.Errorf("device %s has no IP addresses", name)
		return
	}
	if nextHop, err = s.NextHop(ip); err != nil {
		return
	}
	mac, cached, err = s.Resolve(ctx, nextHop)
	return
}

// ReceivedFlow is one signed flow received by a device.
type ReceivedFlow struct {
	Device string
	flowrx.Flow
}

// ReceivedFlows lists the flows received by one device, or all if name is
// empty.
func (e *Engine) ReceivedFlows(name string) ([]ReceivedFlow, error) {
	e.mu.RLock()
	var devs []*device
	if name != "" {
		d, ok := e.devices[name]
		if !ok {
			e.mu.RUnlock()
			return nil, fmt.Errorf("device %s: %w", name, ErrNotFound)
		}
		devs = []*device{d}
	} else {
		for _, d := range e.devices {
			devs = append(devs, d)
		}
	}
	e.mu.RUnlock()
	sort.Slice(devs, func(i, j int) bool { return devs[i].name < devs[j].name })
	var out []ReceivedFlow
	for _, d := range devs {
		for _, f := range d.ip.ReceivedFlows() {
			out = append(out, ReceivedFlow{Device: d.name, Flow: f})
		}
	}
	return out, nil
}

// ClearReceivedFlows forgets received flows on one device (or all, if
// name is empty), optionally only those with one flow ID.
func (e *Engine) ClearReceivedFlows(name string, flowID *uint32) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	clear := func(d *device) {
		if flowID != nil {
			d.ip.ClearReceivedFlow(*flowID)
		} else {
			d.ip.ClearReceivedFlows()
		}
	}
	if name != "" {
		d, ok := e.devices[name]
		if !ok {
			return fmt.Errorf("device %s: %w", name, ErrNotFound)
		}
		clear(d)
		return nil
	}
	for _, d := range e.devices {
		clear(d)
	}
	return nil
}

func (e *Engine) ClearNeighbors(name string) error {
	s, err := e.deviceStack(name)
	if err != nil {
		return err
	}
	s.ClearNeighbors()
	return nil
}

// ---- env ----

type env struct {
	e   *Engine
	in  *instance
	dev *device
	log *slog.Logger
}

func (v *env) InstanceName() string    { return v.in.spec.Name }
func (v *env) IP() protocol.IP         { return v.dev.ip }
func (v *env) Device() protocol.Device { return v.dev }
func (v *env) Log() *slog.Logger       { return v.log }

func (v *env) Send(dst pkt.MAC, et uint16, payload []byte, opt protocol.SendOpts) error {
	return v.e.sendFrom(v.dev, dst, et, payload, opt)
}

// sendFrom transmits a payload from a device, adding its MAC and VLANs.
func (e *Engine) sendFrom(d *device, dst pkt.MAC, et uint16, payload []byte, opt protocol.SendOpts) error {
	tags := d.vlans
	if opt.SetPCP && len(tags) > 0 {
		tags = append([]pkt.VLANTag(nil), tags...)
		for i := range tags {
			tags[i].PCP = opt.PCP
		}
	}
	f := pkt.Frame{Dst: dst, Src: d.mac, Tags: tags, EtherType: et, Payload: payload}
	return e.transmit(d, f.Encode())
}

func (v *env) Emit(kind, msg string, data map[string]any) {
	v.e.publish("protocol/"+v.in.spec.Name, kind, msg, data)
}

func (v *env) Siblings() []protocol.Session {
	v.e.mu.RLock()
	defer v.e.mu.RUnlock()
	var out []protocol.Session
	for _, in := range v.dev.insts {
		if in != v.in && in.typ == v.in.typ && in.sess != nil {
			out = append(out, in.sess)
		}
	}
	return out
}
