package ldp

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/ripnet/clabgen/internal/prefixset"
)

// Config is an LDP router (RFC 5036) on a device, IPv4 only: the far end of
// LSPs. It advertises labels and takes traffic for them; it doesn't switch.
type Config struct {
	RouterID         string `json:"router_id" jsonschema:"required" jsonschema_description:"LSR ID, a /32 the peers can route to (advertise it in IS-IS or OSPF too). The device answers for it" jsonschema_extras:"x-group=Router"`
	TransportAddress string `json:"transport_address,omitempty" jsonschema:"description=Address LDP sessions use. Default: the router ID" jsonschema_extras:"x-group=Router,x-advanced=true"`

	FECs      []FEC  `json:"fecs,omitempty" jsonschema:"description=Prefixes to advertise labels for. The device answers for them; the peers also need routes to them" jsonschema_extras:"x-summary={prefix}[ ×{count}][ {mode}]"`
	LabelBase uint32 `json:"label_base" jsonschema:"minimum=16,maximum=1048575,description=First label handed out to FECs without their own" jsonschema_extras:"x-group=Labels"`

	Targeted       []Targeted `json:"targeted,omitempty" jsonschema_description:"Peers to reach with targeted hellos, not only those on the link" jsonschema_extras:"x-summary={address}"`
	AcceptTargeted bool       `json:"accept_targeted,omitempty" jsonschema:"description=Answer targeted hellos from peers we don't list" jsonschema_extras:"x-group=Discovery"`

	HelloInterval         int  `json:"hello_interval" jsonschema:"minimum=1,maximum=65534,description=Seconds between link hellos" jsonschema_extras:"x-group=Discovery"`
	HoldTime              int  `json:"hold_time" jsonschema:"minimum=1,maximum=65534,description=Link hello hold time in seconds" jsonschema_extras:"x-group=Discovery"`
	TargetedHelloInterval int  `json:"targeted_hello_interval" jsonschema:"minimum=1,maximum=65534,description=Seconds between targeted hellos" jsonschema_extras:"x-group=Discovery"`
	TargetedHoldTime      int  `json:"targeted_hold_time" jsonschema:"minimum=1,maximum=65534,description=Targeted hello hold time in seconds" jsonschema_extras:"x-group=Discovery"`
	NoLinkHellos          bool `json:"no_link_hellos,omitempty" jsonschema:"description=Targeted sessions only" jsonschema_extras:"x-group=Discovery,x-advanced=true"`

	KeepAlive int `json:"keepalive" jsonschema:"minimum=1,maximum=65535,description=Session keepalive time in seconds (the lower of the two sides' is used)" jsonschema_extras:"x-group=Session"`
	MaxPDU    int `json:"max_pdu,omitempty" jsonschema:"minimum=256,maximum=65535,description=Largest PDU we accept. Default 4096" jsonschema_extras:"x-group=Session,x-advanced=true"`
}

// Targeted is a peer reached with targeted hellos.
type Targeted struct {
	Address string `json:"address" jsonschema:"required,description=The peer's address"`
}

// FEC is count consecutive prefixes and how labels are given to them.
type FEC struct {
	Prefix string `json:"prefix" jsonschema:"required,description=e.g. 192.0.2.2/32 or 198.51.100.0/24"`
	Count  int    `json:"count,omitempty" jsonschema:"minimum=1,description=Consecutive prefixes of this length. Default 1"`
	Mode   string `json:"mode,omitempty" jsonschema:"enum=label,enum=implicit-null,enum=explicit-null" jsonschema_description:"label: a real label each (the default). implicit-null: the peer pops it, and sends plain IP. explicit-null: label 0"`
	Label  uint32 `json:"label,omitempty" jsonschema:"maximum=1048575,description=First label for mode label; one more per prefix. Default: from label_base"`
}

const maxFECs = 100000

func defaultConfig() *Config {
	return &Config{LabelBase: 100000, HelloInterval: 5, HoldTime: 15, TargetedHelloInterval: 15, TargetedHoldTime: 45, KeepAlive: 30}
}

func v4(s, what string) (netip.Addr, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !a.Is4() {
		return a, fmt.Errorf("%s %q must be an IPv4 address", what, s)
	}
	return a, nil
}

