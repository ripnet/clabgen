package core_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	_ "github.com/ripnet/clabgen/protocols/udp"
	_ "github.com/ripnet/clabgen/protocols/udpecho"
)

func rxFlow(t *testing.T, e *core.Engine, id string) map[string]any {
	t.Helper()
	list, err := e.ReceivedFlows("d")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range list {
		if f.FlowID == id {
			b, _ := json.Marshal(f.Flow)
			var m map[string]any
			json.Unmarshal(b, &m)
			return m
		}
	}
	return nil
}

func TestUDPFlowOneWay(t *testing.T) {
	a, b, wa, _ := ipPair(t)
	mkUDP := func(name, cfg string) {
		t.Helper()
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: name, Type: "udp", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	mkUDP("u", `{"destination":"10.0.0.2","dst_port":5001,"rate":100,"auto_start":true}`)
	id, _ := flowStatus(t, a, "u")["flow_id"].(string)
	eventually(t, "receiver sees the flow", func() bool { m := rxFlow(t, b, id); return m != nil && num(m, "rx") >= 20 })
	m := rxFlow(t, b, id)
	if m["proto"] != "udp" || m["src"] != "10.0.0.1" || num(m, "dst_port") != 5001 || num(m, "lost") != 0 || num(m, "latency_avg_us") <= 0 {
		t.Fatalf("rx %v", m)
	}
	if st := flowStatus(t, a, "u"); st["tracks_replies"] != false || num(st, "lost") != 0 {
		t.Fatalf("one-way flow tracks replies: %v", st)
	}
	// Cut a→b for ~300 ms: the receiver sees one gap of about that length.
	wa.mu.Lock()
	wa.up = false
	wa.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	wa.mu.Lock()
	wa.up = true
	wa.mu.Unlock()
	eventually(t, "receiver outage", func() bool { return num(rxFlow(t, b, id), "outage_max_ms") >= 200 })
	m = rxFlow(t, b, id)
	if ms := num(m, "outage_max_ms"); ms < 200 || ms > 450 || num(m, "lost") < 15 {
		t.Fatalf("outage %v", m)
	}
	run(t, a, "u", "stop")

	// Echo: b reflects port 7; a measures round trips.
	if _, err := b.CreateProtocol(core.ProtocolSpec{Name: "echo", Type: "udp-echo", Device: "d"}); err != nil {
		t.Fatal(err)
	}
	mkUDP("e", `{"destination":"10.0.0.2","reply":"echo","mode":"count","count":20,"rate":100,"timeout":"200ms","auto_start":true}`)
	eventually(t, "echo done", func() bool { return flowStatus(t, a, "e")["state"] == "done" })
	st := flowStatus(t, a, "e")
	if num(st, "rx") != 20 || num(st, "lost") != 0 || st["kind"] != "→ :7 echo" {
		t.Fatalf("echo %v", st)
	}
	if rtt, _ := st["rtt"].(map[string]any); rtt == nil || num(rtt, "avg_us") <= 0 {
		t.Fatalf("echo rtt %v", st)
	}
	// The reflector measured the outbound leg; the sender doesn't list its
	// own flow's echoes as received (the flow accounts for them).
	eid, _ := st["flow_id"].(string)
	if m := rxFlow(t, b, eid); m == nil || num(m, "rx") != 20 {
		t.Fatalf("reflector rx %v", m)
	}
	if m := rxFlow(t, a, eid); m != nil {
		t.Fatalf("own echoes listed as received: %v", m)
	}
	// Deleting the flow releases its ID (see the ipstack claim test).
	if err := a.DeleteProtocol("e"); err != nil {
		t.Fatal(err)
	}

	// Bad UDP checksums are counted as errors and not recorded.
	mkUDP("bad", `{"destination":"10.0.0.2","udp_checksum":"invalid","mode":"count","count":5,"rate":100,"auto_start":true}`)
	eventually(t, "bad done", func() bool { return flowStatus(t, a, "bad")["state"] == "done" })
	bid, _ := flowStatus(t, a, "bad")["flow_id"].(string)
	if rxFlow(t, b, bid) != nil {
		t.Fatal("recorded datagrams with bad checksums")
	}
	ipb, _ := b.DeviceIP("d")
	if ipb.Counters["udp_errors"] < 5 {
		t.Fatalf("udp errors %v", ipb.Counters)
	}

	// IPv6 with a source port range.
	mkUDP("v6", `{"destination":"2001:db8::2","src_port":40000,"src_port_count":4,"mode":"count","count":8,"rate":200,"auto_start":true}`)
	eventually(t, "v6 done", func() bool { return flowStatus(t, a, "v6")["state"] == "done" })
	if st := flowStatus(t, a, "v6"); st["src_ports"] != "40000-40003" {
		t.Fatalf("v6 %v", st)
	}
	vid, _ := flowStatus(t, a, "v6")["flow_id"].(string)
	eventually(t, "v6 received", func() bool { m := rxFlow(t, b, vid); return m != nil && num(m, "rx") == 8 })

	var vidN uint32
	fmt.Sscanf(vid, "%x", &vidN)
	if err := b.ClearReceivedFlows("", &vidN); err != nil {
		t.Fatal(err)
	}
	if rxFlow(t, b, vid) != nil || rxFlow(t, b, eid) == nil {
		t.Fatal("clearing one flow ID")
	}
	if err := b.ClearReceivedFlows("", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := b.ReceivedFlows(""); len(l) != 0 {
		t.Fatal("clear")
	}
	for _, bad := range []string{
		`{"destination":"10.0.0.2","reply":"echo","signature":false}`,
		`{"destination":"10.0.0.2","src_port":65535,"src_port_count":2}`,
		`{"destination":"10.0.0.2","dst_port":0}`,
	} {
		if _, err := a.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "udp", Device: "d", Config: json.RawMessage(bad)}); err == nil {
			a.DeleteProtocol("x")
			t.Errorf("accepted %s", bad)
		}
	}
}
