package core_test

import (
	"encoding/json"
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
