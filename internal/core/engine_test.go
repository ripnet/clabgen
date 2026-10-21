package core_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/events"
	"github.com/ripnet/clabgen/internal/pkt"
	_ "github.com/ripnet/clabgen/protocols/cfm"
)

// wire joins port "eth1" of two engines.
type wire struct {
	mu    sync.Mutex
	peer  *core.Engine
	up    bool
	admin map[string]bool
}

func (w *wire) Send(port string, b []byte) error {
	w.mu.Lock()
	p, up := w.peer, w.up
	w.mu.Unlock()
	if up && p != nil {
		go p.Receive("eth1", append([]byte(nil), b...), time.Now())
	}
	return nil
}
func (w *wire) PortUp(string) bool { w.mu.Lock(); defer w.mu.Unlock(); return w.up }
func (w *wire) SetAdmin(p string, up bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.admin[p] = up
	return nil
}
func (w *wire) Desired() map[string]bool { return map[string]bool{} }

func newPair() (*core.Engine, *core.Engine, *wire, *wire) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	wa, wb := &wire{up: true, admin: map[string]bool{}}, &wire{up: true, admin: map[string]bool{}}
	a := core.New(wa, events.NewBus(), log)
	b := core.New(wb, events.NewBus(), log)
	wa.peer, wb.peer = b, a
	return a, b, wa, wb
}

const scenarioA = `
ports:
  eth1: {admin: up}
devices:
  - name: d100
    port: eth1
    mac: 02:00:00:00:01:00
    vlans: [{vid: 100}]
  - name: d200
    port: eth1
    mac: 02:00:00:00:02:00
    vlans: [{tpid: 0x88a8, vid: 200}, {vid: 10}]
protocols:
  - name: mep100
    type: cfm-mep
    device: d100
    config: {mep_id: 1, remote_meps: [2], ccm_interval: 10ms, ma_name: v100}
  - name: mep200
    type: cfm-mep
    device: d200
    config: {mep_id: 1, remote_meps: [2], ccm_interval: 10ms, ma_name: v200}
`

func mepStatus(t *testing.T, e *core.Engine, name string) map[string]any {
	t.Helper()
	v, err := e.GetProtocol(name)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v.Status)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func rmepOK(t *testing.T, e *core.Engine, name string) bool {
	r, _ := mepStatus(t, e, name)["remote_meps"].([]any)
	return len(r) == 1 && r[0].(map[string]any)["state"] == "ok"
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

func TestEngineVLANDispatchAndScenario(t *testing.T) {
	a, b, wa, _ := newPair()
	sa, err := core.ParseScenario([]byte(scenarioA))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(sa, false); err != nil {
		t.Fatal(err)
	}
	if !wa.admin["eth1"] {
		t.Error("port admin not applied")
	}
	// Side b: create protocols before devices to exercise the waiting state.
	sb := strings.NewReplacer("mep_id: 1, remote_meps: [2]", "mep_id: 2, remote_meps: [1]", "02:00:00:00:0", "02:00:00:00:f").Replace(scenarioA)
	scb, _ := core.ParseScenario([]byte(sb))
	devs := scb.Devices
	scb.Devices = nil
	if err := b.Apply(scb, false); err != nil {
		t.Fatal(err)
	}
	if v, _ := b.GetProtocol("mep100"); v.State != core.StateWaiting {
		t.Fatalf("expected waiting, got %s", v.State)
	}
	for _, d := range devs {
		if _, err := b.CreateDevice(d); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "both VLAN MEPs up on both sides", func() bool {
		return rmepOK(t, a, "mep100") && rmepOK(t, a, "mep200") && rmepOK(t, b, "mep100") && rmepOK(t, b, "mep200")
	})
	// A MAID mismatch would show if the VLANs were crossed.
	for _, n := range []string{"mep100", "mep200"} {
		if d := mepStatus(t, a, n)["defects"].([]any); len(d) != 0 {
			t.Errorf("%s defects %v", n, d)
		}
	}

	// Deleting b's d200 device makes a's mep200 lose its peer.
	if err := b.DeleteDevice("d200"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "LOC on mep200", func() bool {
		return mepStatus(t, a, "mep200")["highest_defect"] == "DefRemoteCCM"
	})
	if rmepOK(t, a, "mep100") == false {
		t.Error("mep100 should be unaffected")
	}

	// Export and re-import round-trips.
	y, err := a.Export().YAML()
	if err != nil {
		t.Fatal(err)
	}
	re, err := core.ParseScenario(y)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, y)
	}
	if len(re.Devices) != 2 || len(re.Protocols) != 2 {
		t.Errorf("export lost objects:\n%s", y)
	}
	if err := a.Apply(re, true); err != nil {
		t.Fatal(err)
	}
	if len(a.Protocols(false)) != 2 {
		t.Error("replace apply should leave exactly 2 protocols")
	}
}

