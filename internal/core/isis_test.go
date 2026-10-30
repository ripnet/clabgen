package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/events"
	"github.com/ripnet/clabgen/internal/pkt"
	_ "github.com/ripnet/clabgen/protocols/isis"
)

// isisStatus returns the status with each LSDB row merged with its full
// content (the status itself only has summaries).
func isisStatus(t *testing.T, e *core.Engine) map[string]any {
	t.Helper()
	st := flowStatus(t, e, "isis")
	rows, _ := st["lsdb"].([]any)
	for _, r := range rows {
		m := r.(map[string]any)
		d, err := e.RunAction(context.Background(), "isis", "lsp", json.RawMessage(fmt.Sprintf(`{"id":%q,"level":%v}`, m["lsp_id"], m["level"])))
		if err != nil {
			continue // removed meanwhile
		}
		b, _ := json.Marshal(d)
		var dm map[string]any
		json.Unmarshal(b, &dm)
		for k, v := range dm {
			m[k] = v
		}
	}
	return st
}

func lsdb(st map[string]any) []map[string]any {
	l, _ := st["lsdb"].([]any)
	out := make([]map[string]any, 0, len(l))
	for _, x := range l {
		out = append(out, x.(map[string]any))
	}
	return out
}

// lspOf returns sysid's fragment 0 as seen in st's LSDB.
func lspOf(st map[string]any, sysid string) map[string]any {
	for _, l := range lsdb(st) {
		if l["lsp_id"] == sysid+".00-00" {
			return l
		}
	}
	return nil
}

func prefixes(l map[string]any) map[string]bool {
	out := map[string]bool{}
	ps, _ := l["prefixes"].([]any)
	for _, p := range ps {
		out[p.(map[string]any)["prefix"].(string)] = true
	}
	return out
}

func allPrefixes(st map[string]any, sysid string) int {
	n := 0
	for _, l := range lsdb(st) {
		if strings.HasPrefix(l["lsp_id"].(string), sysid+".00-") {
			ps, _ := l["prefixes"].([]any)
			n += len(ps)
		}
	}
	return n
}

