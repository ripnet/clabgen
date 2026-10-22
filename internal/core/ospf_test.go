package core_test

import (
	"context"
	"encoding/json"
	"testing"

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
	for _, x := range flowStatus(t, e, name)["lsdb"].([]any) {
		l := x.(map[string]any)
		if l["type"] == typ && l["id"] == id {
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
	d := ospfRun(t, b, "ospf", "lsa", `{"type": "router", "id": "192.0.2.1"}`)
	links := d["links"].([]any)
	if d["flags"].(map[string]any)["asbr"] != true || len(links) != 4 {
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
