package core_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	_ "github.com/ripnet/clabgen/protocols/icmp"
	_ "github.com/ripnet/clabgen/protocols/rsvp"
)

const (
	rsvpTail = `{"router_id":"192.0.2.2","refresh_interval":1,"hello_interval":1,"retry_interval":1}`
	rsvpHead = `{"router_id":"192.0.2.1","refresh_interval":1,"hello_interval":1,"retry_interval":1,"lsps":[{"name":"l1","to":"192.0.2.2","protection":"link","hops":[{"address":"10.0.0.2"}]}]}`
)

// rsvpPair is a with the head and b with the tail, on a link.
func rsvpPair(t *testing.T, cfgA, cfgB string) (*core.Engine, *core.Engine, *wire) {
	t.Helper()
	a, b, wa, _ := rsvpPairWires(t, cfgA, cfgB)
	return a, b, wa
}

func rsvpPairWires(t *testing.T, cfgA, cfgB string) (*core.Engine, *core.Engine, *wire, *wire) {
	t.Helper()
	a, b, wa, wb := newPair()
	for i, e := range []*core.Engine{a, b} {
		addr, gw, mac, cfg := "10.0.0.1/24", "10.0.0.2", "02:00:00:00:0a:01", cfgA
		if i == 1 {
			addr, gw, mac, cfg = "10.0.0.2/24", "10.0.0.1", "02:00:00:00:0b:01", cfgB
		}
		if _, err := e.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", MAC: mac, Addresses: []string{addr}, Gateway4: gw}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "rsvp", Type: "rsvp", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	return a, b, wa, wb
}

func rsvpStatus(t *testing.T, e *core.Engine) map[string]any {
	t.Helper()
	v, err := e.GetProtocol("rsvp")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v.Status)
	var st map[string]any
	json.Unmarshal(b, &st)
	return st
}

func rsvpHeadOf(t *testing.T, e *core.Engine) map[string]any {
	t.Helper()
	return rsvpStatus(t, e)["heads"].([]any)[0].(map[string]any)
}

func rsvpTails(t *testing.T, e *core.Engine) []any {
	t.Helper()
	return rsvpStatus(t, e)["tails"].([]any)
}

