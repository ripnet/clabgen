package core_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
)

// A link cut for ~300 ms, measured by an echo flow at its sender and a
// one-way flow at its receiver; statistics and checks are left alone.
func TestConvergence(t *testing.T) {
	a, b, wa, _ := ipPair(t)
	mk := func(name, typ, cfg string) string {
		t.Helper()
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: name, Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
		id, _ := flowStatus(t, a, name)["flow_id"].(string)
		return id
	}
	echo := mk("echo", "icmp", `{"destination":"10.0.0.2","rate":200,"timeout":"200ms","auto_start":true}`)
	oneWay := mk("oneway", "udp", `{"destination":"10.0.0.2","dst_port":5001,"rate":200,"auto_start":true}`)
	eventually(t, "flows running", func() bool {
		return num(flowStatus(t, a, "echo"), "rx") >= 50 && rxFlow(t, b, oneWay) != nil && num(rxFlow(t, b, oneWay), "rx") >= 50
	})

	one := func(e *core.Engine, id, side string, since time.Time) core.ConvergenceFlow {
		t.Helper()
		for _, f := range e.Convergence(since, time.Now(), []string{id}) {
			if f.Side == side {
				return f
			}
		}
		t.Fatalf("no %s for %s", side, id)
		return core.ConvergenceFlow{}
	}
	since := time.Now()
	if f := one(a, echo, "sender", since); f.Name != "echo" || f.Device != "d" || f.OneWay || !f.Running || f.Status != "no_impact" || f.ResolutionMs != 5 {
		t.Fatalf("before: %+v", f)
	}

	wa.mu.Lock()
	wa.up = false
	wa.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	wa.mu.Lock()
	wa.up = true
	wa.mu.Unlock()

	inRange := func(f core.ConvergenceFlow) {
		t.Helper()
		if f.Status != "restored" || f.LossMs < 200 || f.LossMs > 450 || f.FirstLossMs == nil || *f.FirstLossMs > 100 ||
			*f.LastLossMs < 200 || *f.LastLossMs > 500 || len(f.Outages) != 1 {
			t.Fatalf("%+v", f)
		}
	}
	eventually(t, "echo measured", func() bool { f := one(a, echo, "sender", since); return f.Status == "restored" && f.CleanMs > 0 })
	inRange(one(a, echo, "sender", since))
	eventually(t, "receiver measured", func() bool { return one(b, oneWay, "receiver", since).Status == "restored" })
	inRange(one(b, oneWay, "receiver", since))
	if f := one(a, oneWay, "sender", since); !f.OneWay || f.Status != "no_impact" {
		t.Fatalf("one-way sender knows of no loss: %+v", f)
	}
	if num(flowStatus(t, a, "echo"), "lost") == 0 {
		t.Fatal("statistics cleared")
	}
	// Measuring from after the failure: clean.
	if f := one(a, echo, "sender", time.Now()); f.Status != "no_impact" {
		t.Fatalf("after: %+v", f)
	}

	run(t, a, "echo", "stop")
	if f := one(a, echo, "sender", since); f.Status != "stopped" || f.Running {
		t.Fatalf("stopped: %+v", f)
	}
	run(t, a, "oneway", "stop")
}

