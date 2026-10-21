package core

import (
	"errors"
	"fmt"
	"sort"

	"sigs.k8s.io/yaml"
)

type PortSpec struct {
	Admin string `json:"admin,omitempty"` // "up" or "down"
}

// Scenario is the declarative form of an agent's configuration.
type Scenario struct {
	Ports     map[string]PortSpec `json:"ports,omitempty"`
	Devices   []DeviceSpec        `json:"devices,omitempty"`
	Protocols []ProtocolSpec      `json:"protocols,omitempty"`
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
		s.Devices = append(s.Devices, d.spec)
	}
	for _, in := range e.insts {
		s.Protocols = append(s.Protocols, in.spec)
	}
	e.mu.RUnlock()
	sortScenario(s)
	return s
}

func (s *Scenario) YAML() ([]byte, error) { return yaml.Marshal(s) }

// Apply creates everything in s. With replace, existing devices and
// protocols are removed first. All errors are collected; valid parts of the
// scenario are still applied.
func (e *Engine) Apply(s *Scenario, replace bool) error {
	var errs []error
	if replace {
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
	sort.Slice(s.Protocols, func(i, j int) bool { return s.Protocols[i].Name < s.Protocols[j].Name })
}
