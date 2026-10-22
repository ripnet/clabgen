package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	_ "github.com/ripnet/clabgen/protocols/bgp"
)

func bgpPeer(t *testing.T, e *core.Engine, addr string) map[string]any {
	t.Helper()
	for _, x := range flowStatus(t, e, "bgp")["peers"].([]any) {
		if p := x.(map[string]any); p["address"] == addr {
			return p
		}
	}
	t.Fatalf("no peer %s", addr)
	return nil
}

func bgpRun(t *testing.T, e *core.Engine, action, args string) map[string]any {
	t.Helper()
	r, err := e.RunAction(context.Background(), "bgp", action, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	b, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func received(p map[string]any, fam string) float64 { return num(p["received"].(map[string]any), fam) }

// eventuallyLong is eventually with more time (TCP, BGP timers).
func eventuallyLong(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

func TestBGP(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mk := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "bgp", Type: "bgp", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	// eBGP over IPv4 carrying both families, and iBGP over IPv6; both sides
	// dial (connection collision).
	mk(a, `{"local_as":65001,"hold_time":9,"peers":[
			{"address":"10.0.0.2","remote_as":4200000002,"families":["ipv4-unicast","ipv6-unicast"],"connect_retry":1},
			{"address":"2001:db8::2","remote_as":65001,"connect_retry":1}],
		"routes":[{"prefix":"198.51.100.0/24","count":1000,"med":5,"communities":["65001:100","no-export"],"large_communities":["65001:1:2"],"ext_communities":["rt:65001:7"]},
			{"prefix":"2001:db8:100::/48","count":300,"as_path":[64999]},
			{"prefix":"192.0.2.0/24","local_pref":250,"peers":["2001:db8::2"]}]}`)
	mk(b, `{"local_as":4200000002,"router_id":"10.0.0.2","hold_time":9,"peers":[
			{"address":"10.0.0.1","remote_as":65001,"families":["ipv4-unicast","ipv6-unicast"],"connect_retry":1}],
		"routes":[{"prefix":"203.0.113.0/24"}]}`)
	// b's second speaker can't share the device's port 179: an iBGP peer on b
	// would need it; instead a second instance shows the listen error.
	if _, err := b.CreateProtocol(core.ProtocolSpec{Name: "bgp2", Type: "bgp", Device: "d", Config: json.RawMessage(`{"local_as":1}`)}); err == nil {
		v, _ := b.GetProtocol("bgp2")
		if v.State != "error" || !strings.Contains(v.Error, "179") {
			t.Fatalf("second speaker on a device: %+v", v)
		}
		b.DeleteProtocol("bgp2")
	}
	eventuallyLong(t, "eBGP up with all routes", func() bool {
		p := bgpPeer(t, b, "10.0.0.1")
		return p["state"] == "established" && received(p, "ipv4-unicast") == 1000 && received(p, "ipv6-unicast") == 300
	})
	pb := bgpPeer(t, b, "10.0.0.1")
	if fmt.Sprint(pb["end_of_rib_received"]) != "[ipv4-unicast ipv6-unicast]" || pb["type"] != "ebgp" || pb["remote_id"] != "10.0.0.1" || num(pb, "hold_time") != 9 {
		t.Fatalf("b's peer %v", pb)
	}
	if pa := bgpPeer(t, a, "10.0.0.2"); received(pa, "ipv4-unicast") != 1 || num(pa["advertised"].(map[string]any), "ipv4-unicast") != 1000 {
		t.Fatalf("a's peer %v", pa)
	}
	rib := bgpRun(t, b, "rib", `{"peer":"10.0.0.1","prefix":"198.51.100.0/24"}`)
	r := rib["routes"].([]any)[0].(map[string]any)
	if r["next_hop"] != "10.0.0.1" || r["as_path"] != "65001" || num(r, "med") != 5 || fmt.Sprint(r["communities"]) != "[65001:100 no-export]" ||
		fmt.Sprint(r["large_communities"]) != "[65001:1:2]" || !strings.Contains(fmt.Sprint(r["ext_communities"]), "65001:7") || r["local_pref"] != nil {
		t.Fatalf("eBGP route %v", r)
	}
	r6 := bgpRun(t, b, "rib", `{"peer":"10.0.0.1","family":"ipv6-unicast","limit":1}`)["routes"].([]any)[0].(map[string]any)
	if r6["prefix"] != "2001:db8:100::/48" || r6["next_hop"] != "2001:db8::1" || r6["as_path"] != "65001 64999" {
		t.Fatalf("IPv6 route %v", r6)
	}
	// A route with our own AS in its path is a loop: dropped, counted.
	bgpRun(t, b, "advertise", `{"routes":[{"prefix":"10.123.0.0/16","as_path":[65001]}]}`)
	eventuallyLong(t, "loop counted", func() bool {
		return num(bgpPeer(t, a, "10.0.0.2")["counters"].(map[string]any), "rx_as_path_loops") == 1
	})
	if received(bgpPeer(t, a, "10.0.0.2"), "ipv4-unicast") != 1 {
		t.Fatal("looped route stored")
	}
	// Withdraw half, advertise more.
	bgpRun(t, a, "withdraw", `{"routes":[{"prefix":"198.51.100.0/24","count":500}]}`)
	bgpRun(t, a, "advertise", `{"routes":[{"prefix":"10.99.0.0/16","origin":"incomplete"}]}`)
	eventuallyLong(t, "changes reach b", func() bool { return received(bgpPeer(t, b, "10.0.0.1"), "ipv4-unicast") == 501 })
	if x := bgpRun(t, b, "rib", `{"peer":"10.0.0.1","prefix":"10.99.0.0/16"}`)["routes"].([]any); len(x) != 1 || x[0].(map[string]any)["origin"] != "incomplete" {
		t.Fatalf("new route %v", x)
	}
	// Reset: back at once, all routes again.
	ups := num(bgpPeer(t, b, "10.0.0.1"), "ups")
	bgpRun(t, a, "reset", `{"peer":"10.0.0.2"}`)
	eventuallyLong(t, "back after reset", func() bool {
		p := bgpPeer(t, b, "10.0.0.1")
		return num(p, "ups") > ups && p["state"] == "established" && received(p, "ipv4-unicast") == 501
	})
	// A NOTIFICATION of our choosing; the peer reports it.
	bgpRun(t, a, "notify", `{"peer":"10.0.0.2","code":6,"subcode":2,"message":"maintenance"}`)
	eventuallyLong(t, "b saw the notification", func() bool {
		return strings.Contains(fmt.Sprint(bgpPeer(t, b, "10.0.0.1")["last_down"]), "maintenance")
	})
	// Silent flap: b's hold timer (9 s) expires.
	downs := num(bgpPeer(t, b, "10.0.0.1"), "downs")
	for end := time.Now().Add(10 * time.Second); bgpPeer(t, b, "10.0.0.1")["state"] != "established"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("not up before flap:\na: %v\nb: %v", bgpPeer(t, a, "10.0.0.2"), bgpPeer(t, b, "10.0.0.1"))
		}
	}
	bgpRun(t, a, "flap", `{"peer":"10.0.0.2","seconds":12,"mode":"silent"}`)
	eventuallyLong(t, "hold timer expires at b", func() bool { return num(bgpPeer(t, b, "10.0.0.1"), "downs") > downs })
	if e := fmt.Sprint(bgpPeer(t, b, "10.0.0.1")["last_down"]); !strings.Contains(e, "hold timer") {
		t.Fatalf("b's reason %q", e)
	}
	// The iBGP peer over IPv6 never comes up (b has no such peer): active.
	if p := bgpPeer(t, a, "2001:db8::2"); p["state"] == "established" {
		t.Fatalf("iBGP peer %v", p)
	}
	if c := flowStatus(t, b, "bgp")["counters"].(map[string]any); num(c, "rx_unknown_peer_connections") == 0 {
		t.Fatalf("a's dials to an unconfigured peer are counted at b: %v", c)
	}
}
