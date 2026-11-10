package macsec

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ripnet/clabgen/internal/macsec"
	"github.com/ripnet/clabgen/internal/mka"
	"github.com/ripnet/clabgen/internal/pkt"
)

// Config is one MACsec SecY: on its device, or on the device's whole port.
type Config struct {
	Scope string `json:"scope" jsonschema:"enum=device,enum=port" jsonschema_description:"device: this device's frames, with its VLAN tags in clear (see clear_tags). port: every device on the port shares one secure channel, their VLAN tags encrypted, like a router's port with subinterfaces"`
	// Pointer: unset means all of the device's tags.
	ClearTags *int `json:"clear_tags,omitempty" jsonschema:"minimum=0,maximum=2" jsonschema_description:"Device scope: how many of the device's VLAN tags stay in front of the SecTAG; the rest are encrypted. Default: all of them"`

	Cipher     string `json:"cipher" jsonschema:"enum=gcm-aes-128,enum=gcm-aes-256,enum=gcm-aes-xpn-128,enum=gcm-aes-xpn-256"`
	Encrypt    bool   `json:"encrypt" jsonschema_description:"Confidentiality; off protects integrity only (the data stays readable)"`
	Offset     int    `json:"confidentiality_offset" jsonschema:"enum=0,enum=30,enum=50" jsonschema_description:"Bytes after the SecTAG left unencrypted"`
	IncludeSCI bool   `json:"include_sci" jsonschema_description:"Send the SCI in every SecTAG: 32 bytes of overhead; off: 24"`
	EndStation bool   `json:"end_station,omitempty" jsonschema_description:"ES bit: the SCI is the source MAC and port 1 (without include_sci)"`
	SCB        bool   `json:"scb,omitempty" jsonschema_description:"Single copy broadcast bit"`
	PortID     uint16 `json:"port_id" jsonschema:"minimum=1" jsonschema_description:"The SCI's port number (the MAC is the device's)"`

	Validation    string `json:"validate" jsonschema:"enum=strict,enum=check,enum=disabled" jsonschema_description:"strict drops frames that fail validation and frames without MACsec; check delivers integrity-only frames that fail, and frames without MACsec"`
	ReplayProtect bool   `json:"replay_protect"`
	ReplayWindow  uint32 `json:"replay_window" jsonschema_description:"Packets a frame may arrive out of order (0: in order only)"`
	Policy        string `json:"policy" jsonschema:"enum=must-secure,enum=should-secure" jsonschema_description:"Without a transmit SA: must-secure drops frames, should-secure sends them unprotected"`

	MKA    *MKA    `json:"mka,omitempty" jsonschema_description:"MACsec Key Agreement with a pre-shared CAK (what routers use); or static keys"`
	Static *Static `json:"static,omitempty" jsonschema_description:"Static keys: one SAK shared by both ends, as with ip macsec"`
}

// MKA is key agreement with a pre-shared CAK.
type MKA struct {
	CAK      string `json:"cak" jsonschema:"required" jsonschema_description:"Connectivity association key as hex: 32 digits (128-bit) or 64 (256-bit)"`
	CKN      string `json:"ckn" jsonschema:"required" jsonschema_description:"CAK name as hex: 2 to 64 digits"`
	Priority *int   `json:"key_server_priority,omitempty" jsonschema:"minimum=0,maximum=255" jsonschema_description:"Lowest wins the key server election (ties: lowest SCI); 255 never serves. Default 16"`
	Hello    string `json:"hello_time,omitempty" jsonschema_description:"Interval between MKPDUs; a peer times out after 3. Default: 2s"`
	EAPOLDst string `json:"eapol_dst,omitempty" jsonschema_description:"Where MKPDUs go: nearest-non-tpmr (01:80:c2:00:00:03), nearest-bridge (…:0e), nearest-customer (…:00), or a MAC. Default: nearest-non-tpmr"`
}

// Static is a static SAK used both to send and to receive.
type Static struct {
	Key string `json:"key" jsonschema:"required" jsonschema_description:"SAK as hex: 32 digits (128-bit suites) or 64 (256-bit)"`
	AN  uint8  `json:"an" jsonschema:"maximum=3"`
	PN  uint64 `json:"pn,omitempty" jsonschema_description:"First packet number to send, and the lowest accepted. Default: 1"`
	// XPN suites only.
	Salt     string `json:"salt,omitempty" jsonschema_description:"XPN suites: 96-bit salt as hex (24 digits)"`
	SSCI     uint32 `json:"ssci,omitempty" jsonschema_description:"XPN suites: this end's short SCI"`
	PeerSSCI uint32 `json:"peer_ssci,omitempty" jsonschema_description:"XPN suites: the far end's short SCI"`
}

func defaultConfig() *Config {
	return &Config{Scope: "device", Cipher: "gcm-aes-128", Encrypt: true, IncludeSCI: true, PortID: 1,
		Validation: "strict", ReplayProtect: true, Policy: "must-secure"}
}

