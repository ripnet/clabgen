package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pkt"
	_ "github.com/ripnet/clabgen/protocols/ospf"
)

// Fast timers for tests.
const ospfTimers = `"hello_interval": 1, "dead_interval": 3, "retransmit_interval": 1`

func ospfRun(t *testing.T, e *core.Engine, name, action, args string) map[string]any {
	t.Helper()
	r, err := e.RunAction(context.Background(), name, action, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s %s: %v", name, action, err)
	}
	b, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func mkOSPF(t *testing.T, e *core.Engine, name, dev, cfg string) {
	t.Helper()
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: "ospf", Device: dev, Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

func ospfNbrs(t *testing.T, e *core.Engine, name string) []map[string]any {
	var out []map[string]any
	for _, x := range flowStatus(t, e, name)["neighbors"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func ospfFull(t *testing.T, e *core.Engine, name string, n int) bool {
	c := 0
	for _, x := range ospfNbrs(t, e, name) {
		if x["state"] == "full" {
			c++
		}
	}
	return c == n
}

func lsdbTypes(t *testing.T, e *core.Engine, name string) map[string]float64 {
	out := map[string]float64{}
	for k, v := range flowStatus(t, e, name)["lsdb_types"].(map[string]any) {
		out[k] = v.(float64)
	}
	return out
}

func lsaSeq(t *testing.T, e *core.Engine, name, typ, id string) string {
	return lsaSeqBy(t, e, name, typ, id, "")
}

// lsaSeqBy is an LSA's sequence number; adv narrows it to one router.
func lsaSeqBy(t *testing.T, e *core.Engine, name, typ, id, adv string) string {
	for _, x := range flowStatus(t, e, name)["lsdb"].([]any) {
		l := x.(map[string]any)
		if l["type"] == typ && l["id"] == id && (adv == "" || l["adv_router"] == adv) {
			return l["seq"].(string)
		}
	}
	return ""
}

func TestOSPFPointToPoint(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mkOSPF(t, a, "ospf", "d", `{"router_id": "192.0.2.1", "network": "point-to-point", `+ospfTimers+`,
		"loopbacks": ["192.0.2.1/32"], "prefixes": [{"prefix": "198.18.0.0/24", "count": 300}, {"prefix": "198.20.0.0/24", "type": "internal", "metric": 5}]}`)
	mkOSPF(t, b, "ospf", "d", `{"router_id": "192.0.2.2", "network": "point-to-point", `+ospfTimers+`}`)
	eventuallyLong(t, "full", func() bool { return ospfFull(t, a, "ospf", 1) && ospfFull(t, b, "ospf", 1) })
	defer func() {
		if t.Failed() {
			t.Logf("b: %v %v", lsdbTypes(t, b, "ospf"), flowStatus(t, b, "ospf")["counters"])
			t.Logf("a: %v %v", lsdbTypes(t, a, "ospf"), flowStatus(t, a, "ospf")["counters"])
			t.Logf("a nbrs %v", ospfNbrs(t, a, "ospf"))
		}
	}()
	eventuallyLong(t, "b has a's LSAs", func() bool {
		c := lsdbTypes(t, b, "ospf")
		return c["router"] == 2 && c["external"] == 300
	})
	// a re-originates its router LSA once full (with the p2p link); b may
	// still have the copy from before.
	var d map[string]any
	var links []any
	eventuallyLong(t, "a's router LSA with the p2p link", func() bool {
		d = ospfRun(t, b, "ospf", "lsa", `{"type": "router", "id": "192.0.2.1"}`)
		links, _ = d["links"].([]any)
		return len(links) == 4
	})
	if d["flags"].(map[string]any)["asbr"] != true {
		t.Fatalf("a's router LSA: %v", d)
	}
	kinds := map[string]int{}
	for _, l := range links {
		kinds[l.(map[string]any)["type"].(string)]++
	}
	if kinds["point-to-point"] != 1 || kinds["stub"] != 3 {
		t.Fatalf("links %v", kinds)
	}
	e := ospfRun(t, b, "ospf", "lsa", `{"type": "external", "id": "198.18.1.0"}`)
	if e["metric"] != 20.0 || e["e2"] != true || e["mask"] != "255.255.255.0" {
		t.Fatalf("external: %v", e)
	}

	// Withdraw: the externals are flushed from b.
	ospfRun(t, a, "ospf", "withdraw", `{"prefixes": [{"prefix": "198.18.0.0/24", "count": 100}]}`)
	eventuallyLong(t, "flushed", func() bool { return lsdbTypes(t, b, "ospf")["external"] == 200 })

	// max-metric raises the p2p link's metric.
	ospfRun(t, a, "ospf", "max-metric", `{"set": true}`)
	eventuallyLong(t, "max-metric", func() bool {
		for _, l := range ospfRun(t, b, "ospf", "lsa", `{"type": "router", "id": "192.0.2.1"}`)["links"].([]any) {
			if x := l.(map[string]any); x["type"] == "point-to-point" {
				return x["metric"] == 65535.0
			}
		}
		return false
	})

	// A restarted a starts again at the initial sequence; b holds the old
	// instance, so a takes over above it.
	before := lsaSeq(t, b, "ospf", "router", "192.0.2.1")
	if err := a.DeleteProtocol("ospf"); err != nil {
		t.Fatal(err)
	}
	mkOSPF(t, a, "ospf", "d", `{"router_id": "192.0.2.1", "network": "point-to-point", `+ospfTimers+`}`)
	eventuallyLong(t, "re-full", func() bool { return ospfFull(t, a, "ospf", 1) && ospfFull(t, b, "ospf", 1) })
	eventuallyLong(t, "taken over", func() bool {
		s := lsaSeq(t, b, "ospf", "router", "192.0.2.1")
		return s > before && lsdbTypes(t, b, "ospf")["external"] == 0
	})

	// Flap: b drops a at once (a's hellos stop listing it), and it comes back.
	ospfRun(t, a, "ospf", "flap", `{"seconds": 2, "mode": "reset"}`)
	eventuallyLong(t, "dropped", func() bool { return !ospfFull(t, b, "ospf", 1) })
	eventuallyLong(t, "back", func() bool { return ospfFull(t, a, "ospf", 1) && ospfFull(t, b, "ospf", 1) })
}

func TestOSPFBroadcastAndAuth(t *testing.T) {
	a, b, _, _ := ipPair(t)
	// A third router on b's side of the wire: same port and VLAN.
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "d2", Port: "eth1", MAC: "02:00:00:00:0b:02", VLANs: []pkt.VLANTag{{VID: 10}}, Addresses: []string{"10.0.0.3/24"}}); err != nil {
		t.Fatal(err)
	}
	md5 := `"auth": {"type": "md5", "key": "lab", "key_id": 1}`
	mkOSPF(t, a, "ospf", "d", `{"router_id": "192.0.2.1", "priority": 1, `+md5+`, `+ospfTimers+`}`)
	mkOSPF(t, b, "ospf", "d", `{"router_id": "192.0.2.2", "priority": 10, `+md5+`, `+ospfTimers+`}`)
	mkOSPF(t, b, "ospf3", "d2", `{"router_id": "192.0.2.3", "priority": 0, `+md5+`, `+ospfTimers+`}`)
	defer func() {
		if t.Failed() {
			for _, x := range []struct {
				e *core.Engine
				n string
			}{{a, "ospf"}, {b, "ospf"}, {b, "ospf3"}} {
				st := flowStatus(t, x.e, x.n)
				t.Logf("%s if %v", st["router_id"], st["interfaces"])
				for _, nb := range ospfNbrs(t, x.e, x.n) {
					t.Logf("   nbr %v %v role=%v reason=%v req=%v rx=%v", nb["router_id"], nb["state"], nb["role"], nb["reason"], nb["requests"], nb["retransmit"])
				}
				t.Logf("   %v", st["counters"])
			}
		}
	}()
	// The DR (b, priority 10) and BDR (a) are adjacent with everyone; the
	// DR Other (priority 0) stays 2-way with nobody but them.
	eventuallyLong(t, "adjacencies", func() bool {
		return ospfFull(t, a, "ospf", 2) && ospfFull(t, b, "ospf", 2) && ospfFull(t, b, "ospf3", 2)
	})
	ifc := flowStatus(t, b, "ospf")["interfaces"].([]any)[0].(map[string]any)
	if ifc["state"] != "dr" || ifc["dr"] != "10.0.0.2" || ifc["bdr"] != "10.0.0.1" {
		t.Fatalf("b's interface: %v", ifc)
	}
	eventuallyLong(t, "network LSA with 3 routers", func() bool {
		if lsdbTypes(t, a, "ospf")["network"] != 1 || lsdbTypes(t, a, "ospf")["router"] != 3 {
			return false
		}
		r, _ := ospfRun(t, a, "ospf", "lsa", `{"type": "network", "id": "10.0.0.2"}`)["attached_routers"].([]any)
		return len(r) == 3
	})

	// The DR goes: the BDR (a) takes over and originates the network LSA.
	if err := b.DeleteProtocol("ospf"); err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "a is DR", func() bool {
		f := flowStatus(t, a, "ospf")["interfaces"].([]any)[0].(map[string]any)
		return f["state"] == "dr" && ospfFull(t, a, "ospf", 1)
	})
	eventuallyLong(t, "a's network LSA", func() bool { return lsaSeq(t, b, "ospf3", "network", "10.0.0.1") != "" })

	// A wrong key: no adjacency, counted.
	mkOSPF(t, b, "ospf", "d", `{"router_id": "192.0.2.2", "auth": {"type": "md5", "key": "nope", "key_id": 1}, `+ospfTimers+`}`)
	eventuallyLong(t, "auth failures", func() bool {
		c, _ := flowStatus(t, a, "ospf")["counters"].(map[string]any)["rx_auth_failures"].(float64)
		return c > 2
	})
	if len(ospfNbrs(t, a, "ospf")) != 1 {
		t.Fatalf("neighbors with a wrong key: %v", ospfNbrs(t, a, "ospf"))
	}
	if _, err := b.UpdateProtocol("ospf", json.RawMessage(`{"router_id": "192.0.2.2", `+md5+`, `+ospfTimers+`}`)); err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "fixed key", func() bool { return ospfFull(t, a, "ospf", 2) })
}

