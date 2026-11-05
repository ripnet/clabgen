package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
)

// vpnPE is one end of vpnPair.
type vpnPE struct {
	addr, gw, mac, rid, peer string
	fec                      string // LDP: how it advertises its loopback
}

var (
	vpnA = vpnPE{"10.0.0.1/24", "10.0.0.2", "02:00:00:00:0a:01", "192.0.2.1", "10.0.0.2", `{"prefix":"192.0.2.1/32","label":5001}`}
	vpnB = vpnPE{"10.0.0.2/24", "10.0.0.1", "02:00:00:00:0b:01", "192.0.2.2", "10.0.0.1", `{"prefix":"192.0.2.2/32","label":5002}`}
)

// vpnPair is two PEs on a link, with an LDP session and an iBGP session
// between them. Neither can reach the other's VPN prefixes in the global
// table: traffic can only work in the VPN.
func vpnPair(t *testing.T, routesA, routesB string) (*core.Engine, *core.Engine) {
	t.Helper()
	a, b, _, _ := newPair()
	for i, e := range []*core.Engine{a, b} {
		pe, routes := vpnA, routesA
		if i == 1 {
			pe, routes = vpnB, routesB
		}
		if _, err := e.CreateDevice(core.DeviceSpec{Name: "d", Port: "eth1", MAC: pe.mac, Addresses: []string{pe.addr}, Gateway4: pe.gw}); err != nil {
			t.Fatal(err)
		}
		ldp := fmt.Sprintf(`{"router_id":%q,"hello_interval":1,"hold_time":3,"keepalive":3,"fecs":[%s]}`, pe.rid, pe.fec)
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "ldp", Type: "ldp", Device: "d", Config: json.RawMessage(ldp)}); err != nil {
			t.Fatal(err)
		}
		bgp := fmt.Sprintf(`{"local_as":65000,"router_id":%q,"peers":[{"address":%q,"remote_as":65000,"families":["ipv4-vpn","ipv6-vpn"],"passive":%v}],"routes":%s}`, pe.rid, pe.peer, i == 1, routes)
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "bgp", Type: "bgp", Device: "d", Config: json.RawMessage(bgp)}); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

