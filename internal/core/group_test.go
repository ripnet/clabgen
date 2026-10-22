package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ripnet/clabgen/internal/core"
)

func groupSpec(t *testing.T, y string) core.GroupSpec {
	t.Helper()
	var g core.GroupSpec
	if err := yaml.UnmarshalStrict([]byte(y), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// One side is a PE with a /30 per VLAN, the other a group of CEs, each
// its own AS, peering with the PE over its VLAN.
const groupPE = `
name: pe
count: %d
device:
  port: eth1
  vlans: [{vid: {start: 1000}}]
  addresses: [{start: 10.1.0.1/30, step: 0.0.0.4}]
protocols:
  - type: bgp
    config:
      local_as: 65000
      hold_time: 30
      peers: [{address: {start: 10.1.0.2, step: 0.0.0.4}, remote_as: {start: 65001}}]
`

const groupCE = `
name: ce
count: %d
device:
  port: eth1
  vlans: [{vid: {start: 1000}}]
  addresses: [{start: 10.1.0.2/30, step: 0.0.0.4}]
  gateway4: {start: 10.1.0.1, step: 0.0.0.4}
protocols:
  - type: bgp
    config:
      local_as: {start: 65001}
      hold_time: 30
      peers: [{address: "{gateway4}", remote_as: 65000, passive: true}]
      routes: [{prefix: {start: 100.64.0.0/24, step: 0.0.1.0}, count: 3, communities: ["65000:{i}"]}]
`

func healthOf(t *testing.T, e *core.Engine, g string) map[string]int {
	t.Helper()
	v, err := e.GetGroup(g, false)
	if err != nil {
		t.Fatal(err)
	}
	return v.Protocols[0].Health
}

func TestGroupBGP(t *testing.T) {
	const n = 40
	a, b, _, _ := newPair()
	if _, err := a.CreateGroup(groupSpec(t, fmt.Sprintf(groupPE, n))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateGroup(groupSpec(t, fmt.Sprintf(groupCE, n))); err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "all established", func() bool { return healthOf(t, a, "pe")["established"] == n })

	// Members are ordinary devices and protocols with rendered specs.
	devs := b.Devices()
	if len(devs) != n || devs[0].Name != "ce-1" || devs[0].Group != "ce" {
		t.Fatalf("devices: %+v", devs[:1])
	}
	p, err := b.GetProtocol("ce-7-bgp")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	json.Unmarshal(p.Config, &cfg)
	if cfg["local_as"] != 65007.0 || p.Device != "ce-7" || p.Group != "ce" ||
		!strings.Contains(string(p.Config), `"address":"10.1.0.25"`) || !strings.Contains(string(p.Config), `"100.64.6.0/24"`) {
		t.Fatalf("ce-7-bgp: %s", p.Config)
	}
	eventuallyLong(t, "routes from ce-7", func() bool {
		return received(bgpPeerOf(t, a, "pe-7-bgp", "10.1.0.26"), "ipv4-unicast") == 3
	})

	// Members change only through the group.
	if err := b.DeleteDevice("ce-1"); err == nil || !strings.Contains(err.Error(), "belongs to group ce") {
		t.Fatalf("delete member: %v", err)
	}
	if _, err := b.UpdateProtocol("ce-1-bgp", nil); err == nil {
		t.Fatal("update member protocol: no error")
	}
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "ce-2", Port: "eth1"}); err == nil {
		t.Fatal("duplicate member name: no error")
	}

	// Actions on a selection.
	res, err := b.RunGroupAction(context.Background(), core.GroupActionRequest{Group: "ce", Action: "withdraw",
		Args: json.RawMessage(`{"routes":[{"prefix":"100.64.0.0/24","count":3}]}`), Members: "1-2,5"})
	if err != nil || len(res) != 3 || res[2].Index != 5 || res[2].Protocol != "ce-5-bgp" || res[0].Error != "" {
		t.Fatalf("withdraw: %+v %v", res, err)
	}
	eventuallyLong(t, "ce-1 withdrew", func() bool { return received(bgpPeerOf(t, a, "pe-1-bgp", "10.1.0.2"), "ipv4-unicast") == 0 })
	res, err = b.RunGroupAction(context.Background(), core.GroupActionRequest{Group: "ce", Action: "reset", Random: 4})
	if err != nil || len(res) != 4 {
		t.Fatalf("random reset: %+v %v", res, err)
	}
	if _, err := b.RunGroupAction(context.Background(), core.GroupActionRequest{Group: "ce", Action: "nope"}); err == nil {
		t.Fatal("unknown action: no error")
	}
	reset := map[int]bool{}
	for _, r := range res {
		reset[r.Index] = true
	}
	eventuallyLong(t, "re-established", func() bool { return healthOf(t, a, "pe")["established"] == n })
	keep := 3
	for reset[keep] {
		keep++
	}

	// Update: grow both sides, and change one field of every CE (hold
	// time); members whose specs are unchanged keep running.
	keepName := fmt.Sprintf("pe-%d-bgp", keep)
	before, _ := a.GetProtocol(keepName)
	if _, err := a.UpdateGroup(groupSpec(t, fmt.Sprintf(groupPE, n+10))); err != nil {
		t.Fatal(err)
	}
	after, _ := a.GetProtocol(keepName)
	if peerUps(before) != peerUps(after) || peerUps(after) == 0 {
		t.Fatalf("unchanged member restarted: ups %v → %v", peerUps(before), peerUps(after))
	}
	if _, err := b.UpdateGroup(groupSpec(t, strings.Replace(fmt.Sprintf(groupCE, n+10), "hold_time: 30", "hold_time: 60", 1))); err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "grown", func() bool { return healthOf(t, a, "pe")["established"] == n+10 })
	if p, _ := b.GetProtocol("ce-50-bgp"); !strings.Contains(string(p.Config), `"hold_time":60`) {
		t.Fatalf("ce-50: %s", p.Config)
	}

	// Export keeps the group, not its members; shrink and delete.
	s := b.Export()
	if len(s.Groups) != 1 || len(s.Devices) != 0 || len(s.Protocols) != 0 {
		t.Fatalf("export: %d groups %d devices %d protocols", len(s.Groups), len(s.Devices), len(s.Protocols))
	}
	y, _ := s.YAML()
	if !strings.Contains(string(y), "device_groups:") || !strings.Contains(string(y), "start: 10.1.0.2/30") {
		t.Fatalf("export yaml:\n%s", y)
	}
	if _, err := b.UpdateGroup(groupSpec(t, fmt.Sprintf(groupCE, 5))); err != nil {
		t.Fatal(err)
	}
	if len(b.Devices()) != 5 || len(b.Protocols(false)) != 5 {
		t.Fatalf("shrunk: %d devices", len(b.Devices()))
	}
	eventuallyLong(t, "PE sees 5", func() bool { return healthOf(t, a, "pe")["established"] == 5 })
	if err := b.Apply(s, true); err != nil {
		t.Fatal(err)
	}
	if len(b.Devices()) != n+10 {
		t.Fatalf("re-applied: %d devices", len(b.Devices()))
	}
	if err := b.DeleteGroup("ce"); err != nil {
		t.Fatal(err)
	}
	if len(b.Devices()) != 0 || len(b.Protocols(false)) != 0 {
		t.Fatal("members left after delete")
	}
}