// A restarted DR comes back while the others carry on: the BDR has taken
// over as DR, the restarted router becomes BDR, and the DR Other forms
// adjacencies with both again.
func TestOSPFRestartedDR(t *testing.T) {
	a, b, _, _ := ipPair(t)
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "d2", Port: "eth1", MAC: "02:00:00:00:0b:02", VLANs: []pkt.VLANTag{{VID: 10}}, Addresses: []string{"10.0.0.3/24"}}); err != nil {
		t.Fatal(err)
	}
	cfgA := `{"router_id": "192.0.2.1", "priority": 100, ` + ospfTimers + `}`
	mkOSPF(t, a, "ospf", "d", cfgA)
	mkOSPF(t, b, "ospf", "d", `{"router_id": "192.0.2.2", "priority": 1, `+ospfTimers+`}`)
	mkOSPF(t, b, "ospf3", "d2", `{"router_id": "192.0.2.3", "priority": 0, `+ospfTimers+`}`)
	all := func() bool {
		return ospfFull(t, a, "ospf", 2) && ospfFull(t, b, "ospf", 2) && ospfFull(t, b, "ospf3", 2)
	}
	defer func() {
		if t.Failed() {
			for _, x := range []struct {
				e *core.Engine
				n string
			}{{a, "ospf"}, {b, "ospf"}, {b, "ospf3"}} {
				st := flowStatus(t, x.e, x.n)
				f := st["interfaces"].([]any)[0].(map[string]any)
				t.Logf("%s %v dr=%v bdr=%v", st["router_id"], f["state"], f["dr"], f["bdr"])
				for _, nb := range ospfNbrs(t, x.e, x.n) {
					t.Logf("   nbr %v %v declares %v/%v reason=%v", nb["router_id"], nb["state"], nb["dr"], nb["bdr"], nb["reason"])
				}
				t.Logf("   %v", st["counters"])
			}
		}
	}()
	eventuallyLong(t, "adjacencies", all)
	for i := 0; i < 2; i++ {
		if _, err := a.UpdateProtocol("ospf", json.RawMessage(cfgA)); err != nil {
			t.Fatal(err)
		}
		eventuallyLong(t, "adjacencies after a restart", all)
		f := flowStatus(t, b, "ospf3")["interfaces"].([]any)[0].(map[string]any)
		if f["dr"] == nil || f["bdr"] == nil {
			t.Fatalf("DR Other's view: %v", f)
		}
	}
}

