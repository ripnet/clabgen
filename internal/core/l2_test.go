package core_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pkt"
	_ "github.com/ripnet/clabgen/protocols/l2stream"
)

// l2Pair is two engines with a device each on VLAN 100 and no IP config.
func l2Pair(t *testing.T) (*core.Engine, *core.Engine, *wire, pkt.MAC) {
	a, b, wa, _ := newPair()
	for i, e := range []*core.Engine{a, b} {
		mac := []string{"02:00:00:00:0a:01", "02:00:00:00:0b:01"}[i]
		if _, err := e.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", MAC: mac, VLANs: []pkt.VLANTag{{VID: 100}}}); err != nil {
			t.Fatal(err)
		}
	}
	bMAC, _ := pkt.ParseMAC("02:00:00:00:0b:01")
	return a, b, wa, bMAC
}

func mkL2(t *testing.T, e *core.Engine, name, typ, cfg string) {
	t.Helper()
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

func l2Received(t *testing.T, e *core.Engine) []core.ReceivedFlow {
	t.Helper()
	rs, err := e.ReceivedFlows("d")
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestL2OneWay(t *testing.T) {
	a, b, wa, bMAC := l2Pair(t)
	mkL2(t, a, "s", "l2", `{"dst_mac":"`+bMAC.String()+`","mode":"count","count":300,"rate":1000,"frame_size":128,"auto_start":true}`)
	eventually(t, "b counted 300 frames", func() bool {
		rs := l2Received(t, b)
		return len(rs) == 1 && rs[0].Flow.Rx == 300
	})
	f := l2Received(t, b)[0].Flow
	if f.Proto != "l2" || f.Src != "02:00:00:00:0a:01" || f.Dst != bMAC.String() || f.EtherType != "0x88b5" || f.Lost != 0 || f.Bytes != 300*128 {
		t.Fatalf("received %+v", f)
	}

	// Every 10th data frame lost on the wire, from 64 source MACs.
	if err := b.ClearReceivedFlows("d", nil); err != nil {
		t.Fatal(err)
	}
	wa.mu.Lock()
	wa.drop = dropEvery(10)
	wa.mu.Unlock()
	mkL2(t, a, "s2", "l2", `{"dst_mac":"`+bMAC.String()+`","src_mac":"02:aa:00:00:00:00","src_mac_count":64,"ethertype":36864,
		"mode":"count","count":500,"rate":2000,"auto_start":true}`)
	eventually(t, "b counted the rest", func() bool {
		rs := l2Received(t, b)
		return len(rs) == 1 && rs[0].Flow.Rx+rs[0].Flow.Lost >= 499
	})
	f = l2Received(t, b)[0].Flow
	if f.Lost < 45 || f.Lost > 55 || f.SrcMACs != 64 || f.EtherType != "0x9000" {
		t.Fatalf("lossy stream %+v", f)
	}
}

// A flooded stream reaches every device on the port, but is counted once,
// by a standalone device ahead of group members (even ones created first).
func TestL2FloodCountedOnce(t *testing.T) {
	a, b, _, _ := l2Pair(t)
	if err := b.DeleteDevice("d"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateGroup(groupSpec(t, "name: m\ncount: 3\ndevice: {port: eth1, vlans: [{vid: 100}]}")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", VLANs: []pkt.VLANTag{{VID: 100}}}); err != nil {
		t.Fatal(err)
	}
	mkL2(t, a, "s", "l2", `{"dst_mac":"ff:ff:ff:ff:ff:ff","mode":"count","count":100,"rate":1000,"auto_start":true}`)
	eventually(t, "d counted 100 frames", func() bool {
		rs := l2Received(t, b)
		return len(rs) == 1 && rs[0].Flow.Rx == 100
	})
	if rs, _ := b.ReceivedFlows(""); len(rs) != 1 || rs[0].Device != "d" {
		t.Fatalf("received %+v", rs)
	}

	// With only group members on the port, one of them counts it.
	if err := b.DeleteDevice("d"); err != nil {
		t.Fatal(err)
	}
	mkL2(t, a, "s2", "l2", `{"dst_mac":"ff:ff:ff:ff:ff:ff","mode":"count","count":100,"rate":1000,"auto_start":true}`)
	eventually(t, "a member counted 100 frames", func() bool {
		rs, _ := b.ReceivedFlows("")
		n := 0
		for _, r := range rs {
			n += int(r.Flow.Rx)
		}
		return len(rs) == 1 && n == 100
	})
}

func TestL2Echo(t *testing.T) {
	a, b, wa, bMAC := l2Pair(t)
	mkL2(t, b, "echo", "l2-echo", `{"ethertypes":[35000]}`)
	mkL2(t, a, "s", "l2", `{"dst_mac":"`+bMAC.String()+`","ethertype":35000,"reply":"echo","src_mac_count":8,"rate":200,"timeout":"300ms",
		"auto_start":true,"checks":{"max_loss_pct":1,"max_outage_ms":100}}`)
	verdict := func() map[string]any {
		v, _ := flowStatus(t, a, "s")["verdict"].(map[string]any)
		return v
	}
	// Echoes to all eight source MACs come back.
	eventually(t, "passing", func() bool {
		s := flowStatus(t, a, "s")
		return verdict()["result"] == "pass" && num(s, "rx") > 50 && num(s, "lost") == 0
	})
	if s := flowStatus(t, a, "s"); s["kind"] != "0x88b8 echo" || s["source"] != "02:00:00:00:0a:01 ×8" || s["rtt"] == nil {
		t.Errorf("status %v %v %v", s["kind"], s["source"], s["rtt"])
	}
	if s := flowStatus(t, b, "echo"); num(s, "sent") == 0 {
		t.Errorf("echo status %v", s)
	}
	// The sender doesn't count its own echoes as received flows.
	if rs := l2Received(t, a); len(rs) != 0 {
		t.Errorf("a received %+v", rs)
	}

	// A 400 ms blackout fails the outage check.
	wa.mu.Lock()
	wa.drop = func([]byte) bool { return true }
	wa.mu.Unlock()
	eventuallyLong(t, "failed", func() bool { return verdict()["result"] == "fail" })
	wa.mu.Lock()
	wa.drop = nil
	wa.mu.Unlock()
}

func TestL2Validation(t *testing.T) {
	a, _, _, _ := l2Pair(t)
	for _, cfg := range []string{`{"ethertype":2048}`, `{"ethertype":34825}`, `{"src_mac":"01:00:5e:00:00:01"}`, `{"frame_size":59}`, `{"reply":"maybe"}`} {
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "l2", Device: "d", Config: json.RawMessage(cfg)}); err == nil {
			t.Errorf("%s accepted", cfg)
			a.DeleteProtocol("x")
		}
	}
	// Caught at start: the destination inside the source range.
	mkL2(t, a, "overlap", "l2", `{"dst_mac":"02:00:00:00:0a:05","src_mac_count":8}`)
	if v, _ := a.GetProtocol("overlap"); v.State != "error" || !strings.Contains(v.Error, "one of the source MACs") {
		t.Errorf("overlap: %s %q", v.State, v.Error)
	}
}