func bgpPeerOf(t *testing.T, e *core.Engine, inst, addr string) map[string]any {
	t.Helper()
	for _, x := range flowStatus(t, e, inst)["peers"].([]any) {
		if p := x.(map[string]any); p["address"] == addr {
			return p
		}
	}
	t.Fatalf("%s: no peer %s", inst, addr)
	return nil
}

func peerUps(v core.ProtocolView) float64 {
	b, _ := json.Marshal(v.Status)
	var m map[string]any
	json.Unmarshal(b, &m)
	if ps, _ := m["peers"].([]any); len(ps) > 0 {
		return num(ps[0].(map[string]any), "ups")
	}
	return -1
}

func TestGroupErrors(t *testing.T) {
	a, _, _, _ := newPair()
	for _, c := range []struct{ y, err string }{
		{"name: g\ncount: 0\ndevice: {port: eth1}", "count must be"},
		{"name: g\ncount: 2\ndevice: {port: eth1, name: x}", "repeats"},
		{"name: g\ncount: 2\ndevice: {port: eth1, colour: red}", "unknown field"},
		{"name: g\ncount: 2\ndevice: {port: eth1, vlans: [{vid: {start: 4095}}]}", "member 2: invalid VLAN"},
		{"name: g\ncount: 1\ndevice: {port: eth1}\nprotocols: [{type: nope}]", "unknown protocol type"},
		{"name: g\ncount: 1\ndevice: {port: eth1}\nprotocols: [{type: bgp, config: {local_as: 1, peers: [{address: '{gateway4}', remote_as: 2}]}}]", "{gateway4}: the device has none"},
		{"name: g\ncount: 1\ndevice: {port: eth1}\nprotocols: [{type: lldp}, {type: lldp}]", "used twice"},
	} {
		_, err := a.CreateGroup(groupSpec(t, c.y))
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: err %v, want %q", c.y, err, c.err)
		}
	}
	if len(a.Groups()) != 0 || len(a.Devices()) != 0 {
		t.Fatal("a failed group left something behind")
	}
}
