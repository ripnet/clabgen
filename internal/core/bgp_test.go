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

// VPN families over iBGP: VPNv4/v6, EVPN and L2VPN routes from a reach b's
// RIB with their labels and communities, and withdrawals take them out.
func TestBGPVPN(t *testing.T) {
	a, b, _, _ := ipPair(t)
	fams := `"families":["ipv4-unicast","ipv4-vpn","ipv6-vpn","evpn","l2vpn"]`
	mk := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "bgp", Type: "bgp", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	mk(a, `{"local_as":65100,"hold_time":9,"peers":[{"address":"10.0.0.2","remote_as":65100,`+fams+`,"connect_retry":1}],
		"routes":[{"prefix":"10.50.0.0/24","count":100,"rd":"65100:500","label":5000,"route_targets":["65100:500"]},
			{"prefix":"2001:db8:500::/48","count":10,"rd":"65100:500","route_targets":["65100:500"]},
			{"prefix":"198.51.100.0/24"}],
		"evpn":[{"type":"mac-ip","rd":"65100:600","route_targets":["65100:600"],"mac":"02:00:00:00:06:01","ip":"10.60.0.1","count":50,"label":600},
			{"type":"inclusive-multicast","rd":"65100:600","route_targets":["65100:600"],"label":600}],
		"l2vpn":[{"rd":"65100:700","route_targets":["65100:700"],"ve_id":2,"label_base":700000}]}`)
	mk(b, `{"local_as":65100,"router_id":"10.0.0.2","hold_time":9,"peers":[{"address":"10.0.0.1","remote_as":65100,`+fams+`,"connect_retry":1}]}`)
	defer func() {
		if t.Failed() {
			t.Logf("b %v", bgpPeer(t, b, "10.0.0.1"))
			t.Logf("a %v", bgpPeer(t, a, "10.0.0.2"))
		}
	}()
	eventuallyLong(t, "all families received", func() bool {
		p := bgpPeer(t, b, "10.0.0.1")
		return p["state"] == "established" && received(p, "ipv4-vpn") == 100 && received(p, "ipv6-vpn") == 10 &&
			received(p, "evpn") == 51 && received(p, "l2vpn") == 1 && received(p, "ipv4-unicast") == 1
	})
	if adv := bgpPeer(t, a, "10.0.0.2")["advertised"].(map[string]any); num(adv, "evpn") != 51 || num(adv, "ipv4-vpn") != 100 {
		t.Fatalf("a advertised %v", adv)
	}
	one := func(fam, sub string) map[string]any {
		rs := bgpRun(t, b, "rib", `{"peer":"10.0.0.1","family":"`+fam+`","prefix":"`+sub+`"}`)["routes"].([]any)
		if len(rs) == 0 {
			t.Fatalf("no %s route matching %q", fam, sub)
		}
		return rs[0].(map[string]any)
	}
	if r := one("ipv4-vpn", "65100:500:10.50.7.0/24"); !strings.Contains(fmt.Sprint(r["prefix"]), "label [5000]") || r["next_hop"] != "10.0.0.1" ||
		!strings.Contains(fmt.Sprint(r["ext_communities"]), "65100:500") || num(r, "local_pref") != 100 {
		t.Fatalf("VPNv4 route %v", r)
	}
	if r := one("ipv6-vpn", "2001:db8:509::/48"); r["next_hop"] != "10.0.0.1" { // IPv4-mapped on the wire
		t.Fatalf("VPNv6 route %v", r)
	}
	if r := one("evpn", "mac:02:00:00:00:06:32"); !strings.Contains(fmt.Sprint(r["prefix"]), "ip:10.60.0.50") || !strings.Contains(fmt.Sprint(r["prefix"]), "label [600]") {
		t.Fatalf("EVPN MAC route %v", r)
	}
	if r := one("evpn", "multicast"); !strings.Contains(fmt.Sprint(r["other_attributes"]), "PMSI") {
		t.Fatalf("EVPN IMET route %v", r)
	}
	if r := one("l2vpn", "65100:700"); !strings.Contains(fmt.Sprint(r["prefix"]), "labels from 700000") {
		t.Fatalf("L2VPN route %v", r)
	}
	bgpRun(t, a, "withdraw", `{"routes":[{"prefix":"10.50.0.0/24","count":50,"rd":"65100:500"}],
		"evpn":[{"type":"mac-ip","rd":"65100:600","mac":"02:00:00:00:06:01","ip":"10.60.0.1","count":50,"label":600}],
		"l2vpn":[{"rd":"65100:700","ve_id":2,"label_base":700000}]}`)
	eventuallyLong(t, "withdrawn", func() bool {
		p := bgpPeer(t, b, "10.0.0.1")
		return received(p, "ipv4-vpn") == 50 && received(p, "evpn") == 1 && received(p, "l2vpn") == 0
	})
}