func mkOSPF3(t *testing.T, e *core.Engine, name, dev, cfg string) {
	t.Helper()
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: "ospfv3", Device: dev, Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

func TestOSPFv3(t *testing.T) {
	a, b, _, _ := ipPair(t)
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "d2", Port: "eth1", MAC: "02:00:00:00:0b:02", VLANs: []pkt.VLANTag{{VID: 10}}, Addresses: []string{"2001:db8::3/64"}}); err != nil {
		t.Fatal(err)
	}
	mkOSPF3(t, a, "ospf6", "d", `{"router_id": "192.0.2.1", "priority": 100, `+ospfTimers+`,
		"loopbacks": ["2001:db8:ffff::1/128"],
		"prefixes": [{"prefix": "2001:db8:100::/48", "count": 120, "tag": 7}, {"prefix": "2001:db8:200::/64", "type": "internal", "metric": 5}]}`)
	mkOSPF3(t, b, "ospf6", "d", `{"router_id": "192.0.2.2", "priority": 10, `+ospfTimers+`}`)
	mkOSPF3(t, b, "ospf6c", "d2", `{"router_id": "192.0.2.3", "priority": 0, `+ospfTimers+`}`)
	all := func() bool {
		return ospfFull(t, a, "ospf6", 2) && ospfFull(t, b, "ospf6", 2) && ospfFull(t, b, "ospf6c", 2)
	}
	eventuallyLong(t, "v3 adjacencies", all)
	f := flowStatus(t, b, "ospf6c")["interfaces"].([]any)[0].(map[string]any)
	if f["dr"] != "192.0.2.1" || f["bdr"] != "192.0.2.2" {
		t.Fatalf("roles (router IDs in v3): %v", f)
	}
	// Each router sees 3 router LSAs, the DR's network LSA and its
	// intra-area-prefix LSA for the LAN, a's own prefixes, 120 externals,
	// and the 3 link LSAs of the shared link.
	eventuallyLong(t, "v3 LSDB", func() bool {
		c := lsdbTypes(t, b, "ospf6c")
		return c["router"] == 3 && c["network"] == 1 && c["external"] == 120 && c["link"] == 3 && c["intra-area-prefix"] == 2
	})
	r := ospfRun(t, b, "ospf6c", "lsa", `{"type": "router", "id": "0.0.0.0", "adv_router": "192.0.2.1"}`)
	links := r["links"].([]any)
	if r["flags"].(map[string]any)["asbr"] != true || len(links) != 1 || links[0].(map[string]any)["type"] != "transit" ||
		links[0].(map[string]any)["neighbor_router"] != "192.0.2.1" {
		t.Fatalf("a's router LSA: %v", r)
	}
	own := ospfRun(t, b, "ospf6c", "lsa", `{"type": "intra-area-prefix", "id": "0.0.0.0", "adv_router": "192.0.2.1"}`)
	ps := map[string]bool{}
	for _, p := range own["prefixes"].([]any) {
		ps[p.(map[string]any)["prefix"].(string)] = true
	}
	if !ps["2001:db8:ffff::1/128"] || !ps["2001:db8:200::/64"] || ps["2001:db8::/64"] {
		t.Fatalf("a's prefixes (the LAN is in the network's, not a's): %v", own)
	}
	lan := ospfRun(t, b, "ospf6c", "lsa", `{"type": "intra-area-prefix", "id": "0.0.0.1", "adv_router": "192.0.2.1"}`)
	if lan["references"] != "network 0.0.0.1 192.0.2.1" || len(lan["prefixes"].([]any)) != 1 {
		t.Fatalf("LAN prefix LSA: %v", lan)
	}
	ext := ospfRun(t, b, "ospf6c", "lsa", `{"type": "external", "id": "0.0.0.3", "adv_router": "192.0.2.1"}`)
	if ext["prefix"] != "2001:db8:102::/48" || ext["tag"] != 7.0 || ext["e2"] != true {
		t.Fatalf("external: %v", ext)
	}

	// Withdraw and restart: externals flushed; a's LSAs taken over above
	// the old sequence numbers.
	ospfRun(t, a, "ospf6", "withdraw", `{"prefixes": [{"prefix": "2001:db8:100::/48", "count": 20}]}`)
	eventuallyLong(t, "v3 flushed", func() bool { return lsdbTypes(t, b, "ospf6c")["external"] == 100 })
	before := lsaSeqBy(t, b, "ospf6c", "router", "0.0.0.0", "192.0.2.1")
	if _, err := a.UpdateProtocol("ospf6", json.RawMessage(`{"router_id": "192.0.2.1", "priority": 100, `+ospfTimers+`}`)); err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "v3 after restart", all)
	eventuallyLong(t, "v3 takeover", func() bool {
		return lsdbTypes(t, b, "ospf6c")["external"] == 0 && lsaSeqBy(t, b, "ospf6c", "router", "0.0.0.0", "192.0.2.1") > before
	})
}

func linksOf(t *testing.T, e *core.Engine, name, typ, id, adv string) []map[string]any {
	t.Helper()
	r, err := e.RunAction(context.Background(), name, "lsa", json.RawMessage(`{"type": "`+typ+`", "id": "`+id+`", "adv_router": "`+adv+`"}`))
	if err != nil {
		return nil
	}
	b, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(b, &m)
	var out []map[string]any
	ls, _ := m["links"].([]any)
	for _, l := range ls {
		out = append(out, l.(map[string]any))
	}
	return out
}

func TestOSPFTopology(t *testing.T) {
	for _, v3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "v3"}[v3], func(t *testing.T) {
			a, b, _, _ := ipPair(t)
			typ, lb, pfx, rid := "ospf", "10.255.0.1/32", "10.200.0.0/24", "192.0.2.1"
			if v3 {
				typ, lb, pfx = "ospfv3", "2001:db8:ff::1/128", "2001:db8:200::/64"
			}
			mk := func(e *core.Engine, cfg string) {
				t.Helper()
				if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "o", Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
					t.Fatal(err)
				}
			}
			mk(a, `{"router_id": "`+rid+`", "network": "point-to-point", `+ospfTimers+`,
				"topology": {"kind": "grid", "rows": 3, "cols": 3, "metric": 20, "loopback_base": "`+lb+`", "prefix_base": "`+pfx+`", "prefixes_per_node": 2}}`)
			mk(b, `{"router_id": "192.0.2.2", "network": "point-to-point", `+ospfTimers+`}`)
			eventuallyLong(t, "full", func() bool { return ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1) })
			eventuallyLong(t, "11 router LSAs at b", func() bool { return lsdbTypes(t, b, "o")["router"] == 11 })
			id := func(r string) string {
				if v3 {
					return "0.0.0.0"
				}
				return r
			}
			// r1-1 (10.254.0.1) links to us (a), r1-2 and r2-1; a links to b and r1-1.
			sim := linksOf(t, b, "o", "router", id("10.254.0.1"), "10.254.0.1")
			nbrOf := func(l map[string]any) any {
				if v3 {
					return l["neighbor_router"]
				}
				return l["id"]
			}
			p2p := map[any]any{}
			for _, l := range sim {
				if l["type"] == "point-to-point" {
					p2p[nbrOf(l)] = l["metric"]
				}
			}
			if len(p2p) != 3 || p2p[rid] != 20.0 || p2p["10.254.0.2"] != 20.0 || p2p["10.254.0.4"] != 20.0 {
				t.Fatalf("r1-1's links: %v", sim)
			}
			eventuallyLong(t, "a links to b and r1-1", func() bool {
				ours := map[any]bool{}
				for _, l := range linksOf(t, b, "o", "router", id(rid), rid) {
					ours[nbrOf(l)] = true
				}
				return ours["10.254.0.1"] && ours["192.0.2.2"]
			})
			if v3 {
				ip := ospfRun(t, b, "o", "lsa", `{"type": "intra-area-prefix", "id": "0.0.0.0", "adv_router": "10.254.0.5"}`)
				if n := len(ip["prefixes"].([]any)); n != 3 {
					t.Fatalf("r2-2's prefixes: %v", ip)
				}
			}

			// Fail the a–r1-1 link and a router; they come out of b's view.
			ospfRun(t, a, "o", "topology-link", `{"a": "us", "b": "r1-1", "up": false}`)
			eventuallyLong(t, "link down", func() bool {
				for _, l := range linksOf(t, b, "o", "router", id(rid), rid) {
					if nbrOf(l) == "10.254.0.1" {
						return false
					}
				}
				return true
			})
			ospfRun(t, a, "o", "topology-node", `{"node": "r3-3", "up": false}`)
			eventuallyLong(t, "node down", func() bool { return lsdbTypes(t, b, "o")["router"] == 10 })
			st := flowStatus(t, a, "o")["topology"].(map[string]any)
			if st["nodes"] != 9.0 || st["prefixes"] != 27.0 || len(st["down_nodes"].([]any)) != 1 {
				t.Fatalf("topology status: %v", st)
			}
		})
	}
}

