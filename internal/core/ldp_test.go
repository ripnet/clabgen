package core_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	_ "github.com/ripnet/clabgen/protocols/ldp"
)

// ldpWait is eventually with a longer limit, for hello and session timers.
func ldpWait(t *testing.T, what string, f func() bool, limit time.Duration) {
	t.Helper()
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

func ldpPeer(t *testing.T, e *core.Engine, lsr string) map[string]any {
	t.Helper()
	v, err := e.GetProtocol("ldp")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v.Status)
	var st map[string]any
	json.Unmarshal(b, &st)
	for _, x := range st["peers"].([]any) {
		if p := x.(map[string]any); p["lsr"] == lsr {
			return p
		}
	}
	return nil
}

func ldpState(t *testing.T, e *core.Engine, lsr string) string {
	t.Helper()
	p := ldpPeer(t, e, lsr)
	if p == nil {
		return ""
	}
	s, _ := p["state"].(string)
	return s
}

func ldpBindings(t *testing.T, e *core.Engine, peer string) map[string]uint32 {
	t.Helper()
	r, err := e.RunAction(context.Background(), "ldp", "bindings", json.RawMessage(`{"peer":"`+peer+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	var out struct {
		Bindings []struct {
			Prefix string `json:"prefix"`
			Label  uint32 `json:"label"`
		} `json:"bindings"`
	}
	json.Unmarshal(b, &out)
	m := map[string]uint32{}
	for _, x := range out.Bindings {
		m[x.Prefix] = x.Label
	}
	return m
}

// ldpPair is two LDP routers on a link, a with 192.0.2.1 (the lower, so b
// opens the session) and b with 192.0.2.2.
func ldpPair(t *testing.T, cfgA, cfgB string) (*core.Engine, *core.Engine, *wire) {
	t.Helper()
	a, b, wa, _ := newPair()
	for i, e := range []*core.Engine{a, b} {
		addr, gw, mac := "10.0.0.1/24", "10.0.0.2", "02:00:00:00:0a:01"
		cfg := cfgA
		if i == 1 {
			addr, gw, mac = "10.0.0.2/24", "10.0.0.1", "02:00:00:00:0b:01"
			cfg = cfgB
		}
		if _, err := e.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", MAC: mac, Addresses: []string{addr}, Gateway4: gw}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "ldp", Type: "ldp", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	return a, b, wa
}

const (
	ldpA = `{"router_id":"192.0.2.1","hello_interval":1,"hold_time":3,"keepalive":3,"fecs":[{"prefix":"198.51.100.0/24","count":3},{"prefix":"192.0.2.1/32","mode":"implicit-null"}]}`
	ldpB = `{"router_id":"192.0.2.2","hello_interval":1,"hold_time":3,"keepalive":3,"label_base":200000,"fecs":[{"prefix":"203.0.113.0/24","mode":"explicit-null"},{"prefix":"192.0.2.2/32","label":5000}]}`
)

func TestLDPSessionAndLabels(t *testing.T) {
	a, b, _ := ldpPair(t, ldpA, ldpB)
	ldpWait(t, "sessions operational", func() bool {
		return ldpState(t, a, "192.0.2.2") == "operational" && ldpState(t, b, "192.0.2.1") == "operational"
	}, 15*time.Second)
	if pa, pb := ldpPeer(t, a, "192.0.2.2"), ldpPeer(t, b, "192.0.2.1"); pa["role"] != "passive" || pb["role"] != "active" || pa["keepalive_s"].(float64) != 3 {
		t.Fatalf("%v %v", pa, pb)
	}
	eventually(t, "labels exchanged", func() bool {
		return len(ldpBindings(t, a, "192.0.2.2")) == 2 && len(ldpBindings(t, b, "192.0.2.1")) == 4
	})
	gotA, gotB := ldpBindings(t, a, "192.0.2.2"), ldpBindings(t, b, "192.0.2.1")
	if gotA["203.0.113.0/24"] != 0 || gotA["192.0.2.2/32"] != 5000 {
		t.Fatalf("a got %v", gotA)
	}
	if gotB["198.51.100.0/24"] != 100000 || gotB["198.51.101.0/24"] != 100001 || gotB["198.51.102.0/24"] != 100002 || gotB["192.0.2.1/32"] != 3 {
		t.Fatalf("b got %v", gotB)
	}
	addrs := ldpPeer(t, b, "192.0.2.1")["addresses"].([]any)
	if len(addrs) != 2 { // the LSR ID and the interface
		t.Fatalf("addresses %v", addrs)
	}

	// Withdraw: gone at the peer, and given back (a release).
	bgpLike := func(action, args string) map[string]any {
		t.Helper()
		r, err := a.RunAction(context.Background(), "ldp", action, json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return r.(map[string]any)
	}
	if r := bgpLike("withdraw", `{"fecs":[{"prefix":"198.51.101.0/24"}]}`); r["withdrawn"] != 1 || r["total"] != 3 {
		t.Fatalf("%v", r)
	}
	eventually(t, "withdrawn at the peer", func() bool { _, have := ldpBindings(t, b, "192.0.2.1")["198.51.101.0/24"]; return !have })
	eventually(t, "released back", func() bool {
		c, _ := ldpPeer(t, a, "192.0.2.2")["counters"].(map[string]any)
		return c["rx_release"] == float64(1)
	})
	// Advertise more: labels after the ones in use.
	if r := bgpLike("advertise", `{"fecs":[{"prefix":"100.64.0.0/24","count":2}]}`); r["advertised"] != 2 {
		t.Fatalf("%v", r)
	}
	eventually(t, "advertised at the peer", func() bool { return len(ldpBindings(t, b, "192.0.2.1")) == 5 })
	got := ldpBindings(t, b, "192.0.2.1")
	if l := got["100.64.0.0/24"]; l < 16 || l == got["198.51.100.0/24"] || l == got["198.51.102.0/24"] || l == got["100.64.1.0/24"] {
		t.Fatalf("labels clash: %v", got)
	}

	// A notification that isn't fatal leaves the session up.
	bgpLike("notify", `{"code":13}`)
	time.Sleep(200 * time.Millisecond)
	if ldpState(t, a, "192.0.2.2") != "operational" {
		t.Fatal("advisory notification ended the session")
	}

	// Reset: the session ends and comes back, with its labels.
	before := ldpPeer(t, a, "192.0.2.2")["sessions"].(float64)
	bgpLike("reset", `{}`)
	ldpWait(t, "session back", func() bool {
		p := ldpPeer(t, a, "192.0.2.2")
		return p != nil && p["state"] == "operational" && p["sessions"].(float64) > before && len(ldpBindings(t, b, "192.0.2.1")) == 5
	}, 15*time.Second)

	// Silent: the peer's timers end it; it comes back afterwards.
	bgpLike("flap", `{"seconds":6,"mode":"silent"}`)
	ldpWait(t, "peer's timers end it", func() bool { return ldpState(t, b, "192.0.2.1") != "operational" }, 8*time.Second)
	ldpWait(t, "back after the flap", func() bool { return ldpState(t, b, "192.0.2.1") == "operational" }, 25*time.Second)
}

// The hold time ends the session when the link goes quiet.
func TestLDPHoldExpires(t *testing.T) {
	a, b, wa := ldpPair(t, ldpA, ldpB)
	ldpWait(t, "up", func() bool { return ldpState(t, b, "192.0.2.1") == "operational" }, 15*time.Second)
	wa.mu.Lock()
	wa.up = false
	wa.mu.Unlock()
	ldpWait(t, "b lost a", func() bool { return ldpState(t, b, "192.0.2.1") != "operational" }, 10*time.Second)
	wa.mu.Lock()
	wa.up = true
	wa.mu.Unlock()
	ldpWait(t, "back", func() bool {
		return ldpState(t, a, "192.0.2.2") == "operational" && ldpState(t, b, "192.0.2.1") == "operational"
	}, 20*time.Second)
}

func TestLDPTargeted(t *testing.T) {
	a, b, _ := ldpPair(t, `{"router_id":"10.0.0.1","no_link_hellos":true,"accept_targeted":true,"keepalive":3,"fecs":[{"prefix":"198.51.100.0/24"}]}`,
		`{"router_id":"10.0.0.2","no_link_hellos":true,"targeted":[{"address":"10.0.0.1"}],"targeted_hello_interval":1,"targeted_hold_time":3,"keepalive":3}`)
	ldpWait(t, "targeted session up", func() bool {
		return ldpState(t, a, "10.0.0.2") == "operational" && ldpState(t, b, "10.0.0.1") == "operational"
	}, 25*time.Second)
	if k := ldpPeer(t, b, "10.0.0.1")["adjacencies"].([]any)[0].(map[string]any)["kind"]; k != "targeted" {
		t.Fatalf("%v", k)
	}
	eventually(t, "label", func() bool { return ldpBindings(t, b, "10.0.0.1")["198.51.100.0/24"] == 100000 })
}

func TestLDPRefusesTargetedUnlessAccepted(t *testing.T) {
	a, _, _ := ldpPair(t, `{"router_id":"10.0.0.1","no_link_hellos":true,"targeted":[{"address":"10.0.0.9"}]}`,
		`{"router_id":"10.0.0.2","no_link_hellos":true,"targeted":[{"address":"10.0.0.1"}],"targeted_hello_interval":1,"targeted_hold_time":3}`)
	time.Sleep(2500 * time.Millisecond)
	if ldpPeer(t, a, "10.0.0.2") != nil {
		t.Fatal("took a targeted hello it should refuse")
	}
}

// An MPLS packet with a label we advertised reaches the IP stack; another
// label doesn't. The ping is answered from inside, as plain IP.
func TestMPLSReceive(t *testing.T) {
	a, b, _ := ldpPair(t, ldpA, ldpB)
	ldpWait(t, "up", func() bool { return ldpState(t, b, "192.0.2.1") == "operational" }, 15*time.Second)
	// a has 198.51.100.0/24 at label 100000; a flow from b reaches it labelled.
	mk := func(name string, label int) {
		t.Helper()
		cfg, _ := json.Marshal(map[string]any{"rate": 100, "auto_start": true, "layers": []any{
			map[string]any{"mpls": map[string]any{"label": label}},
			map[string]any{"ipv4": map[string]any{"dst": "198.51.100.1"}},
			map[string]any{"udp": map[string]any{"dst_port": 5001}},
			map[string]any{"payload": map[string]any{"size": 64}},
		}})
		if _, err := b.CreateProtocol(core.ProtocolSpec{Name: name, Type: "packet", Device: "d", Config: cfg}); err != nil {
			t.Fatal(err)
		}
	}
	mk("ours", 100000)
	mk("other", 99999)
	id := func(n string) string { s, _ := flowStatus(t, b, n)["flow_id"].(string); return s }
	eventually(t, "our label delivered", func() bool { m := rxFlow(t, a, id("ours")); return m != nil && num(m, "rx") >= 10 })
	if m := rxFlow(t, a, id("other")); m != nil {
		t.Fatalf("delivered with a label that isn't ours: %v", m)
	}
	// Withdrawn: the label is no longer ours.
	if _, err := a.RunAction(context.Background(), "ldp", "withdraw", json.RawMessage(`{"fecs":[{"prefix":"198.51.100.0/24"}]}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	n := num(rxFlow(t, a, id("ours")), "rx")
	time.Sleep(300 * time.Millisecond)
	if d := num(rxFlow(t, a, id("ours")), "rx") - n; d > 2 {
		t.Fatalf("still delivered after the withdraw: %v more", d)
	}
	// Only the label went: plain IP to the prefix is still answered.
	if _, err := b.CreateProtocol(core.ProtocolSpec{Name: "ping", Type: "icmp", Device: "d", Config: json.RawMessage(`{"destination":"198.51.100.1","rate":50,"timeout":"200ms","auto_start":true}`)}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "plain IP answered", func() bool { return num(flowStatus(t, b, "ping"), "rx") >= 5 })
}
