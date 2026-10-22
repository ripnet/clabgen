package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/events"
	"github.com/ripnet/clabgen/internal/pkt"
)

// lspAt returns sysid's fragment 0 at a level as seen in st's LSDB.
func lspAt(st map[string]any, sysid string, level int) map[string]any {
	for _, l := range lsdb(st) {
		if l["lsp_id"] == sysid+".00-00" && num(l, "level") == float64(level) {
			return l
		}
	}
	return nil
}

// prefixInfo returns one prefix entry of an LSP.
func prefixInfo(l map[string]any, p string) map[string]any {
	ps, _ := l["prefixes"].([]any)
	for _, x := range ps {
		if m := x.(map[string]any); m["prefix"] == p {
			return m
		}
	}
	return nil
}

// hubEngines makes n engines on one shared segment (links by VLAN).
func hubEngines(n int) []*core.Engine {
	h := &hub{}
	var out []*core.Engine
	for range n {
		p := &hubPort{h: h}
		e := core.New(p, events.NewBus(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		p.eng = e
		h.ports = append(h.ports, p)
		out = append(out, e)
	}
	return out
}

func hubDev(t *testing.T, e *core.Engine, name, mac string, vid uint16, addrs ...string) {
	t.Helper()
	if _, err := e.CreateDevice(core.DeviceSpec{Name: name, Port: "eth1", MAC: mac, VLANs: []pkt.VLANTag{{VID: vid}}, Addresses: addrs}); err != nil {
		t.Fatal(err)
	}
}

func mkISIS(t *testing.T, e *core.Engine, cfg string) {
	t.Helper()
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "isis", Type: "isis", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

// An L1 router (a) behind an L1L2 router (b) with an L2 router (c) of
// another area: b carries a's prefixes into level 2 (summarised, SIDs
// re-advertised), leaks what its ranges cover into level 1 with the down
// bit, and sets the attached bit.
func TestISISLevels(t *testing.T) {
	es := hubEngines(3)
	a, b, c := es[0], es[1], es[2]
	hubDev(t, a, "d", "02:00:00:00:0a:01", 10, "10.0.0.1/24")
	hubDev(t, b, "d", "02:00:00:00:0b:01", 10, "10.0.0.2/24")
	hubDev(t, b, "d2", "02:00:00:00:0b:02", 20, "10.2.0.2/24")
	hubDev(t, c, "d", "02:00:00:00:0c:01", 20, "10.2.0.3/24")
	mkISIS(t, a, `{"system_id":"0000.0000.000a","hostname":"ra","level":"L1","areas":["49.0002"],"hello_interval":1,"csnp_interval":1,
		"loopbacks":["192.0.2.1"],"prefixes":[{"prefix":"198.51.100.0/24","metric":5},{"prefix":"198.51.101.0/24","metric":8}],
		"segment_routing":{"enabled":true,"srgb_base":16000,"srgb_size":8000,"node_sids":[{"prefix":"192.0.2.1/32","index":1}]}}`)
	mkISIS(t, b, `{"system_id":"0000.0000.000b","hostname":"rb","level":"L1L2","areas":["49.0002"],"hello_interval":1,"csnp_interval":1,
		"interfaces":[{"device":"d2"}],"summaries":[{"prefix":"198.51.100.0/23"}],"leak_to_l1":["203.0.113.0/24"],
		"segment_routing":{"enabled":true,"srgb_base":16000,"srgb_size":8000}}`)
	mkISIS(t, c, `{"system_id":"0000.0000.000c","hostname":"rc","level":"L2","areas":["49.0003"],"hello_interval":1,"csnp_interval":1,
		"prefixes":[{"prefix":"203.0.113.0/24","metric":7},{"prefix":"203.0.114.0/24"}]}`)

	// Level 1 into level 2: the summary (lowest covered metric: 10 + 5)
	// instead of a's two prefixes, and a's node SID with R and P.
	eventuallyLong(t, "b's level-2 LSP carries level 1", func() bool {
		l := lspAt(isisStatus(t, c), "0000.0000.000b", 2)
		return l != nil && prefixInfo(l, "198.51.100.0/23") != nil && prefixInfo(l, "192.0.2.1/32") != nil
	})
	l2 := lspAt(isisStatus(t, c), "0000.0000.000b", 2)
	if m := num(prefixInfo(l2, "198.51.100.0/23"), "metric"); m != 15 {
		t.Errorf("summary metric %v, want 15", m)
	}
	if prefixInfo(l2, "198.51.100.0/24") != nil || prefixInfo(l2, "198.51.101.0/24") != nil {
		t.Errorf("summarised prefixes advertised: %v", l2["prefixes"])
	}
	sid, _ := prefixInfo(l2, "192.0.2.1/32")["sid"].(map[string]any)
	if sid == nil || num(sid, "index") != 1 || sid["flags"] != "RNP" {
		t.Errorf("re-advertised SID %v", sid)
	}
	if l2["attached"] == true {
		t.Error("attached bit in a level-2 LSP")
	}

	// Level 2 into level 1: the leak range only, with the down bit; ATT
	// set since c is in another area.
	eventuallyLong(t, "b's level-1 LSP leaks", func() bool {
		l := lspAt(isisStatus(t, a), "0000.0000.000b", 1)
		return l != nil && prefixInfo(l, "203.0.113.0/24") != nil && l["attached"] == true
	})
	l1 := lspAt(isisStatus(t, a), "0000.0000.000b", 1)
	leak := prefixInfo(l1, "203.0.113.0/24")
	if leak["down"] != true || num(leak, "metric") != 17 {
		t.Errorf("leaked prefix %v", leak)
	}
	if prefixInfo(l1, "203.0.114.0/24") != nil {
		t.Error("prefix outside the leak range leaked")
	}
	eventuallyLong(t, "a lists b as attached router", func() bool {
		il, _ := isisStatus(t, a)["inter_level"].(map[string]any)
		rs, _ := il["attached_routers"].([]any)
		return len(rs) == 1 && rs[0] == "0000.0000.000b"
	})
	ilb, _ := isisStatus(t, b)["inter_level"].(map[string]any)
	if ilb["attached"] != true || num(ilb, "to_l2") < 2 || num(ilb, "leaked_to_l1") != 1 {
		t.Errorf("b's inter-level status: %v", ilb)
	}

	// Changes follow: c withdraws the leaked prefix; a withdraws one of
	// the summarised ones (the summary stays, at the other's metric).
	act(t, c, "withdraw", `{"prefixes":[{"prefix":"203.0.113.0/24"}]}`)
	act(t, a, "withdraw", `{"prefixes":[{"prefix":"198.51.100.0/24"}]}`)
	eventuallyLong(t, "leak withdrawn, summary re-costed", func() bool {
		l1 := lspAt(isisStatus(t, a), "0000.0000.000b", 1)
		l2 := lspAt(isisStatus(t, c), "0000.0000.000b", 2)
		return l1 != nil && prefixInfo(l1, "203.0.113.0/24") == nil && l2 != nil && num(prefixInfo(l2, "198.51.100.0/23"), "metric") == 18
	})
}

func TestISISBFD(t *testing.T) {
	a, b, _, _ := ipPair(t)
	cfg := `{"system_id":"0000.0000.00%s","hello_interval":3,"csnp_interval":2,"bfd":{"enabled":true,"min_tx_ms":300,"min_rx_ms":300,"families":["ipv4","ipv6"],"tlv":true}}`
	mkISIS(t, a, fmt.Sprintf(cfg, "0a"))
	mkISIS(t, b, fmt.Sprintf(cfg, "0b"))
	bfdUp := func(e *core.Engine) bool {
		adj, _ := isisStatus(t, e)["adjacency"].(map[string]any)
		ss, _ := adj["bfd"].([]any)
		if adj["state"] != "up" || len(ss) != 2 {
			return false
		}
		for _, x := range ss {
			if x.(map[string]any)["state"] != "up" {
				return false
			}
		}
		return true
	}
	eventuallyLong(t, "adjacency and both BFD sessions up", func() bool { return bfdUp(a) && bfdUp(b) })
	// Both sent the BFD-enabled TLV, so the adjacency waited for BFD.
	if c, _ := isisStatus(t, a)["counters"].(map[string]any); num(c, "adj_waiting_bfd") == 0 {
		t.Errorf("adjacency didn't wait for BFD: %v", c)
	}

	// Silenced BFD on a: b drops the adjacency well before the 9 s hold.
	t0 := time.Now()
	act(t, a, "bfd-fail", `{"seconds":2}`)
	eventuallyLong(t, "b drops the adjacency", func() bool {
		adj, _ := isisStatus(t, b)["adjacency"].(map[string]any)
		return adj["state"] != "up"
	})
	if d := time.Since(t0); d > 4*time.Second {
		t.Errorf("adjacency dropped after %s", d)
	}
	if c, _ := isisStatus(t, b)["counters"].(map[string]any); num(c, "bfd_down") == 0 {
		t.Errorf("counters %v", c)
	}
	eventuallyLong(t, "back up after the silence", func() bool { return bfdUp(a) && bfdUp(b) })
}

func TestISISBFDOneSided(t *testing.T) {
	// Only a runs BFD: b sends no BFD-enabled TLV, so the adjacency comes
	// up without BFD; a's sessions stay down without bringing it down.
	a, b, _, _ := ipPair(t)
	mkISIS(t, a, `{"system_id":"0000.0000.000a","hello_interval":1,"bfd":{"enabled":true,"min_tx_ms":300,"min_rx_ms":300}}`)
	mkISIS(t, b, `{"system_id":"0000.0000.000b","hello_interval":1}`)
	up := func(e *core.Engine) bool {
		adj, _ := isisStatus(t, e)["adjacency"].(map[string]any)
		return adj["state"] == "up"
	}
	eventuallyLong(t, "adjacency up", func() bool { return up(a) && up(b) })
	time.Sleep(1500 * time.Millisecond)
	if !up(a) || !up(b) {
		t.Fatal("adjacency dropped")
	}
	if c, _ := isisStatus(t, b)["counters"].(map[string]any); num(c, "rx_bfd_unknown") != 0 || num(c, "rx_bfd") != 0 {
		t.Errorf("b handled BFD: %v", c)
	}
}

func TestISISGracefulRestartHelper(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mkISIS(t, a, `{"system_id":"0000.0000.000a","hello_interval":1,"csnp_interval":5}`)
	mkISIS(t, b, `{"system_id":"0000.0000.000b","hello_interval":1,"csnp_interval":5,"prefixes":[{"prefix":"198.51.100.0/24","count":50}]}`)
	eventuallyLong(t, "a has b's LSP", func() bool { return allPrefixes(isisStatus(t, a), "0000.0000.000b") >= 50 })
	transitions := func(e *core.Engine) float64 {
		adj, _ := isisStatus(t, e)["adjacency"].(map[string]any)
		return num(adj, "transitions")
	}
	tb := transitions(b)

	act(t, a, "graceful-restart", `{"seconds":30}`)
	gr := func() map[string]any {
		g, _ := isisStatus(t, a)["graceful_restart"].(map[string]any)
		return g
	}
	if gr()["restarting"] == nil {
		t.Fatal("not restarting")
	}
	eventuallyLong(t, "restart completes", func() bool {
		c, _ := isisStatus(t, a)["counters"].(map[string]any)
		return gr()["restarting"] == nil && num(c, "gr_completed") == 1
	})
	// b helped and kept the adjacency; a relearned b's LSP.
	eventuallyLong(t, "b's help ends", func() bool {
		c, _ := isisStatus(t, b)["counters"].(map[string]any)
		return num(c, "gr_helper") == 1 && num(c, "gr_helper_done") == 1
	})
	if transitions(b) != tb {
		t.Errorf("b's adjacency went down: %v → %v transitions", tb, transitions(b))
	}
	if n := allPrefixes(isisStatus(t, a), "0000.0000.000b"); n < 50 {
		t.Errorf("a relearned %d of b's prefixes", n)
	}
	if _, err := a.RunAction(context.Background(), "isis", "graceful-restart", json.RawMessage(`{}`)); err != nil {
		t.Errorf("second restart: %v", err)
	}
}

func TestISISGracefulRestartNoHelper(t *testing.T) {
	// b doesn't help: no RA, so a's restart runs into its hold time.
	a, b, _, _ := ipPair(t)
	mkISIS(t, a, `{"system_id":"0000.0000.000a","hello_interval":1,"csnp_interval":1}`)
	mkISIS(t, b, `{"system_id":"0000.0000.000b","hello_interval":1,"csnp_interval":1,"graceful_restart":{"helper":false}}`)
	eventuallyLong(t, "synced", func() bool { return lspOf(isisStatus(t, a), "0000.0000.000b") != nil })
	act(t, a, "graceful-restart", `{"seconds":2}`)
	eventuallyLong(t, "restart times out", func() bool {
		c, _ := isisStatus(t, a)["counters"].(map[string]any)
		return num(c, "gr_timed_out") == 1
	})
}

func TestISISOverloadOnStartup(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mkISIS(t, a, `{"system_id":"0000.0000.000a","hello_interval":1,"overload_on_startup_s":2}`)
	mkISIS(t, b, `{"system_id":"0000.0000.000b","hello_interval":1}`)
	eventuallyLong(t, "b sees a overloaded", func() bool {
		l := lspOf(isisStatus(t, b), "0000.0000.000a")
		return l != nil && l["overload"] == true
	})
	if isisStatus(t, a)["overload_on_startup_until"] == nil {
		t.Error("no end time in status")
	}
	eventuallyLong(t, "overload cleared", func() bool {
		l := lspOf(isisStatus(t, b), "0000.0000.000a")
		return l != nil && l["overload"] != true
	})
}

func TestISISLANBFDAndRestart(t *testing.T) {
	es := hubEngines(2)
	a, b := es[0], es[1]
	hubDev(t, a, "d", "02:00:00:00:0a:01", 10, "10.0.0.1/24")
	hubDev(t, b, "d", "02:00:00:00:0b:01", 10, "10.0.0.2/24")
	cfg := `{"system_id":"0000.0000.00%s","circuit":"broadcast","hello_interval":3,"csnp_interval":1,"priority":%d,
		"bfd":{"enabled":true,"min_tx_ms":300,"min_rx_ms":300,"tlv":true}}`
	mkISIS(t, a, fmt.Sprintf(cfg, "0a", 10))
	mkISIS(t, b, fmt.Sprintf(cfg, "0b", 100))
	lanBFDUp := func(e *core.Engine) bool {
		as, _ := isisStatus(t, e)["adjacencies"].([]any)
		for _, x := range as {
			m := x.(map[string]any)
			ss, _ := m["bfd"].([]any)
			if m["state"] == "up" && len(ss) == 1 && ss[0].(map[string]any)["state"] == "up" {
				return true
			}
		}
		return false
	}
	eventuallyLong(t, "LAN adjacency and BFD up", func() bool { return lanBFDUp(a) && lanBFDUp(b) })

	// a restarts gracefully: b (the DIS) helps, the adjacency stays.
	act(t, a, "graceful-restart", `{"seconds":20}`)
	eventuallyLong(t, "restart completes", func() bool {
		c, _ := isisStatus(t, a)["counters"].(map[string]any)
		return num(c, "gr_completed") == 1
	})
	if c, _ := isisStatus(t, b)["counters"].(map[string]any); num(c, "gr_helper") != 1 {
		t.Errorf("b didn't help: %v", c)
	}
	if upCount(isisStatus(t, b)) != 1 {
		t.Error("b lost the adjacency")
	}

	// BFD failure drops the LAN adjacency at once (hold 9 s, DIS 3 s).
	act(t, a, "bfd-fail", `{"seconds":2}`)
	eventuallyLong(t, "b drops a", func() bool { return upCount(isisStatus(t, b)) == 0 })
	eventuallyLong(t, "back", func() bool { return lanBFDUp(a) && lanBFDUp(b) })
}