func act(t *testing.T, e *core.Engine, action, args string) {
	t.Helper()
	if _, err := e.RunAction(context.Background(), "isis", action, json.RawMessage(args)); err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

func TestISISP2P(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mk := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	mk(a, `{"system_id":"0000.0000.000a","hostname":"ra","hello_interval":1,"csnp_interval":2,
		"prefixes":[{"prefix":"198.51.100.0/24","metric":20}]}`)
	mk(b, `{"system_id":"0000.0000.000b","hostname":"rb","hello_interval":1,"csnp_interval":2}`)
	up := func(e *core.Engine) bool {
		adj, _ := isisStatus(t, e)["adjacency"].(map[string]any)
		return adj["state"] == "up"
	}
	eventually(t, "adjacency up", func() bool { return up(a) && up(b) })
	eventually(t, "LSDBs synced", func() bool {
		return lspOf(isisStatus(t, b), "0000.0000.000a") != nil && lspOf(isisStatus(t, a), "0000.0000.000b") != nil
	})
	st := isisStatus(t, b)
	if adj := st["adjacency"].(map[string]any); adj["hostname"] != "ra" || adj["three_way"] != true {
		t.Fatalf("adjacency %v", adj)
	}
	la := lspOf(st, "0000.0000.000a")
	if p := prefixes(la); !p["198.51.100.0/24"] || !p["10.0.0.0/24"] || !p["2001:db8::/64"] {
		t.Fatalf("a's prefixes at b: %v", la["prefixes"])
	}
	if nb, _ := la["neighbors"].([]any); len(nb) != 1 || nb[0].(map[string]any)["id"] != "0000.0000.000b.00" {
		t.Fatalf("a's neighbors %v", la["neighbors"])
	}

	// Runtime changes reach the peer.
	seq0 := num(lspOf(isisStatus(t, b), "0000.0000.000a"), "seq")
	act(t, a, "advertise", `{"prefixes":[{"prefix":"203.0.113.0/24"}]}`)
	act(t, a, "withdraw", `{"prefixes":[{"prefix":"198.51.100.0/24"}]}`)
	act(t, a, "overload", `{"set":true}`)
	act(t, a, "metric", `{"metric":77}`)
	eventually(t, "changes flooded", func() bool {
		l := lspOf(isisStatus(t, b), "0000.0000.000a")
		p := prefixes(l)
		nb, _ := l["neighbors"].([]any)
		return p["203.0.113.0/24"] && !p["198.51.100.0/24"] && l["overload"] == true && len(nb) == 1 && num(nb[0].(map[string]any), "metric") == 77
	})
	if num(lspOf(isisStatus(t, b), "0000.0000.000a"), "seq") <= seq0 {
		t.Fatal("sequence not bumped")
	}

	// Scale: 3000 prefixes need several fragments; all arrive.
	act(t, a, "advertise", `{"prefixes":[{"prefix":"10.100.0.0/24","count":3000}]}`)
	eventually(t, "fragments flooded", func() bool { return allPrefixes(isisStatus(t, b), "0000.0000.000a") >= 3003 })
	frags := 0
	for _, l := range lsdb(isisStatus(t, b)) {
		if strings.HasPrefix(l["lsp_id"].(string), "0000.0000.000a.00-") {
			frags++
		}
	}
	if frags < 10 {
		t.Fatalf("%d fragments", frags)
	}
	// Withdrawing them purges the extra fragments.
	act(t, a, "withdraw", `{"prefixes":[{"prefix":"10.100.0.0/24","count":3000}]}`)
	eventually(t, "fragments purged", func() bool {
		n := 0
		for _, l := range lsdb(isisStatus(t, b)) {
			if strings.HasPrefix(l["lsp_id"].(string), "0000.0000.000a.00-") && num(l, "lifetime") > 0 {
				n++
			}
		}
		return n == 1
	})

	// Flap: down, then back up on its own.
	act(t, a, "flap", `{"seconds":1,"mode":"reset"}`)
	eventually(t, "b sees the adjacency drop", func() bool { return !up(b) })
	eventuallyLong(t, "back up after the flap", func() bool { return up(a) && up(b) })

	// Restart a with the same system ID: it takes over its old LSP with a
	// higher sequence number.
	seqBefore := num(lspOf(isisStatus(t, b), "0000.0000.000a"), "seq")
	if err := a.DeleteProtocol("isis"); err != nil {
		t.Fatal(err)
	}
	mk(a, `{"system_id":"0000.0000.000a","hostname":"ra2","hello_interval":1,"csnp_interval":2}`)
	eventually(t, "takeover", func() bool {
		l := lspOf(isisStatus(t, b), "0000.0000.000a")
		return l != nil && l["hostname"] == "ra2" && num(l, "seq") > seqBefore
	})
	time.Sleep(10 * time.Millisecond)
}

// hub joins port eth1 of several engines like a shared segment.
type hub struct {
	mu    sync.Mutex
	ports []*hubPort
}

type hubPort struct {
	h   *hub
	eng *core.Engine
}

func (p *hubPort) Send(_ string, b []byte) error {
	p.h.mu.Lock()
	peers := append([]*hubPort(nil), p.h.ports...)
	p.h.mu.Unlock()
	for _, q := range peers {
		if q != p && q.eng != nil {
			go q.eng.Receive("eth1", append([]byte(nil), b...), time.Now())
		}
	}
	return nil
}
func (p *hubPort) PortUp(string) bool          { return true }
func (p *hubPort) SetAdmin(string, bool) error { return nil }
func (p *hubPort) Desired() map[string]bool    { return map[string]bool{} }

func lanRouter(t *testing.T, h *hub, i int, prio int) *core.Engine {
	t.Helper()
	p := &hubPort{h: h}
	e := core.New(p, events.NewBus(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.eng = e
	h.mu.Lock()
	h.ports = append(h.ports, p)
	h.mu.Unlock()
	mac := fmt.Sprintf("02:00:00:00:00:%02x", i)
	if _, err := e.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", MAC: mac, Addresses: []string{fmt.Sprintf("10.9.0.%d/24", i)}}); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"system_id":"0000.0000.00%02x","hostname":"r%d","circuit":"broadcast","priority":%d,"hello_interval":1,"csnp_interval":1}`, i, i, prio)
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
	return e
}

func upCount(st map[string]any) int {
	n := 0
	l, _ := st["adjacencies"].([]any)
	for _, a := range l {
		if a.(map[string]any)["state"] == "up" {
			n++
		}
	}
	return n
}

func lanDIS(st map[string]any) (dis, lanID string) {
	l, _ := st["lan"].([]any)
	if len(l) == 0 {
		return "", ""
	}
	m := l[0].(map[string]any)
	return m["dis"].(string), m["lan_id"].(string)
}

func lspNeighbors(l map[string]any) []string {
	var out []string
	nb, _ := l["neighbors"].([]any)
	for _, x := range nb {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	sort.Strings(out)
	return out
}

func TestISISLAN(t *testing.T) {
	h := &hub{}
	a := lanRouter(t, h, 1, 64)
	b := lanRouter(t, h, 2, 64)
	c := lanRouter(t, h, 3, 100)
	all := []*core.Engine{a, b, c}
	eventually(t, "full mesh of LAN adjacencies", func() bool {
		for _, e := range all {
			if upCount(isisStatus(t, e)) != 2 {
				return false
			}
		}
		return true
	})
	// c has the highest priority: it is DIS, and everyone uses its LAN ID.
	eventually(t, "c elected DIS", func() bool {
		for _, e := range all {
			if d, id := lanDIS(isisStatus(t, e)); d != "0000.0000.0003" || id != "0000.0000.0003.01" {
				return false
			}
		}
		return true
	})
	pn := "0000.0000.0003.01-00"
	eventually(t, "pseudonode and router LSPs everywhere", func() bool {
		for _, e := range all {
			st := isisStatus(t, e)
			var p map[string]any
			for _, l := range lsdb(st) {
				if l["lsp_id"] == pn {
					p = l
				}
			}
			if p == nil || strings.Join(lspNeighbors(p), ",") != "0000.0000.0001.00,0000.0000.0002.00,0000.0000.0003.00" {
				return false
			}
			for _, sys := range []string{"0000.0000.0001", "0000.0000.0002", "0000.0000.0003"} {
				l := lspOf(st, sys)
				if l == nil || strings.Join(lspNeighbors(l), ",") != "0000.0000.0003.01" {
					return false
				}
			}
		}
		return true
	})
	// Flooding on the LAN: a's new prefix reaches b and c.
	act(t, a, "advertise", `{"prefixes":[{"prefix":"203.0.113.0/24"}]}`)
	eventually(t, "flooded on the LAN", func() bool {
		return prefixes(lspOf(isisStatus(t, b), "0000.0000.0001"))["203.0.113.0/24"] && prefixes(lspOf(isisStatus(t, c), "0000.0000.0001"))["203.0.113.0/24"]
	})
	// c leaves: a and b (equal priority) elect b, the higher MAC.
	if err := c.DeleteProtocol("isis"); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.ports[2].eng = nil
	h.mu.Unlock()
	eventually(t, "b takes over as DIS", func() bool {
		da, ida := lanDIS(isisStatus(t, a))
		db, _ := lanDIS(isisStatus(t, b))
		return da == "0000.0000.0002" && db == "0000.0000.0002" && ida == "0000.0000.0002.01" && upCount(isisStatus(t, a)) == 1
	})
	eventually(t, "LSPs point at the new pseudonode", func() bool {
		l := lspOf(isisStatus(t, b), "0000.0000.0001")
		return l != nil && strings.Join(lspNeighbors(l), ",") == "0000.0000.0002.01"
	})
}

func TestISISTopology(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mk := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	topo := `"topology":{"kind":"grid","rows":3,"cols":3,"loopback_base":"10.255.0.1/32","prefix_base":"10.200.0.0/24","prefixes_per_node":2}`
	mk(a, `{"system_id":"0000.0000.000a","hello_interval":1,"csnp_interval":1,`+topo+`}`)
	mk(b, `{"system_id":"0000.0000.000b","hello_interval":1,"csnp_interval":1}`)
	sim := func(i int) string { return fmt.Sprintf("0000.1000.%04x", i) } // r1-1 = 1, r2-2 = 5, r3-3 = 9
	eventually(t, "simulated LSPs at b", func() bool { return num(isisStatus(t, b), "lsdb_total") == 11 })
	st := isisStatus(t, b)
	if nb := strings.Join(lspNeighbors(lspOf(st, sim(5))), ","); nb != sim(2)+".00,"+sim(4)+".00,"+sim(6)+".00,"+sim(8)+".00" {
		t.Fatalf("r2-2 neighbors %s", nb)
	}
	if nb := lspNeighbors(lspOf(st, sim(1))); len(nb) != 3 || nb[0] != "0000.0000.000a.00" {
		t.Fatalf("r1-1 neighbors %v", nb)
	}
	if nb := lspNeighbors(lspOf(st, "0000.0000.000a")); len(nb) != 2 { // b and r1-1
		t.Fatalf("a neighbors %v", nb)
	}
	if p := prefixes(lspOf(st, sim(9))); !p["10.255.0.9/32"] || !p["10.200.16.0/24"] || !p["10.200.17.0/24"] {
		t.Fatalf("r3-3 prefixes %v", p)
	}
	if lspOf(st, sim(5))["hostname"] != "sim-r2-2" {
		t.Fatal("hostname")
	}

	// Link r1-1 — r1-2 down: both LSPs drop it.
	act(t, a, "topology-link", `{"a":"r1-1","b":"r1-2","up":false}`)
	eventually(t, "link down flooded", func() bool {
		st := isisStatus(t, b)
		return len(lspNeighbors(lspOf(st, sim(1)))) == 2 && len(lspNeighbors(lspOf(st, sim(2)))) == 2
	})
	// Metric change on the attachment.
	act(t, a, "topology-link", `{"a":"us","b":"r1-1","up":true,"metric":99}`)
	eventually(t, "metric flooded", func() bool {
		nb, _ := lspOf(isisStatus(t, b), "0000.0000.000a")["neighbors"].([]any)
		for _, x := range nb {
			if m := x.(map[string]any); m["id"] == sim(1)+".00" && num(m, "metric") == 99 {
				return true
			}
		}
		return false
	})
	// Node r3-3 down: purged at b; up again: back.
	act(t, a, "topology-node", `{"node":"r3-3","up":false}`)
	eventually(t, "node purged", func() bool { l := lspOf(isisStatus(t, b), sim(9)); return l != nil && num(l, "lifetime") == 0 })
	act(t, a, "topology-node", `{"node":"r3-3","up":true,"overload":true}`)
	eventually(t, "node back, overloaded", func() bool {
		l := lspOf(isisStatus(t, b), sim(9))
		return l != nil && num(l, "lifetime") > 0 && l["overload"] == true
	})
	if tp, _ := isisStatus(t, a)["topology"].(map[string]any); tp == nil || num(tp, "nodes") != 9 || len(tp["down_links"].([]any)) != 1 {
		t.Fatalf("topology status %v", tp)
	}
	// Restart a: b still holds the simulated LSPs; a takes them over.
	// r1-1 changed (link down, metric), so b holds a newer copy than a
	// fresh start produces.
	seq := num(lspOf(isisStatus(t, b), sim(1)), "seq")
	a.DeleteProtocol("isis")
	mk(a, `{"system_id":"0000.0000.000a","hello_interval":1,"csnp_interval":1,`+topo+`}`)
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		l := lspOf(isisStatus(t, b), sim(1))
		if l != nil && num(l, "seq") > seq && len(lspNeighbors(l)) == 3 {
			t.Logf("taken over after %d ms (seq %v → %v)", i*100, seq, num(l, "seq"))
			return
		}
	}
	t.Fatalf("not taken over: b has %v, a has %v", lspOf(isisStatus(t, b), sim(1))["seq"], lspOf(isisStatus(t, a), sim(1))["seq"])
}

func TestISISAuth(t *testing.T) {
	cases := []struct {
		name       string
		a, b       string
		adj, synch bool
		failure    string // counter at b
	}{
		{"md5 both", `"hello_auth":{"type":"hmac-md5","key":"k1"},"lsp_auth":{"type":"hmac-md5","key":"k2"}`,
			`"hello_auth":{"type":"hmac-md5","key":"k1"},"lsp_auth":{"type":"hmac-md5","key":"k2"}`, true, true, ""},
		{"password both", `"hello_auth":{"type":"password","key":"p"},"lsp_auth":{"type":"password","key":"q"}`,
			`"hello_auth":{"type":"password","key":"p"},"lsp_auth":{"type":"password","key":"q"}`, true, true, ""},
		{"hello key mismatch", `"hello_auth":{"type":"hmac-md5","key":"k1"}`, `"hello_auth":{"type":"hmac-md5","key":"other"}`, false, false, "rx_auth_failures_hello"},
		{"lsp key mismatch", `"lsp_auth":{"type":"hmac-md5","key":"k2"}`, `"lsp_auth":{"type":"hmac-md5","key":"nope"}`, true, false, "rx_auth_failures_lsp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b, _, _ := ipPair(t)
			for _, x := range []struct {
				e   *core.Engine
				id  string
				cfg string
			}{{a, "000a", c.a}, {b, "000b", c.b}} {
				cfg := fmt.Sprintf(`{"system_id":"0000.0000.%s","hello_interval":1,"csnp_interval":1,%s}`, x.id, x.cfg)
				if _, err := x.e.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
					t.Fatal(err)
				}
			}
			up := func(e *core.Engine) bool {
				adj, _ := isisStatus(t, e)["adjacency"].(map[string]any)
				return adj["state"] == "up"
			}
			if c.adj {
				eventually(t, "adjacency up", func() bool { return up(a) && up(b) })
			}
			if c.synch {
				eventually(t, "synced", func() bool { return lspOf(isisStatus(t, b), "0000.0000.000a") != nil })
			}
			if c.failure != "" {
				eventually(t, "failures counted", func() bool {
					cs, _ := isisStatus(t, b)["counters"].(map[string]any)
					return num(cs, c.failure) > 0
				})
				time.Sleep(1500 * time.Millisecond)
				if !c.adj && (up(a) || up(b)) {
					t.Fatal("adjacency up despite hello auth mismatch")
				}
				if !c.synch && lspOf(isisStatus(t, b), "0000.0000.000a") != nil {
					t.Fatal("LSP accepted despite auth mismatch")
				}
			}
			if st := isisStatus(t, b); c.name == "md5 both" && (st["hello_auth"] != "hmac-md5" || st["lsp_auth"] != "hmac-md5") {
				t.Fatalf("status auth %v %v", st["hello_auth"], st["lsp_auth"])
			}
		})
	}
}

func TestISISMultiTopology(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mk := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	mk(a, `{"system_id":"0000.0000.000a","hello_interval":1,"csnp_interval":1,"topologies":["ipv6-unicast"],
		"topology":{"kind":"line","nodes":2,"loopback6_base":"2001:db8:ff::1/128","prefix6_base":"2001:db8:200::/64","prefixes_per_node":1}}`)
	mk(b, `{"system_id":"0000.0000.000b","hello_interval":1,"csnp_interval":1,"topologies":["ipv6-unicast"]}`)
	eventually(t, "MT LSPs at b", func() bool { return num(isisStatus(t, b), "lsdb_total") == 4 })
	st := isisStatus(t, b)
	if adj := st["adjacency"].(map[string]any); fmt.Sprint(adj["topologies"]) != "[ipv4-unicast ipv6-unicast]" {
		t.Fatalf("adjacency topologies %v", adj["topologies"])
	}
	la := lspOf(st, "0000.0000.000a")
	if fmt.Sprint(la["topologies"]) != "[ipv4-unicast ipv6-unicast]" {
		t.Fatalf("a's topologies %v", la["topologies"])
	}
	mt2 := func(l map[string]any) (nbrs, v6 int) {
		for _, x := range l["neighbors"].([]any) {
			if num(x.(map[string]any), "mt") == 2 {
				nbrs++
			}
		}
		for _, x := range l["prefixes"].([]any) {
			m := x.(map[string]any)
			if num(m, "mt") == 2 {
				v6++
			} else if strings.Contains(m["prefix"].(string), ":") {
				t.Fatalf("IPv6 prefix outside MT 2: %v", m)
			}
		}
		return
	}
	if n, v := mt2(la); n != 2 || v != 1 { // b and r1; 2001:db8::/64
		t.Fatalf("a: %d MT-2 neighbors, %d MT-2 prefixes", n, v)
	}
	if n, v := mt2(lspOf(st, "0000.1000.0002")); n != 1 || v != 2 { // r1; loopback + 1 prefix
		t.Fatalf("r2: %d MT-2 neighbors, %d MT-2 prefixes", n, v)
	}

	// A single-topology router still peers (over MT 0) with an MT one.
	b.DeleteProtocol("isis")
	mk(b, `{"system_id":"0000.0000.000b","hello_interval":1,"csnp_interval":1}`)
	eventually(t, "MT 0 adjacency", func() bool {
		adj, _ := isisStatus(t, a)["adjacency"].(map[string]any)
		return adj["state"] == "up" && fmt.Sprint(adj["topologies"]) == "[ipv4-unicast]"
	})
}

func TestISISSegmentRouting(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mk := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	mk(a, `{"system_id":"0000.0000.000a","hello_interval":1,"csnp_interval":1,
		"segment_routing":{"enabled":true,"srgb_base":16000,"srgb_size":8000,"adj_sid_base":24000,"node_sids":[{"prefix":"192.0.2.10/32","index":5}]},
		"topology":{"kind":"line","nodes":2,"loopback_base":"10.255.0.1/32","node_sid_base":100}}`)
	mk(b, `{"system_id":"0000.0000.000b","hello_interval":1,"csnp_interval":1}`)
	eventually(t, "SR LSPs at b", func() bool {
		l := lspOf(isisStatus(t, b), "0000.0000.000a")
		return l != nil && l["sr"] != nil && num(isisStatus(t, b), "lsdb_total") == 4
	})
	st := isisStatus(t, b)
	la := lspOf(st, "0000.0000.000a")
	sr := la["sr"].(map[string]any)
	if g := sr["srgb"].([]any)[0].(map[string]any); num(g, "base") != 16000 || num(g, "size") != 8000 || sr["router_id"] != "10.0.0.1" {
		t.Fatalf("a's SR capability %v", sr)
	}
	sid := func(l map[string]any, prefix string) map[string]any {
		for _, x := range l["prefixes"].([]any) {
			if m := x.(map[string]any); m["prefix"] == prefix {
				s, _ := m["sid"].(map[string]any)
				return s
			}
		}
		return nil
	}
	if s := sid(la, "192.0.2.10/32"); s == nil || num(s, "index") != 5 || s["flags"] != "N" {
		t.Fatalf("a's node SID %v", s)
	}
	adj := func(l map[string]any, nbr string) []any {
		for _, x := range l["neighbors"].([]any) {
			if m := x.(map[string]any); m["id"] == nbr {
				a, _ := m["adj_sids"].([]any)
				return a
			}
		}
		return nil
	}
	// a's adjacencies: b and r1 (the attachment), labels from 24000; IPv6
	// too since a has IPv6 in a single topology.
	if x := adj(la, "0000.0000.000b.00"); len(x) != 2 || num(x[0].(map[string]any), "label") < 24000 {
		t.Fatalf("a → b adj SIDs %v", x)
	}
	if x := adj(la, "0000.1000.0001.00"); len(x) == 0 {
		t.Fatal("no adj SID toward the simulated router")
	}
	r2 := lspOf(st, "0000.1000.0002")
	if s := sid(r2, "10.255.0.2/32"); s == nil || num(s, "index") != 101 {
		t.Fatalf("r2 node SID %v", s)
	}
	if x := adj(r2, "0000.1000.0001.00"); len(x) != 1 || num(x[0].(map[string]any), "label") != 24000 {
		t.Fatalf("r2's own label space %v", x)
	}
	// Status summarises what a advertises.
	ss, _ := isisStatus(t, a)["segment_routing"].(map[string]any)
	if ss == nil || len(ss["node_sids"].([]any)) != 1 || num(ss["node_sids"].([]any)[0].(map[string]any), "label") != 16005 {
		t.Fatalf("SR status %v", ss)
	}
	// Invalid SR configs are rejected.
	for _, bad := range []string{
		`{"segment_routing":{"enabled":true,"srgb_base":16000,"srgb_size":10,"node_sids":[{"prefix":"192.0.2.1/32","index":10}]}}`,
		`{"segment_routing":{"enabled":true,"srgb_base":16000,"srgb_size":8000,"adj_sid_base":20000}}`,
		`{"segment_routing":{"enabled":true,"srgb_base":16000,"srgb_size":100,"adj_sid_base":24000},"topology":{"kind":"line","nodes":5,"loopback_base":"10.9.0.1/32","node_sid_base":98}}`,
	} {
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "isis", Device: "d", Config: json.RawMessage(bad)}); err == nil {
			a.DeleteProtocol("x")
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestISISKeychainRollover(t *testing.T) {
	var switchAt string
	chain := func(withKey2 bool) string {
		keys := `{"id":1,"type":"hmac-md5","key":"old","send_until":"` + switchAt + `"}`
		if withKey2 {
			keys += `,{"id":2,"type":"hmac-sha256","key":"new","send_from":"` + switchAt + `"}`
		}
		return `{"type":"keychain","keys":[` + keys + `]}`
	}
	for _, c := range []struct {
		name     string
		bHasKey2 bool
	}{{"both have key 2", true}, {"b lacks key 2", false}} {
		t.Run(c.name, func(t *testing.T) {
			// RFC 3339 has whole seconds: switch 2-3 s from now.
			switchAt = time.Now().Add(3 * time.Second).Truncate(time.Second).UTC().Format(time.RFC3339)
			a, b, _, _ := ipPair(t)
			for _, x := range []struct {
				e  *core.Engine
				id string
				k2 bool
			}{{a, "000a", true}, {b, "000b", c.bHasKey2}} {
				cfg := fmt.Sprintf(`{"system_id":"0000.0000.%s","hello_interval":1,"hello_multiplier":2,"csnp_interval":1,"hello_auth":%s,"lsp_auth":%s}`, x.id, chain(x.k2), chain(x.k2))
				if _, err := x.e.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
					t.Fatal(err)
				}
			}
			up := func(e *core.Engine) bool {
				adj, _ := isisStatus(t, e)["adjacency"].(map[string]any)
				return adj["state"] == "up"
			}
			eventually(t, "up on key 1", func() bool { return up(a) && up(b) && lspOf(isisStatus(t, b), "0000.0000.000a") != nil })
			seq := num(lspOf(isisStatus(t, b), "0000.0000.000a"), "seq")
			if st := isisStatus(t, a); !strings.Contains(st["hello_auth"].(string), "sending key 1") {
				t.Fatalf("before: %v", st["hello_auth"])
			}
			time.Sleep(time.Until(mustTime(t, switchAt)) + 500*time.Millisecond)
			if !strings.Contains(isisStatus(t, a)["hello_auth"].(string), "sending key 2") {
				t.Fatalf("after: %v", isisStatus(t, a)["hello_auth"])
			}
			if c.bHasKey2 {
				// Re-signed LSPs reach b (new sequence), no failures, still up.
				eventually(t, "re-signed LSP at b", func() bool { return num(lspOf(isisStatus(t, b), "0000.0000.000a"), "seq") > seq })
				time.Sleep(1500 * time.Millisecond)
				cs, _ := isisStatus(t, b)["counters"].(map[string]any)
				for k, v := range cs {
					if strings.HasPrefix(k, "rx_auth_failures") && v.(float64) > 0 {
						t.Fatalf("%s = %v", k, v)
					}
				}
				if !up(a) || !up(b) {
					t.Fatal("adjacency dropped during rollover")
				}
				return
			}
			// b can't verify key 2: failures, then the adjacency drops.
			eventually(t, "b rejects key 2", func() bool {
				cs, _ := isisStatus(t, b)["counters"].(map[string]any)
				return num(cs, "rx_auth_failures_hello") > 0
			})
			eventually(t, "adjacency drops", func() bool { return !up(b) })
		})
	}
}

func mustTime(t *testing.T, s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A router with two interfaces (point-to-point toward a, a LAN toward c,
// on VLANs of one segment) floods between them and advertises both.
func TestISISMultipleInterfaces(t *testing.T) {
	h := &hub{}
	eng := func() *core.Engine {
		p := &hubPort{h: h}
		e := core.New(p, events.NewBus(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		p.eng = e
		h.mu.Lock()
		h.ports = append(h.ports, p)
		h.mu.Unlock()
		return e
	}
	dev := func(e *core.Engine, name, mac string, vid uint16, addr string, gw ...string) {
		t.Helper()
		spec := core.DeviceSpec{Name: name, Port: "eth1", MAC: mac, VLANs: []pkt.VLANTag{{VID: vid}}, Addresses: []string{addr}}
		if len(gw) > 0 {
			spec.Gateway4 = gw[0]
		}
		if _, err := e.CreateDevice(spec); err != nil {
			t.Fatal(err)
		}
	}
	proto := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	a, b, c := eng(), eng(), eng()
	dev(a, "d", "02:00:00:00:0a:01", 10, "10.0.0.1/24", "10.0.0.2")
	dev(b, "d", "02:00:00:00:0b:01", 10, "10.0.0.2/24")
	dev(b, "d2", "02:00:00:00:0b:02", 20, "10.2.0.2/24")
	dev(c, "d", "02:00:00:00:0c:01", 20, "10.2.0.3/24", "10.2.0.2")
	proto(a, `{"system_id":"0000.0000.000a","hostname":"ra","hello_interval":1,"csnp_interval":1}`)
	proto(b, `{"system_id":"0000.0000.000b","hostname":"rb","hello_interval":1,"csnp_interval":1,"loopbacks":["192.0.2.11"],
		"interfaces":[{"device":"d2","circuit":"broadcast","metric":30,"priority":100}]}`)
	proto(c, `{"system_id":"0000.0000.000c","hostname":"rc","circuit":"broadcast","hello_interval":1,"csnp_interval":1,
		"prefixes":[{"prefix":"198.51.100.0/24"}]}`)

	eventually(t, "both adjacencies up", func() bool { return upCount(isisStatus(t, b)) == 2 })
	eventually(t, "c's LSP reaches a through b", func() bool {
		l := lspOf(isisStatus(t, a), "0000.0000.000c")
		return l != nil && prefixes(l)["198.51.100.0/24"]
	})
	eventually(t, "a's LSP reaches c through b", func() bool { return lspOf(isisStatus(t, c), "0000.0000.000a") != nil })
	st := isisStatus(t, b)
	ifs, _ := st["interfaces"].([]any)
	if len(ifs) != 2 || ifs[1].(map[string]any)["name"] != "d2" || ifs[1].(map[string]any)["attached"] != true || ifs[1].(map[string]any)["circuit"] != "broadcast" {
		t.Fatalf("interfaces %v", ifs)
	}
	if dis, lanID := lanDIS(st); dis != "0000.0000.000b" || lanID != "0000.0000.000b.02" {
		t.Fatalf("DIS %s, LAN ID %s (b's second circuit has pseudonode 2)", dis, lanID)
	}
	eventually(t, "b's LSP lists both circuits", func() bool {
		l := lspOf(isisStatus(t, a), "0000.0000.000b")
		p := prefixes(l)
		return l != nil && fmt.Sprint(lspNeighbors(l)) == "[0000.0000.000a.00 0000.0000.000b.02]" && p["10.0.0.0/24"] && p["10.2.0.0/24"] && p["192.0.2.11/32"]
	})
	// The loopback: router ID, metric 0, answered on both interfaces.
	if l := lspOf(isisStatus(t, a), "0000.0000.000b"); l["router_id"] != "192.0.2.11" {
		t.Fatalf("router ID %v", l["router_id"])
	} else {
		for _, x := range l["prefixes"].([]any) {
			if m := x.(map[string]any); m["prefix"] == "192.0.2.11/32" && num(m, "metric") != 0 {
				t.Fatalf("loopback metric %v", m["metric"])
			}
		}
	}
	for _, e := range []*core.Engine{a, c} {
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "ping", Type: "icmp", Device: "d", Config: json.RawMessage(`{"destination":"192.0.2.11","rate":50,"auto_start":true}`)}); err != nil {
			t.Fatal(err)
		}
		eventually(t, "loopback answers", func() bool { return num(flowStatus(t, e, "ping"), "rx") >= 5 })
		if err := e.DeleteProtocol("ping"); err != nil {
			t.Fatal(err)
		}
	}
	pn := func(e *core.Engine) []string {
		for _, l := range lsdb(isisStatus(t, e)) {
			if l["lsp_id"] == "0000.0000.000b.02-00" {
				return lspNeighbors(l)
			}
		}
		return nil
	}
	if got := fmt.Sprint(pn(b)); got != "[0000.0000.000b.00 0000.0000.000c.00]" {
		t.Fatalf("pseudonode %s", got)
	}
	eventually(t, "a has b's pseudonode", func() bool { return len(pn(a)) == 2 })

	// A per-interface metric change touches that interface only.
	act(t, b, "metric", `{"metric":55,"interface":"d2"}`)
	eventually(t, "metric", func() bool {
		l := lspOf(isisStatus(t, a), "0000.0000.000b")
		ms := map[string]float64{}
		nb, _ := l["neighbors"].([]any)
		for _, x := range nb {
			ms[x.(map[string]any)["id"].(string)] = num(x.(map[string]any), "metric")
		}
		return ms["0000.0000.000b.02"] == 55 && ms["0000.0000.000a.00"] == 10
	})
	if _, err := b.RunAction(context.Background(), "isis", "metric", json.RawMessage(`{"metric":5,"interface":"nope"}`)); err == nil {
		t.Fatal("metric on an unknown interface")
	}

	// Flapping one interface leaves the other up.
	act(t, b, "flap", `{"seconds":1,"mode":"reset","interface":"d"}`)
	eventually(t, "a drops", func() bool { return upCount(isisStatus(t, a)) == 0 })
	if upCount(isisStatus(t, c)) != 1 {
		t.Fatal("LAN adjacency dropped by flapping the other interface")
	}
	eventuallyLong(t, "a back", func() bool { return upCount(isisStatus(t, a)) == 1 })

	// Deleting the device detaches the interface; recreating it re-attaches.
	if err := b.DeleteDevice("d2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "detached", func() bool {
		st := isisStatus(t, b)
		ifs, _ := st["interfaces"].([]any)
		return ifs[1].(map[string]any)["attached"] == false && upCount(st) == 1
	})
	eventually(t, "b's LSP without the LAN", func() bool {
		l := lspOf(isisStatus(t, a), "0000.0000.000b")
		return fmt.Sprint(lspNeighbors(l)) == "[0000.0000.000a.00]" && !prefixes(l)["10.2.0.0/24"]
	})
	dev(b, "d2", "02:00:00:00:0b:02", 20, "10.2.0.2/24")
	eventually(t, "re-attached", func() bool { return upCount(isisStatus(t, b)) == 2 })
	eventually(t, "c's LSP still at a", func() bool { return lspOf(isisStatus(t, a), "0000.0000.000c") != nil })
}

// SR extensions: SRLB, mapping server, flex-algo (definition, link
// attributes, per-algorithm SIDs) and SRv6 (locators, End/End.X SIDs), for
// us and simulated routers, as the peer decodes them.
func TestISISSRExtensions(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mk := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	mk(a, `{"system_id":"0000.0000.000a","hello_interval":1,"csnp_interval":1,"topologies":["ipv6-unicast"],"delay_us":700,"admin_groups":[3],
		"segment_routing":{"enabled":true,"srgb_base":16000,"srgb_size":8000,"adj_sid_base":24000,"srlb_base":15000,"srlb_size":1000,
			"node_sids":[{"prefix":"192.0.2.10/32","index":5},{"prefix":"192.0.2.10/32","index":205,"algorithm":128}],
			"mappings":[{"prefix":"10.254.0.1/32","range":4,"index":900}]},
		"flex_algos":[{"algorithm":128,"metric_type":"delay","exclude_any":[5]},{"algorithm":129,"participate_only":true}],
		"srv6":{"enabled":true,"end_x":true,"behavior":"un","topology_locator_base":"fc00:0:100::/48",
			"locators":[{"prefix":"fc00:0:a::/48"},{"prefix":"fc00:80:a::/48","algorithm":128}]},
		"topology":{"kind":"line","nodes":2,"loopback_base":"10.255.0.1/32","node_sid_base":100,"link_delay_us":1000,
			"flex_algo_sids":[{"algorithm":128,"node_sid_base":300}]}}`)
	mk(b, `{"system_id":"0000.0000.000b","hello_interval":1,"csnp_interval":1,"topologies":["ipv6-unicast"]}`)
	eventually(t, "LSPs at b", func() bool {
		l := lspOf(isisStatus(t, b), "0000.0000.000a")
		return l != nil && l["sr"] != nil && num(isisStatus(t, b), "lsdb_total") == 4
	})
	st := isisStatus(t, b)
	la := lspOf(st, "0000.0000.000a")
	sr := la["sr"].(map[string]any)
	if fmt.Sprint(sr["algorithms"]) != "[0 128 129]" || sr["srv6"] != true || fmt.Sprint(sr["srlb"]) != "[map[base:15000 size:1000]]" {
		t.Fatalf("a's router capability %v", sr)
	}
	if f := sr["flex_algo_definitions"].([]any); len(f) != 1 || fmt.Sprint(f[0]) != "map[algorithm:128 exclude_any:32 metric_type:delay priority:128]" {
		t.Fatalf("FADs %v", f)
	}
	if fmt.Sprint(la["sid_bindings"]) != "[map[index:900 prefix:10.254.0.1/32 range:4]]" {
		t.Fatalf("bindings %v", la["sid_bindings"])
	}
	locs := fmt.Sprint(la["srv6_locators"])
	if !strings.Contains(locs, "locator:fc00:0:a::/48") || !strings.Contains(locs, "end_sids:[map[behavior:un sid:fc00:0:a::]]") || !strings.Contains(locs, "algorithm:128") {
		t.Fatalf("locators %s", locs)
	}
	if p := prefixes(la); !p["fc00:0:a::/48"] || p["fc00:80:a::/48"] {
		t.Fatalf("algorithm-0 locator is a prefix too, the flex-algo one isn't: %v", p)
	}
	var toB map[string]any
	for _, x := range la["neighbors"].([]any) {
		if m := x.(map[string]any); m["id"] == "0000.0000.000b.00" {
			if num(m, "mt") == 2 {
				if x := fmt.Sprint(m["end_x_sids"]); !strings.HasPrefix(x, "[map[behavior:ua sid:fc00:0:a:e00") {
					t.Fatalf("End.X toward b %v", m["end_x_sids"])
				}
			} else {
				toB = m
			}
		}
	}
	if toB == nil || fmt.Sprint(toB["link_attributes"]) != "map[admin_groups:8 delay_us:700 flex_algo:true]" {
		t.Fatalf("a → b link attributes %v", toB)
	}
	for _, x := range la["prefixes"].([]any) {
		if m := x.(map[string]any); m["prefix"] == "192.0.2.10/32" && (fmt.Sprint(m["flex_sids"]) != "[map[algorithm:128 flags:N index:205]]" || num(m["sid"].(map[string]any), "index") != 5) {
			t.Fatalf("per-algorithm SIDs %v", m)
		}
	}
	// Simulated routers: flex-algo SIDs, link delay, their own locators.
	r2 := lspOf(st, "0000.1000.0002")
	if fmt.Sprint(r2["sr"].(map[string]any)["algorithms"]) != "[0 128 129]" || r2["sr"].(map[string]any)["flex_algo_definitions"] != nil {
		t.Fatalf("r2 capability %v", r2["sr"])
	}
	if !strings.Contains(fmt.Sprint(r2["srv6_locators"]), "locator:fc00:0:101::/48") {
		t.Fatalf("r2 locator %v", r2["srv6_locators"])
	}
	ok := false
	for _, x := range r2["prefixes"].([]any) {
		if m := x.(map[string]any); m["prefix"] == "10.255.0.2/32" {
			ok = fmt.Sprint(m["flex_sids"]) == "[map[algorithm:128 flags:N index:301]]"
		}
	}
	if !ok {
		t.Fatalf("r2 flex-algo SID %v", r2["prefixes"])
	}
	if !strings.Contains(fmt.Sprint(r2["neighbors"]), "delay_us:1000") {
		t.Fatalf("simulated link delay %v", r2["neighbors"])
	}
	// The delay changes at runtime.
	act(t, a, "topology-link", `{"a":"r1","b":"r2","up":true,"delay_us":4000}`)
	eventually(t, "new delay", func() bool {
		return strings.Contains(fmt.Sprint(lspOf(isisStatus(t, b), "0000.1000.0002")["neighbors"]), "delay_us:4000")
	})
	ss := isisStatus(t, a)["segment_routing"].(map[string]any)
	if len(ss["srv6_sids"].([]any)) < 3 || len(ss["flex_algos"].([]any)) != 2 || ss["srlb"] == nil || len(ss["mappings"].([]any)) != 1 {
		t.Fatalf("status %v", ss)
	}
	for _, bad := range []string{
		`{"segment_routing":{"enabled":true,"srgb_base":16000,"srgb_size":8000,"node_sids":[{"prefix":"192.0.2.1/32","index":1,"algorithm":130}]}}`,
		`{"flex_algos":[{"algorithm":12}]}`,
		`{"segment_routing":{"enabled":true,"srgb_base":16000,"srgb_size":8000,"srlb_base":17000,"srlb_size":100}}`,
		`{"segment_routing":{"enabled":true,"srgb_base":16000,"srgb_size":8000,"node_sids":[{"prefix":"192.0.2.1/32","index":3}],"mappings":[{"prefix":"10.0.0.1/32","range":5,"index":1}]}}`,
		`{"srv6":{"enabled":true,"locators":[{"prefix":"10.0.0.0/8"}]}}`,
		`{"srv6":{"enabled":true,"locators":[{"prefix":"fc00::/48","end_sid":"fc01::1"}]}}`,
	} {
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "isis", Device: "d", Config: json.RawMessage(bad)}); err == nil {
			a.DeleteProtocol("x")
			t.Errorf("accepted %s", bad)
		}
	}
}

