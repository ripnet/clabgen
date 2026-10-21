package cfm

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// fakeEnv delivers frames straight to the peer's sessions, as if the two
// devices were joined by a wire.
type fakeEnv struct {
	name  string
	mac   pkt.MAC
	vlans []pkt.VLANTag
	peer  *fakeEnv
	drop  atomic.Bool // drop everything this side transmits

	mu       sync.Mutex
	sessions []protocol.Session
	self     protocol.Session
	events   []string
}

func (e *fakeEnv) Name() string            { return e.name }
func (e *fakeEnv) Port() string            { return "eth1" }
func (e *fakeEnv) MAC() pkt.MAC            { return e.mac }
func (e *fakeEnv) VLANs() []pkt.VLANTag    { return e.vlans }
func (e *fakeEnv) PortUp() bool            { return true }
func (e *fakeEnv) Device() protocol.Device { return e }
func (e *fakeEnv) InstanceName() string    { return "test" }
func (e *fakeEnv) IP() protocol.IP         { return nil }
func (e *fakeEnv) Log() *slog.Logger       { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (e *fakeEnv) Send(dst pkt.MAC, et uint16, payload []byte, opt protocol.SendOpts) error {
	if e.drop.Load() {
		return nil
	}
	f := pkt.Frame{Dst: dst, Src: e.mac, Tags: e.vlans, EtherType: et, Payload: payload}
	g, err := pkt.Decode(f.Encode())
	if err != nil {
		return err
	}
	g.RxTime = time.Now()
	p := e.peer
	if g.Dst != p.mac && !g.Dst.IsMulticast() {
		return nil
	}
	p.mu.Lock()
	ss := slices.Clone(p.sessions)
	p.mu.Unlock()
	for _, s := range ss {
		s.HandleRx(g)
	}
	return nil
}

func (e *fakeEnv) Emit(kind, msg string, data map[string]any) {
	e.mu.Lock()
	e.events = append(e.events, kind+": "+msg)
	e.mu.Unlock()
}

func (e *fakeEnv) Siblings() []protocol.Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []protocol.Session
	for _, s := range e.sessions {
		if s != e.self {
			out = append(out, s)
		}
	}
	return out
}

func pair() (*fakeEnv, *fakeEnv) {
	a := &fakeEnv{name: "a", mac: pkt.MAC{2, 0, 0, 0, 0, 0xa}}
	b := &fakeEnv{name: "b", mac: pkt.MAC{2, 0, 0, 0, 0, 0xb}}
	a.peer, b.peer = b, a
	return a, b
}

