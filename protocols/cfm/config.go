package cfm

import (
	"errors"
	"fmt"
)

// Config is one down-MEP. JSON names follow SR OS terminology where it has one.
type Config struct {
	Level    uint8  `json:"level" jsonschema:"minimum=0,maximum=7,description=Maintenance domain level" jsonschema_extras:"x-group=Identity"`
	MEPID    uint16 `json:"mep_id" jsonschema:"required,minimum=1,maximum=8191" jsonschema_extras:"x-group=Identity"`
	MDFormat string `json:"md_format" jsonschema:"enum=none,enum=dns,enum=mac-int,enum=string" jsonschema_extras:"x-group=Identity"`
	MDName   string `json:"md_name,omitempty" jsonschema:"description=Empty when md_format is none. mac-int is MAC:integer." jsonschema_extras:"x-group=Identity"`
	MAFormat string `json:"ma_format" jsonschema:"enum=string,enum=icc,enum=primary-vid,enum=integer,enum=vpn-id" jsonschema_extras:"x-group=Identity"`
	MAName   string `json:"ma_name" jsonschema:"description=icc: up to 13 chars (Y.1731 MEG ID). vpn-id: OUI:index in hex." jsonschema_extras:"x-group=Identity"`

	CCMEnabled  bool   `json:"ccm_enabled" jsonschema_extras:"x-group=CCM"`
	CCMInterval string `json:"ccm_interval" jsonschema:"enum=3.33ms,enum=10ms,enum=100ms,enum=1s,enum=10s,enum=1m,enum=10m,description=Aliases 60s and 600s are accepted" jsonschema_extras:"x-group=CCM"`
	CCMPriority uint8  `json:"ccm_priority" jsonschema:"minimum=0,maximum=7,description=PCP for CCM and LTM frames (applies to all VLAN tags)" jsonschema_extras:"x-group=CCM"`

	// Empty means auto-discover: accept any MEPID and learn it.
	RemoteMEPs []uint16 `json:"remote_meps,omitempty" jsonschema:"description=Expected remote MEP IDs; empty enables auto-discovery" jsonschema_extras:"x-group=Identity"`
	// MEPs on the same port of this agent (a device group's members) hear
	// each other's CCMs, which remote sites behind a router's down MEP
	// wouldn't: they are ignored unless ignore_local_meps is off.
	IgnoreLocalMEPs   bool `json:"ignore_local_meps" jsonschema:"description=Ignore CCMs from other MEPs on the same port of this agent (e.g. a device group's members): they don't count as remote MEPs or errors" jsonschema_extras:"x-group=Identity,x-advanced=true"`
	IgnoreUnknownMEPs bool `json:"ignore_unknown_meps,omitempty" jsonschema:"description=With remote_meps set: ignore CCMs from other MEP IDs instead of raising DefErrorCCM" jsonschema_extras:"x-group=Identity,x-advanced=true"`

	PortStatusTLV      bool `json:"port_status_tlv,omitempty" jsonschema:"description=Include Port Status TLV in CCMs" jsonschema_extras:"x-group=CCM"`
	InterfaceStatusTLV bool `json:"interface_status_tlv,omitempty" jsonschema:"description=Include Interface Status TLV in CCMs" jsonschema_extras:"x-group=CCM"`

	// More TLVs in CCMs.
	SenderID   string  `json:"sender_id,omitempty" jsonschema_description:"Chassis ID (subtype local) for a Sender ID TLV in CCMs, LBMs and LTMs; empty: none" jsonschema_extras:"x-group=CCM,x-advanced=true"`
	OrgTLV     *OrgTLV `json:"org_tlv,omitempty" jsonschema:"description=An Organization-Specific TLV in CCMs"`
	CCMPadding int     `json:"ccm_padding,omitempty" jsonschema:"minimum=0,maximum=1400,description=Bytes of Data TLV padding in CCMs" jsonschema_extras:"x-group=CCM,x-advanced=true"`

	// AIS (Y.1731 7.4) toward the client level while we have a defect.
	AIS AISConfig `json:"ais" jsonschema_description:"AIS (and LCK) toward the client MEG level: AIS while the MEP has a defect (or forced), LCK while locked (fault action)"`

	// Proactive measurement sessions with history.
	Sessions []Session `json:"sessions,omitempty" jsonschema_description:"Continuous delay / synthetic loss / frame loss measurements, kept in bins" jsonschema_extras:"x-summary={name}: {type}[ → MEP {target_mep_id}][ → {target_mac}]"`

	// Responder controls, for negative testing.
	DisableLBR bool `json:"disable_lbr,omitempty" jsonschema_extras:"x-group=Responders,x-advanced=true"`
	DisableLTR bool `json:"disable_ltr,omitempty" jsonschema_extras:"x-group=Responders,x-advanced=true"`
	DisableDMR bool `json:"disable_dmr,omitempty" jsonschema_extras:"x-group=Responders,x-advanced=true"`
	DisableSLR bool `json:"disable_slr,omitempty" jsonschema_extras:"x-group=Responders,x-advanced=true"`
}