// Config edits that only change what a router advertises apply in place:
// the neighbor sees new LSPs while the adjacency stays up; a new hello
// interval restarts the instance (the adjacency re-forms).
func TestISISReconfigure(t *testing.T) {
	a, b, _, _ := ipPair(t)
	base := `"system_id":"0000.0000.000a","hostname":"ra","hello_interval":1,"csnp_interval":1`
	if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(`{` + base + `,
		"prefixes":[{"prefix":"198.51.100.0/24"}],
		"topology":{"kind":"grid","rows":3,"cols":3,"loopback_base":"10.255.0.1/32"}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(`{"system_id":"0000.0000.000b","hello_interval":1,"csnp_interval":1}`)}); err != nil {
		t.Fatal(err)
	}
	sim := func(i int) string { return fmt.Sprintf("0000.1000.%04x", i) }
	adj := func(e *core.Engine) map[string]any {
		l, _ := isisStatus(t, e)["adjacencies"].([]any)
		if len(l) == 0 {
			return map[string]any{}
		}
		return l[0].(map[string]any)
	}
	eventuallyLong(t, "up with a's LSPs at b", func() bool {
		st := isisStatus(t, b)
		return upCount(st) == 1 && prefixes(lspOf(st, "0000.0000.000a"))["198.51.100.0/24"] && lspOf(st, sim(9)) != nil
	})
	sinceA, sinceB := adj(a)["since"], adj(b)["since"]
	update := func(cfg string) {
		t.Helper()
		if _, err := a.UpdateProtocol("isis", json.RawMessage(`{`+base+cfg+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	kept := func(what string) {
		t.Helper()
		for _, x := range []struct {
			e     *core.Engine
			since any
		}{{a, sinceA}, {b, sinceB}} {
			if s := adj(x.e); s["state"] != "up" || s["since"] != x.since {
				t.Fatalf("%s: the adjacency was reset: %v", what, s)
			}
		}
	}

	// New prefixes, a loopback, a metric and a smaller simulated topology.
	update(`,"metric":30,"loopbacks":["192.0.2.1/32"],"prefixes":[{"prefix":"203.0.113.0/24"}],
		"topology":{"kind":"grid","rows":2,"cols":2,"loopback_base":"10.255.0.1/32"}`)
	eventuallyLong(t, "the new LSPs at b", func() bool {
		st := isisStatus(t, b)
		p := prefixes(lspOf(st, "0000.0000.000a"))
		gone := lspOf(st, sim(9))
		return p["203.0.113.0/24"] && p["192.0.2.1/32"] && !p["198.51.100.0/24"] && (gone == nil || num(gone, "lifetime") == 0)
	})
	if l := lspOf(isisStatus(t, b), "0000.0000.000a"); !strings.Contains(fmt.Sprint(l["neighbors"]), "30") {
		t.Fatalf("metric 30 not advertised: %v", l["neighbors"])
	}
	kept("prefixes, loopback, metric and topology")

	// A new hello interval changes the adjacency: the instance restarts
	// (its adjacency is new; b may keep its side up through the handshake).
	// since has whole seconds: let one pass first.
	time.Sleep(1100 * time.Millisecond)
	update(`,"hello_interval":2`)
	eventuallyLong(t, "a's adjacency re-formed", func() bool {
		s := adj(a)
		return s["state"] == "up" && s["since"] != sinceA
	})
}
