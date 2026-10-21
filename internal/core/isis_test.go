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
	eventually(t, "back up after the flap", func() bool { return up(a) && up(b) })

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
