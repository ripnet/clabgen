package cfm

import (
	"errors"
	"fmt"
)

// Config is one down-MEP. JSON names follow SR OS terminology where it has one.
type Config struct {
	Level    uint8  `json:"level" jsonschema:"minimum=0,maximum=7,description=Maintenance domain level"`
	MEPID    uint16 `json:"mep_id" jsonschema:"minimum=1,maximum=8191"`
	MDFormat string `json:"md_format" jsonschema:"enum=none,enum=dns,enum=mac-int,enum=string"`
	MDName   string `json:"md_name,omitempty" jsonschema:"description=Empty when md_format is none. mac-int is MAC:integer."`
	MAFormat string `json:"ma_format" jsonschema:"enum=string,enum=icc,enum=primary-vid,enum=integer,enum=vpn-id"`
	MAName   string `json:"ma_name" jsonschema:"description=icc: up to 13 chars (Y.1731 MEG ID). vpn-id: OUI:index in hex."`

	CCMEnabled  bool   `json:"ccm_enabled"`
	CCMInterval string `json:"ccm_interval" jsonschema:"enum=3.33ms,enum=10ms,enum=100ms,enum=1s,enum=10s,enum=1m,enum=10m,description=Aliases 60s and 600s are accepted"`
	CCMPriority uint8  `json:"ccm_priority" jsonschema:"minimum=0,maximum=7,description=PCP for CCM and LTM frames (applies to all VLAN tags)"`

	// Empty means auto-discover: accept any MEPID and learn it.
	RemoteMEPs []uint16 `json:"remote_meps,omitempty" jsonschema:"description=Expected remote MEP IDs; empty enables auto-discovery"`

	PortStatusTLV      bool `json:"port_status_tlv,omitempty" jsonschema:"description=Include Port Status TLV in CCMs"`
	InterfaceStatusTLV bool `json:"interface_status_tlv,omitempty" jsonschema:"description=Include Interface Status TLV in CCMs"`

	// Responder controls, for negative testing.
	DisableLBR bool `json:"disable_lbr,omitempty"`
	DisableLTR bool `json:"disable_ltr,omitempty"`
	DisableDMR bool `json:"disable_dmr,omitempty"`
	DisableSLR bool `json:"disable_slr,omitempty"`
}

func defaultConfig() *Config {
	return &Config{Level: 0, MEPID: 1, MDFormat: "none", MAFormat: "string", MAName: "clabgen",
		CCMEnabled: true, CCMInterval: "1s", CCMPriority: 7}
}

func (c *Config) Validate() error {
	if c.Level > 7 {
		return errors.New("level must be 0-7")
	}
	if c.MEPID < 1 || c.MEPID > 8191 {
		return errors.New("mep_id must be 1-8191")
	}
	if c.CCMPriority > 7 {
		return errors.New("ccm_priority must be 0-7")
	}
	if _, _, ok := intervalByName(c.CCMInterval); !ok {
		return fmt.Errorf("invalid ccm_interval %q", c.CCMInterval)
	}
	for _, r := range c.RemoteMEPs {
		if r < 1 || r > 8191 {
			return fmt.Errorf("remote MEP ID %d out of range", r)
		}
		if r == c.MEPID {
			return errors.New("remote_meps must not contain the local mep_id")
		}
	}
	_, err := encodeMAID(c.MDFormat, c.MDName, c.MAFormat, c.MAName)
	return err
}

// Target selects a remote MEP for on-demand tools.
type Target struct {
	TargetMAC   string `json:"target_mac,omitempty" jsonschema:"description=Destination MAC"`
	TargetMEPID uint16 `json:"target_mep_id,omitempty" jsonschema:"description=Remote MEP ID (MAC learned from its CCMs)"`
	Priority    *uint8 `json:"priority,omitempty" jsonschema:"minimum=0,maximum=7"`
}

type LoopbackArgs struct {
	Target
	Count      int `json:"count" jsonschema:"minimum=1,maximum=10000"`
	IntervalMS int `json:"interval_ms" jsonschema:"minimum=1"`
	TimeoutMS  int `json:"timeout_ms" jsonschema:"minimum=1"`
	DataSize   int `json:"data_size,omitempty" jsonschema:"minimum=0,maximum=1400,description=Bytes of Data TLV"`
}

