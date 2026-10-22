package core_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// probe attaches to the device named in its config and records frames.
type probeType struct{}

type probeCfg struct {
	Other string `json:"other"`
}

type probe struct {
	env  protocol.Env
	mu   sync.Mutex
	got  []string // "own:src" / "other:src"
	att  protocol.Attachment
	errs []string
}

func (probeType) Name() string                                    { return "attach-probe" }
func (probeType) Description() string                             { return "test" }
func (probeType) NewConfig() any                                  { return &probeCfg{} }
func (probeType) Actions() []protocol.Action                      { return nil }
func (probeType) EtherTypes() []uint16                            { return []uint16{0x88b5} }
func (p *probe) Status() any                                      { return nil }
func (p *probe) Action(context.Context, string, any) (any, error) { return nil, nil }
func (p *probe) Stop()                                            {}
func (p *probe) HandleRx(f *pkt.Frame) {
	p.mu.Lock()
	p.got = append(p.got, "own:"+f.Src.String())
	p.mu.Unlock()
}

var probes = map[string]*probe{}
var probesMu sync.Mutex

func (probeType) Start(env protocol.Env, c any) (protocol.Session, error) {
	p := &probe{env: env}
	att, err := env.Attach(c.(*probeCfg).Other, func(f *pkt.Frame) {
		p.mu.Lock()
		p.got = append(p.got, "other:"+f.Src.String())
		p.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	p.att = att
	probesMu.Lock()
	probes[env.InstanceName()] = p
	probesMu.Unlock()
	return p, nil
}

func init() { protocol.Register(probeType{}) }

func TestAttach(t *testing.T) {
	a, b, _, _ := newPair()
	for _, d := range []core.DeviceSpec{{Name: "x", Port: "eth1", MAC: "02:00:00:00:01:01", VLANs: []pkt.VLANTag{{VID: 1}}},
		{Name: "y", Port: "eth1", MAC: "02:00:00:00:01:02", VLANs: []pkt.VLANTag{{VID: 2}}}} {
		if _, err := a.CreateDevice(d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "peer", Port: "eth1", MAC: "02:00:00:00:02:02", VLANs: []pkt.VLANTag{{VID: 2}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "p", Type: "attach-probe", Device: "x", Config: []byte(`{"other":"y"}`)}); err != nil {
		t.Fatal(err)
	}
	if v, _ := a.GetProtocol("p"); v.State != core.StateRunning {
		t.Fatalf("probe %s %s", v.State, v.Error)
	}
	probesMu.Lock()
	p := probes["p"]
	probesMu.Unlock()
	// A frame from the peer in VLAN 2 reaches the probe through y.
	peer := pkt.Frame{Dst: pkt.MAC{2, 0, 0, 0, 1, 2}, Src: pkt.MAC{2, 0, 0, 0, 2, 2}, Tags: []pkt.VLANTag{{VID: 2}}, EtherType: 0x88b5, Payload: []byte{1}}
	a.Receive("eth1", peer.Encode(), time.Now())
	p.mu.Lock()
	if len(p.got) != 1 || p.got[0] != "other:02:00:00:00:02:02" {
		t.Fatalf("got %v", p.got)
	}
	p.mu.Unlock()
	if p.att.Device().Name() != "y" || p.att.Device().VLANs()[0].VID != 2 {
		t.Fatal("attachment device")
	}
	if err := p.att.Send(pkt.Broadcast, 0x88b5, []byte{2}, protocol.SendOpts{}); err != nil {
		t.Fatal(err)
	}
	// Deleting y closes the attachment.
	if err := a.DeleteDevice("y"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.att.Closed():
	case <-time.After(time.Second):
		t.Fatal("attachment not closed")
	}
	if err := p.att.Send(pkt.Broadcast, 0x88b5, []byte{2}, protocol.SendOpts{}); err == nil {
		t.Fatal("send on a closed attachment")
	}
	// Attaching to a missing device fails.
	if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "q", Type: "attach-probe", Device: "x", Config: []byte(`{"other":"nope"}`)}); err == nil {
		if v, _ := a.GetProtocol("q"); v.State != core.StateError {
			t.Fatal("attach to a missing device succeeded")
		}
	}
	// Deleting the instance closes its attachments.
	a.CreateDevice(core.DeviceSpec{Name: "y", Port: "eth1", MAC: "02:00:00:00:01:02", VLANs: []pkt.VLANTag{{VID: 2}}})
	a.DeleteProtocol("p")
	a.CreateProtocol(core.ProtocolSpec{Name: "p", Type: "attach-probe", Device: "x", Config: []byte(`{"other":"y"}`)})
	probesMu.Lock()
	p2 := probes["p"]
	probesMu.Unlock()
	a.DeleteProtocol("p")
	select {
	case <-p2.att.Closed():
	case <-time.After(time.Second):
		t.Fatal("attachment not closed with its instance")
	}
}