// start launches a MEP on env. Each MEP gets its own env view sharing the
// device's session list so Siblings works.
func start(t *testing.T, e *fakeEnv, mod func(*Config)) *mep {
	t.Helper()
	c := defaultConfig()
	c.CCMInterval = "10ms"
	if mod != nil {
		mod(c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	view := &sessView{fakeEnv: e}
	s, err := mepType{}.Start(view, c)
	if err != nil {
		t.Fatal(err)
	}
	view.self = s
	e.mu.Lock()
	e.sessions = append(e.sessions, s)
	e.mu.Unlock()
	t.Cleanup(s.Stop)
	return s.(*mep)
}

type sessView struct {
	*fakeEnv
	self protocol.Session
}

func (v *sessView) Siblings() []protocol.Session {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []protocol.Session
	for _, s := range v.sessions {
		if s != v.self {
			out = append(out, s)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func st(m *mep) Status { return m.Status().(Status) }

func rmepState(m *mep, id uint16) string {
	for _, r := range st(m).RemoteMEPs {
		if r.MEPID == id {
			return r.State
		}
	}
	return ""
}

func hasDefect(m *mep, d string) bool { return slices.Contains(st(m).Defects, d) }

func TestMEPUpLOCAndRDI(t *testing.T) {
	ea, eb := pair()
	a := start(t, ea, func(c *Config) { c.MEPID = 1; c.RemoteMEPs = []uint16{2} })
	b := start(t, eb, func(c *Config) { c.MEPID = 2; c.RemoteMEPs = []uint16{1} })

	waitFor(t, "both rmeps up", func() bool { return rmepState(a, 2) == rmepOK && rmepState(b, 1) == rmepOK })
	waitFor(t, "no defects", func() bool { return len(st(a).Defects) == 0 && len(st(b).Defects) == 0 })

	f := false
	if _, err := a.Action(context.Background(), "fault", &FaultArgs{CCMEnabled: &f}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "LOC on b", func() bool { return hasDefect(b, "DefRemoteCCM") && rmepState(b, 1) == rmepFailed })
	if !st(b).PresentRDI {
		t.Error("b should transmit RDI while in LOC")
	}
	waitFor(t, "RDI seen on a", func() bool { return hasDefect(a, "DefRDICCM") })
	if st(a).HighestDefect != "DefRDICCM" {
		t.Errorf("highest defect on a = %s", st(a).HighestDefect)
	}

	tr := true
	a.Action(context.Background(), "fault", &FaultArgs{CCMEnabled: &tr})
	waitFor(t, "recovery", func() bool { return len(st(a).Defects) == 0 && len(st(b).Defects) == 0 })

	eb.mu.Lock()
	evs := slices.Clone(eb.events)
	eb.mu.Unlock()
	var down, up bool
	for _, e := range evs {
		down = down || len(e) > 9 && e[:9] == "rmep-down"
		up = up || len(e) > 7 && e[:7] == "rmep-up"
	}
	if !down || !up {
		t.Errorf("expected rmep-up and rmep-down events, got %v", evs)
	}
}

func TestMEPStartTimeoutWithoutPeer(t *testing.T) {
	ea, _ := pair()
	a := start(t, ea, func(c *Config) { c.RemoteMEPs = []uint16{9} })
	waitFor(t, "configured rmep fails", func() bool { return rmepState(a, 9) == rmepFailed && hasDefect(a, "DefRemoteCCM") })
}

func TestMEPXconAndError(t *testing.T) {
	ea, eb := pair()
	a := start(t, ea, func(c *Config) { c.MEPID = 1; c.RemoteMEPs = []uint16{2} })
	b := start(t, eb, func(c *Config) { c.MEPID = 2 })
	waitFor(t, "up", func() bool { return rmepState(a, 2) == rmepOK })

	other := "other-ma"
	b.Action(context.Background(), "fault", &FaultArgs{MAIDOverride: &other})
	waitFor(t, "xcon", func() bool { return hasDefect(a, "DefXconCCM") })
	if st(a).HighestDefect != "DefXconCCM" || st(a).XconReason == "" {
		t.Errorf("status %+v", st(a))
	}
	empty := ""
	b.Action(context.Background(), "fault", &FaultArgs{MAIDOverride: &empty})
	waitFor(t, "xcon clears", func() bool { return !hasDefect(a, "DefXconCCM") })

	id := uint16(7)
	b.Action(context.Background(), "fault", &FaultArgs{MEPIDOverride: &id})
	waitFor(t, "error ccm", func() bool { return hasDefect(a, "DefErrorCCM") })
	zero := uint16(0)
	b.Action(context.Background(), "fault", &FaultArgs{MEPIDOverride: &zero})
	waitFor(t, "error clears", func() bool { return !hasDefect(a, "DefErrorCCM") && !hasDefect(a, "DefRemoteCCM") })
}

func TestMEPIntervalMismatch(t *testing.T) {
	ea, eb := pair()
	a := start(t, ea, func(c *Config) { c.MEPID = 1 })
	start(t, eb, func(c *Config) { c.MEPID = 2; c.CCMInterval = "100ms" })
	waitFor(t, "interval mismatch", func() bool { return hasDefect(a, "DefErrorCCM") })
}

func TestMEPLowerLevelIsXcon(t *testing.T) {
	ea, eb := pair()
	a := start(t, ea, func(c *Config) { c.MEPID = 1; c.Level = 5 })
	start(t, eb, func(c *Config) { c.MEPID = 2; c.Level = 3 })
	waitFor(t, "xcon from lower level", func() bool { return hasDefect(a, "DefXconCCM") })
}

func TestMEPSiblingLevelsNoXcon(t *testing.T) {
	ea, eb := pair()
	hi := start(t, ea, func(c *Config) { c.MEPID = 1; c.Level = 5; c.MAName = "hi" })
	lo := start(t, ea, func(c *Config) { c.MEPID = 11; c.Level = 3; c.MAName = "lo" })
	start(t, eb, func(c *Config) { c.MEPID = 2; c.Level = 5; c.MAName = "hi" })
	start(t, eb, func(c *Config) { c.MEPID = 12; c.Level = 3; c.MAName = "lo" })
	waitFor(t, "both levels up", func() bool { return rmepState(hi, 2) == rmepOK && rmepState(lo, 12) == rmepOK })
	time.Sleep(50 * time.Millisecond)
	if len(st(hi).Defects) != 0 || len(st(lo).Defects) != 0 {
		t.Errorf("unexpected defects hi=%v lo=%v", st(hi).Defects, st(lo).Defects)
	}
}

func TestMEPMACStatusAndSeq(t *testing.T) {
	ea, eb := pair()
	a := start(t, ea, func(c *Config) { c.MEPID = 1 })
	b := start(t, eb, func(c *Config) { c.MEPID = 2; c.InterfaceStatusTLV = true; c.PortStatusTLV = true })
	waitFor(t, "up", func() bool { return rmepState(a, 2) == rmepOK })
	down := "down"
	b.Action(context.Background(), "fault", &FaultArgs{InterfaceStatus: &down, SkipSequence: 5})
	waitFor(t, "mac status and seq error", func() bool {
		return hasDefect(a, "DefMACstatus") && st(a).RemoteMEPs[0].SeqErrors == 1
	})
	r := st(a).RemoteMEPs[0]
	if r.InterfaceStatus != "down" || r.PortStatus != "up" || r.SeqErrors != 1 {
		t.Errorf("rmep %+v", r)
	}
}

func TestMEPTools(t *testing.T) {
	ea, eb := pair()
	a := start(t, ea, func(c *Config) { c.MEPID = 1 })
	b := start(t, eb, func(c *Config) { c.MEPID = 2 })
	waitFor(t, "up", func() bool { return rmepState(a, 2) == rmepOK })
	ctx := context.Background()

	r, err := a.Action(ctx, "loopback", &LoopbackArgs{Target: Target{TargetMEPID: 2}, Count: 3, IntervalMS: 5, TimeoutMS: 200, DataSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	lb := r.(*LoopbackResult)
	if lb.Sent != 3 || lb.Received != 3 || lb.Target != eb.mac.String() {
		t.Errorf("loopback %+v", lb)
	}

	r, _ = a.Action(ctx, "loopback", &LoopbackArgs{Count: 1, IntervalMS: 5, TimeoutMS: 50})
	if lb := r.(*LoopbackResult); lb.Received != 1 || len(lb.Responders) != 1 {
		t.Errorf("multicast loopback %+v", lb)
	}

	r, err = a.Action(ctx, "linktrace", &LinktraceArgs{Target: Target{TargetMEPID: 2}, TTL: 10, TimeoutMS: 100})
	if err != nil {
		t.Fatal(err)
	}
	lt := r.(*LinktraceResult)
	if !lt.Reached || len(lt.Hops) != 1 || lt.Hops[0].Hop != 1 || !lt.Hops[0].TerminalMEP || lt.Hops[0].IngressMAC != eb.mac.String() {
		t.Errorf("linktrace %+v", lt)
	}

	r, err = a.Action(ctx, "delay", &DelayArgs{Target: Target{TargetMEPID: 2}, Count: 3, IntervalMS: 5, TimeoutMS: 100})
	if err != nil {
		t.Fatal(err)
	}
	if d := r.(*DelayResult); d.Received != 3 || d.Delay == nil {
		t.Errorf("delay %+v", d)
	}

	r, err = a.Action(ctx, "synthetic-loss", &SyntheticLossArgs{Target: Target{TargetMEPID: 2}, Count: 20, IntervalMS: 1, TimeoutMS: 50})
	if err != nil {
		t.Fatal(err)
	}
	if s := r.(*SyntheticLossResult); s.Sent != 20 || s.Received != 20 || s.FarEndLoss != 0 || s.NearEndLoss != 0 || s.ResponderMEPID != 2 {
		t.Errorf("slm %+v", s)
	}

	// Far-end loss: b stops answering half way by dropping its transmissions.
	eb.drop.Store(true)
	r, _ = a.Action(ctx, "loopback", &LoopbackArgs{Target: Target{TargetMEPID: 2}, Count: 2, IntervalMS: 1, TimeoutMS: 20})
	if lb := r.(*LoopbackResult); lb.Received != 0 || lb.LossPct != 100 {
		t.Errorf("expected total loss %+v", lb)
	}
	eb.drop.Store(false)

	off := true
	b.cfg.DisableLBR = off
	r, _ = a.Action(ctx, "loopback", &LoopbackArgs{Target: Target{TargetMEPID: 2}, Count: 1, IntervalMS: 1, TimeoutMS: 20})
	if lb := r.(*LoopbackResult); lb.Received != 0 {
		t.Errorf("LBR should be disabled %+v", lb)
	}

	if st(b).Tx["LBR"] < 4 || st(b).Tx["DMR"] != 3 || st(b).Tx["SLR"] != 20 || st(b).Tx["LTR"] != 1 {
		t.Errorf("responder counters %+v", st(b).Tx)
	}
}

func TestSchemaHasDefaults(t *testing.T) {
	s := protocol.Schema(defaultConfig())
	for _, want := range []string{`"ccm_interval"`, `"default":"1s"`, `"enum":["3.33ms"`} {
		if !contains(s, want) {
			t.Errorf("schema missing %s: %s", want, s)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestSchemaOrder(t *testing.T) {
	s := protocol.Schema(defaultConfig())
	if !contains(s, `"x-order":["level","mep_id","md_format"`) {
		t.Errorf("schema order missing: %s", s)
	}
}

func TestSchemaOrderEmbedded(t *testing.T) {
	s := protocol.Schema(&LoopbackArgs{})
	if !contains(s, `"x-order":["target_mac","target_mep_id","priority","count"`) {
		t.Errorf("embedded order: %s", s)
	}
}
