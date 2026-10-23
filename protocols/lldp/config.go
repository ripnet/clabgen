package lldp

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
)

// Config is one LLDP agent on a device (normally an untagged device on the
// port: LLDP frames are not VLAN tagged).
type Config struct {
	Dest string `json:"dest" jsonschema:"enum=nearest-bridge,enum=nearest-nontpmr,enum=nearest-customer,description=Destination group address (agent scope)"`
	Mode string `json:"mode" jsonschema:"enum=tx-rx,enum=tx-only,enum=rx-only,enum=disabled,description=Admin status"`

	TxInterval int `json:"tx_interval" jsonschema:"minimum=1,maximum=3600,description=Seconds between LLDPDUs (msgTxInterval)"`
	TxHold     int `json:"tx_hold" jsonschema:"minimum=1,maximum=100" jsonschema_description:"TTL = interval × hold (msgTxHold), capped at 65535"`
	FastCount  int `json:"fast_count" jsonschema:"minimum=0,maximum=8,description=LLDPDUs sent 1 s apart at start and when a new neighbor appears (txFastInit)"`

	ChassisIDSubtype string `json:"chassis_id_subtype" jsonschema:"enum=mac,enum=local,enum=network-address,enum=interface-name,enum=interface-alias,enum=chassis-component,enum=port-component"`
	ChassisID        string `json:"chassis_id,omitempty" jsonschema:"description=Default: the device MAC (mac) or the system name"`
	PortIDSubtype    string `json:"port_id_subtype" jsonschema:"enum=interface-name,enum=mac,enum=local,enum=interface-alias,enum=network-address,enum=port-component,enum=agent-circuit-id"`
	PortID           string `json:"port_id,omitempty" jsonschema_description:"Default: the port name (interface-name), the device MAC (mac) or the device name"`

	PortDescription   string   `json:"port_description,omitempty" jsonschema:"description=Default: the device name"`
	SystemName        string   `json:"system_name,omitempty" jsonschema:"description=Default: the agent's hostname"`
	SystemDescription string   `json:"system_description,omitempty" jsonschema:"description=Default: clabgen and its version"`
	Capabilities      []string `json:"capabilities,omitempty" jsonschema:"description=System capabilities: other repeater bridge wlan-ap router telephone docsis station c-vlan s-vlan tpmr. Default router"`
	EnabledCaps       []string `json:"enabled_capabilities,omitempty" jsonschema:"description=Enabled capabilities. Default: all of capabilities"`
	// "auto" (the default when empty) advertises the device's addresses.
	MgmtAddresses []string `json:"management_addresses,omitempty" jsonschema:"description=Management addresses; empty: the device's IP addresses; none: no Management Address TLVs"`
	NoOptional    bool     `json:"no_optional_tlvs,omitempty" jsonschema:"description=Send only the mandatory TLVs"`

	// 802.1 / 802.3 organizationally specific TLVs.
	PortVLANID   uint16      `json:"port_vlan_id,omitempty" jsonschema:"maximum=4094,description=802.1 Port VLAN ID TLV (0: none)"`
	VLANNames    []VLANName  `json:"vlan_names,omitempty" jsonschema:"description=802.1 VLAN Name TLVs"`
	MaxFrameSize uint16      `json:"max_frame_size,omitempty" jsonschema:"description=802.3 Maximum Frame Size TLV (0: none)"`
	MACPHY       *MACPHY     `json:"mac_phy,omitempty" jsonschema:"description=802.3 MAC/PHY Configuration/Status TLV"`
	LinkAgg      *LinkAgg    `json:"link_aggregation,omitempty" jsonschema:"description=802.1 Link Aggregation TLV"`
	CustomTLVs   []CustomTLV `json:"custom_tlvs,omitempty" jsonschema_description:"Extra TLVs (any type, or org-specific with an OUI), e.g. for negative testing"`
}

type MACPHY struct {
	AutonegSupported bool   `json:"autoneg_supported"`
	AutonegEnabled   bool   `json:"autoneg_enabled"`
	Advertised       uint16 `json:"advertised_capabilities,omitempty" jsonschema:"description=PMD auto-negotiation advertised capability bits"`
	MAUType          uint16 `json:"mau_type" jsonschema_description:"Operational MAU type (IANA dot3MauType, e.g. 16 = 100BASE-TX FD, 30 = 1000BASE-T FD)"`
}

type LinkAgg struct {
	Capable   bool   `json:"capable"`
	Enabled   bool   `json:"enabled"`
	AggPortID uint32 `json:"aggregated_port_id,omitempty"`
}

