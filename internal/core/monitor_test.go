package core_test

import (
	"strings"
	"testing"

	"github.com/ripnet/clabgen/internal/core"
	_ "github.com/ripnet/clabgen/protocols/l2stream"
	_ "github.com/ripnet/clabgen/protocols/macsec"
	_ "github.com/ripnet/clabgen/protocols/monitor"
)

// A monitor port takes every frame arriving on it, nothing else on the port
// sees them (a device there neither counts a flow nor answers ARP), and
// nothing is sent from it.
func TestMonitorPort(t *testing.T) {
	a, b, _, wb := newPair()
	var sent wireLog
	sent.watch(wb) // what b sends toward a
	mkDev(t, a, "d", "02:00:00:00:0a:01", "10.0.0.1/24")
	mkDev(t, b, "mon", "02:00:00:00:0b:01", "")
	mkDev(t, b, "d", "02:00:00:00:0b:02", "10.0.0.2/24")
	mkProto(t, b, "m", "monitor", "mon", `{}`)
	sent.reset() // b's gratuitous ARP went before the monitor started

	mkProto(t, a, "s", "l2", "d", `{"dst_mac":"02:00:00:00:0b:02","rate":100,"auto_start":true}`)
	mkProto(t, a, "p", "icmp", "d", `{"destination":"10.0.0.2","rate":10,"auto_start":true}`)
	var flowID string
	eventually(t, "the monitor sees the stream", func() bool {
		st := flowStatus(t, b, "m")
		flowID, _ = flowStatus(t, a, "s")["flow_id"].(string)
		for _, s := range st["services"].([]any) {
			for _, d := range asList(s.(map[string]any)["data"]) {
				if d.(map[string]any)["flow_id"] == flowID && num(d.(map[string]any), "count") >= 20 {
					return true
				}
			}
		}
		return false
	})
	if rx, err := b.ReceivedFlows("d"); err != nil || len(rx) != 0 {
		t.Fatalf("device on the monitor port counted flows: %v %v", rx, err)
	}
	if st := flowStatus(t, a, "p"); num(st, "rx") != 0 {
		t.Fatalf("pings answered through a monitor port: %v", st)
	}
	// Sending from the port fails; nothing went out.
	mkProto(t, b, "s", "l2", "d", `{"rate":100,"auto_start":true}`)
	eventually(t, "sending fails", func() bool { return num(flowStatus(t, b, "s"), "tx_errors") >= 10 })
	if l := sent.layouts(); len(l) != 0 {
		t.Fatalf("b sent %v", l)
	}
	// One monitor per port, and no MACsec beside it.
	if _, err := b.CreateProtocol(core.ProtocolSpec{Name: "m2", Type: "monitor", Device: "d", Config: []byte(`{}`)}); err == nil || !strings.Contains(err.Error(), "monitor port") {
		if v, _ := b.GetProtocol("m2"); v.Error == "" || !strings.Contains(v.Error, "monitor port") {
			t.Fatalf("second monitor on the port: %v %+v", err, v)
		}
	}
	if _, err := b.CreateProtocol(core.ProtocolSpec{Name: "ms", Type: "macsec", Device: "d", Config: []byte(`{"static":{"key":"` + msKey + `"}}`)}); err == nil {
		if v, _ := b.GetProtocol("ms"); !strings.Contains(v.Error, "monitor port") {
			t.Fatalf("MACsec on a monitor port: %+v", v)
		}
	}
	// Without the monitor, the port is an ordinary one again.
	if err := b.DeleteProtocol("m"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "pings answered", func() bool { return num(flowStatus(t, a, "p"), "rx") > 0 })
}

func asList(v any) []any { l, _ := v.([]any); return l }
