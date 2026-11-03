package rsvp

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Config is an RSVP-TE router (RFC 3209) on a device, IPv4 only. It is the
// head of the LSPs it lists and the tail of those signalled to it. It doesn't
// switch labels between neighbors.
type Config struct {
	RouterID string `json:"router_id" jsonschema:"required" jsonschema_description:"Router ID, a /32 the neighbors can route to (advertise it in IS-IS or OSPF too). The device answers for it, and LSPs to it end here" jsonschema_extras:"x-group=Router"`

	LSPs []LSP `json:"lsps,omitempty" jsonschema_description:"LSPs to signal from here. Flows on this device can send into them by name" jsonschema_extras:"x-summary={name} to {to}[ {protection}]"`

	TailMode   string `json:"tail_mode" jsonschema:"enum=label,enum=implicit-null,enum=explicit-null" jsonschema_description:"What LSPs ending here get. label: a real label each. implicit-null: the previous hop pops it, and sends plain IP. explicit-null: label 0" jsonschema_extras:"x-group=Tail"`
	LabelBase  uint32 `json:"label_base" jsonschema:"minimum=16,maximum=1048575,description=First label handed out to LSPs ending here" jsonschema_extras:"x-group=Tail"`
	RefuseLSPs bool   `json:"refuse_lsps,omitempty" jsonschema:"description=Answer LSPs signalled to us with an admission control error" jsonschema_extras:"x-group=Tail"`

	RefreshInterval int `json:"refresh_interval" jsonschema:"minimum=1,maximum=3600,description=Seconds between refreshes we send (state times out after about 5 refreshes)" jsonschema_extras:"x-group=Timers"`
	HelloInterval   int `json:"hello_interval" jsonschema:"minimum=0,maximum=3600,description=Seconds between hellos to neighbors; 0 for none. A neighbor that has answered is declared down after 3.5 intervals" jsonschema_extras:"x-group=Timers"`
	RetryInterval   int `json:"retry_interval" jsonschema:"minimum=1,maximum=3600,description=Seconds between tries while an LSP isn't up" jsonschema_extras:"x-group=Timers"`
}

// LSP is one LSP signalled from here.
type LSP struct {
	Name          string  `json:"name" jsonschema:"required,description=Flows refer to the LSP by this name"`
	To            string  `json:"to" jsonschema:"required,description=The tail's router ID"`
	TunnelID      uint16  `json:"tunnel_id,omitempty" jsonschema:"description=Default: the LSP's place in the list"`
	Hops          []Hop4  `json:"hops,omitempty" jsonschema_description:"Explicit route. It must start at the next router. Routers may refuse fast reroute unless every hop is strict. Default: wherever the routers send it" jsonschema_extras:"x-summary={address}[ {loose}]"`
	Protection    string  `json:"protection,omitempty" jsonschema:"enum=none,enum=link,enum=node" jsonschema_description:"Fast reroute to ask of the routers on the way (facility backup). Default none"`
	BandwidthMbps float64 `json:"bandwidth_mbps,omitempty" jsonschema:"minimum=0,description=Bandwidth to ask for"`
	SetupPriority *uint8  `json:"setup_priority,omitempty" jsonschema:"minimum=0,maximum=7,description=Default 7" jsonschema_extras:"x-advanced=true"`
	HoldPriority  uint8   `json:"hold_priority,omitempty" jsonschema:"minimum=0,maximum=7,description=Default 0" jsonschema_extras:"x-advanced=true"`
	Shutdown      bool    `json:"shutdown,omitempty" jsonschema:"description=Don't signal it until the signal action"`
}

// Hop4 is a hop of an explicit route.
type Hop4 struct {
	Address string `json:"address" jsonschema:"required,description=A router's interface or router ID"`
	Loose   bool   `json:"loose,omitempty" jsonschema:"description=Other routers may be on the way to it"`
}

func defaultConfig() *Config {
	return &Config{TailMode: "label", LabelBase: 100000, RefreshInterval: 30, HelloInterval: 5, RetryInterval: 5}
}

func v4(s, what string) (netip.Addr, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !a.Is4() {
		return a, fmt.Errorf("%s %q must be an IPv4 address", what, s)
	}
	return a, nil
}

func eroFrom(hops []Hop4) ([]EROHop, error) {
	var out []EROHop
	for _, h := range hops {
		a, err := v4(h.Address, "hop")
		if err != nil {
			return nil, err
		}
		out = append(out, EROHop{Addr: a, Bits: 32, Loose: h.Loose})
	}
	return out, nil
}