type CustomTLV struct {
	Type    int    `json:"type" jsonschema:"minimum=0,maximum=127,description=TLV type (127: organizationally specific)"`
	OUI     string `json:"oui,omitempty" jsonschema:"description=For type 127: e.g. 00-80-c2"`
	Subtype int    `json:"subtype,omitempty" jsonschema:"maximum=255"`
	Value   string `json:"value" jsonschema:"description=Hex bytes"`
}

func defaultConfig() *Config {
	return &Config{Dest: "nearest-bridge", Mode: "tx-rx", TxInterval: 30, TxHold: 4, FastCount: 4,
		ChassisIDSubtype: "mac", PortIDSubtype: "interface-name"}
}

var dests = map[string]bool{"nearest-bridge": true, "nearest-nontpmr": true, "nearest-customer": true}

func (c *Config) Validate() error {
	if !dests[c.Dest] {
		return fmt.Errorf("invalid dest %q", c.Dest)
	}
	switch c.Mode {
	case "tx-rx", "tx-only", "rx-only", "disabled":
	default:
		return fmt.Errorf("invalid mode %q", c.Mode)
	}
	if c.TxInterval < 1 || c.TxInterval > 3600 || c.TxHold < 1 || c.TxHold > 100 || c.FastCount < 0 || c.FastCount > 8 {
		return errors.New("tx_interval 1-3600, tx_hold 1-100, fast_count 0-8")
	}
	cs, ok := chassisSubtypes[c.ChassisIDSubtype]
	if !ok {
		return fmt.Errorf("invalid chassis_id_subtype %q", c.ChassisIDSubtype)
	}
	if c.ChassisID != "" {
		if _, err := idValue(cs == chassisSubtypes["mac"], cs == chassisSubtypes["network-address"], c.ChassisID); err != nil {
			return fmt.Errorf("chassis_id: %w", err)
		}
	} else if cs == chassisSubtypes["network-address"] {
		return errors.New("chassis_id: network-address needs an address")
	}
	ps, ok := portSubtypes[c.PortIDSubtype]
	if !ok {
		return fmt.Errorf("invalid port_id_subtype %q", c.PortIDSubtype)
	}
	if c.PortID != "" {
		if _, err := idValue(ps == portSubtypes["mac"], ps == portSubtypes["network-address"], c.PortID); err != nil {
			return fmt.Errorf("port_id: %w", err)
		}
	} else if ps == portSubtypes["network-address"] {
		return errors.New("port_id: network-address needs an address")
	}
	if _, err := capMask(c.Capabilities); err != nil {
		return err
	}
	if _, err := capMask(c.EnabledCaps); err != nil {
		return err
	}
	for _, a := range c.MgmtAddresses {
		if a == "none" || a == "auto" {
			continue
		}
		if _, err := netip.ParseAddr(a); err != nil {
			return fmt.Errorf("management address %q: %w", a, err)
		}
	}
	for _, n := range c.VLANNames {
		if n.VID < 1 || n.VID > 4094 || len(n.Name) > 32 {
			return fmt.Errorf("vlan name %d %q: VID 1-4094, name up to 32 bytes", n.VID, n.Name)
		}
	}
	if c.PortVLANID > 4094 {
		return errors.New("port_vlan_id must be 0-4094")
	}
	for _, t := range c.CustomTLVs {
		if _, _, err := t.encode(); err != nil {
			return err
		}
	}
	return nil
}

// encode returns a custom TLV's type and value.
func (t CustomTLV) encode() (int, []byte, error) {
	v, err := hex.DecodeString(t.Value)
	if err != nil {
		return 0, nil, fmt.Errorf("custom TLV value %q is not hex", t.Value)
	}
	if t.Type < 0 || t.Type > 127 {
		return 0, nil, errors.New("custom TLV type must be 0-127")
	}
	if t.Type == tlvOrg {
		o, err := hex.DecodeString(stripDashes(t.OUI))
		if err != nil || len(o) != 3 || t.Subtype < 0 || t.Subtype > 255 {
			return 0, nil, fmt.Errorf("custom org TLV: OUI %q must be 3 bytes (e.g. 00-80-c2), subtype 0-255", t.OUI)
		}
		v = append(append(o, byte(t.Subtype)), v...)
	}
	if len(v) > 511 {
		return 0, nil, errors.New("custom TLV longer than 511 bytes")
	}
	return t.Type, v, nil
}

func stripDashes(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '-' && s[i] != ':' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// Action args.
type PauseArgs struct {
	Seconds int `json:"seconds" jsonschema:"minimum=1,maximum=86400,description=Stop transmitting for this long (neighbors age us out)"`
}

func (a *PauseArgs) Validate() error {
	if a.Seconds < 1 || a.Seconds > 86400 {
		return errors.New("seconds must be 1-86400")
	}
	return nil
}