type OrgTLV struct {
	OUI     string `json:"oui" jsonschema:"required,description=e.g. 00:16:4d"`
	Subtype uint8  `json:"subtype"`
	Value   string `json:"value,omitempty" jsonschema:"description=Hex bytes"`
}

type AISConfig struct {
	Enabled     bool   `json:"enabled"`
	ClientLevel uint8  `json:"client_level" jsonschema:"minimum=0,maximum=7,description=MEG level AIS and LCK are sent at (above ours). Default: our level + 1"`
	Interval    string `json:"interval" jsonschema:"enum=1s,enum=1m,description=Transmission period"`
	Priority    *uint8 `json:"priority,omitempty" jsonschema:"minimum=0,maximum=7,description=Default: ccm_priority"`
}

// Session is a proactive measurement: one PDU per interval toward a remote
// MEP, results summed in bins of bin_s seconds.
type Session struct {
	Name string `json:"name" jsonschema:"required"`
	Type string `json:"type" jsonschema:"required,enum=delay,enum=synthetic-loss,enum=loss,description=delay: DMM; synthetic-loss: SLM; loss: LMM (data frames)"`
	Target
	IntervalMS int    `json:"interval_ms,omitempty" jsonschema:"minimum=10,description=Default 1000"`
	BinS       int    `json:"bin_s,omitempty" jsonschema:"minimum=1,maximum=86400,description=Measurement interval. Default 60"`
	Bins       int    `json:"bins,omitempty" jsonschema:"minimum=1,maximum=96,description=Completed bins kept. Default 10"`
	TestID     uint32 `json:"test_id,omitempty" jsonschema_extras:"x-show-if=type=synthetic-loss"`
}

func defaultConfig() *Config {
	return &Config{Level: 0, MEPID: 1, MDFormat: "none", MAFormat: "string", MAName: "clabgen",
		CCMEnabled: true, IgnoreLocalMEPs: true, CCMInterval: "1s", CCMPriority: 7, AIS: AISConfig{Interval: "1s"}}
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
	if _, err := encodeMAID(c.MDFormat, c.MDName, c.MAFormat, c.MAName); err != nil {
		return err
	}
	if len(c.SenderID) > 255 {
		return errors.New("sender_id: at most 255 bytes")
	}
	if c.CCMPadding < 0 || c.CCMPadding > 1400 {
		return errors.New("ccm_padding must be 0-1400")
	}
	if c.OrgTLV != nil {
		if _, err := c.OrgTLV.encode(); err != nil {
			return err
		}
	}
	if a := c.AIS; a.Enabled || a.ClientLevel != 0 {
		if a.ClientLevel != 0 && a.ClientLevel <= c.Level || a.ClientLevel > 7 {
			return errors.New("ais: client_level must be above the MEP's level (and at most 7)")
		}
		if c.Level == 7 && a.ClientLevel == 0 {
			return errors.New("ais: a level-7 MEP has no client level")
		}
	}
	if c.AIS.Interval != "1s" && c.AIS.Interval != "1m" {
		return errors.New("ais: interval must be 1s or 1m")
	}
	if c.AIS.Priority != nil && *c.AIS.Priority > 7 {
		return errors.New("ais: priority must be 0-7")
	}
	seen := map[string]bool{}
	for _, x := range c.Sessions {
		if x.Name == "" || seen[x.Name] {
			return fmt.Errorf("sessions: name missing or repeated (%q)", x.Name)
		}
		seen[x.Name] = true
		if x.Type != "delay" && x.Type != "synthetic-loss" && x.Type != "loss" {
			return fmt.Errorf("session %s: type must be delay, synthetic-loss or loss", x.Name)
		}
		if x.TargetMAC == "" && x.TargetMEPID == 0 {
			return fmt.Errorf("session %s: target_mac or target_mep_id is required", x.Name)
		}
		if x.IntervalMS != 0 && x.IntervalMS < 10 || x.BinS < 0 || x.BinS > 86400 || x.Bins < 0 || x.Bins > 96 {
			return fmt.Errorf("session %s: interval_ms ≥ 10, bin_s 1-86400, bins 1-96", x.Name)
		}
		if err := validTarget(x.Target); err != nil {
			return fmt.Errorf("session %s: %w", x.Name, err)
		}
	}
	return nil
}