func (c *Config) Validate() error {
	if _, err := v4(c.RouterID, "router_id"); err != nil {
		return err
	}
	switch c.TailMode {
	case "label", "implicit-null", "explicit-null":
	default:
		return fmt.Errorf("invalid tail_mode %q", c.TailMode)
	}
	if c.LabelBase < MinLabel || c.LabelBase > MaxLabel {
		return fmt.Errorf("label_base must be %d-%d", MinLabel, MaxLabel)
	}
	if c.RefreshInterval < 1 || c.RefreshInterval > 3600 {
		return errors.New("refresh_interval must be 1-3600")
	}
	if c.HelloInterval < 0 || c.HelloInterval > 3600 {
		return errors.New("hello_interval must be 0-3600")
	}
	if c.RetryInterval < 1 || c.RetryInterval > 3600 {
		return errors.New("retry_interval must be 1-3600")
	}
	names, tunnels := map[string]bool{}, map[uint16]bool{}
	for i, l := range c.LSPs {
		if strings.TrimSpace(l.Name) == "" {
			return errors.New("every LSP needs a name")
		}
		if names[l.Name] {
			return fmt.Errorf("LSP name %q is used twice", l.Name)
		}
		names[l.Name] = true
		if _, err := v4(l.To, "LSP "+l.Name+": to"); err != nil {
			return err
		}
		id := l.tunnelID(i)
		if tunnels[id] {
			return fmt.Errorf("LSP %s: tunnel ID %d is used twice", l.Name, id)
		}
		tunnels[id] = true
		if _, err := eroFrom(l.Hops); err != nil {
			return fmt.Errorf("LSP %s: %w", l.Name, err)
		}
		switch l.Protection {
		case "", "none", "link", "node":
		default:
			return fmt.Errorf("LSP %s: invalid protection %q", l.Name, l.Protection)
		}
		if l.BandwidthMbps < 0 {
			return fmt.Errorf("LSP %s: bandwidth_mbps must not be negative", l.Name)
		}
		if (l.SetupPriority != nil && *l.SetupPriority > 7) || l.HoldPriority > 7 {
			return fmt.Errorf("LSP %s: priorities are 0-7", l.Name)
		}
		if l.setup() < l.HoldPriority {
			return fmt.Errorf("LSP %s: setup_priority must not be better than hold_priority", l.Name)
		}
	}
	return nil
}

func (l LSP) tunnelID(i int) uint16 {
	if l.TunnelID != 0 {
		return l.TunnelID
	}
	return uint16(i + 1)
}

func (l LSP) setup() uint8 {
	if l.SetupPriority != nil {
		return *l.SetupPriority
	}
	return 7
}

// Action arguments.

type SelectArgs struct {
	LSP string `json:"lsp" jsonschema:"required,description=An LSP's name"`
}

type TearArgs struct {
	LSP     string `json:"lsp,omitempty" jsonschema:"description=An LSP's name. Default: every LSP"`
	Seconds int    `json:"seconds,omitempty" jsonschema:"minimum=0,description=Stay away this long. Default: an LSP from here stays down until the signal action; an LSP ending here is answered at its next refresh"`
}

type ResignalArgs struct {
	LSP  string `json:"lsp" jsonschema:"required,description=An LSP from here"`
	Hops []Hop4 `json:"hops,omitempty" jsonschema_description:"A new explicit route. Default: the one it has" jsonschema_extras:"x-summary={address}[ {loose}]"`
}

type ErrorArgs struct {
	LSP         string `json:"lsp,omitempty" jsonschema:"description=An LSP's name. Default: every LSP"`
	Code        uint8  `json:"code" jsonschema:"required" jsonschema_description:"Error code (RFC 2205 and 3209), e.g. 1 admission control failure, 24 routing problem, 25 notify"`
	Value       uint16 `json:"value,omitempty" jsonschema_description:"Error value, e.g. 5 no route with code 24"`
	RemoveState bool   `json:"remove_state,omitempty" jsonschema:"description=Tell the routers on the way to remove their path state (only for an LSP ending here)"`
}

type RelabelArgs struct {
	LSP string `json:"lsp,omitempty" jsonschema:"description=An LSP ending here. Default: every LSP ending here"`
}

type FlapArgs struct {
	Seconds int    `json:"seconds" jsonschema:"minimum=1,description=How long to stay away"`
	Mode    string `json:"mode" jsonschema:"enum=tear,enum=silent" jsonschema_description:"tear: tear LSPs down first. silent: stop sending anything, so the neighbors' timers expire"`
}

func (a *FlapArgs) Validate() error {
	if a.Seconds < 1 {
		return errors.New("seconds must be at least 1")
	}
	if a.Mode != "tear" && a.Mode != "silent" {
		return errors.New("mode must be tear or silent")
	}
	return nil
}