type LinktraceArgs struct {
	Target
	TTL       uint8 `json:"ttl" jsonschema:"minimum=1,maximum=255"`
	TimeoutMS int   `json:"timeout_ms" jsonschema:"minimum=1"`
}

type DelayArgs struct {
	Target
	Count      int `json:"count" jsonschema:"minimum=1,maximum=10000"`
	IntervalMS int `json:"interval_ms" jsonschema:"minimum=1"`
	TimeoutMS  int `json:"timeout_ms" jsonschema:"minimum=1"`
}

type SyntheticLossArgs struct {
	Target
	Count      int    `json:"count" jsonschema:"minimum=1,maximum=100000"`
	IntervalMS int    `json:"interval_ms" jsonschema:"minimum=1"`
	TimeoutMS  int    `json:"timeout_ms" jsonschema:"minimum=1,description=Wait after the last SLM"`
	TestID     uint32 `json:"test_id,omitempty"`
}

// FaultArgs changes the MEP's transmitted state. Omitted fields are unchanged.
type FaultArgs struct {
	CCMEnabled      *bool   `json:"ccm_enabled,omitempty" jsonschema:"description=Stop/start CCM transmission (simulates LOC at the peer)"`
	ForceRDI        *bool   `json:"force_rdi,omitempty" jsonschema:"description=Set RDI in CCMs regardless of local defects"`
	PortStatus      *string `json:"port_status,omitempty" jsonschema:"enum=up,enum=blocked"`
	InterfaceStatus *string `json:"interface_status,omitempty" jsonschema:"enum=up,enum=down,enum=testing,enum=unknown,enum=dormant,enum=notPresent,enum=lowerLayerDown"`
	SkipSequence    uint32  `json:"skip_sequence,omitempty" jsonschema:"description=Add N to the CCM sequence number once (peer sees out-of-sequence)"`
	MEPIDOverride   *uint16 `json:"mep_id_override,omitempty" jsonschema:"description=Send CCMs with this MEP ID (0 restores); provokes DefErrorCCM"`
	MAIDOverride    *string `json:"ma_name_override,omitempty" jsonschema:"description=Send CCMs with this MA name in the configured format (empty restores); provokes DefXconCCM"`
	LevelOverride   *int    `json:"level_override,omitempty" jsonschema:"minimum=-1,maximum=7,description=Send CCMs at this level (-1 restores)"`
}

type ClearArgs struct{}

func validTarget(t Target) error {
	if t.Priority != nil && *t.Priority > 7 {
		return errors.New("priority must be 0-7")
	}
	return nil
}

func (a *LoopbackArgs) Validate() error {
	if a.Count < 1 || a.IntervalMS < 1 || a.TimeoutMS < 1 {
		return errors.New("count, interval_ms and timeout_ms must be positive")
	}
	if a.DataSize < 0 || a.DataSize > 1400 {
		return errors.New("data_size must be 0-1400")
	}
	return validTarget(a.Target)
}

func (a *LinktraceArgs) Validate() error {
	if a.TTL == 0 || a.TimeoutMS < 1 {
		return errors.New("ttl and timeout_ms must be positive")
	}
	return validTarget(a.Target)
}

func (a *DelayArgs) Validate() error {
	if a.Count < 1 || a.IntervalMS < 1 || a.TimeoutMS < 1 {
		return errors.New("count, interval_ms and timeout_ms must be positive")
	}
	return validTarget(a.Target)
}

func (a *SyntheticLossArgs) Validate() error {
	if a.Count < 1 || a.IntervalMS < 1 || a.TimeoutMS < 1 {
		return errors.New("count, interval_ms and timeout_ms must be positive")
	}
	return validTarget(a.Target)
}

func (a *FaultArgs) Validate() error {
	if a.PortStatus != nil {
		if _, ok := lookupName(portStatusNames, *a.PortStatus); !ok {
			return fmt.Errorf("invalid port_status %q", *a.PortStatus)
		}
	}
	if a.InterfaceStatus != nil {
		if _, ok := lookupName(intfStatusNames, *a.InterfaceStatus); !ok {
			return fmt.Errorf("invalid interface_status %q", *a.InterfaceStatus)
		}
	}
	if a.LevelOverride != nil && (*a.LevelOverride < -1 || *a.LevelOverride > 7) {
		return errors.New("level_override must be -1..7")
	}
	return nil
}