// SR Policy (SAFI 73) over iBGP: candidate paths from a reach b with their
// route target or NO_ADVERTISE and the tunnel encapsulation attribute, a
// colored unicast route carries its color, and a withdrawal removes one.
func TestBGPSRPolicy(t *testing.T) {
	a, b, _, _ := ipPair(t)
	fams := `"families":["ipv4-unicast","ipv4-srpolicy","ipv6-srpolicy"]`
	mk := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "bgp", Type: "bgp", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	mk(a, `{"local_as":65000,"hold_time":9,"peers":[{"address":"10.0.0.2","remote_as":65000,`+fams+`,"connect_retry":1}],
		"routes":[{"prefix":"198.51.100.0/24","ext_communities":["color:100"]}],
		"sr_policies":[
			{"endpoint":"10.0.0.5","color":100,"segment_lists":[{"labels":[40002,40005]}]},
			{"endpoint":"10.0.0.5","color":100,"distinguisher":2,"preference":200,"headend":"10.0.0.2","binding_sid":100000,
			 "segment_lists":[{"labels":[40003,40005],"weight":1},{"labels":[40004,40005],"weight":2}]},
			{"endpoint":"2001:db8::5","color":200,"segment_lists":[{"labels":[40005]}]}]}`)
	mk(b, `{"local_as":65000,"router_id":"10.0.0.2","hold_time":9,"peers":[{"address":"10.0.0.1","remote_as":65000,`+fams+`,"connect_retry":1}]}`)
	eventuallyLong(t, "policies received", func() bool {
		p := bgpPeer(t, b, "10.0.0.1")
		return p["state"] == "established" && received(p, "ipv4-srpolicy") == 2 && received(p, "ipv6-srpolicy") == 1 && received(p, "ipv4-unicast") == 1
	})
	rib := func(fam, sub string) map[string]any {
		rs := bgpRun(t, b, "rib", `{"peer":"10.0.0.1","family":"`+fam+`","prefix":"`+sub+`"}`)["routes"].([]any)
		if len(rs) == 0 {
			t.Fatalf("no %s route matching %q", fam, sub)
		}
		return rs[0].(map[string]any)
	}
	if r := rib("ipv4-srpolicy", "(distinguisher 1)"); !strings.Contains(fmt.Sprint(r["prefix"]), "color 100 endpoint 10.0.0.5") ||
		!strings.Contains(fmt.Sprint(r["communities"]), "no-advertise") || !strings.Contains(fmt.Sprint(r["other_attributes"]), "TUNNEL_ENCAP") {
		t.Fatalf("policy without headend %v", r)
	}
	if r := rib("ipv4-srpolicy", "(distinguisher 2)"); !strings.Contains(fmt.Sprint(r["ext_communities"]), "10.0.0.2:0") {
		t.Fatalf("policy for headend 10.0.0.2 %v", r)
	}
	if r := rib("ipv6-srpolicy", "2001:db8::5"); !strings.Contains(fmt.Sprint(r["prefix"]), "color 200") {
		t.Fatalf("IPv6 policy %v", r)
	}
	if r := rib("ipv4-unicast", "198.51.100.0/24"); !strings.Contains(strings.ToLower(fmt.Sprint(r["ext_communities"])), "100") {
		t.Fatalf("colored route %v", r)
	}
	bgpRun(t, a, "withdraw", `{"sr_policies":[{"endpoint":"10.0.0.5","color":100,"distinguisher":2,"segment_lists":[{"labels":[16]}]}]}`)
	eventuallyLong(t, "withdrawn", func() bool { return received(bgpPeer(t, b, "10.0.0.1"), "ipv4-srpolicy") == 1 })
}