// clientLevel is where AIS and LCK go.
func (c *Config) clientLevel() uint8 {
	if c.AIS.ClientLevel != 0 {
		return c.AIS.ClientLevel
	}
	return min(c.Level+1, 7)
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
	ForceAIS        *bool   `json:"force_ais,omitempty" jsonschema:"description=Send AIS to the client level regardless of defects"`
	Lock            *bool   `json:"lock,omitempty" jsonschema:"description=Administrative lock: send LCK to the client level"`
	CSF             *string `json:"csf,omitempty" jsonschema:"enum=none,enum=los,enum=fdi,enum=rdi,enum=dci,description=Send CSF (client signal fail) of this type to the peer MEPs (none stops)"`
}

type ClearArgs struct{}

type OneWayDelayArgs struct {
	Target
	Count      int `json:"count" jsonschema:"minimum=1,maximum=10000"`
	IntervalMS int `json:"interval_ms" jsonschema:"minimum=1"`
}

type TestSignalArgs struct {
	Target
	Count      int    `json:"count" jsonschema:"minimum=1,maximum=100000"`
	IntervalMS int    `json:"interval_ms" jsonschema:"minimum=1"`
	Size       int    `json:"size,omitempty" jsonschema:"minimum=0,maximum=1400,description=Bytes of test pattern"`
	Pattern    string `json:"pattern,omitempty" jsonschema:"enum=null,enum=prbs,description=All zeros or PRBS 2^31-1. Default null"`
	CRC        bool   `json:"crc,omitempty" jsonschema:"description=Add a CRC-32 over the pattern (receivers count bit errors)"`
}

type LossArgs struct {
	Target
	Count      int `json:"count" jsonschema:"minimum=2,maximum=100000"`
	IntervalMS int `json:"interval_ms" jsonschema:"minimum=1"`
	TimeoutMS  int `json:"timeout_ms" jsonschema:"minimum=1,description=Wait for each LMR"`
}

func (a *OneWayDelayArgs) Validate() error {
	if a.Count < 1 || a.IntervalMS < 1 {
		return errors.New("count and interval_ms must be positive")
	}
	return validTarget(a.Target)
}

func (a *TestSignalArgs) Validate() error {
	if a.Count < 1 || a.IntervalMS < 1 || a.Size < 0 || a.Size > 1400 {
		return errors.New("count and interval_ms must be positive, size 0-1400")
	}
	if a.Pattern != "" && a.Pattern != "null" && a.Pattern != "prbs" {
		return errors.New("pattern must be null or prbs")
	}
	return validTarget(a.Target)
}

func (a *LossArgs) Validate() error {
	if a.Count < 2 || a.IntervalMS < 1 || a.TimeoutMS < 1 {
		return errors.New("count ≥ 2, interval_ms and timeout_ms positive")
	}
	return validTarget(a.Target)
}

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
	if a.CSF != nil {
		if _, ok := csfTypes[*a.CSF]; !ok && *a.CSF != "none" {
			return fmt.Errorf("invalid csf %q", *a.CSF)
		}
	}
	return nil
}