func TestFlowToBGPRoutes(t *testing.T) {
	a, b, _, _ := newPair()
	dev := func(e *core.Engine, mac, addr, gw string) {
		t.Helper()
		if _, err := e.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", MAC: mac, Addresses: []string{addr}, Gateway4: gw}); err != nil {
			t.Fatal(err)
		}
	}
	dev(a, "02:00:00:00:0a:01", "10.0.0.1/24", "10.0.0.2")
	dev(b, "02:00:00:00:0b:01", "10.0.0.2/24", "10.0.0.1")
	mk := func(e *core.Engine, name, typ, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	// b answers for its next-hop-self routes, not one with its next hop elsewhere.
	mk(b, "bgp", "bgp", `{"local_as":65002,"peers":[{"address":"10.0.0.1","remote_as":65001,"passive":true}],
		"routes":[{"prefix":"198.51.100.0/24","count":4},{"prefix":"203.0.113.0/24","next_hop":"10.0.0.9"}]}`)
	mk(a, "many", "icmp", `{"destinations":[{"prefix":"198.51.100.0/24","count":4}],"rate":200,"timeout":"100ms","auto_start":true}`)
	mk(a, "other", "icmp", `{"destination":"203.0.113.1","rate":100,"timeout":"100ms","auto_start":true}`)
	eventually(t, "replies from every prefix", func() bool { return num(flowStatus(t, a, "many"), "rx") >= 40 })
	st := flowStatus(t, a, "many")
	if num(st, "lost") != 0 || st["destination"] != "198.51.100.0/24 +3 more" {
		t.Fatalf("%v", st)
	}
	if p := st["prefixes"].(map[string]any); num(p, "total") != 4 || num(p, "impacted") != 0 || num(p, "probe_ms") != 20 || p["measured"] != true {
		t.Fatalf("%v", p)
	}
	eventually(t, "next hop elsewhere: unanswered", func() bool { return num(flowStatus(t, a, "other"), "lost") >= 5 })
	if num(flowStatus(t, a, "other"), "rx") != 0 {
		t.Fatal("answered for a route whose next hop is elsewhere")
	}
	run(t, a, "other", "stop")

	// Withdraw two of the four: only they lose traffic, and are named.
	since := time.Now()
	if _, err := b.RunAction(context.Background(), "bgp", "withdraw", json.RawMessage(`{"routes":[{"prefix":"198.51.101.0/24","count":2}]}`)); err != nil {
		t.Fatal(err)
	}
	var f core.ConvergenceFlow
	eventually(t, "two prefixes lose traffic", func() bool {
		for _, x := range a.Convergence(since, time.Now(), nil) {
			if x.Name == "many" {
				f = x
			}
		}
		return f.Prefixes != nil && f.Prefixes.Impacted == 2 && f.Prefixes.List[1].Lost >= 3
	})
	if n := f.Prefixes.List[0].Prefix + " " + f.Prefixes.List[1].Prefix; n != "198.51.101.0/24 198.51.102.0/24" && n != "198.51.102.0/24 198.51.101.0/24" {
		t.Fatalf("named %s", n)
	}
	if f.Status != "not_restored" || !f.Prefixes.List[0].Ongoing || f.ResolutionMs != 20 || f.LossMs != f.Prefixes.List[0].LossMs {
		t.Fatalf("still withdrawn, and the flow's loss is the worst prefix's: %+v", f)
	}
	// Back: clean again from here on.
	if _, err := b.RunAction(context.Background(), "bgp", "advertise", json.RawMessage(`{"routes":[{"prefix":"198.51.101.0/24","count":2}]}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	since = time.Now()
	time.Sleep(300 * time.Millisecond)
	for _, x := range a.Convergence(since, time.Now(), nil) {
		if x.Name == "many" && x.Prefixes.Impacted != 0 {
			t.Fatalf("%+v", x.Prefixes)
		}
	}
	run(t, a, "many", "stop")
}

func TestFlowDestinationsMustBeOffLink(t *testing.T) {
	a, _, _, _ := ipPair(t)
	if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "f", Type: "icmp", Device: "d", Config: json.RawMessage(`{"destinations":[{"prefix":"10.0.0.0/30"}],"rate":10,"auto_start":true}`)}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "refused", func() bool {
		s, _ := flowStatus(t, a, "f")["error"].(string)
		return strings.Contains(s, "off-link")
	})
	for _, bad := range []string{`{"rate":1}`, `{"destination":"10.0.0.2","destinations":[{"prefix":"198.51.100.0/24"}],"rate":1}`,
		`{"destinations":[{"prefix":"198.51.100.0/24"},{"prefix":"2001:db8::/48"}],"rate":1}`, `{"destinations":[{"prefix":"nonsense"}],"rate":1}`} {
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "bad", Type: "icmp", Device: "d", Config: json.RawMessage(bad)}); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