func TestEngineErrors(t *testing.T) {
	a, _, _, _ := newPair()
	if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "nope", Device: "d"}); err == nil {
		t.Error("unknown type accepted")
	}
	if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "cfm-mep", Device: "d", Config: json.RawMessage(`{"bogus":1}`)}); err == nil {
		t.Error("unknown config field accepted")
	}
	if _, err := a.CreateDevice(core.DeviceSpec{Name: "bad name", Port: "eth1"}); err == nil {
		t.Error("bad device name accepted")
	}
	d, err := a.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1"})
	if err != nil {
		t.Fatal(err)
	}
	_ = d
	devs := a.Devices()
	if len(devs) != 1 || !strings.ContainsRune("26ae", rune(devs[0].MAC[1])) {
		t.Errorf("derived MAC should be locally administered: %+v", devs)
	}
	if _, err := core.ParseScenario([]byte("devices: [{name: d, port: eth1, colour: red}]")); err == nil {
		t.Error("unknown scenario field accepted")
	}
}

func TestEngineUpdateDevice(t *testing.T) {
	a, b, _, _ := newPair()
	for _, e := range []*core.Engine{a, b} {
		if _, err := e.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", VLANs: []pkt.VLANTag{{VID: 10}}}); err != nil {
			t.Fatal(err)
		}
	}
	a.CreateProtocol(core.ProtocolSpec{Name: "m", Type: "cfm-mep", Device: "d", Config: json.RawMessage(`{"mep_id":1,"remote_meps":[2],"ccm_interval":"10ms"}`)})
	b.CreateProtocol(core.ProtocolSpec{Name: "m", Type: "cfm-mep", Device: "d", Config: json.RawMessage(`{"mep_id":2,"remote_meps":[1],"ccm_interval":"10ms"}`)})
	eventually(t, "up", func() bool { return rmepOK(t, a, "m") && rmepOK(t, b, "m") })

	// Retag b: a loses its peer.
	if _, err := b.UpdateDevice(core.DeviceSpec{Name: "d", Port: "eth1", VLANs: []pkt.VLANTag{{VID: 20}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "LOC after retag", func() bool { return mepStatus(t, a, "m")["highest_defect"] == "DefRemoteCCM" })
	if v, _ := b.GetProtocol("m"); v.State != core.StateRunning {
		t.Fatalf("protocol not restarted: %s %s", v.State, v.Error)
	}

	// Restore the tag and change the MAC: a recovers and learns the new MAC.
	if _, err := b.UpdateDevice(core.DeviceSpec{Name: "d", Port: "eth1", MAC: "02:aa:bb:cc:dd:ee", VLANs: []pkt.VLANTag{{VID: 10}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "recovery with new MAC", func() bool {
		r, _ := mepStatus(t, a, "m")["remote_meps"].([]any)
		return len(r) == 1 && r[0].(map[string]any)["state"] == "ok" && r[0].(map[string]any)["mac"] == "02:aa:bb:cc:dd:ee"
	})
	if d := b.Devices(); len(d) != 1 || d[0].MAC != "02:aa:bb:cc:dd:ee" || d[0].VLANs[0].VID != 10 {
		t.Errorf("devices: %+v", d)
	}

	for _, bad := range []core.DeviceSpec{
		{Name: "nope", Port: "eth1"},
		{Name: "d", Port: "eth1", MAC: "01:00:5e:00:00:01"},
		{Name: "d", Port: "eth1", VLANs: []pkt.VLANTag{{TPID: 0x1234, VID: 1}}},
		{Name: "d", Port: ""},
	} {
		if _, err := b.UpdateDevice(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	// A rejected update leaves the device as it was.
	if d := b.Devices(); d[0].MAC != "02:aa:bb:cc:dd:ee" {
		t.Errorf("device changed by failed update: %+v", d)
	}
}

func TestEngineIP(t *testing.T) {
	a, b, _, _ := newPair()
	va := core.DeviceSpec{Name: "d", Port: "eth1", VLANs: []pkt.VLANTag{{VID: 10}}, Addresses: []string{"10.0.0.1/24", "2001:db8::1/64"}}
	if _, err := a.CreateDevice(va); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", VLANs: []pkt.VLANTag{{VID: 10}},
		Addresses: []string{"10.0.0.2/24", "2001:db8::2/64"}, Gateway4: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	b.CreateProtocol(core.ProtocolSpec{Name: "m", Type: "cfm-mep", Device: "d", Config: json.RawMessage(`{"mep_id":2,"remote_meps":[1],"ccm_interval":"10ms"}`)})
	a.CreateProtocol(core.ProtocolSpec{Name: "m", Type: "cfm-mep", Device: "d", Config: json.RawMessage(`{"mep_id":1,"remote_meps":[2],"ccm_interval":"10ms"}`)})

	ctx := context.Background()
	for _, ip := range []string{"10.0.0.2", "2001:db8::2"} {
		nh, mac, _, err := a.ResolveNeighbor(ctx, "d", netip.MustParseAddr(ip))
		if err != nil || nh.String() != ip || mac.String() != b.Devices()[0].MAC {
			t.Fatalf("resolve %s: %s %s %v", ip, nh, mac, err)
		}
	}
	// Off-link: b resolves through its gateway; a has none.
	if nh, _, _, err := b.ResolveNeighbor(ctx, "d", netip.MustParseAddr("192.0.2.1")); err != nil || nh.String() != "10.0.0.1" {
		t.Fatalf("via gateway: %s %v", nh, err)
	}
	if _, _, _, err := a.ResolveNeighbor(ctx, "d", netip.MustParseAddr("192.0.2.1")); err == nil {
		t.Fatal("resolved off-link without a gateway")
	}
	st, err := b.DeviceIP("d")
	if err != nil || len(st.Addresses) != 3 || st.Counters["arp_tx_replies"] == 0 || st.Counters["nd_tx_adverts"] == 0 {
		t.Fatalf("b ip status %+v %v", st, err)
	}

	// An IP-only change is applied in place: the MEP keeps running.
	eventually(t, "mep up", func() bool { return rmepOK(t, a, "m") })
	before := mepStatus(t, a, "m")["tx"].(map[string]any)["CCM"].(float64)
	va.Addresses = []string{"10.0.0.11/24"}
	va.VLANs = []pkt.VLANTag{{TPID: pkt.TPIDDot1Q, VID: 10}} // same tag, spelled out as clients may send it
	if _, err := a.UpdateDevice(va); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := b.ResolveNeighbor(ctx, "d", netip.MustParseAddr("10.0.0.11")); err != nil {
		t.Fatal(err)
	}
	if after := mepStatus(t, a, "m")["tx"].(map[string]any)["CCM"].(float64); after < before {
		t.Fatalf("a's MEP restarted (%v → %v)", before, after)
	}
	if v, _ := a.GetProtocol("m"); v.State != core.StateRunning {
		t.Fatal(v.Error)
	}
	if d := a.Devices()[0]; len(d.Addresses) != 1 || d.Addresses[0] != "10.0.0.11/24" {
		t.Fatalf("addresses %v", d.Addresses)
	}
	for _, bad := range []core.DeviceSpec{
		{Name: "x", Port: "eth1", Addresses: []string{"10.0.0.1"}},
		{Name: "x", Port: "eth1", Addresses: []string{"10.0.0.1/24"}, Gateway4: "10.1.0.1"},
	} {
		if _, err := a.CreateDevice(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// Two devices on one port and VLAN hear each other, like hosts on a
// segment; devices in other VLANs don't.
func TestLocalDelivery(t *testing.T) {
	a, _, _, _ := newPair()
	for _, d := range []core.DeviceSpec{
		{Name: "x", Port: "eth1", MAC: "02:00:00:00:00:0a", VLANs: []pkt.VLANTag{{VID: 7}}, Addresses: []string{"10.7.0.1/24"}},
		{Name: "y", Port: "eth1", MAC: "02:00:00:00:00:0b", VLANs: []pkt.VLANTag{{VID: 7}}, Addresses: []string{"10.7.0.2/24"}},
		{Name: "z", Port: "eth1", MAC: "02:00:00:00:00:0c", VLANs: []pkt.VLANTag{{VID: 8}}, Addresses: []string{"10.7.0.3/24"}},
	} {
		if _, err := a.CreateDevice(d); err != nil {
			t.Fatal(err)
		}
	}
	nh, mac, _, err := a.ResolveNeighbor(context.Background(), "x", netip.MustParseAddr("10.7.0.2"))
	if err != nil || mac.String() != "02:00:00:00:00:0b" || nh.String() != "10.7.0.2" {
		t.Fatalf("x → y: %s %v", mac, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, _, err := a.ResolveNeighbor(ctx, "x", netip.MustParseAddr("10.7.0.3")); err == nil {
		t.Fatal("resolved a device in another VLAN")
	}
}
