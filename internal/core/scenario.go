package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/ripnet/clabgen/internal/protocol"
	"sigs.k8s.io/yaml"
	yaml3 "gopkg.in/yaml.v3"
)

type PortSpec struct {
	Admin string `json:"admin,omitempty" jsonschema:"enum=up,enum=down" jsonschema_description:"Link state of the port"` // "up" or "down"
}

// Scenario is the declarative form of an agent's configuration.
type Scenario struct {
	Ports     map[string]PortSpec `json:"ports,omitempty" jsonschema_description:"Data ports by name (eth1...)"`
	Devices   []DeviceSpec        `json:"devices,omitempty" jsonschema_description:"Emulated hosts: a MAC and addresses on a port"`
	Groups    []GroupSpec         `json:"device_groups,omitempty" jsonschema_description:"Many devices from one template, each with the same protocols"`
	Protocols []ProtocolSpec      `json:"protocols,omitempty" jsonschema_description:"Protocols and traffic flows on devices"`
}

func ParseScenario(b []byte) (*Scenario, error) {
	var s Scenario
	if err := yaml.UnmarshalStrict(b, &s); err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	return &s, nil
}

func (e *Engine) Export() *Scenario {
	s := &Scenario{Ports: map[string]PortSpec{}}
	for name, up := range e.ports.Desired() {
		a := "down"
		if up {
			a = "up"
		}
		s.Ports[name] = PortSpec{Admin: a}
	}
	e.mu.RLock()
	for _, d := range e.devices {
		if d.group == "" {
			s.Devices = append(s.Devices, d.spec)
		}
	}
	for _, g := range e.groups {
		s.Groups = append(s.Groups, g.spec)
	}
	for _, in := range e.insts {
		if in.group == "" {
			s.Protocols = append(s.Protocols, in.spec)
		}
	}
	e.mu.RUnlock()
	sortScenario(s)
	return s
}

// YAML writes s with fields in their struct order (a protocol's name,
// type and device before its config), not sorted.
func (s *Scenario) YAML() ([]byte, error) {
	j, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var n yaml3.Node
	if err := yaml3.Unmarshal(j, &n); err != nil {
		return nil, err
	}
	plainStyle(&n)
	var b bytes.Buffer
	enc := yaml3.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// plainStyle drops the JSON styles (flow collections, quoted strings): the
// encoder quotes only the strings that need it.
func plainStyle(n *yaml3.Node) {
	n.Style = 0
	for _, c := range n.Content {
		plainStyle(c)
	}
}

// Apply creates everything in s. With replace, existing devices and
// protocols are removed first. All errors are collected; valid parts of the
// scenario are still applied. Groups are created after devices and before
// protocols, which may run on group members.
func (e *Engine) Apply(s *Scenario, replace bool) error {
	var errs []error
	if replace {
		for _, g := range e.Groups() {
			errs = append(errs, e.DeleteGroup(g.Spec.Name))
		}
		for _, p := range e.Protocols(false) {
			errs = append(errs, e.DeleteProtocol(p.Name))
		}
		for _, d := range e.Devices() {
			errs = append(errs, e.DeleteDevice(d.Name))
		}
	}
	for name, p := range s.Ports {
		switch p.Admin {
		case "":
		case "up", "down":
			errs = append(errs, e.ports.SetAdmin(name, p.Admin == "up"))
		default:
			errs = append(errs, fmt.Errorf("port %s: admin must be up or down", name))
		}
	}
	for _, d := range s.Devices {
		_, err := e.CreateDevice(d)
		errs = append(errs, err)
	}
	for _, g := range s.Groups {
		_, err := e.CreateGroup(g)
		errs = append(errs, err)
	}
	for _, p := range s.Protocols {
		_, err := e.CreateProtocol(p)
		if err != nil {
			err = fmt.Errorf("protocol %s: %w", p.Name, err)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func sortScenario(s *Scenario) {
	sort.Slice(s.Devices, func(i, j int) bool { return s.Devices[i].Name < s.Devices[j].Name })
	sort.Slice(s.Groups, func(i, j int) bool { return s.Groups[i].Name < s.Groups[j].Name })
	sort.Slice(s.Protocols, func(i, j int) bool { return s.Protocols[i].Name < s.Protocols[j].Name })
}

// ScenarioSchema is the JSON schema of a scenario file, for editors. A
// protocol's config is marked "x-config-of": "type": its schema is that of
// the protocol type named by the sibling type field (ListProtocolTypes has
// them). A group's device is the device schema and its configs take
// templates, marked "x-template".
func ScenarioSchema() string {
	var m map[string]any
	if json.Unmarshal([]byte(protocol.Schema(&Scenario{})), &m) != nil {
		return "{}"
	}
	delete(m, "$id")
	var types []any
	for _, t := range protocol.Types() {
		types = append(types, t.Name())
	}
	props := m["properties"].(map[string]any)
	item := func(k string) map[string]any {
		return props[k].(map[string]any)["items"].(map[string]any)
	}
	byType := func(p map[string]any) {
		pp := p["properties"].(map[string]any)
		pp["type"].(map[string]any)["enum"] = types
		c := pp["config"].(map[string]any)
		c["type"] = "object"
		c["x-config-of"] = "type"
	}
	byType(item("protocols"))
	g := item("device_groups")
	g["x-template"] = true
	gp := g["properties"].(map[string]any)
	byType(gp["protocols"].(map[string]any)["items"].(map[string]any))
	dev := map[string]any{}
	for k, v := range item("devices") {
		dev[k] = v
	}
	dev["required"] = []any{"port"}
	dev["description"] = gp["device"].(map[string]any)["description"]
	gp["device"] = dev
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}