func (c *Config) Validate() error {
	if c.Scope != "device" && c.Scope != "port" {
		return fmt.Errorf("scope: device or port, not %q", c.Scope)
	}
	if c.ClearTags != nil && (c.Scope == "port" || *c.ClearTags < 0 || *c.ClearTags > 2) {
		if c.Scope == "port" {
			return errors.New("clear_tags is for device scope: a port's VLAN tags are encrypted")
		}
		return errors.New("clear_tags: 0 to 2")
	}
	cs, err := macsec.ParseCipher(c.Cipher)
	if err != nil {
		return err
	}
	if c.Offset != 0 && c.Offset != 30 && c.Offset != 50 {
		return errors.New("confidentiality_offset: 0, 30 or 50")
	}
	if c.Offset != 0 && !c.Encrypt {
		return errors.New("confidentiality_offset needs encrypt")
	}
	if c.EndStation && c.IncludeSCI {
		return errors.New("end_station frames don't carry the SCI: turn include_sci off")
	}
	if c.PortID == 0 {
		return errors.New("port_id: 1 or more")
	}
	if _, err := macsec.ParseValidation(c.Validation); err != nil {
		return err
	}
	if c.Policy != "must-secure" && c.Policy != "should-secure" {
		return fmt.Errorf("policy: must-secure or should-secure, not %q", c.Policy)
	}
	switch {
	case c.Static != nil && c.MKA != nil:
		return errors.New("mka or static keys, not both")
	case c.Static != nil:
		_, err = c.Static.key(cs)
		return err
	case c.MKA != nil:
		if cs.XPN() {
			return errors.New("MKA with XPN cipher suites isn't supported yet: use static keys")
		}
		_, err = c.MKA.parse()
		return err
	}
	return errors.New("mka or static keys are needed")
}

type mkaParams struct {
	cak, ckn []byte
	priority uint8
	hello    time.Duration
	dst      pkt.MAC
}

func (m *MKA) parse() (mkaParams, error) {
	var p mkaParams
	var err error
	if p.cak, err = hex.DecodeString(strings.ReplaceAll(m.CAK, ":", "")); err != nil || (len(p.cak) != 16 && len(p.cak) != 32) {
		return p, errors.New("mka cak: 32 or 64 hex digits")
	}
	if p.ckn, err = hex.DecodeString(strings.ReplaceAll(m.CKN, ":", "")); err != nil || len(p.ckn) < 1 || len(p.ckn) > 32 {
		return p, errors.New("mka ckn: 2 to 64 hex digits")
	}
	p.priority = 16
	if m.Priority != nil {
		if *m.Priority < 0 || *m.Priority > 255 {
			return p, errors.New("mka key_server_priority: 0 to 255")
		}
		p.priority = uint8(*m.Priority)
	}
	p.hello = mka.HelloTime
	if m.Hello != "" {
		if p.hello, err = time.ParseDuration(m.Hello); err != nil || p.hello < 100*time.Millisecond || p.hello > time.Minute {
			return p, errors.New("mka hello_time: a duration from 100ms to 1m")
		}
	}
	switch m.EAPOLDst {
	case "", "nearest-non-tpmr":
		p.dst = mka.GroupAddress
	case "nearest-bridge":
		p.dst = pkt.MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e}
	case "nearest-customer":
		p.dst = pkt.MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x00}
	default:
		if p.dst, err = pkt.ParseMAC(m.EAPOLDst); err != nil {
			return p, fmt.Errorf("mka eapol_dst: %v", err)
		}
	}
	return p, nil
}

// key decodes the static key for cs.
func (s *Static) key(cs macsec.Cipher) (macsec.Key, error) {
	k, err := hex.DecodeString(strings.ReplaceAll(s.Key, ":", ""))
	if err != nil {
		return macsec.Key{}, fmt.Errorf("static key: %v", err)
	}
	if len(k) != cs.KeyLen() {
		return macsec.Key{}, fmt.Errorf("static key: %s takes %d hex digits, not %d", cs, 2*cs.KeyLen(), 2*len(k))
	}
	if s.AN > 3 {
		return macsec.Key{}, errors.New("static an: 0 to 3")
	}
	if !cs.XPN() && s.PN > cs.MaxPN() {
		return macsec.Key{}, fmt.Errorf("static pn: %s packet numbers are 32-bit", cs)
	}
	key := macsec.Key{AN: s.AN, SAK: k, SSCI: s.SSCI}
	if cs.XPN() {
		salt, err := hex.DecodeString(strings.ReplaceAll(s.Salt, ":", ""))
		if err != nil || len(salt) != 12 {
			return macsec.Key{}, errors.New("static salt: 24 hex digits, needed by XPN suites")
		}
		copy(key.Salt[:], salt)
		if s.SSCI == 0 || s.PeerSSCI == 0 || s.SSCI == s.PeerSSCI {
			return macsec.Key{}, errors.New("static ssci and peer_ssci: XPN suites need both, different")
		}
	} else if s.Salt != "" || s.SSCI != 0 || s.PeerSSCI != 0 {
		return macsec.Key{}, errors.New("static salt, ssci and peer_ssci are for XPN suites")
	}
	return key, nil
}
