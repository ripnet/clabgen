package ipflow

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Config parts shared by IP flow types. A type embeds them (in this order,
// with its own transport fields between IPHeader and Payload) so forms
// list run, addressing, IP, transport and payload fields in that order.

type Target struct {
	Destination string `json:"destination" jsonschema:"required,description=IPv4 or IPv6 address to send to"`
}

type Run struct {
	Mode      string  `json:"mode" jsonschema:"enum=continuous,enum=count,enum=duration"`
	Count     uint64  `json:"count,omitempty" jsonschema:"description=Packets to send in count mode"`
	Duration  string  `json:"duration,omitempty" jsonschema:"description=How long to send in duration mode (e.g. 30s or 5m)"`
	Rate      float64 `json:"rate" jsonschema:"minimum=0.01,maximum=100000,description=Packets per second"`
	Timeout   string  `json:"timeout" jsonschema:"description=How long to wait for a reply before counting a packet lost"`
	AutoStart bool    `json:"auto_start,omitempty" jsonschema:"description=Start sending as soon as the flow is created (e.g. from a scenario)"`
}

type Addressing struct {
	Source string `json:"source,omitempty" jsonschema:"description=Source address. Default: the device's address on the destination's subnet; else its first address of that family"`
	DstMAC string `json:"dst_mac,omitempty" jsonschema:"description=Destination MAC. Default: resolve the next hop (destination or gateway) with ARP/ND"`
	PCP    *uint8 `json:"vlan_pcp,omitempty" jsonschema:"minimum=0,maximum=7,description=PCP on every VLAN tag. Default: the device's"`
}

type IPHeader struct {
	TTL        uint8   `json:"ttl" jsonschema:"minimum=0,maximum=255,description=IPv4 TTL or IPv6 hop limit"`
	DSCP       uint8   `json:"dscp,omitempty" jsonschema:"minimum=0,maximum=63"`
	ECN        uint8   `json:"ecn,omitempty" jsonschema:"minimum=0,maximum=3"`
	DF         bool    `json:"df,omitempty" jsonschema:"description=IPv4 don't-fragment bit"`
	IPID       *uint16 `json:"ip_id,omitempty" jsonschema:"description=Fixed IPv4 identification. Default: increments per packet"`
	FlowLabel  uint32  `json:"flow_label,omitempty" jsonschema:"minimum=0,maximum=1048575,description=IPv6 flow label"`
	IPChecksum string  `json:"ip_checksum" jsonschema:"enum=correct,enum=zero,enum=invalid,description=IPv4 header checksum"`
}

type Payload struct {
	PayloadSize    int    `json:"payload_size" jsonschema:"minimum=0,maximum=9000,description=Data bytes after the transport header (the signature counts)"`
	PayloadPattern string `json:"payload_pattern" jsonschema:"enum=increment,enum=zeros,enum=random,enum=text,enum=hex"`
	PayloadData    string `json:"payload_data,omitempty" jsonschema:"description=Text or hex digits for the text and hex patterns; repeated to fill the payload"`
	Signature      bool   `json:"signature" jsonschema:"description=Start the payload with a 24-byte signature (flow ID; sequence; send time) for matching; RTT and one-way stats at clabgen receivers"`
}

// Spec gathers the shared parts of a flow's config.
type Spec struct {
	Target
	Run
	Addressing
	IPHeader
	Payload
	Checks Checks
}

// Defaults for the shared parts.
var (
	DefaultRun      = Run{Mode: "continuous", Rate: 1, Timeout: "1s"}
	DefaultIPHeader = IPHeader{TTL: 64, IPChecksum: "correct"}
	DefaultPayload  = Payload{PayloadSize: 56, PayloadPattern: "increment", Signature: true}
)

func (t Target) Dst() (netip.Addr, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(t.Destination))
	if err != nil || a.Zone() != "" {
		return a, fmt.Errorf("destination %q is not an IP address", t.Destination)
	}
	return a.Unmap(), nil
}

func (r Run) Interval() time.Duration { return time.Duration(float64(time.Second) / r.Rate) }

func (r Run) TimeoutD() time.Duration {
	d, _ := time.ParseDuration(r.Timeout)
	return d
}

func (r Run) DurationD() time.Duration {
	d, _ := time.ParseDuration(r.Duration)
	return d
}

// Validate checks the shared parts and returns the destination.
func (s Spec) Validate() (netip.Addr, error) {
	dst, err := s.Dst()
	if err != nil {
		return dst, err
	}
	if dst.IsUnspecified() {
		return dst, errors.New("destination must not be unspecified")
	}
	if s.Source != "" {
		a, err := netip.ParseAddr(s.Source)
		if err != nil || a.Is4() != dst.Is4() {
			return dst, fmt.Errorf("source %q must be an address of the destination's family", s.Source)
		}
	}
	if s.DstMAC != "" {
		if _, err := pkt.ParseMAC(s.DstMAC); err != nil {
			return dst, fmt.Errorf("dst_mac: %w", err)
		}
	}
	switch s.Mode {
	case "continuous":
	case "count":
		if s.Count == 0 {
			return dst, errors.New("count mode needs count > 0")
		}
	case "duration":
		if d, err := time.ParseDuration(s.Duration); err != nil || d <= 0 {
			return dst, fmt.Errorf("duration mode needs a duration like 30s (got %q)", s.Duration)
		}
	default:
		return dst, fmt.Errorf("invalid mode %q", s.Mode)
	}
	if s.Rate < 0.01 || s.Rate > 100000 {
		return dst, errors.New("rate must be 0.01-100000 pps")
	}
	if to, err := time.ParseDuration(s.Timeout); err != nil || to <= 0 {
		return dst, fmt.Errorf("invalid timeout %q", s.Timeout)
	}
	if s.PCP != nil && *s.PCP > 7 {
		return dst, errors.New("vlan_pcp must be 0-7")
	}
	if s.DSCP > 63 || s.ECN > 3 || s.FlowLabel > 0xfffff {
		return dst, errors.New("dscp must be 0-63, ecn 0-3, flow_label 0-1048575")
	}
	if err := CheckChecksumMode(s.IPChecksum); err != nil {
		return dst, err
	}
	if err := s.Checks.validate(); err != nil {
		return dst, err
	}
	if s.PayloadSize < 0 || s.PayloadSize > 9000 {
		return dst, errors.New("payload_size must be 0-9000")
	}
	if s.Signature && s.PayloadSize < SigLen {
		return dst, fmt.Errorf("payload_size must be at least %d with the signature on", SigLen)
	}
	switch s.PayloadPattern {
	case "increment", "zeros", "random":
	case "text":
		if s.PayloadData == "" {
			return dst, errors.New("the text pattern needs payload_data")
		}
	case "hex":
		if _, err := hex.DecodeString(strings.ReplaceAll(s.PayloadData, " ", "")); err != nil || s.PayloadData == "" {
			return dst, errors.New("the hex pattern needs payload_data of hex digits")
		}
	default:
		return dst, fmt.Errorf("invalid payload_pattern %q", s.PayloadPattern)
	}
	return dst, nil
}

func CheckChecksumMode(v string) error {
	if v != "correct" && v != "zero" && v != "invalid" {
		return fmt.Errorf("invalid checksum mode %q", v)
	}
	return nil
}

// Checksum applies a checksum mode to the correct value.
func Checksum(mode string, correct uint16) uint16 {
	switch mode {
	case "zero":
		return 0
	case "invalid":
		if v := correct + 1; v != 0 {
			return v
		}
		return 1
	}
	return correct
}