func TestOSPFHMAC(t *testing.T) {
	for _, v3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "v3"}[v3], func(t *testing.T) {
			a, b, _, _ := ipPair(t)
			typ, auth, bad := "ospf", `{"type": "hmac-sha256", "key": "lab", "key_id": 4}`, `{"type": "hmac-sha256", "key": "nope", "key_id": 4}`
			if v3 {
				typ, auth, bad = "ospfv3", `{"type": "hmac-sha256", "key": "lab", "sa_id": 4}`, `{"type": "hmac-sha256", "key": "nope", "sa_id": 4}`
			}
			cfg := func(rid, auth string) json.RawMessage {
				return json.RawMessage(`{"router_id": "` + rid + `", "network": "point-to-point", "auth": ` + auth + `, ` + ospfTimers + `}`)
			}
			for _, x := range []struct {
				e   *core.Engine
				rid string
			}{{a, "192.0.2.1"}, {b, "192.0.2.2"}} {
				if _, err := x.e.CreateProtocol(core.ProtocolSpec{Name: "o", Type: typ, Device: "d", Config: cfg(x.rid, auth)}); err != nil {
					t.Fatal(err)
				}
			}
			eventuallyLong(t, "full with HMAC", func() bool { return ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1) })
			eventuallyLong(t, "LSDB synced", func() bool { return lsdbTypes(t, b, "o")["router"] == 2 })
			if _, err := b.UpdateProtocol("o", cfg("192.0.2.2", bad)); err != nil {
				t.Fatal(err)
			}
			eventuallyLong(t, "wrong key dropped", func() bool {
				c, _ := flowStatus(t, a, "o")["counters"].(map[string]any)["rx_auth_failures"].(float64)
				return c > 2 && !ospfFull(t, a, "o", 1)
			})
		})
	}
}

func lsaRows(t *testing.T, e *core.Engine, name string) []map[string]any {
	var out []map[string]any
	for _, x := range flowStatus(t, e, name)["lsdb"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

// countLSAs counts LSAs of a type (from adv, if set) in an area ("" for
// AS scope).
func countLSAs(t *testing.T, e *core.Engine, name, typ, adv, area string) int {
	n := 0
	for _, l := range lsaRows(t, e, name) {
		a, _ := l["area"].(string)
		if l["type"] == typ && (adv == "" || l["adv_router"] == adv) && a == area && l["age"].(float64) < 3600 {
			n++
		}
	}
	return n
}

// An area border router: area 0 toward b, an emulated topology in area 1
// (summarised with a range), and an NSSA (area 2) toward c on a second
// VLAN. b sees the summaries and the translated type-7 LSA of c; c sees
// summaries but no AS-externals.
func TestOSPFAreas(t *testing.T) {
	for _, v3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "v3"}[v3], func(t *testing.T) {
			a, b, _, _ := ipPair(t)
			for i, e := range []*core.Engine{a, b} {
				addr := []string{"10.0.1.1/24", "2001:db8:1::1/64"}
				if i == 1 {
					addr = []string{"10.0.1.2/24", "2001:db8:1::2/64"}
				}
				if _, err := e.CreateDevice(core.DeviceSpec{Name: "e", Port: "eth1", MAC: []string{"02:00:00:00:0a:02", "02:00:00:00:0b:02"}[i],
					VLANs: []pkt.VLANTag{{VID: 20}}, Addresses: addr}); err != nil {
					t.Fatal(err)
				}
			}
			typ, lb, pfx, rng, lbB, ext := "ospf", "10.255.0.1/32", "10.200.0.0/24", "10.200.0.0/22", "192.0.2.2/32", "198.51.100.0/24"
			sum, cFwd := "summary", "10.0.1.2"
			if v3 {
				typ, lb, pfx, rng, lbB, ext = "ospfv3", "2001:db8:ff::1/128", "2001:db8:200::/64", "2001:db8:200::/62", "2001:db8:ffff::2/128", "2001:db8:5100::/48"
				sum, cFwd = "inter-area-prefix", "2001:db8:1::2"
			}
			mk := func(e *core.Engine, name, dev, cfg string) {
				t.Helper()
				if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: typ, Device: dev, Config: json.RawMessage(cfg)}); err != nil {
					t.Fatal(err)
				}
			}
			mk(a, "o", "d", `{"router_id": "192.0.2.1", "network": "point-to-point", `+ospfTimers+`,
				"interfaces": [{"device": "e", "area": "2", "network": "broadcast"}],
				"areas": [{"id": "1", "ranges": [{"prefix": "`+rng+`"}]}, {"id": "2", "type": "nssa"}],
				"topology": {"kind": "line", "nodes": 2, "metric": 20, "area": "1", "loopback_base": "`+lb+`", "prefix_base": "`+pfx+`", "prefixes_per_node": 2}}`)
			mk(b, "o", "d", `{"router_id": "192.0.2.2", "network": "point-to-point", "loopbacks": ["`+lbB+`"], `+ospfTimers+`}`)
			mk(b, "c", "e", `{"router_id": "192.0.2.3", "area": "2", "areas": [{"id": "2", "type": "nssa"}], `+ospfTimers+`,
				"prefixes": [{"prefix": "`+ext+`", "metric": 30}]}`)
			defer func() {
				if t.Failed() {
					for _, x := range []struct {
						e *core.Engine
						n string
					}{{a, "o"}, {b, "o"}, {b, "c"}} {
						st := flowStatus(t, x.e, x.n)
						t.Logf("%s areas %v", st["router_id"], st["areas"])
						for _, l := range lsaRows(t, x.e, x.n) {
							t.Logf("   %v %v %v %v %v", l["area"], l["type"], l["id"], l["adv_router"], l["summary"])
						}
						t.Logf("   %v", st["counters"])
					}
				}
			}()
			eventuallyLong(t, "full", func() bool { return ospfFull(t, a, "o", 2) && ospfFull(t, b, "o", 1) && ospfFull(t, b, "c", 1) })
			if st := flowStatus(t, a, "o"); st["abr"] != true || len(st["areas"].([]any)) != 3 {
				t.Fatalf("a: abr %v areas %v", st["abr"], st["areas"])
			}
			// b: the two simulated loopbacks, the range (not its four
			// prefixes), c's NSSA interface subnet... and c's external,
			// translated by a.
			eventuallyLong(t, "b's summaries", func() bool {
				var ids []string
				for _, l := range lsaRows(t, b, "o") {
					if l["type"] == sum && l["adv_router"] == "192.0.2.1" {
						ids = append(ids, l["summary"].(string)+" "+l["id"].(string))
					}
				}
				want := 2 + 1 + 1 // loopbacks, range, the NSSA link
				return len(ids) == want && countLSAs(t, b, "o", "external", "192.0.2.1", "") == 1
			})
			if !v3 {
				r2 := ospfRun(t, b, "o", "lsa", `{"type": "summary", "id": "10.255.0.2"}`)
				if r2["metric"] != 40.0 {
					t.Fatalf("r2's loopback summary: %v", r2)
				}
				x := ospfRun(t, b, "o", "lsa", `{"type": "summary", "id": "10.200.0.0"}`)
				if x["mask"] != "255.255.252.0" || x["metric"] != 50.0 { // the largest: r2's prefixes at 20 + 20 + 10
					t.Fatalf("range summary: %v", x)
				}
				e := ospfRun(t, b, "o", "lsa", `{"type": "external", "id": "198.51.100.0"}`)
				if e["forwarding_address"] != cFwd || e["metric"] != 30.0 {
					t.Fatalf("translated external: %v", e)
				}
				f := ospfRun(t, b, "o", "lsa", `{"type": "router", "id": "192.0.2.1"}`)["flags"].(map[string]any)
				if f["abr"] != true || f["asbr"] != true {
					t.Fatalf("a's router LSA flags in area 0: %v", f)
				}
			} else {
				id := ""
				for _, l := range lsaRows(t, b, "o") {
					if l["type"] == "external" && l["adv_router"] == "192.0.2.1" {
						id = l["id"].(string)
					}
				}
				e := ospfRun(t, b, "o", "lsa", `{"type": "external", "id": "`+id+`", "adv_router": "192.0.2.1"}`)
				if e["forwarding_address"] != cFwd || e["metric"] != 30.0 || e["prefix"] != ext {
					t.Fatalf("translated external: %v", e)
				}
			}
			// c: summaries of b's loopback and the topology, its own type-7,
			// no AS-externals and no ASBR summaries.
			eventuallyLong(t, "c's view", func() bool {
				return countLSAs(t, b, "c", sum, "192.0.2.1", "0.0.0.2") >= 4 && countLSAs(t, b, "c", "nssa", "192.0.2.3", "0.0.0.2") == 1
			})
			if n := countLSAs(t, b, "c", "external", "", ""); n != 0 {
				t.Fatalf("c has %d AS-externals", n)
			}

			// Withdraw the external at c: the translation goes.
			ospfRun(t, b, "c", "withdraw", `{"prefixes": [{"prefix": "`+ext+`"}]}`)
			eventuallyLong(t, "translation flushed", func() bool { return countLSAs(t, b, "o", "external", "192.0.2.1", "") == 0 })
			// A simulated router goes: its loopback summary goes too.
			ospfRun(t, a, "o", "topology-node", `{"node": "r2", "up": false}`)
			eventuallyLong(t, "summary flushed", func() bool {
				return countLSAs(t, b, "o", sum, "192.0.2.1", "0.0.0.0") == 3
			})
		})
	}
}