func rsvpAction(t *testing.T, e *core.Engine, name, args string) {
	t.Helper()
	if _, err := e.RunAction(context.Background(), "rsvp", name, json.RawMessage(args)); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func rsvpUp(t *testing.T, a *core.Engine) {
	t.Helper()
	ldpWait(t, "LSP up", func() bool { return rsvpHeadOf(t, a)["state"] == "up" }, 10*time.Second)
}

func lspFlow(t *testing.T, a *core.Engine) {
	t.Helper()
	cfg := `{"destination":"192.0.2.2","lsp":"l1","rate":100,"auto_start":true,"timeout":"300ms"}`
	if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "f", Type: "icmp", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

func TestRSVPSignalAndFlow(t *testing.T) {
	a, b, _ := rsvpPair(t, rsvpHead, rsvpTail)
	rsvpUp(t, a)
	h := rsvpHeadOf(t, a)
	if h["label"] != float64(100000) || h["next_hop"] != "10.0.0.2" || h["setups"] != float64(1) {
		t.Fatalf("%v", h)
	}
	path := h["path"].([]any)
	if p := path[0].(map[string]any); p["address"] != "10.0.0.2" || p["label"] != float64(100000) {
		t.Fatalf("%v", path)
	}
	tails := rsvpTails(t, b)
	if len(tails) != 1 {
		t.Fatalf("%v", tails)
	}
	if x := tails[0].(map[string]any); x["name"] != "l1" || x["from"] != "192.0.2.1" || x["style"] != "shared explicit" || x["protection"] != "link" || x["previous_hop"] != "10.0.0.1" {
		t.Fatalf("%v", x)
	}
	ldpWait(t, "hellos up", func() bool {
		n := rsvpStatus(t, a)["neighbors"].([]any)
		return len(n) == 1 && n[0].(map[string]any)["hellos"] == "up"
	}, 5*time.Second)

	// Traffic goes in with the label: it's answered, and b saw the label.
	lspFlow(t, a)
	eventually(t, "replies", func() bool { return num(flowStatus(t, a, "f"), "rx") >= 20 })
	st := flowStatus(t, a, "f")
	if st["lsp"] != "l1" || st["lsp_up"] != true || st["next_hop"] != "10.0.0.2" || num(st, "frame_size") < 102 {
		t.Fatalf("%v", st)
	}
	if ls := st["lsp_labels"].([]any); len(ls) != 1 || ls[0] != float64(100000) {
		t.Fatalf("%v", ls)
	}
	r, err := a.RunAction(context.Background(), "f", "preview", nil)
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := json.Marshal(r)
	if !strings.Contains(string(pb), "8847186a0") { // ethertype and label 100000
		t.Fatalf("not labelled: %s", pb)
	}

	// A new label from the tail: traffic follows it.
	rsvpAction(t, b, "relabel", `{}`)
	ldpWait(t, "new label", func() bool { return rsvpHeadOf(t, a)["label"] == float64(100001) }, 5*time.Second)
	eventually(t, "flow takes it", func() bool {
		ls, _ := flowStatus(t, a, "f")["lsp_labels"].([]any)
		return len(ls) == 1 && ls[0] == float64(100001)
	})

	// Make before break: another LSP ID, the old one goes, no traffic lost.
	before := num(flowStatus(t, a, "f"), "lost")
	id := h["lsp_id"]
	rsvpAction(t, a, "resignal", `{"lsp":"l1"}`)
	ldpWait(t, "moved", func() bool { x := rsvpHeadOf(t, a); return x["lsp_id"] != id && x["setups"] == float64(2) }, 5*time.Second)
	ldpWait(t, "old one gone at the tail", func() bool { return len(rsvpTails(t, b)) == 1 }, 3*time.Second)
	time.Sleep(500 * time.Millisecond)
	if d := num(flowStatus(t, a, "f"), "lost") - before; d > 0 {
		t.Fatalf("make before break lost %v packets", d)
	}

	// The tail tears it down for a while: the head has no LSP, the flow loses
	// packets without sending them, and it all comes back.
	rsvpAction(t, b, "tear", `{"seconds":2}`)
	ldpWait(t, "down", func() bool { return rsvpHeadOf(t, a)["state"] != "up" }, 3*time.Second)
	eventually(t, "flow says so", func() bool { return flowStatus(t, a, "f")["lsp_up"] != true })
	lost := num(flowStatus(t, a, "f"), "lost")
	ldpWait(t, "back up", func() bool { return rsvpHeadOf(t, a)["state"] == "up" }, 15*time.Second)
	if num(flowStatus(t, a, "f"), "lost") <= before || lost <= before {
		t.Fatalf("no loss counted while the LSP was down: %v", lost)
	}
	rx := num(flowStatus(t, a, "f"), "rx")
	eventually(t, "traffic back", func() bool { return num(flowStatus(t, a, "f"), "rx") > rx+10 })

	// The head tears it down until told: gone at the tail.
	rsvpAction(t, a, "tear", `{"lsp":"l1"}`)
	eventually(t, "gone at the tail", func() bool { return len(rsvpTails(t, b)) == 0 })
	time.Sleep(1200 * time.Millisecond)
	if rsvpHeadOf(t, a)["state"] != "down" {
		t.Fatal("came back without the signal action")
	}
	rsvpAction(t, a, "signal", `{"lsp":"l1"}`)
	rsvpUp(t, a)
}

// With implicit null the head sends plain IP.
func TestRSVPImplicitNull(t *testing.T) {
	a, _, _ := rsvpPair(t, rsvpHead, `{"router_id":"192.0.2.2","tail_mode":"implicit-null","refresh_interval":1,"retry_interval":1}`)
	rsvpUp(t, a)
	if rsvpHeadOf(t, a)["label"] != float64(3) {
		t.Fatalf("%v", rsvpHeadOf(t, a))
	}
	lspFlow(t, a)
	eventually(t, "replies", func() bool { return num(flowStatus(t, a, "f"), "rx") >= 5 })
	if ls, _ := flowStatus(t, a, "f")["lsp_labels"].([]any); len(ls) != 0 {
		t.Fatalf("%v", ls)
	}
}

func TestRSVPRefused(t *testing.T) {
	a, b, _ := rsvpPair(t, rsvpHead, `{"router_id":"192.0.2.2","refuse_lsps":true}`)
	eventually(t, "error at the head", func() bool {
		s, _ := rsvpHeadOf(t, a)["last_error"].(string)
		return strings.Contains(s, "admission control")
	})
	if rsvpHeadOf(t, a)["state"] == "up" || len(rsvpTails(t, b)) != 0 {
		t.Fatal("refused LSP came up")
	}
}

// Not ours: no label switching here, and the head hears why.
func TestRSVPNotForUs(t *testing.T) {
	a, _, _ := rsvpPair(t, `{"router_id":"192.0.2.1","retry_interval":1,"lsps":[{"name":"l1","to":"192.0.2.9"}]}`, rsvpTail)
	eventually(t, "routing error", func() bool {
		s, _ := rsvpHeadOf(t, a)["last_error"].(string)
		return strings.Contains(s, "routing problem")
	})
}

// Soft state: refreshes stop, and both ends notice from their timers.
func TestRSVPSoftState(t *testing.T) {
	a, b, wa, wb := rsvpPairWires(t, rsvpHead, rsvpTail)
	rsvpUp(t, a)
	// Hellos only time out once they've been answered.
	ldpWait(t, "hellos up", func() bool {
		for _, e := range []*core.Engine{a, b} {
			n := rsvpStatus(t, e)["neighbors"].([]any)
			if len(n) != 1 || n[0].(map[string]any)["hellos"] != "up" {
				return false
			}
		}
		return true
	}, 5*time.Second)
	link := func(up bool) {
		for _, w := range []*wire{wa, wb} {
			w.mu.Lock()
			w.up = up
			w.mu.Unlock()
		}
	}
	link(false)
	// Hellos at 1 s notice in about 3.5 s, well before the state's 5.25 refreshes.
	ldpWait(t, "head noticed", func() bool { return rsvpHeadOf(t, a)["state"] != "up" }, 6*time.Second)
	ldpWait(t, "tail noticed", func() bool { return len(rsvpTails(t, b)) == 0 }, 6*time.Second)
	link(true)
	rsvpUp(t, a)
}

// The silent flap leaves the far end to its timers; afterwards it recovers.
func TestRSVPSilentFlap(t *testing.T) {
	a, b, _ := rsvpPair(t, rsvpHead, rsvpTail)
	rsvpUp(t, a)
	ldpWait(t, "hellos up", func() bool {
		n := rsvpStatus(t, b)["neighbors"].([]any)
		return len(n) == 1 && n[0].(map[string]any)["hellos"] == "up"
	}, 5*time.Second)
	rsvpAction(t, b, "flap", `{"seconds":10,"mode":"silent"}`)
	ldpWait(t, "head's timers", func() bool { return rsvpHeadOf(t, a)["state"] != "up" }, 10*time.Second)
	ldpWait(t, "back", func() bool { return rsvpHeadOf(t, a)["state"] == "up" }, 20*time.Second)
}

func TestRSVPConfigChecks(t *testing.T) {
	a, _, _, _ := newPair()
	if _, err := a.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", MAC: "02:00:00:00:0a:01", Addresses: []string{"10.0.0.1/24"}}); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []string{
		`{}`,
		`{"router_id":"1.1.1.1","lsps":[{"name":"","to":"2.2.2.2"}]}`,
		`{"router_id":"1.1.1.1","lsps":[{"name":"a","to":"x"}]}`,
		`{"router_id":"1.1.1.1","lsps":[{"name":"a","to":"2.2.2.2"},{"name":"a","to":"3.3.3.3"}]}`,
		`{"router_id":"1.1.1.1","lsps":[{"name":"a","to":"2.2.2.2","protection":"x"}]}`,
		`{"router_id":"1.1.1.1","lsps":[{"name":"a","to":"2.2.2.2","setup_priority":0,"hold_priority":3}]}`,
		`{"router_id":"1.1.1.1","tail_mode":"x"}`,
	} {
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "rsvp", Device: "d", Config: json.RawMessage(cfg)}); err == nil {
			t.Errorf("accepted %s", cfg)
		}
	}
	for _, cfg := range []string{
		`{"destination":"2001:db8::1","lsp":"a"}`,
		`{"destination":"1.1.1.1","lsp":"a","dst_mac":"02:00:00:00:00:01"}`,
	} {
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "icmp", Device: "d", Config: json.RawMessage(cfg)}); err == nil {
			t.Errorf("accepted %s", cfg)
		}
	}
}