// A config edit applies in place: routes change without the session going
// down, and only a peer whose session settings change is reset.
func TestBGPReconfigure(t *testing.T) {
	a, b, _, _ := ipPair(t)
	fams := `"families":["ipv4-unicast","ipv4-srpolicy"]`
	peer := `{"address":"10.0.0.2","remote_as":65000,` + fams + `,"connect_retry":1%s}`
	cfg := func(peerExtra, routes, policies string) json.RawMessage {
		return json.RawMessage(`{"local_as":65000,"hold_time":9,"peers":[` + fmt.Sprintf(peer, peerExtra) + `],
			"routes":[` + routes + `],"sr_policies":[` + policies + `]}`)
	}
	r1 := `{"prefix":"198.51.100.0/24"}`
	pol := func(comm string) string {
		return `{"endpoint":"10.0.0.5","color":100,"communities":["` + comm + `"],"segment_lists":[{"labels":[40005]}]}`
	}
	if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "bgp", Type: "bgp", Device: "d", Config: cfg("", r1, pol("65000:1"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateProtocol(core.ProtocolSpec{Name: "bgp", Type: "bgp", Device: "d", Config: json.RawMessage(
		`{"local_as":65000,"router_id":"10.0.0.2","hold_time":9,"peers":[{"address":"10.0.0.1","remote_as":65000,` + fams + `,"connect_retry":1}]}`)}); err != nil {
		t.Fatal(err)
	}
	at := func() map[string]any { return bgpPeer(t, b, "10.0.0.1") }
	eventuallyLong(t, "up", func() bool {
		p := at()
		return p["state"] == "established" && received(p, "ipv4-unicast") == 1 && received(p, "ipv4-srpolicy") == 1
	})
	update := func(c json.RawMessage) {
		t.Helper()
		if _, err := a.UpdateProtocol("bgp", c); err != nil {
			t.Fatal(err)
		}
	}
	steady := func(what string) {
		t.Helper()
		if p := at(); num(p, "ups") != 1 || p["state"] != "established" {
			t.Fatalf("%s: the session went down: %v", what, p)
		}
	}
	comm := func() string {
		rs := bgpRun(t, b, "rib", `{"peer":"10.0.0.1","family":"ipv4-srpolicy"}`)["routes"].([]any)
		if len(rs) != 1 {
			t.Fatalf("policies %v", rs)
		}
		return fmt.Sprint(rs[0].(map[string]any)["communities"])
	}

	// An injected route stays through config edits.
	bgpRun(t, a, "advertise", `{"routes":[{"prefix":"192.0.2.0/24"}]}`)
	// Add routes and edit a policy's attributes.
	update(cfg("", r1+`,{"prefix":"203.0.113.0/24","count":2}`, pol("65000:2")))
	eventuallyLong(t, "routes added, policy replaced", func() bool {
		return received(at(), "ipv4-unicast") == 4 && strings.Contains(comm(), "65000:2")
	})
	steady("adding routes")
	// Remove a route and the policy; a description is no reason to reset.
	update(cfg(`,"description":"lab"`, `{"prefix":"203.0.113.0/24","count":2}`, ""))
	eventuallyLong(t, "route and policy withdrawn", func() bool {
		p := at()
		return received(p, "ipv4-unicast") == 3 && received(p, "ipv4-srpolicy") == 0
	})
	steady("withdrawing routes")
	if p := bgpPeer(t, a, "10.0.0.2"); p["description"] != "lab" {
		t.Fatalf("description %v", p["description"])
	}
	// A new hold time resets that peer, which comes back with everything.
	update(cfg(`,"description":"lab","hold_time":12`, `{"prefix":"203.0.113.0/24","count":2}`, ""))
	eventuallyLong(t, "peer reset and back", func() bool {
		p := at()
		return num(p, "ups") == 2 && p["state"] == "established" && received(p, "ipv4-unicast") == 3
	})
	// Without the peer, the session goes and stays down.
	update(json.RawMessage(`{"local_as":65000,"hold_time":9}`))
	eventuallyLong(t, "peer removed", func() bool { return at()["state"] != "established" })
	time.Sleep(1500 * time.Millisecond)
	if p := at(); p["state"] == "established" {
		t.Fatalf("a removed peer came back: %v", p)
	}
	// A new AS restarts the speaker.
	update(json.RawMessage(`{"local_as":65001,"hold_time":9}`))
	if v, _ := a.GetProtocol("bgp"); v.State != "running" || !strings.Contains(string(v.Config), "65001") {
		t.Fatalf("after a new AS: %+v", v)
	}
}