func vpnFlow(t *testing.T, e *core.Engine, name, typ, cfg string) {
	t.Helper()
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

func vpnWait(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(40 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

func vpnLabels(st map[string]any) []uint32 {
	l, _ := st["vpn_labels"].([]any)
	out := make([]uint32, len(l))
	for i, x := range l {
		out[i] = uint32(x.(float64))
	}
	return out
}

const (
	// With next hops on the link: no LSP is needed.
	vpnRoutesA = `[{"prefix":"198.51.100.0/24","rd":"65000:1","label":3001,"route_targets":["65000:100"]},{"prefix":"2001:db8:a::/48","rd":"65000:1","label":3001,"route_targets":["65000:100"]}]`
	vpnRoutesB = `[{"prefix":"203.0.113.0/24","rd":"65000:2","label":4001,"route_targets":["65000:100"]},{"prefix":"2001:db8:b::/48","rd":"65000:2","label":4001,"route_targets":["65000:100"]},{"prefix":"203.0.113.0/24","rd":"65000:9","label":4009,"route_targets":["65000:900"]}]`
	// With loopbacks as next hops: LDP's labels to them go on top.
	vpnLoopA = `[{"prefix":"198.51.100.0/24","next_hop":"192.0.2.1","rd":"65000:1","label":3001,"route_targets":["65000:100"]},{"prefix":"2001:db8:a::/48","next_hop":"192.0.2.1","rd":"65000:1","label":3001,"route_targets":["65000:100"]}]`
	vpnLoopB = `[{"prefix":"203.0.113.0/24","next_hop":"192.0.2.2","rd":"65000:2","label":4001,"route_targets":["65000:100"]},{"prefix":"2001:db8:b::/48","next_hop":"192.0.2.2","rd":"65000:2","label":4001,"route_targets":["65000:100"]}]`

	vpnPing = `{"destination":"203.0.113.1","source":"198.51.100.1","route_target":"65000:100","rate":100,"auto_start":true,"timeout":"500ms"}`
)

// Echo traffic both ways in a VPN, with labels learned from BGP.
func TestVPNEcho(t *testing.T) {
	a, _ := vpnPair(t, vpnRoutesA, vpnRoutesB)
	vpnFlow(t, a, "v4", "icmp", vpnPing)
	vpnFlow(t, a, "v6", "icmp", `{"destination":"2001:db8:b::1","source":"2001:db8:a::1","route_target":"65000:100","rate":100,"auto_start":true,"timeout":"500ms"}`)
	vpnWait(t, "replies in the VPN", func() bool {
		return num(flowStatus(t, a, "v4"), "rx") > 10 && num(flowStatus(t, a, "v6"), "rx") > 10
	})
	st := flowStatus(t, a, "v4")
	if l := vpnLabels(st); len(l) != 1 || l[0] != 4001 {
		t.Fatalf("a next hop on the link takes the VPN label alone: %v", st)
	}
	if st["route_target"] != "65000:100" || st["vpn_route"] != "203.0.113.0/24" || st["vpn_peer"] != "10.0.0.2" || num(st, "vpn_resolved") != 1 {
		t.Fatalf("status %v", st)
	}
}

// With a loopback as next hop the packet takes the label LDP gave for it on
// top, and replies do too. Without it there's no way there.
func TestVPNOverLDP(t *testing.T) {
	a, b := vpnPair(t, vpnLoopA, vpnLoopB)
	vpnFlow(t, a, "f", "icmp", vpnPing)
	vpnFlow(t, a, "f6", "icmp", `{"destination":"2001:db8:b::1","source":"2001:db8:a::1","route_target":"65000:100","rate":100,"auto_start":true,"timeout":"500ms"}`)
	vpnWait(t, "replies over LDP", func() bool {
		return num(flowStatus(t, a, "f"), "rx") > 10 && num(flowStatus(t, a, "f6"), "rx") > 10
	})
	if st := flowStatus(t, a, "f6"); st["vpn_peer"] != "192.0.2.2" {
		t.Fatalf("an IPv4-mapped next hop is that IPv4 address: %v", st)
	}
	st := flowStatus(t, a, "f")
	if l := vpnLabels(st); len(l) != 2 || l[0] != 5002 || l[1] != 4001 {
		t.Fatalf("labels: %v", st)
	}
	if st["vpn_peer"] != "192.0.2.2" || st["next_hop"] != "10.0.0.2" {
		t.Fatalf("status %v", st)
	}
	// The label for the next hop goes: no LSP, so nothing is sent.
	if _, err := b.RunAction(context.Background(), "ldp", "withdraw", json.RawMessage(`{"fecs":[{"prefix":"192.0.2.2/32"}]}`)); err != nil {
		t.Fatal(err)
	}
	vpnWait(t, "no LSP", func() bool {
		e, _ := flowStatus(t, a, "f")["error"].(string)
		return strings.Contains(e, "no LSP to next hop 192.0.2.2")
	})
}

// A packet to a VPN prefix without the label isn't ours; a route under another
// target isn't in this VPN.
func TestVPNPrefixOnlyWithItsLabel(t *testing.T) {
	a, _ := vpnPair(t, vpnRoutesA, vpnRoutesB)
	vpnFlow(t, a, "plain", "icmp", `{"destination":"203.0.113.1","dst_mac":"02:00:00:00:0b:01","rate":100,"auto_start":true,"timeout":"200ms"}`)
	vpnFlow(t, a, "other", "icmp", `{"destination":"203.0.113.1","source":"198.51.100.1","route_target":"65000:555","rate":100,"auto_start":true,"timeout":"200ms"}`)
	vpnFlow(t, a, "good", "icmp", vpnPing)
	vpnWait(t, "the good flow works", func() bool { return num(flowStatus(t, a, "good"), "rx") > 20 })
	if n := num(flowStatus(t, a, "plain"), "rx"); n != 0 {
		t.Fatalf("answered without the VPN label: %v", n)
	}
	other := flowStatus(t, a, "other")
	if num(other, "rx") != 0 || !strings.Contains(fmt.Sprint(other["error"]), "no VPN route") {
		t.Fatalf("other target: %v", other)
	}
}

// A flow follows the RIB: a withdrawn route is lost traffic until it's back,
// and a new label is used at once.
func TestVPNFollowsRIB(t *testing.T) {
	a, b := vpnPair(t, vpnRoutesA, vpnRoutesB)
	vpnFlow(t, a, "f", "icmp", vpnPing)
	vpnWait(t, "replies", func() bool { return num(flowStatus(t, a, "f"), "rx") > 10 })
	route := `{"routes":[{"prefix":"203.0.113.0/24","rd":"65000:2","label":4001,"route_targets":["65000:100"]}]}`
	if _, err := b.RunAction(context.Background(), "bgp", "withdraw", json.RawMessage(route)); err != nil {
		t.Fatal(err)
	}
	vpnWait(t, "the route gone", func() bool {
		s := flowStatus(t, a, "f")
		e, _ := s["error"].(string)
		return strings.Contains(e, "no VPN route") && s["lsp_up"] == nil
	})
	route = strings.ReplaceAll(route, "4001", "4002")
	if _, err := b.RunAction(context.Background(), "bgp", "advertise", json.RawMessage(route)); err != nil {
		t.Fatal(err)
	}
	vpnWait(t, "the new label", func() bool {
		l := vpnLabels(flowStatus(t, a, "f"))
		return len(l) == 1 && l[0] == 4002
	})
	before := num(flowStatus(t, a, "f"), "rx")
	vpnWait(t, "replies again", func() bool { return num(flowStatus(t, a, "f"), "rx") > before+10 })
}

// A BGP session going down takes its routes with it.
func TestVPNPeerDown(t *testing.T) {
	a, b := vpnPair(t, vpnRoutesA, vpnRoutesB)
	vpnFlow(t, a, "f", "icmp", vpnPing)
	vpnWait(t, "replies", func() bool { return num(flowStatus(t, a, "f"), "rx") > 10 })
	if err := b.DeleteProtocol("bgp"); err != nil {
		t.Fatal(err)
	}
	vpnWait(t, "routes gone", func() bool {
		e, _ := flowStatus(t, a, "f")["error"].(string)
		return strings.Contains(e, "no VPN route")
	})
}

// A UDP flow echoed by a responder in the VPN, and one measured at the far
// end: both need the packet handed on with its labels off.
func TestVPNUDP(t *testing.T) {
	a, b := vpnPair(t, vpnRoutesA, vpnRoutesB)
	vpnFlow(t, b, "echo", "udp-echo", `{"ports":[5000]}`)
	vpnFlow(t, a, "echoed", "udp", `{"destination":"203.0.113.1","source":"198.51.100.1","route_target":"65000:100","dst_port":5000,"reply":"echo","rate":100,"auto_start":true,"timeout":"500ms"}`)
	vpnFlow(t, a, "oneway", "udp", `{"destination":"203.0.113.2","source":"198.51.100.1","route_target":"65000:100","dst_port":5001,"rate":100,"auto_start":true}`)
	vpnWait(t, "echoed by the responder", func() bool { return num(flowStatus(t, a, "echoed"), "rx") > 10 })
	vpnWait(t, "seen at the far end", func() bool {
		st, err := b.DeviceIP("d")
		if err != nil {
			t.Fatal(err)
		}
		return st.Counters["udp_rx"] > 20
	})
}

func TestVPNFlowValidate(t *testing.T) {
	a, _ := vpnPair(t, `[]`, `[]`)
	for _, cfg := range []string{
		`{"destination":"1.1.1.1","route_target":"65000:1"}`,
		`{"destination":"1.1.1.1","source":"2.2.2.2","route_target":"nope"}`,
		`{"destination":"1.1.1.1","source":"2.2.2.2","route_target":"65000:1","dst_mac":"02:00:00:00:00:01"}`,
	} {
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "icmp", Device: "d", Config: json.RawMessage(cfg)}); err == nil {
			t.Errorf("%s: accepted", cfg)
		}
	}
}

// Through an RSVP LSP: by name, or as the LSP to the route's next hop. The far
// end has no LSP back, so this is one way, measured there.
func TestVPNOverRSVP(t *testing.T) {
	a, b, _ := rsvpPair(t, rsvpHead, rsvpTail)
	rsvpUp(t, a)
	for i, e := range []*core.Engine{a, b} {
		peer, routes := "10.0.0.2", `[]`
		if i == 1 {
			peer, routes = "10.0.0.1", `[{"prefix":"203.0.113.0/24","next_hop":"192.0.2.2","rd":"65000:2","label":4001,"route_targets":["65000:100"]}]`
		}
		bgp := fmt.Sprintf(`{"local_as":65000,"router_id":"192.0.2.%d","peers":[{"address":%q,"remote_as":65000,"families":["ipv4-vpn"],"passive":%v}],"routes":%s}`, i+1, peer, i == 1, routes)
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "bgp", Type: "bgp", Device: "d", Config: json.RawMessage(bgp)}); err != nil {
			t.Fatal(err)
		}
	}
	vpnFlow(t, a, "any", "udp", `{"destination":"203.0.113.1","source":"198.51.100.1","route_target":"65000:100","dst_port":5001,"rate":100,"auto_start":true}`)
	vpnFlow(t, a, "named", "udp", `{"destination":"203.0.113.1","source":"198.51.100.1","route_target":"65000:100","lsp":"l1","dst_port":5002,"rate":100,"auto_start":true}`)
	for _, name := range []string{"any", "named"} {
		vpnWait(t, name+" takes the LSP's label and the VPN's", func() bool {
			l := vpnLabels(flowStatus(t, a, name))
			return len(l) == 2 && l[0] == 100000 && l[1] == 4001
		})
	}
	vpnWait(t, "received at the far end", func() bool {
		st, err := b.DeviceIP("d")
		if err != nil {
			t.Fatal(err)
		}
		return st.Counters["udp_rx"] > 20
	})
	rsvpAction(t, a, "tear", `{"lsp":"l1"}`)
	vpnWait(t, "the named LSP down", func() bool {
		e, _ := flowStatus(t, a, "named")["error"].(string)
		return strings.Contains(e, "LSP l1 is down")
	})
}
