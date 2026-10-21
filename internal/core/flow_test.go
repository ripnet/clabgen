package core_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pkt"
	_ "github.com/ripnet/clabgen/protocols/icmp"
)

func flowStatus(t *testing.T, e *core.Engine, name string) map[string]any {
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

func num(m map[string]any, k string) float64 { f, _ := m[k].(float64); return f }

func ipPair(t *testing.T) (*core.Engine, *core.Engine, *wire, *wire) {
	a, b, wa, wb := newPair()
	for i, e := range []*core.Engine{a, b} {
		ip := []string{"10.0.0.1/24", "2001:db8::1/64"}
		if i == 1 {
			ip = []string{"10.0.0.2/24", "2001:db8::2/64"}
		}
		if _, err := e.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", VLANs: []pkt.VLANTag{{VID: 10}}, Addresses: ip}); err != nil {
			t.Fatal(err)
		}
	}
	return a, b, wa, wb
}

func mkFlow(t *testing.T, e *core.Engine, name, cfg string) {
	t.Helper()
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: "icmp", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, e *core.Engine, name, action string) {
	t.Helper()
	if _, err := e.RunAction(context.Background(), name, action, nil); err != nil {
		t.Fatalf("%s %s: %v", name, action, err)
	}
}

func TestICMPFlowOutage(t *testing.T) {
	a, _, wa, wb := ipPair(t)
	mkFlow(t, a, "p", `{"destination":"10.0.0.2","rate":100,"timeout":"200ms"}`)
	run(t, a, "p", "start")
	eventually(t, "replies", func() bool { return num(flowStatus(t, a, "p"), "rx") >= 20 })
	st := flowStatus(t, a, "p")
	if st["state"] != "running" || st["source"] != "10.0.0.1" || st["kind"] != "echo" || num(st, "lost") != 0 {
		t.Fatalf("running status %v", st)
	}

	// Cut the link for ~300 ms: one outage of roughly that length.
	for _, w := range []*wire{wa, wb} {
		w.mu.Lock()
		w.up = false
		w.mu.Unlock()
	}
	time.Sleep(300 * time.Millisecond)
	for _, w := range []*wire{wa, wb} {
		w.mu.Lock()
		w.up = true
		w.mu.Unlock()
	}
	eventually(t, "outage recorded", func() bool { return num(flowStatus(t, a, "p"), "outages") >= 1 })
	st = flowStatus(t, a, "p")
	if ms := num(st, "outage_max_ms"); ms < 200 || ms > 500 {
		t.Fatalf("outage %v ms, want ~300 (%v)", ms, st)
	}
	run(t, a, "p", "stop")
	time.Sleep(300 * time.Millisecond)
	st = flowStatus(t, a, "p")
	if st["state"] != "idle" || num(st, "pending") != 0 || num(st, "tx") != num(st, "rx")+num(st, "lost") {
		t.Fatalf("after stop %v", st)
	}
	if tl, _ := st["timeline"].([]any); len(tl) == 0 {
		t.Fatal("no timeline")
	}
	rtt, _ := st["rtt"].(map[string]any)
	if rtt == nil || num(rtt, "avg_us") <= 0 {
		t.Fatalf("rtt %v", st["rtt"])
	}
	run(t, a, "p", "clear")
	if st = flowStatus(t, a, "p"); num(st, "tx") != 0 {
		t.Fatalf("clear: %v", st)
	}
}

func TestICMPFlowVariants(t *testing.T) {
	a, b, _, _ := ipPair(t)
	// IPv6, count mode, auto start, no signature.
	mkFlow(t, a, "v6", `{"destination":"2001:db8::2","mode":"count","count":5,"rate":50,"timeout":"200ms","auto_start":true,"signature":false,"payload_size":10}`)
	eventually(t, "v6 done", func() bool { return flowStatus(t, a, "v6")["state"] == "done" })
	if st := flowStatus(t, a, "v6"); num(st, "tx") != 5 || num(st, "rx") != 5 {
		t.Fatalf("v6 %v", st)
	}
	// Restarting a finished count flow sends another count.
	run(t, a, "v6", "start")
	eventually(t, "v6 second run", func() bool { st := flowStatus(t, a, "v6"); return st["state"] == "done" && num(st, "rx") == 10 })

	// A bad ICMP checksum is dropped by the receiver: everything is lost.
	mkFlow(t, a, "bad", `{"destination":"10.0.0.2","mode":"count","count":3,"rate":50,"timeout":"100ms","icmp_checksum":"invalid","auto_start":true}`)
	eventually(t, "bad done", func() bool { return flowStatus(t, a, "bad")["state"] == "done" })
	if st := flowStatus(t, a, "bad"); num(st, "lost") != 3 || num(st, "rx") != 0 {
		t.Fatalf("bad checksum %v", st)
	}
	ipb, _ := b.DeviceIP("d")
	if ipb.Counters["ipv4_errors"] < 3 {
		t.Fatalf("receiver didn't count bad checksums: %v", ipb.Counters)
	}

	// Non-echo types are sent without reply tracking.
	mkFlow(t, a, "ts", `{"destination":"10.0.0.2","mode":"count","count":2,"rate":50,"icmp_type":13,"auto_start":true}`)
	eventually(t, "ts done", func() bool { return flowStatus(t, a, "ts")["state"] == "done" })
	if st := flowStatus(t, a, "ts"); st["tracks_replies"] != false || num(st, "tx") != 2 || num(st, "lost") != 0 {
		t.Fatalf("type 13 %v", st)
	}

	// Preview shows the crafted header fields.
	mkFlow(t, a, "pv", `{"destination":"10.0.0.2","ttl":3,"dscp":46,"df":true,"identifier":4660,"seq_start":100,"ip_checksum":"zero"}`)
	r, err := a.RunAction(context.Background(), "pv", "preview", nil)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(r)
	var pv struct {
		Hex    string
		Length int
	}
	json.Unmarshal(b2, &pv)
	// 14 + 4 (VLAN) + 20 + 8 + 56 bytes; TOS 0xb8, DF, TTL 3, checksum 0, id 0x1234 seq 100.
	if pv.Length != 102 || pv.Hex[36:38] != "45" || pv.Hex[38:40] != "b8" || pv.Hex[48:50] != "40" || pv.Hex[52:54] != "03" ||
		pv.Hex[56:60] != "0000" || pv.Hex[84:88] != "1234" || pv.Hex[88:92] != "0064" {
		t.Fatalf("preview %+v", pv)
	}

	// No address of the family: the flow reports why it can't start.
	mkFlow(t, a, "nov6", `{"destination":"2001:db8:9::9","auto_start":true}`)
	eventually(t, "no gateway error", func() bool {
		st := flowStatus(t, a, "nov6")
		e, _ := st["error"].(string)
		return st["state"] == "resolving" && e != ""
	})

	for _, bad := range []string{
		`{"destination":"nope"}`,
		`{"destination":"10.0.0.2","mode":"count"}`,
		`{"destination":"10.0.0.2","payload_size":10}`,
		`{"destination":"10.0.0.2","seq_mode":"fixed","signature":false}`,
		`{"destination":"10.0.0.2","rate":100000,"timeout":"1s"}`,
		`{"destination":"10.0.0.2","source":"2001:db8::1"}`,
	} {
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "icmp", Device: "d", Config: json.RawMessage(bad)}); err == nil {
			a.DeleteProtocol("x")
			t.Errorf("accepted %s", bad)
		}
	}
}
