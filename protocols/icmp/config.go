package icmp

import (
	"errors"
	"net/netip"
	"time"

	"github.com/ripnet/clabgen/internal/ipflow"
	"github.com/ripnet/clabgen/internal/pkt"
)

// Config is one ICMP flow: the shared IP flow parts plus the ICMP header.
type Config struct {
	ipflow.Target
	ipflow.Run
	ipflow.Addressing
	ipflow.IPHeader

	ICMPType     *uint8  `json:"icmp_type,omitempty" jsonschema:"minimum=0,maximum=255,description=Default: echo request (8; 128 for IPv6). Other types are sent without reply tracking"`
	ICMPCode     uint8   `json:"icmp_code,omitempty" jsonschema:"minimum=0,maximum=255"`
	Identifier   *uint16 `json:"identifier,omitempty" jsonschema:"description=Echo identifier. Default: derived from the flow name"`
	SeqStart     uint16  `json:"seq_start,omitempty" jsonschema:"description=First echo sequence number"`
	SeqMode      string  `json:"seq_mode" jsonschema:"enum=increment,enum=fixed"`
	ICMPChecksum string  `json:"icmp_checksum" jsonschema:"enum=correct,enum=zero,enum=invalid"`

	ipflow.Payload

	Checks ipflow.Checks `json:"checks" jsonschema:"description=Pass/fail limits on what the flow measures since it started or was cleared"`
}

func defaultConfig() *Config {
	return &Config{Run: ipflow.DefaultRun, IPHeader: ipflow.DefaultIPHeader, Payload: ipflow.DefaultPayload,
		SeqMode: "increment", ICMPChecksum: "correct"}
}

func (c *Config) spec() ipflow.Spec {
	return ipflow.Spec{Target: c.Target, Run: c.Run, Addressing: c.Addressing, IPHeader: c.IPHeader, Payload: c.Payload, Checks: c.Checks}
}

func (c *Config) Validate() error {
	dst, err := c.spec().Validate()
	if err != nil {
		return err
	}
	if err := ipflow.CheckChecksumMode(c.ICMPChecksum); err != nil {
		return err
	}
	if c.SeqMode != "increment" && c.SeqMode != "fixed" {
		return errors.New("seq_mode must be increment or fixed")
	}
	if c.tracksReplies(dst) {
		if c.SeqMode == "fixed" && !c.Signature {
			return errors.New("seq_mode fixed needs the signature to match replies")
		}
		if c.SeqMode == "increment" && c.Rate*float64(c.TimeoutD())/float64(time.Second) > 30000 {
			return errors.New("rate × timeout must stay under 30000 so 16-bit sequence numbers don't wrap within the timeout")
		}
	}
	return nil
}

func (c *Config) icmpType(dst netip.Addr) uint8 {
	if c.ICMPType != nil {
		return *c.ICMPType
	}
	if dst.Is4() {
		return pkt.ICMPEchoRequest
	}
	return pkt.ICMPv6EchoRequest
}

// tracksReplies reports whether the flow sends echo requests.
func (c *Config) tracksReplies(dst netip.Addr) bool {
	t := c.icmpType(dst)
	return (dst.Is4() && t == pkt.ICMPEchoRequest) || (dst.Is6() && t == pkt.ICMPv6EchoRequest)
}