// A totally stubby area: c gets only a default summary, and no externals.
func TestOSPFStubArea(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mkOSPF(t, a, "o", "d", `{"router_id": "192.0.2.1", "network": "point-to-point", "area": "0", `+ospfTimers+`,
		"loopbacks": ["192.0.2.1/32"], "prefixes": [{"prefix": "198.18.0.0/24", "count": 3}],
		"topology": {"kind": "line", "nodes": 1, "area": "5", "loopback_base": "10.255.0.1/32"},
		"areas": [{"id": "5", "type": "stub", "no_summary": true, "default_cost": 7}]}`)
	mkOSPF(t, b, "o", "d", `{"router_id": "192.0.2.2", "network": "point-to-point", "area": "0", `+ospfTimers+`}`)
	eventuallyLong(t, "full", func() bool { return ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1) })
	// The stub area's routers only exist at a; check a's LSDB there.
	eventuallyLong(t, "default only", func() bool {
		return countLSAs(t, a, "o", "summary", "192.0.2.1", "0.0.0.5") == 1 && countLSAs(t, b, "o", "summary", "192.0.2.1", "0.0.0.0") == 1
	})
	d := ospfRun(t, a, "o", "lsa", `{"type": "summary", "id": "0.0.0.0", "area": "5"}`)
	if d["metric"] != 7.0 || d["mask"] != "0.0.0.0" {
		t.Fatalf("default summary: %v", d)
	}
	if lsdbTypes(t, b, "o")["external"] != 3 {
		t.Fatalf("b: %v", lsdbTypes(t, b, "o"))
	}
}

// Segment routing, flex-algo and TE in opaque LSAs: b decodes a's Router
// Information (SRGB, algorithms, definition), Prefix-SIDs (a's loopback
// and the simulated routers'), adjacency SIDs and link attributes.
func TestOSPFSegmentRouting(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mkOSPF(t, a, "o", "d", `{"router_id": "192.0.2.1", "network": "point-to-point", `+ospfTimers+`,
		"loopbacks": ["192.0.2.1/32"], "delay_us": 500, "traffic_engineering": true,
		"segment_routing": {"enabled": true, "srgb_base": 16000, "srgb_size": 8000, "adj_sid_base": 24000,
			"node_sids": [{"prefix": "192.0.2.1/32", "index": 1}, {"prefix": "192.0.2.1/32", "index": 501, "algorithm": 128}]},
		"flex_algos": [{"algorithm": 128, "metric_type": "delay", "exclude_any": [3]}],
		"topology": {"kind": "line", "nodes": 2, "loopback_base": "10.255.0.1/32", "node_sid_base": 100, "link_delay_us": 1000}}`)
	mkOSPF(t, b, "o", "d", `{"router_id": "192.0.2.2", "network": "point-to-point", `+ospfTimers+`}`)
	eventuallyLong(t, "full", func() bool { return ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1) })
	find := func(adv, sub string) map[string]any {
		for _, l := range lsaRows(t, b, "o") {
			if l["type"] == "opaque-area" && l["adv_router"] == adv && strings.Contains(l["summary"].(string), sub) {
				return l
			}
		}
		return nil
	}
	defer func() {
		if t.Failed() {
			for _, l := range lsaRows(t, b, "o") {
				t.Logf("%v %v %v %v", l["type"], l["id"], l["adv_router"], l["summary"])
			}
		}
	}()
	eventuallyLong(t, "opaque LSAs at b", func() bool {
		return find("192.0.2.1", "SRGB 16000-23999") != nil && find("192.0.2.1", "192.0.2.1/32 index 1 algo 0") != nil &&
			find("192.0.2.1", "adj label 24000") != nil && find("10.254.0.2", "10.255.0.2/32 index 101") != nil &&
			find("10.254.0.1", "SRGB") != nil && find("192.0.2.1", "router address 192.0.2.1") != nil
	})
	ri := ospfRun(t, b, "o", "lsa", `{"type": "opaque-area", "id": "4.0.0.0", "adv_router": "192.0.2.1"}`)
	fads, _ := ri["flex_algo_definitions"].([]any)
	if len(fads) != 1 || fads[0].(map[string]any)["metric_type"] != 1.0 || fads[0].(map[string]any)["exclude_any"] != "3" {
		t.Fatalf("RI: %v", ri)
	}
	if l := find("192.0.2.1", "asla X delay 500/500"); l == nil {
		t.Fatal("no ASLA on a's link to b")
	}
	if l := find("10.254.0.1", "asla X delay 1000/1000"); l == nil {
		t.Fatal("no ASLA on the simulated links")
	}
	st := flowStatus(t, a, "o")["segment_routing"].(map[string]any)
	if len(st["node_sids"].([]any)) != 4 || len(st["adj_sids"].([]any)) != 1 {
		t.Fatalf("SR status: %v", st)
	}
}

// Keychain rollover: both routers move from key 1 to key 2 at the same
// instant (v2: MD5 to HMAC-SHA-256); the adjacency holds and nothing fails.
func TestOSPFKeychain(t *testing.T) {
	for _, v3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "v3"}[v3], func(t *testing.T) {
			a, b, _, _ := ipPair(t)
			at := time.Now().Add(4 * time.Second).UTC().Format(time.RFC3339)
			k1 := "md5"
			typ := "ospf"
			if v3 {
				k1, typ = "hmac-sha1", "ospfv3"
			}
			chain := `"auth": {"type": "keychain", "keys": [{"id": 1, "type": "` + k1 + `", "key": "one", "send_until": "` + at + `"},
				{"id": 2, "type": "hmac-sha256", "key": "two", "send_from": "` + at + `"}]}`
			for i, e := range []*core.Engine{a, b} {
				cfg := fmt.Sprintf(`{"router_id": "192.0.2.%d", "network": "point-to-point", %s, %s}`, i+1, chain, ospfTimers)
				if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "o", Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
					t.Fatal(err)
				}
			}
			eventuallyLong(t, "full", func() bool { return ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1) })
			if s := flowStatus(t, a, "o")["auth"].(string); !strings.Contains(s, "sending key 1") {
				t.Fatalf("auth before: %s", s)
			}
			eventuallyLong(t, "rolled over", func() bool {
				return strings.Contains(flowStatus(t, a, "o")["auth"].(string), "sending key 2")
			})
			time.Sleep(3 * time.Second)
			for _, e := range []*core.Engine{a, b} {
				c := flowStatus(t, e, "o")["counters"].(map[string]any)
				if c["rx_auth_failures"] != nil || !ospfFull(t, e, "o", 1) {
					t.Fatalf("after rollover: %v %v %v", c, ospfNbrs(t, e, "o"), flowStatus(t, e, "o")["interfaces"])
				}
			}
		})
	}
}

