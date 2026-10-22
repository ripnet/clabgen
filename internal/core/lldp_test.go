package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	_ "github.com/ripnet/clabgen/protocols/lldp"
)

func lldpNeighbors(t *testing.T, e *core.Engine) []map[string]any {
	t.Helper()
	l, _ := flowStatus(t, e, "lldp")["neighbors"].([]any)
	out := []map[string]any{}
	for _, x := range l {
		out = append(out, x.(map[string]any))
	}
	return out
}

func TestLLDP(t *testing.T) {
	a, b, _, _ := newPair()
	for i, e := range []*core.Engine{a, b} {
		if _, err := e.CreateDevice(core.DeviceSpec{Name: "lldp0", Port: "eth1", MAC: fmt.Sprintf("02:00:00:00:0%d:01", i+1), Addresses: []string{fmt.Sprintf("10.9.9.%d/24", i+1)}}); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(e *core.Engine, cfg string) {
		t.Helper()
		if _, err := e.CreateProtocol(core.ProtocolSpec{Name: "lldp", Type: "lldp", Device: "lldp0", Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	mk(a, `{"tx_interval":1,"tx_hold":1,"system_name":"gen-a","capabilities":["bridge","router"],"enabled_capabilities":["router"],
		"port_vlan_id":100,"vlan_names":[{"vid":100,"name":"blue"}],"max_frame_size":9212,
		"mac_phy":{"autoneg_supported":true,"autoneg_enabled":true,"mau_type":30},"link_aggregation":{"capable":true,"aggregated_port_id":7},
		"custom_tlvs":[{"type":127,"oui":"00-11-22","subtype":9,"value":"cafe"}]}`)
	mk(b, `{"tx_interval":1,"tx_hold":1,"system_name":"gen-b","chassis_id_subtype":"local","chassis_id":"chassis-b","port_id_subtype":"local","port_id":"p1"}`)
	eventually(t, "both see each other", func() bool { return len(lldpNeighbors(t, a)) == 1 && len(lldpNeighbors(t, b)) == 1 })
	nb := lldpNeighbors(t, b)[0]
	want := map[string]string{"system_name": "gen-a", "chassis_id_subtype": "mac", "chassis_id": "02:00:00:00:01:01", "port_id_subtype": "interface-name",
		"port_id": "eth1", "port_description": "lldp0", "capabilities": "[bridge router]", "enabled_capabilities": "[router]",
		"management_addresses": "[10.9.9.1]", "port_vlan_id": "100", "vlan_names": "[map[name:blue vid:100]]", "max_frame_size": "9212",
		"mac_phy":          "map[advertised_capabilities:0 autoneg_enabled:true autoneg_supported:true mau_type:30]",
		"link_aggregation": "map[aggregated_port_id:7 capable:true dot1:true enabled:false]",
		"other_tlvs":       "[map[oui:00-11-22 subtype:9 type:127 value:cafe]]", "ttl": "2"}
	for k, v := range want {
		if fmt.Sprint(nb[k]) != v {
			t.Errorf("b's neighbor %s = %v, want %s", k, nb[k], v)
		}
	}
	if na := lldpNeighbors(t, a)[0]; na["chassis_id"] != "chassis-b" || na["port_id"] != "p1" || na["system_name"] != "gen-b" {
		t.Fatalf("a's neighbor %v", na)
	}
	run := func(e *core.Engine, action, args string) {
		t.Helper()
		if _, err := e.RunAction(context.Background(), "lldp", action, json.RawMessage(args)); err != nil {
			t.Fatal(err)
		}
	}
	// Shutdown: b drops a at once; resume brings it back.
	run(a, "shutdown", "")
	eventually(t, "shutdown LLDPDU removes a", func() bool { return len(lldpNeighbors(t, b)) == 0 })
	if st := flowStatus(t, a, "lldp"); st["transmit"] != "shut down" || num(st["counters"].(map[string]any), "tx_shutdowns") != 1 {
		t.Fatalf("a after shutdown %v", st)
	}
	run(a, "resume", "")
	eventually(t, "a back", func() bool { return len(lldpNeighbors(t, b)) == 1 })
	// Pause: b ages a out after its TTL.
	run(a, "pause", `{"seconds":5}`)
	// TTL 2 s: allow it to run out.
	time.Sleep(time.Second)
	eventually(t, "aged out", func() bool { return len(lldpNeighbors(t, b)) == 0 })
	if num(flowStatus(t, b, "lldp")["counters"].(map[string]any), "ageouts") < 1 {
		t.Fatal("ageout not counted")
	}
	run(a, "resume", "")
	eventually(t, "a back after pause", func() bool { return len(lldpNeighbors(t, b)) == 1 })
	// A changed advertisement is an update of the same neighbor.
	if _, err := a.UpdateProtocol("lldp", json.RawMessage(`{"tx_interval":1,"tx_hold":1,"system_name":"gen-a2"}`)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "new name", func() bool { n := lldpNeighbors(t, b); return len(n) == 1 && n[0]["system_name"] == "gen-a2" })
	// Deleting the agent sends a shutdown LLDPDU.
	if err := a.DeleteProtocol("lldp"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "removed on delete", func() bool { return len(lldpNeighbors(t, b)) == 0 })
	for _, bad := range []string{`{"dest":"x"}`, `{"chassis_id_subtype":"mac","chassis_id":"nope"}`, `{"capabilities":["toaster"]}`,
		`{"custom_tlvs":[{"type":127,"oui":"00","value":"aa"}]}`, `{"port_id_subtype":"network-address"}`} {
		if _, err := b.CreateProtocol(core.ProtocolSpec{Name: "x", Type: "lldp", Device: "lldp0", Config: json.RawMessage(bad)}); err == nil {
			b.DeleteProtocol("x")
			t.Errorf("accepted %s", bad)
		}
	}
}
