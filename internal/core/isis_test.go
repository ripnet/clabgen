package core_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	_ "github.com/ripnet/clabgen/protocols/isis"
)

func isisStatus(t *testing.T, e *core.Engine) map[string]any { t.Helper(); return flowStatus(t, e, "isis") }

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
	eventually(t, "LSDBs synced", func() bool { return lspOf(isisStatus(t, b), "0000.0000.000a") != nil && lspOf(isisStatus(t, a), "0000.0000.000b") != nil })
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