// Graceful restart: a restarts (losing its neighbors and LSDB, keeping
// BFD running); b helps, so b's router LSA never drops a, BFD never goes
// down, and a's grace LSA is gone at the end.
func TestOSPFGracefulRestart(t *testing.T) {
	for _, v3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "v3"}[v3], func(t *testing.T) {
			a, b, _, _ := ipPair(t)
			typ, rtrID := "ospf", "192.0.2.2"
			if v3 {
				typ, rtrID = "ospfv3", "0.0.0.0"
			}
			for i, e := range []*core.Engine{a, b} {
				cfg := fmt.Sprintf(`{"router_id": "192.0.2.%d", "network": "point-to-point", "bfd": {"enabled": true, "min_tx_ms": 300, "min_rx_ms": 300}, %s}`, i+1, ospfTimers)
				if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "o", Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
					t.Fatal(err)
				}
			}
			eventuallyLong(t, "full", func() bool { return ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1) })
			// a re-originates its router LSA with the link once full (no
			// sooner than MinLSInterval): wait until b has that copy, or
			// the sequence check below sees it arrive during the restart.
			aID := map[bool]string{false: "192.0.2.1", true: "0.0.0.0"}[v3]
			eventuallyLong(t, "b has a's full-state router LSA", func() bool {
				d := ospfRun(t, b, "o", "lsa", fmt.Sprintf(`{"type": "router", "id": %q, "adv_router": "192.0.2.1"}`, aID))
				ls, _ := d["links"].([]any)
				for _, l := range ls {
					if strings.Contains(fmt.Sprint(l.(map[string]any)["type"]), "point") {
						return true
					}
				}
				return false
			})
			time.Sleep(500 * time.Millisecond)
			bSeq := lsaSeqBy(t, b, "o", "router", rtrID, "192.0.2.2")
			aSeq := lsaSeqBy(t, b, "o", "router", map[bool]string{false: "192.0.2.1", true: "0.0.0.0"}[v3], "192.0.2.1")
			ospfRun(t, a, "o", "graceful-restart", `{"seconds": 60}`)
			defer func() {
				if t.Failed() {
					t.Logf("b gr %v seq %s (was %s)", flowStatus(t, b, "o")["graceful_restart"], lsaSeqBy(t, b, "o", "router", map[bool]string{false: "192.0.2.1", true: "0.0.0.0"}[v3], "192.0.2.1"), aSeq)
					for _, l := range lsaRows(t, b, "o") {
						t.Logf("  %v %v %v %v age %v", l["type"], l["id"], l["adv_router"], l["seq"], l["age"])
					}
					t.Logf("a %v", flowStatus(t, a, "o")["counters"])
				}
			}()
			eventuallyLong(t, "b helps", func() bool {
				h, _ := flowStatus(t, b, "o")["graceful_restart"].(map[string]any)["helping"].([]any)
				return len(h) == 1
			})
			eventuallyLong(t, "a restarted and done", func() bool {
				g := flowStatus(t, a, "o")["graceful_restart"].(map[string]any)
				return g["restart"] == nil && ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1)
			})
			// a's router LSA comes back unchanged, so it isn't re-originated.
			eventuallyLong(t, "grace LSA flushed", func() bool {
				g := flowStatus(t, b, "o")["graceful_restart"].(map[string]any)
				s := lsaSeqBy(t, b, "o", "router", map[bool]string{false: "192.0.2.1", true: "0.0.0.0"}[v3], "192.0.2.1")
				return g["helping"] == nil && g["grace_lsas"] == nil && s == aSeq
			})
			if s := lsaSeqBy(t, b, "o", "router", rtrID, "192.0.2.2"); s != bSeq {
				t.Fatalf("b re-originated its router LSA during the restart: %s → %s", bSeq, s)
			}
			if c := flowStatus(t, b, "o")["counters"].(map[string]any); c["bfd_down"] != nil || c["bfd_up"] != 1.0 {
				t.Fatalf("BFD at b: %v", c)
			}
		})
	}
}

// Non-broadcast and point-to-multipoint networks with three routers on one
// segment. NBMA: hellos only to the configured neighbors, a DR, a network
// LSA. P2MP: everyone adjacent, point-to-point links to each neighbor and
// the interface address as a host route.
func TestOSPFNBMAAndP2MP(t *testing.T) {
	for _, tc := range []struct {
		name, typ, network string
	}{{"nbma-v2", "ospf", "non-broadcast"}, {"nbma-v3", "ospfv3", "non-broadcast"}, {"p2mp-v2", "ospf", "point-to-multipoint"}, {"p2mp-v3", "ospfv3", "point-to-multipoint"}} {
		t.Run(tc.name, func(t *testing.T) {
			a, b, _, _ := ipPair(t)
			if _, err := b.CreateDevice(core.DeviceSpec{Name: "d2", Port: "eth1", MAC: "02:00:00:00:0b:02", VLANs: []pkt.VLANTag{{VID: 10}},
				Addresses: []string{"10.0.0.3/24", "2001:db8::3/64"}}); err != nil {
				t.Fatal(err)
			}
			v3 := tc.typ == "ospfv3"
			addr := map[string]string{"a": "10.0.0.1", "b": "10.0.0.2", "c": "10.0.0.3"}
			if v3 { // link-local addresses from the MACs
				addr = map[string]string{"a": "fe80::ff:fe00:a01", "b": "fe80::ff:fe00:b01", "c": "fe80::ff:fe00:b02"}
			}
			mk := func(e *core.Engine, name, dev, rid, prio string, others ...string) {
				t.Helper()
				nb := ""
				if tc.network == "non-broadcast" {
					nb = fmt.Sprintf(`"neighbors": ["%s", "%s"], `, addr[others[0]], addr[others[1]])
				}
				cfg := fmt.Sprintf(`{"router_id": "%s", "network": "%s", "priority": %s, %s%s}`, rid, tc.network, prio, nb, ospfTimers)
				if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: tc.typ, Device: dev, Config: json.RawMessage(cfg)}); err != nil {
					t.Fatal(err)
				}
			}
			mk(a, "o", "d", "192.0.2.1", "10", "b", "c")
			mk(b, "o", "d", "192.0.2.2", "5", "a", "c")
			mk(b, "c", "d2", "192.0.2.3", "1", "a", "b")
			defer func() {
				if t.Failed() {
					for _, x := range []struct {
						e *core.Engine
						n string
					}{{a, "o"}, {b, "o"}, {b, "c"}} {
						st := flowStatus(t, x.e, x.n)
						t.Logf("%s %v nbrs %v %v", st["router_id"], st["interfaces"], ospfNbrs(t, x.e, x.n), st["counters"])
					}
				}
			}()
			eventuallyLong(t, "all adjacent", func() bool {
				return ospfFull(t, a, "o", 2) && ospfFull(t, b, "o", 2) && ospfFull(t, b, "c", 2)
			})
			rid := func(r string) string {
				if v3 {
					return "0.0.0.0"
				}
				return r
			}
			if tc.network == "non-broadcast" {
				f := flowStatus(t, a, "o")["interfaces"].([]any)[0].(map[string]any)
				if f["state"] != "dr" {
					t.Fatalf("a: %v", f)
				}
				eventuallyLong(t, "network LSA", func() bool { return lsdbTypes(t, b, "c")["network"] == 1 })
				return
			}
			eventuallyLong(t, "p2p links to both", func() bool {
				p2p, stubs := 0, 0
				for _, l := range linksOf(t, b, "c", "router", rid("192.0.2.1"), "192.0.2.1") {
					switch l["type"] {
					case "point-to-point":
						p2p++
					case "stub":
						if l["id"] == "10.0.0.1" && l["data"] == "255.255.255.255" {
							stubs++
						}
					}
				}
				return p2p == 2 && (v3 || stubs == 1)
			})
			if lsdbTypes(t, b, "c")["network"] != 0 {
				t.Fatalf("network LSAs on a point-to-multipoint network: %v", lsdbTypes(t, b, "c"))
			}
			if v3 {
				ip := ospfRun(t, b, "c", "lsa", `{"type": "intra-area-prefix", "id": "0.0.0.0", "adv_router": "192.0.2.1"}`)
				ps := ip["prefixes"].([]any)
				if len(ps) != 1 || ps[0].(map[string]any)["prefix"] != "2001:db8::1/128" {
					t.Fatalf("a's prefixes: %v", ip)
				}
			}
		})
	}
}

