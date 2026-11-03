package ipflow

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/prefixset"
)

// Config parts shared by IP flow types. A type embeds them (in this order,
// with its own transport fields between IPHeader and Payload) so forms
// list run, addressing, IP, transport and payload fields in that order.

type Target struct {
	Destination  string        `json:"destination,omitempty" jsonschema:"description=IPv4 or IPv6 address to send to. Or use destinations"`
	Destinations []PrefixBlock `json:"destinations,omitempty" jsonschema_description:"Send to many prefixes instead of one address: the first host of each, in turn. They must be off-link (reached through the device's gateway) and of one address family" jsonschema_extras:"x-summary={prefix}[ ×{count}]"`
}

// PrefixBlock is count consecutive prefixes of one length, as in a BGP route.
type PrefixBlock struct {
	Prefix string `json:"prefix" jsonschema:"required,description=e.g. 198.51.100.0/24 or 2001:db8:1::/48"`
	Count  int    `json:"count,omitempty" jsonschema:"minimum=1,description=Consecutive prefixes of this length. Default 1"`
}

// MaxDestinations caps the prefixes a flow cycles through.
const MaxDestinations = 1 << 20

// FirstHost is the address a flow sends to in a prefix: the one after the
// network address, or the only one (or first) in a /32, /128, /31 or /127.
func FirstHost(p netip.Prefix) netip.Addr {
	p = p.Masked()
	if p.Bits() >= p.Addr().BitLen()-1 {
		return p.Addr()
	}
	return p.Addr().Next()
}

// Prefixes expands the destination blocks, in order.
func (t Target) Prefixes() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, b := range t.Destinations {
		p, err := netip.ParsePrefix(strings.TrimSpace(b.Prefix))
		if err != nil {
			return nil, fmt.Errorf("destinations: %q is not a prefix", b.Prefix)
		}
		p = p.Masked()
		n := b.Count
		if n == 0 {
			n = 1
		}
		if n < 0 || len(out)+n > MaxDestinations {
			return nil, fmt.Errorf("destinations: at most %d prefixes", MaxDestinations)
		}
		for i := 0; i < n; i++ {
			if i > 0 {
				next := prefixset.Last(p).Next()
				if !next.IsValid() || next.Is4() != p.Addr().Is4() {
					return nil, fmt.Errorf("destinations: %s count %d runs past the address space", b.Prefix, n)
				}
				p = netip.PrefixFrom(next, p.Bits())
			}
			out = append(out, p)
		}
	}
	return out, nil
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

// Dst is the destination address: with destinations, the first.
func (t Target) Dst() (netip.Addr, error) {
	if len(t.Destinations) > 0 {
		ps, err := t.Prefixes()
		if err != nil {
			return netip.Addr{}, err
		}
		return FirstHost(ps[0]), nil
	}
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
	if (s.Destination == "") == (len(s.Destinations) == 0) {
		return netip.Addr{}, errors.New("set either destination or destinations")
	}
	dst, err := s.Dst()
	if err != nil {
		return dst, err
	}
	ps, err := s.Prefixes()
	if err != nil {
		return dst, err
	}
	for _, p := range ps {
		if p.Addr().Is4() != dst.Is4() {
			return dst, errors.New("destinations must all be of one address family")
		}
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
	if err := s.Run.Validate(); err != nil {
		return dst, err
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
	if err := s.Checks.Validate(); err != nil {
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

// Validate checks the run settings: mode, count or duration, rate, timeout.
func (r Run) Validate() error {
	switch r.Mode {
	case "continuous":
	case "count":
		if r.Count == 0 {
			return errors.New("count mode needs count > 0")
		}
	case "duration":
		if d, err := time.ParseDuration(r.Duration); err != nil || d <= 0 {
			return fmt.Errorf("duration mode needs a duration like 30s (got %q)", r.Duration)
		}
	default:
		return fmt.Errorf("invalid mode %q", r.Mode)
	}
	if r.Rate < 0.01 || r.Rate > 100000 {
		return errors.New("rate must be 0.01-100000 pps")
	}
	if to, err := time.ParseDuration(r.Timeout); err != nil || to <= 0 {
		return fmt.Errorf("invalid timeout %q", r.Timeout)
	}
	return nil
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
