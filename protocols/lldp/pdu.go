package lldp

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/ripnet/clabgen/internal/pkt"
)

// LLDPDU codec (IEEE 802.1AB-2016 clause 8, with the 802.1 and 802.3
// organizationally specific TLVs of annexes D and E).

const EtherType uint16 = 0x88cc

// Destination addresses (802.1AB table 7-1).
var (
	NearestBridge   = pkt.MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e}
	NearestNonTPMR  = pkt.MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x03}
	NearestCustomer = pkt.MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x00}
)

// TLV types.
const (
	tlvEnd       = 0
	tlvChassisID = 1
	tlvPortID    = 2
	tlvTTL       = 3
	tlvPortDesc  = 4
	tlvSysName   = 5
	tlvSysDesc   = 6
	tlvSysCap    = 7
	tlvMgmtAddr  = 8
	tlvOrg       = 127
)

var (
	oui8021 = [3]byte{0x00, 0x80, 0xc2}
	oui8023 = [3]byte{0x00, 0x12, 0x0f}
)

// Chassis ID subtypes.
var chassisSubtypes = map[string]byte{"chassis-component": 1, "interface-alias": 2, "port-component": 3,
	"mac": 4, "network-address": 5, "interface-name": 6, "local": 7}

// Port ID subtypes.
var portSubtypes = map[string]byte{"interface-alias": 1, "port-component": 2, "mac": 3,
	"network-address": 4, "interface-name": 5, "agent-circuit-id": 6, "local": 7}

// System capabilities (bits of the 16-bit fields).
var capBits = []string{"other", "repeater", "bridge", "wlan-ap", "router", "telephone", "docsis", "station", "c-vlan", "s-vlan", "tpmr"}

func capMask(names []string) (uint16, error) {
	var m uint16
	for _, n := range names {
		found := false
		for i, c := range capBits {
			if c == n {
				m |= 1 << i
				found = true
			}
		}
		if !found {
			return 0, fmt.Errorf("unknown capability %q (one of %s)", n, strings.Join(capBits, ", "))
		}
	}
	return m, nil
}

func capNames(m uint16) []string {
	var out []string
	for i, c := range capBits {
		if m&(1<<i) != 0 {
			out = append(out, c)
		}
	}
	return out
}

func subtypeName(m map[string]byte, v byte) string {
	for n, x := range m {
		if x == v {
			return n
		}
	}
	return fmt.Sprint(v)
}

// ---- encoding ----

type tlvWriter struct{ b []byte }

func (w *tlvWriter) add(t int, v []byte) {
	l := min(len(v), 511)
	w.b = append(w.b, byte(t<<1|l>>8), byte(l))
	w.b = append(w.b, v[:l]...)
}

func (w *tlvWriter) org(oui [3]byte, sub byte, v []byte) {
	w.add(tlvOrg, append(append(oui[:3:3], sub), v...))
}

// idValue encodes a chassis/port ID value: MACs and network addresses in
// binary, the other subtypes as text.
func idValue(isMAC, isNet bool, s string) ([]byte, error) {
	switch {
	case isMAC:
		m, err := pkt.ParseMAC(s)
		if err != nil {
			return nil, err
		}
		return m[:], nil
	case isNet:
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address", s)
		}
		fam := byte(1)
		if a.Is6() {
			fam = 2
		}
		return append([]byte{fam}, a.AsSlice()...), nil
	}
	if s == "" || len(s) > 255 {
		return nil, errors.New("IDs are 1-255 bytes")
	}
	return []byte(s), nil
}

func mgmtAddr(a netip.Addr, ifIndex uint32) []byte {
	fam := byte(1)
	if a.Is6() {
		fam = 2
	}
	b := append([]byte{byte(1 + a.BitLen()/8), fam}, a.AsSlice()...)
	b = append(b, 2) // interface numbering: ifIndex
	b = binary.BigEndian.AppendUint32(b, ifIndex)
	return append(b, 0) // no OID
}

// ---- decoding ----

// Info is a decoded LLDPDU.
type Info struct {
	ChassisSubtype string       `json:"chassis_id_subtype"`
	ChassisID      string       `json:"chassis_id"`
	PortSubtype    string       `json:"port_id_subtype"`
	PortID         string       `json:"port_id"`
	TTL            uint16       `json:"ttl"`
	PortDesc       string       `json:"port_description,omitempty"`
	SysName        string       `json:"system_name,omitempty"`
	SysDesc        string       `json:"system_description,omitempty"`
	Capabilities   []string     `json:"capabilities,omitempty"`
	Enabled        []string     `json:"enabled_capabilities,omitempty"`
	MgmtAddrs      []string     `json:"management_addresses,omitempty"`
	PortVLAN       uint16       `json:"port_vlan_id,omitempty"`
	VLANNames      []VLANName   `json:"vlan_names,omitempty"`
	MaxFrame       uint16       `json:"max_frame_size,omitempty"`
	MACPHY         *MACPHYInfo  `json:"mac_phy,omitempty"`
	LinkAgg        *LinkAggInfo `json:"link_aggregation,omitempty"`
	Other          []RawTLV     `json:"other_tlvs,omitempty"`
}

type VLANName struct {
	VID  uint16 `json:"vid"`
	Name string `json:"name"`
}

type MACPHYInfo struct {
	AutonegSupported bool   `json:"autoneg_supported"`
	AutonegEnabled   bool   `json:"autoneg_enabled"`
	Advertised       uint16 `json:"advertised_capabilities"`
	MAUType          uint16 `json:"mau_type"`
}