// An LSA larger than the MTU: our router LSA with links to 150 simulated
// routers is sent in IP fragments and reassembled at b.
func TestOSPFFragmentation(t *testing.T) {
	for _, v3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "v3"}[v3], func(t *testing.T) {
			a, b, _, _ := ipPair(t)
			typ, id, links := "ospf", "192.0.2.1", 152 // b, the subnet (v2), the simulated routers
			if v3 {
				typ, id, links = "ospfv3", "0.0.0.0", 151
			}
			var att []string
			for i := 1; i <= 150; i++ {
				att = append(att, fmt.Sprintf(`"r%d"`, i))
			}
			for i, e := range []*core.Engine{a, b} {
				cfg := fmt.Sprintf(`{"router_id": "192.0.2.%d", "network": "point-to-point", %s}`, i+1, ospfTimers)
				if i == 0 {
					cfg = fmt.Sprintf(`{"router_id": "192.0.2.1", "network": "point-to-point", %s, "topology": {"kind": "ring", "nodes": 150, "attach": [%s]}}`,
						ospfTimers, strings.Join(att, ","))
				}
				if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "o", Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
					t.Fatal(err)
				}
			}
			eventuallyLong(t, "full", func() bool { return ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1) })
			eventuallyLong(t, "a's big router LSA at b", func() bool { return len(linksOf(t, b, "o", "router", id, "192.0.2.1")) == links })
			ca := flowStatus(t, a, "o")["counters"].(map[string]any)
			cb := flowStatus(t, b, "o")["counters"].(map[string]any)
			if ca["tx_fragmented"] == nil || cb["rx_reassembled"] == nil {
				t.Fatalf("a %v\nb %v", ca, cb)
			}
		})
	}
}

// A virtual link: a is in area 1 (to b) with area 3 behind it and no
// backbone link; the virtual link across area 1 to b (a backbone border
// router) carries a's summaries of area 3 into the backbone.
func TestOSPFVirtualLink(t *testing.T) {
	for _, v3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "v3"}[v3], func(t *testing.T) { testVirtualLink(t, v3) })
	}
}

func testVirtualLink(t *testing.T, v3 bool) {
	a, b, _, _ := ipPair(t)
	typ, lb, lbB, sum, rid := "ospf", "10.255.0.1/32", "192.0.2.2/32", "summary", "192.0.2.1"
	if v3 {
		typ, lb, lbB, sum, rid = "ospfv3", "2001:db8:ff::1/128", "2001:db8:ffff::2/128", "inter-area-prefix", "0.0.0.0"
	}
	mk := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "o", Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	mk(a, `{"router_id": "192.0.2.1", "network": "point-to-point", "area": "1", `+ospfTimers+`,
		"virtual_links": [{"transit_area": "1", "router_id": "192.0.2.2"}],
		"topology": {"kind": "line", "nodes": 2, "area": "3", "loopback_base": "`+lb+`"}}`)
	mk(b, `{"router_id": "192.0.2.2", "network": "point-to-point", "area": "1", `+ospfTimers+`,
		"loopbacks": ["`+lbB+`"], "virtual_links": [{"transit_area": "1", "router_id": "192.0.2.1"}]}`)
	defer func() {
		if t.Failed() {
			for _, e := range []*core.Engine{a, b} {
				st := flowStatus(t, e, "o")
				t.Logf("%s if %v nbrs %v", st["router_id"], st["interfaces"], ospfNbrs(t, e, "o"))
				for _, l := range lsaRows(t, e, "o") {
					t.Logf("   %v %v %v %v %v", l["area"], l["type"], l["id"], l["adv_router"], l["summary"])
				}
			}
		}
	}()
	eventuallyLong(t, "virtual adjacency", func() bool { return ospfFull(t, a, "o", 2) && ospfFull(t, b, "o", 2) })
	eventuallyLong(t, "a's backbone router LSA with the virtual link", func() bool {
		for _, l := range lsaRows(t, b, "o") {
			if l["area"] == "0.0.0.0" && l["type"] == "router" && l["adv_router"] == "192.0.2.1" {
				return strings.Contains(l["summary"].(string), "1 virtual")
			}
		}
		return false
	})
	eventuallyLong(t, "area 3 summarised into the backbone", func() bool {
		return countLSAs(t, b, "o", sum, "192.0.2.1", "0.0.0.0") >= 2
	})
	eventuallyLong(t, "V and B bits in a's transit area router LSA", func() bool {
		f := ospfRun(t, b, "o", "lsa", `{"type": "router", "id": "`+rid+`", "adv_router": "192.0.2.1", "area": "1"}`)["flags"].(map[string]any)
		return f["virtual"] == true && f["abr"] == true
	})
}

// BFD: sessions come up with the adjacency; when a stops sending BFD, b
// drops a within the detection time (the dead interval is 40 s), and BFD
// is up again once a sends again.
func TestOSPFBFD(t *testing.T) {
	for _, v3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "v3"}[v3], func(t *testing.T) {
			a, b, _, _ := ipPair(t)
			typ := "ospf"
			if v3 {
				typ = "ospfv3"
			}
			for i, e := range []*core.Engine{a, b} {
				cfg := fmt.Sprintf(`{"router_id": "192.0.2.%d", "network": "point-to-point", "hello_interval": 1, "dead_interval": 40, "retransmit_interval": 1,
					"bfd": {"enabled": true, "min_tx_ms": 100, "min_rx_ms": 100, "multiplier": 3}}`, i+1)
				if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "o", Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
					t.Fatal(err)
				}
			}
			bfdState := func(e *core.Engine) string {
				for _, n := range ospfNbrs(t, e, "o") {
					if x, ok := n["bfd"].(map[string]any); ok {
						return x["state"].(string)
					}
				}
				return ""
			}
			eventuallyLong(t, "full with BFD up", func() bool {
				return ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1) && bfdState(a) == "up" && bfdState(b) == "up"
			})
			// Fast timers once both have sent with them (a poll sequence).
			eventuallyLong(t, "fast timers", func() bool {
				x, _ := ospfNbrs(t, b, "o")[0]["bfd"].(map[string]any)
				return x["detect_ms"] == 300.0
			})
			ospfRun(t, a, "o", "bfd-fail", `{"seconds": 3}`)
			start := time.Now()
			// b drops a (and a's hellos bring it back at once: BFD doesn't
			// hold the adjacency down).
			eventuallyLong(t, "b drops a", func() bool {
				return flowStatus(t, b, "o")["counters"].(map[string]any)["bfd_down"] != nil
			})
			if d := time.Since(start); d > time.Second {
				t.Fatalf("b took %v to drop a", d)
			}
			eventuallyLong(t, "back", func() bool { return ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1) && bfdState(b) == "up" })
		})
	}
}