func (c *Config) Validate() error {
	if _, err := v4(c.RouterID, "router_id"); err != nil {
		return err
	}
	if c.TransportAddress != "" {
		if _, err := v4(c.TransportAddress, "transport_address"); err != nil {
			return err
		}
	}
	if c.LabelBase < MinLabel || c.LabelBase > MaxLabel {
		return fmt.Errorf("label_base must be %d-%d", MinLabel, MaxLabel)
	}
	if c.HelloInterval < 1 || c.HoldTime <= c.HelloInterval || c.HoldTime > 65534 {
		return errors.New("hold_time must be above hello_interval (and at most 65534)")
	}
	if c.TargetedHelloInterval < 1 || c.TargetedHoldTime <= c.TargetedHelloInterval || c.TargetedHoldTime > 65534 {
		return errors.New("targeted_hold_time must be above targeted_hello_interval (and at most 65534)")
	}
	if c.KeepAlive < 1 || c.KeepAlive > 65535 {
		return errors.New("keepalive must be 1-65535")
	}
	if c.MaxPDU != 0 && (c.MaxPDU < 256 || c.MaxPDU > 65535) {
		return errors.New("max_pdu must be 256-65535")
	}
	for _, t := range c.Targeted {
		if _, err := v4(t.Address, "targeted address"); err != nil {
			return err
		}
	}
	if c.NoLinkHellos && len(c.Targeted) == 0 && !c.AcceptTargeted {
		return errors.New("no_link_hellos needs targeted peers or accept_targeted")
	}
	_, err := expandFECs(c.FECs, c.LabelBase, nil)
	return err
}

// binding is a prefix we advertise and its label.
type binding struct {
	prefix netip.Prefix
	label  uint32
}

// expandFECs lists the prefixes with their labels, handing out labels from
// base that used (labels already taken; nil: none) doesn't hold.
func expandFECs(fecs []FEC, base uint32, used map[uint32]bool) ([]binding, error) {
	if used == nil {
		used = map[uint32]bool{}
	}
	next := base
	var out []binding
	for _, f := range fecs {
		p, err := netip.ParsePrefix(strings.TrimSpace(f.Prefix))
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("FEC %q must be an IPv4 prefix", f.Prefix)
		}
		p = p.Masked()
		n := f.Count
		if n == 0 {
			n = 1
		}
		if n < 0 || len(out)+n > maxFECs {
			return nil, fmt.Errorf("at most %d FECs", maxFECs)
		}
		mode := f.Mode
		if mode == "" {
			mode = "label"
		}
		switch mode {
		case "label":
			if f.Label != 0 && (f.Label < MinLabel || int(f.Label)+n-1 > int(MaxLabel)) {
				return nil, fmt.Errorf("FEC %s: labels must be %d-%d", f.Prefix, MinLabel, MaxLabel)
			}
		case "implicit-null", "explicit-null":
			if f.Label != 0 {
				return nil, fmt.Errorf("FEC %s: label is for mode label", f.Prefix)
			}
		default:
			return nil, fmt.Errorf("FEC %s: invalid mode %q", f.Prefix, mode)
		}
		for i := 0; i < n; i++ {
			if i > 0 {
				nx := prefixset.Last(p).Next()
				if !nx.IsValid() || !nx.Is4() {
					return nil, fmt.Errorf("FEC %s: count %d runs past the address space", f.Prefix, n)
				}
				p = netip.PrefixFrom(nx, p.Bits())
			}
			b := binding{prefix: p}
			switch mode {
			case "implicit-null":
				b.label = LabelImplicitNull
			case "explicit-null":
				b.label = LabelExplicitNull
			default:
				if f.Label != 0 {
					b.label = f.Label + uint32(i)
				} else {
					for used[next] {
						next++
					}
					if next > MaxLabel {
						return nil, errors.New("out of labels from label_base")
					}
					b.label = next
					next++
				}
				if used[b.label] {
					return nil, fmt.Errorf("FEC %s: label %d is used twice", b.prefix, b.label)
				}
				used[b.label] = true
			}
			out = append(out, b)
		}
	}
	return out, nil
}

// Action arguments.

type FlapArgs struct {
	Peer    string `json:"peer,omitempty" jsonschema:"description=LSR ID. Default: every peer"`
	Seconds int    `json:"seconds" jsonschema:"minimum=1,description=How long to stay away"`
	Mode    string `json:"mode" jsonschema:"enum=notify,enum=silent" jsonschema_description:"notify: end the session with a shutdown notification. silent: stop sending anything, so the peer's timers expire"`
}

func (a *FlapArgs) Validate() error {
	if a.Seconds < 1 {
		return errors.New("seconds must be at least 1")
	}
	if a.Mode != "notify" && a.Mode != "silent" {
		return errors.New("mode must be notify or silent")
	}
	return nil
}

type ResetArgs struct {
	Peer string `json:"peer,omitempty" jsonschema:"description=LSR ID. Default: every peer"`
}

type FECArgs struct {
	FECs []FEC `json:"fecs" jsonschema:"required" jsonschema_extras:"x-summary={prefix}[ ×{count}][ {mode}]"`
}

type NotifyArgs struct {
	Peer  string `json:"peer,omitempty" jsonschema:"description=LSR ID. Default: every peer"`
	Code  uint32 `json:"code" jsonschema:"required" jsonschema_description:"Status code (RFC 5036 section 3.9) without the E and F bits, e.g. 10 for shutdown"`
	Fatal bool   `json:"fatal,omitempty" jsonschema_description:"Set the E bit, and end the session"`
}

type BindingsArgs struct {
	Peer   string `json:"peer" jsonschema:"required,description=LSR ID"`
	Prefix string `json:"prefix,omitempty" jsonschema:"description=Only prefixes within this one"`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=At most this many. Default 1000"`
}