type LinkAggInfo struct {
	Capable   bool   `json:"capable"`
	Enabled   bool   `json:"enabled"`
	AggPortID uint32 `json:"aggregated_port_id"`
	FromDot1Q bool   `json:"dot1,omitempty"` // 802.1 OUI (newer) rather than 802.3
}

// RawTLV is a TLV we don't decode (or an org TLV we don't know).
type RawTLV struct {
	Type    int    `json:"type"`
	OUI     string `json:"oui,omitempty"`
	Subtype int    `json:"subtype,omitempty"`
	Value   string `json:"value"` // hex
}

var (
	ErrShort     = errors.New("truncated LLDPDU")
	ErrMandatory = errors.New("missing or misordered mandatory TLVs")
)

func decodeID(network byte, macSub byte, sub byte, v []byte) string {
	switch {
	case sub == macSub && len(v) == 6:
		return pkt.MAC(v).String()
	case sub == network && len(v) >= 1:
		if a, ok := netip.AddrFromSlice(v[1:]); ok {
			return a.String()
		}
		return hex.EncodeToString(v)
	}
	for _, c := range v {
		if c < 0x20 || c > 0x7e {
			return hex.EncodeToString(v)
		}
	}
	return string(v)
}

// Decode parses an LLDPDU. The first three TLVs must be Chassis ID, Port
// ID and TTL (802.1AB 9.2.7.7.1); unknown TLVs are kept raw.
func Decode(b []byte) (*Info, error) {
	in := &Info{}
	n := 0
	for {
		if len(b) < 2 {
			if n < 3 {
				return nil, ErrMandatory
			}
			return in, nil // tolerate a missing End TLV
		}
		t, l := int(b[0]>>1), int(b[0]&1)<<8|int(b[1])
		if len(b) < 2+l {
			return nil, ErrShort
		}
		v := b[2 : 2+l]
		b = b[2+l:]
		if n < 3 && t != n+1 {
			return nil, ErrMandatory
		}
		n++
		switch t {
		case tlvEnd:
			return in, nil
		case tlvChassisID:
			if l < 2 {
				return nil, ErrMandatory
			}
			in.ChassisSubtype = subtypeName(chassisSubtypes, v[0])
			in.ChassisID = decodeID(chassisSubtypes["network-address"], chassisSubtypes["mac"], v[0], v[1:])
		case tlvPortID:
			if l < 2 {
				return nil, ErrMandatory
			}
			in.PortSubtype = subtypeName(portSubtypes, v[0])
			in.PortID = decodeID(portSubtypes["network-address"], portSubtypes["mac"], v[0], v[1:])
		case tlvTTL:
			if l < 2 {
				return nil, ErrMandatory
			}
			in.TTL = binary.BigEndian.Uint16(v)
		case tlvPortDesc:
			in.PortDesc = string(v)
		case tlvSysName:
			in.SysName = string(v)
		case tlvSysDesc:
			in.SysDesc = string(v)
		case tlvSysCap:
			if l >= 4 {
				in.Capabilities, in.Enabled = capNames(binary.BigEndian.Uint16(v)), capNames(binary.BigEndian.Uint16(v[2:]))
			}
		case tlvMgmtAddr:
			if l >= 2 && int(v[0]) >= 1 && 1+int(v[0]) <= l {
				if a, ok := netip.AddrFromSlice(v[2 : 1+int(v[0])]); ok {
					in.MgmtAddrs = append(in.MgmtAddrs, a.String())
				} else if v[1] == 6 && v[0] == 7 {
					in.MgmtAddrs = append(in.MgmtAddrs, pkt.MAC(v[2:8]).String())
				}
			}
		case tlvOrg:
			if l < 4 || !in.decodeOrg([3]byte(v[:3]), v[3], v[4:]) {
				r := RawTLV{Type: t, Value: hex.EncodeToString(v)}
				if l >= 4 {
					r.OUI, r.Subtype, r.Value = fmt.Sprintf("%02x-%02x-%02x", v[0], v[1], v[2]), int(v[3]), hex.EncodeToString(v[4:])
				}
				in.Other = append(in.Other, r)
			}
		default:
			in.Other = append(in.Other, RawTLV{Type: t, Value: hex.EncodeToString(v)})
		}
	}
}

func (in *Info) decodeOrg(oui [3]byte, sub byte, v []byte) bool {
	switch {
	case oui == oui8021 && sub == 1 && len(v) >= 2:
		in.PortVLAN = binary.BigEndian.Uint16(v)
	case oui == oui8021 && sub == 3 && len(v) >= 3 && len(v) >= 3+int(v[2]):
		in.VLANNames = append(in.VLANNames, VLANName{VID: binary.BigEndian.Uint16(v), Name: string(v[3 : 3+int(v[2])])})
	case oui == oui8021 && sub == 7 && len(v) >= 5, oui == oui8023 && sub == 3 && len(v) >= 5:
		in.LinkAgg = &LinkAggInfo{Capable: v[0]&1 != 0, Enabled: v[0]&2 != 0, AggPortID: binary.BigEndian.Uint32(v[1:]), FromDot1Q: oui == oui8021}
	case oui == oui8023 && sub == 1 && len(v) >= 5:
		in.MACPHY = &MACPHYInfo{AutonegSupported: v[0]&1 != 0, AutonegEnabled: v[0]&2 != 0, Advertised: binary.BigEndian.Uint16(v[1:]), MAUType: binary.BigEndian.Uint16(v[3:])}
	case oui == oui8023 && sub == 4 && len(v) >= 2:
		in.MaxFrame = binary.BigEndian.Uint16(v)
	default:
		return false
	}
	return true
}