// A DR restarting gracefully on a LAN stays DR, and the helpers'
// router LSAs and the network LSA don't change.
func TestOSPFGracefulRestartDR(t *testing.T) {
	a, b, _, _ := ipPair(t)
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "d2", Port: "eth1", MAC: "02:00:00:00:0b:02", VLANs: []pkt.VLANTag{{VID: 10}}, Addresses: []string{"10.0.0.3/24"}}); err != nil {
		t.Fatal(err)
	}
	mkOSPF(t, a, "o", "d", `{"router_id": "192.0.2.1", "priority": 100, `+ospfTimers+`}`)
	mkOSPF(t, b, "o", "d", `{"router_id": "192.0.2.2", "priority": 10, `+ospfTimers+`}`)
	mkOSPF(t, b, "c", "d2", `{"router_id": "192.0.2.3", "priority": 0, `+ospfTimers+`}`)
	eventuallyLong(t, "adjacencies", func() bool { return ospfFull(t, a, "o", 2) && ospfFull(t, b, "o", 2) && ospfFull(t, b, "c", 2) })
	eventuallyLong(t, "network LSA", func() bool { return lsaSeqBy(t, b, "c", "network", "10.0.0.1", "192.0.2.1") != "" })
	time.Sleep(2 * time.Second)
	seqs := func() []string {
		return []string{lsaSeqBy(t, b, "c", "router", "192.0.2.2", "192.0.2.2"), lsaSeqBy(t, b, "c", "router", "192.0.2.3", "192.0.2.3"),
			lsaSeqBy(t, b, "c", "network", "10.0.0.1", "192.0.2.1")}
	}
	before := seqs()
	ospfRun(t, a, "o", "graceful-restart", `{"seconds": 60}`)
	eventuallyLong(t, "restart done", func() bool {
		return flowStatus(t, a, "o")["graceful_restart"].(map[string]any)["restart"] == nil && ospfFull(t, a, "o", 2)
	})
	f := flowStatus(t, a, "o")["interfaces"].([]any)[0].(map[string]any)
	if f["state"] != "dr" {
		t.Fatalf("a after the restart: %v", f)
	}
	if after := seqs(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("LSAs changed: %v → %v", before, after)
	}
}

// OSPFv3 segment routing in extended LSAs: b sees a's Router Information
// (SRGB), its E-Router-LSA with an Adj-SID and the Prefix-SIDs of a's
// loopback and the simulated routers'.
func TestOSPFv3SegmentRouting(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mkOSPF3(t, a, "o", "d", `{"router_id": "192.0.2.1", "network": "point-to-point", `+ospfTimers+`, "loopbacks": ["2001:db8:ff::1/128"],
		"segment_routing": {"enabled": true, "srgb_base": 16000, "srgb_size": 8000, "adj_sid_base": 24000,
			"node_sids": [{"prefix": "2001:db8:ff::1/128", "index": 1}]},
		"topology": {"kind": "line", "nodes": 2, "loopback_base": "2001:db8:ff1::1/128", "node_sid_base": 100}}`)
	mkOSPF3(t, b, "o", "d", `{"router_id": "192.0.2.2", "network": "point-to-point", `+ospfTimers+`}`)
	eventuallyLong(t, "full", func() bool { return ospfFull(t, a, "o", 1) && ospfFull(t, b, "o", 1) })
	find := func(typ, adv, sub string) bool {
		for _, l := range lsaRows(t, b, "o") {
			if l["type"] == typ && l["adv_router"] == adv && strings.Contains(l["summary"].(string), sub) {
				return true
			}
		}
		return false
	}
	defer func() {
		if t.Failed() {
			for _, l := range lsaRows(t, b, "o") {
				t.Logf("%v %v %v %v", l["type"], l["id"], l["adv_router"], l["summary"])
			}
		}
	}()
	eventuallyLong(t, "SR LSAs at b", func() bool {
		return find("router-information", "192.0.2.1", "SRGB 16000-23999") && find("e-router", "192.0.2.1", "adj label 24000") &&
			find("e-intra-area-prefix", "192.0.2.1", "2001:db8:ff::1/128 index 1") && find("e-intra-area-prefix", "10.254.0.2", "2001:db8:ff1::2/128 index 101")
	})
}

// Config edits that only change what a router advertises apply in place:
// the neighbor sees new LSAs (and the simulated routers that are gone
// flushed) with the adjacency still full; new timers restart the instance.
func TestOSPFReconfigure(t *testing.T) {
	for _, v3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2", true: "v3"}[v3], func(t *testing.T) {
			a, b, _, _ := ipPair(t)
			typ, lb := "ospf", "10.255.0.1/32"
			if v3 {
				typ, lb = "ospfv3", "2001:db8:ff::1/128"
			}
			base := `"router_id": "192.0.2.1", "network": "point-to-point", ` + ospfTimers
			topo := func(n int) string {
				return fmt.Sprintf(`"topology": {"kind": "grid", "rows": %d, "cols": %d, "loopback_base": "%s"}`, n, n, lb)
			}
			if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "o", Type: typ, Device: "d", Config: json.RawMessage(`{` + base + `, ` + topo(3) + `}`)}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.CreateProtocol(core.ProtocolSpec{Name: "o", Type: typ, Device: "d", Config: json.RawMessage(`{"router_id": "192.0.2.2", "network": "point-to-point", ` + ospfTimers + `}`)}); err != nil {
				t.Fatal(err)
			}
			eventuallyLong(t, "11 router LSAs at b", func() bool { return ospfFull(t, b, "o", 1) && lsdbTypes(t, b, "o")["router"] == 11 })
			since := func(e *core.Engine) any { return ospfNbrs(t, e, "o")[0]["since"] }
			sa, sb := since(a), since(b)
			rid := "192.0.2.1"
			if v3 {
				rid = "0.0.0.0"
			}
			seq := lsaSeqBy(t, b, "o", "router", rid, "192.0.2.1")
			update := func(cfg string) {
				t.Helper()
				if _, err := a.UpdateProtocol("o", json.RawMessage(`{`+base+`, `+cfg+`}`)); err != nil {
					t.Fatal(err)
				}
			}
			// A smaller simulated topology and a new cost.
			update(topo(2) + `, "cost": 30`)
			eventuallyLong(t, "6 router LSAs at b and ours re-originated", func() bool {
				return lsdbTypes(t, b, "o")["router"] == 6 && lsaSeqBy(t, b, "o", "router", rid, "192.0.2.1") != seq
			})
			if !ospfFull(t, a, "o", 1) || since(a) != sa || since(b) != sb {
				t.Fatalf("the adjacency was reset: %v / %v", ospfNbrs(t, a, "o"), ospfNbrs(t, b, "o"))
			}
			// New timers change the adjacency: the instance restarts.
			time.Sleep(1100 * time.Millisecond) // since has whole seconds
			update(topo(2) + `, "cost": 30, "retransmit_interval": 2`)
			eventuallyLong(t, "a's adjacency re-formed", func() bool { return ospfFull(t, a, "o", 1) && since(a) != sa })
		})
	}
}
