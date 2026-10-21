package cfm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Opcodes (IEEE 802.1Q table 21-4, ITU-T Y.1731 table 9-1).
const (
	OpCCM uint8 = 1
	OpLBR uint8 = 2
	OpLBM uint8 = 3
	OpLTR uint8 = 4
	OpLTM uint8 = 5
	OpAIS uint8 = 33
	OpLCK uint8 = 35
	OpLMR uint8 = 42
	OpLMM uint8 = 43
	Op1DM uint8 = 45
	OpDMR uint8 = 46
	OpDMM uint8 = 47
	OpSLR uint8 = 54
	OpSLM uint8 = 55
)

var opNames = map[uint8]string{OpCCM: "CCM", OpLBR: "LBR", OpLBM: "LBM", OpLTR: "LTR", OpLTM: "LTM",
	OpAIS: "AIS", OpLCK: "LCK", OpLMR: "LMR", OpLMM: "LMM", Op1DM: "1DM", OpDMR: "DMR", OpDMM: "DMM",
	OpSLR: "SLR", OpSLM: "SLM"}

func OpName(op uint8) string {
	if n, ok := opNames[op]; ok {
		return n
	}
	return "op" + strconv.Itoa(int(op))
}

// TLV types.
const (
	tlvEnd          uint8 = 0
	tlvSenderID     uint8 = 1
	tlvPortStatus   uint8 = 2
	tlvData         uint8 = 3
	tlvIntfStatus   uint8 = 4
	tlvReplyIngress uint8 = 5
	tlvReplyEgress  uint8 = 6
	tlvLTMEgressID  uint8 = 7
	tlvLTREgressID  uint8 = 8
)

// Port Status TLV values (802.1Q 21.5.4).
const (
	PortBlocked uint8 = 1
	PortUp      uint8 = 2
)

// Interface Status TLV values (802.1Q 21.5.5, from RFC 2863 ifOperStatus).
var intfStatusNames = map[uint8]string{1: "up", 2: "down", 3: "testing", 4: "unknown", 5: "dormant", 6: "notPresent", 7: "lowerLayerDown"}
var portStatusNames = map[uint8]string{PortBlocked: "blocked", PortUp: "up"}

func lookupName(m map[uint8]string, s string) (uint8, bool) {
	for k, v := range m {
		if strings.EqualFold(v, s) {
			return k, true
		}
	}
	return 0, false
}

// CCM interval encodings (802.1Q table 21-15).
var intervals = []struct {
	code uint8
	name string
	d    time.Duration
}{
	{1, "3.33ms", 3333333 * time.Nanosecond},
	{2, "10ms", 10 * time.Millisecond},
	{3, "100ms", 100 * time.Millisecond},
	{4, "1s", time.Second},
	{5, "10s", 10 * time.Second},
	{6, "1m", time.Minute},
	{7, "10m", 10 * time.Minute},
}

// SR OS spellings accepted as aliases.
var intervalAliases = map[string]string{"60s": "1m", "600s": "10m", "1min": "1m", "10min": "10m", "3.3ms": "3.33ms"}

func intervalByName(s string) (uint8, time.Duration, bool) {
	if a, ok := intervalAliases[s]; ok {
		s = a
	}
	for _, iv := range intervals {
		if iv.name == s {
			return iv.code, iv.d, true
		}
	}
	return 0, 0, false
}

func intervalName(code uint8) string {
	for _, iv := range intervals {
		if iv.code == code {
			return iv.name
		}
	}
	return "invalid(" + strconv.Itoa(int(code)) + ")"
}

// Multicast destination addresses (802.1Q table 8-13).
func ccmGroup(level uint8) pkt.MAC { return pkt.MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x30 | level&7} }
func ltmGroup(level uint8) pkt.MAC { return pkt.MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x38 | level&7} }

// ---- common header ----

type header struct {
	Level     uint8
	Version   uint8
	Opcode    uint8
	Flags     uint8
	TLVOffset uint8
}

var errShort = errors.New("CFM PDU too short")

func parseHeader(b []byte) (header, error) {
	if len(b) < 4 {
		return header{}, errShort
	}
	h := header{Level: b[0] >> 5, Version: b[0] & 0x1f, Opcode: b[1], Flags: b[2], TLVOffset: b[3]}
	if len(b) < 4+int(h.TLVOffset) {
		return h, errShort
	}
	return h, nil
}

func putHeader(b []byte, h header) {
	b[0] = h.Level<<5 | h.Version&0x1f
	b[1], b[2], b[3] = h.Opcode, h.Flags, h.TLVOffset
}

type tlv struct {
	Type  uint8
	Value []byte
}

// parseTLVs walks TLVs until the End TLV or the end of the buffer. Ethernet
// padding after the End TLV is ignored.
func parseTLVs(b []byte) ([]tlv, error) {
	var out []tlv
	for len(b) > 0 {
		t := b[0]
		if t == tlvEnd {
			return out, nil
		}
		if len(b) < 3 {
			return out, errShort
		}
		l := int(binary.BigEndian.Uint16(b[1:3]))
		if len(b) < 3+l {
			return out, fmt.Errorf("TLV %d length %d overruns PDU", t, l)
		}
		out = append(out, tlv{t, b[3 : 3+l]})
		b = b[3+l:]
	}
	return out, nil
}

func appendTLV(b []byte, t uint8, v []byte) []byte {
	b = append(b, t, byte(len(v)>>8), byte(len(v)))
	return append(b, v...)
}

// ---- MAID (802.1Q 21.6.5, Y.1731 annex A) ----

type MAID [48]byte

var mdFormats = map[string]uint8{"none": 1, "dns": 2, "mac-int": 3, "string": 4}
var maFormats = map[string]uint8{"primary-vid": 1, "string": 2, "integer": 3, "vpn-id": 4, "icc": 32}

func encodeMAID(mdFormat, mdName, maFormat, maName string) (MAID, error) {
	var m MAID
	mdf, ok := mdFormats[mdFormat]
	if !ok {
		return m, fmt.Errorf("unknown md_format %q", mdFormat)
	}
	maf, ok := maFormats[maFormat]
	if !ok {
		return m, fmt.Errorf("unknown ma_format %q", maFormat)
	}
	var md []byte
	switch mdFormat {
	case "none":
		if mdName != "" {
			return m, errors.New("md_name must be empty with md_format none")
		}
	case "dns", "string":
		if mdName == "" || len(mdName) > 43 {
			return m, errors.New("md_name must be 1-43 characters")
		}
		md = []byte(mdName)
	case "mac-int":
		// "aa:bb:cc:dd:ee:ff:1234"
		i := strings.LastIndexByte(mdName, ':')
		if i < 0 {
			return m, errors.New("mac-int md_name must be MAC:integer")
		}
		mac, err := pkt.ParseMAC(mdName[:i])
		if err != nil {
			return m, err
		}
		n, err := strconv.ParseUint(mdName[i+1:], 10, 16)
		if err != nil {
			return m, err
		}
		md = append(mac[:], byte(n>>8), byte(n))
	}
	var ma []byte
	switch maFormat {
	case "primary-vid":
		v, err := strconv.ParseUint(maName, 10, 12)
		if err != nil {
			return m, fmt.Errorf("primary-vid ma_name must be a VLAN ID: %w", err)
		}
		ma = []byte{byte(v >> 8), byte(v)}
	case "string":
		if maName == "" || len(maName) > 45 {
			return m, errors.New("ma_name must be 1-45 characters")
		}
		ma = []byte(maName)
	case "integer":
		v, err := strconv.ParseUint(maName, 10, 16)
		if err != nil {
			return m, fmt.Errorf("integer ma_name: %w", err)
		}
		ma = []byte{byte(v >> 8), byte(v)}
	case "vpn-id":
		// "oui:index" in hex, e.g. "00aabb:00000001"
		parts := strings.Split(maName, ":")
		if len(parts) != 2 {
			return m, errors.New("vpn-id ma_name must be OUI:index in hex")
		}
		oui, err1 := strconv.ParseUint(parts[0], 16, 24)
		idx, err2 := strconv.ParseUint(parts[1], 16, 32)
		if err1 != nil || err2 != nil {
			return m, errors.New("vpn-id ma_name must be OUI:index in hex")
		}
		ma = []byte{byte(oui >> 16), byte(oui >> 8), byte(oui), byte(idx >> 24), byte(idx >> 16), byte(idx >> 8), byte(idx)}
	case "icc":
		if mdFormat != "none" {
			return m, errors.New("ma_format icc requires md_format none")
		}
		if maName == "" || len(maName) > 13 {
			return m, errors.New("icc MEG ID must be 1-13 characters")
		}
		ma = make([]byte, 13) // null padded per Y.1731 A.1
		copy(ma, maName)
	}
	b := m[:0]
	b = append(b, mdf)
	if mdf != 1 {
		b = append(b, byte(len(md)))
		b = append(b, md...)
	}
	b = append(b, maf, byte(len(ma)))
	b = append(b, ma...)
	if len(b) > 48 {
		return m, errors.New("MD name plus MA name too long for 48-octet MAID")
	}
	return m, nil
}

// String renders a MAID for display.
func (m MAID) String() string {
	b := m[:]
	mdf := b[0]
	b = b[1:]
	md := ""
	if mdf != 1 {
		if len(b) < 1 || int(b[0]) > len(b)-1 {
			return "malformed"
		}
		l := int(b[0])
		raw := b[1 : 1+l]
		switch mdf {
		case 3:
			if l == 8 {
				md = fmt.Sprintf("%s:%d", pkt.MAC(raw[:6]), binary.BigEndian.Uint16(raw[6:]))
			}
		default:
			md = string(raw)
		}
		b = b[1+l:]
	}
	if len(b) < 2 || int(b[1]) > len(b)-2 {
		return "malformed"
	}
	maf, l := b[0], int(b[1])
	raw := b[2 : 2+l]
	var ma string
	switch maf {
	case 1, 3:
		if l == 2 {
			ma = strconv.Itoa(int(binary.BigEndian.Uint16(raw)))
		}
	case 4:
		if l == 7 {
			ma = fmt.Sprintf("%02x%02x%02x:%08x", raw[0], raw[1], raw[2], binary.BigEndian.Uint32(raw[3:]))
		}
	default:
		ma = strings.TrimRight(string(raw), "\x00")
	}
	fname := func(fm map[string]uint8, v uint8) string {
		for k, x := range fm {
			if x == v {
				return k
			}
		}
		return strconv.Itoa(int(v))
	}
	if mdf == 1 {
		return fmt.Sprintf("%s(%s)", fname(maFormats, maf), ma)
	}
	return fmt.Sprintf("%s(%s)/%s(%s)", fname(mdFormats, mdf), md, fname(maFormats, maf), ma)
}

// ---- CCM ----

const ccmTLVOffset = 70

type ccm struct {
	Level      uint8
	RDI        bool
	Interval   uint8
	Seq        uint32
	MEPID      uint16
	MAID       MAID
	TxFCf      uint32
	RxFCb      uint32
	TxFCb      uint32
	PortStatus uint8 // 0 = TLV absent
	IntfStatus uint8 // 0 = TLV absent
}

func (c *ccm) encode() []byte {
	b := make([]byte, 4+ccmTLVOffset, 4+ccmTLVOffset+12)
	flags := c.Interval & 7
	if c.RDI {
		flags |= 0x80
	}
	putHeader(b, header{Level: c.Level, Opcode: OpCCM, Flags: flags, TLVOffset: ccmTLVOffset})
	binary.BigEndian.PutUint32(b[4:], c.Seq)
	binary.BigEndian.PutUint16(b[8:], c.MEPID)
	copy(b[10:58], c.MAID[:])
	binary.BigEndian.PutUint32(b[58:], c.TxFCf)
	binary.BigEndian.PutUint32(b[62:], c.RxFCb)
	binary.BigEndian.PutUint32(b[66:], c.TxFCb)
	if c.PortStatus != 0 {
		b = appendTLV(b, tlvPortStatus, []byte{c.PortStatus})
	}
	if c.IntfStatus != 0 {
		b = appendTLV(b, tlvIntfStatus, []byte{c.IntfStatus})
	}
	return append(b, tlvEnd)
}

func parseCCM(h header, b []byte) (*ccm, error) {
	if h.TLVOffset < ccmTLVOffset {
		return nil, fmt.Errorf("CCM first TLV offset %d < 70", h.TLVOffset)
	}
	c := &ccm{Level: h.Level, RDI: h.Flags&0x80 != 0, Interval: h.Flags & 7,
		Seq: binary.BigEndian.Uint32(b[4:]), MEPID: binary.BigEndian.Uint16(b[8:]) & 0x1fff,
		TxFCf: binary.BigEndian.Uint32(b[58:]), RxFCb: binary.BigEndian.Uint32(b[62:]), TxFCb: binary.BigEndian.Uint32(b[66:])}
	copy(c.MAID[:], b[10:58])
	tlvs, err := parseTLVs(b[4+int(h.TLVOffset):])
	if err != nil {
		return c, err
	}
	for _, t := range tlvs {
		switch t.Type {
		case tlvPortStatus:
			if len(t.Value) >= 1 {
				c.PortStatus = t.Value[0]
			}
		case tlvIntfStatus:
			if len(t.Value) >= 1 {
				c.IntfStatus = t.Value[0]
			}
		}
	}
	return c, nil
}

// ---- LBM / LBR ----

func encodeLBM(level uint8, transID uint32, dataLen int) []byte {
	b := make([]byte, 8, 8+3+dataLen+1)
	putHeader(b, header{Level: level, Opcode: OpLBM, TLVOffset: 4})
	binary.BigEndian.PutUint32(b[4:], transID)
	if dataLen > 0 {
		data := make([]byte, dataLen)
		for i := range data {
			data[i] = byte(i)
		}
		b = appendTLV(b, tlvData, data)
	}
	return append(b, tlvEnd)
}

func transID(h header, b []byte) (uint32, bool) {
	if h.TLVOffset < 4 || len(b) < 8 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[4:]), true
}

// ---- LTM / LTR ----

const (
	ltmTLVOffset = 17
	ltrTLVOffset = 6

	flagUseFDBOnly  = 0x80
	flagFwdYes      = 0x40
	flagTerminalMEP = 0x20

	RlyHit  uint8 = 1
	RlyFDB  uint8 = 2
	RlyMPDB uint8 = 3
)

type ltm struct {
	Level    uint8
	Flags    uint8
	TransID  uint32
	TTL      uint8
	Orig     pkt.MAC
	Target   pkt.MAC
	EgressID []byte // 8 octets, nil if absent
}

func (l *ltm) encode() []byte {
	b := make([]byte, 4+ltmTLVOffset, 4+ltmTLVOffset+12)
	putHeader(b, header{Level: l.Level, Opcode: OpLTM, Flags: l.Flags, TLVOffset: ltmTLVOffset})
	binary.BigEndian.PutUint32(b[4:], l.TransID)
	b[8] = l.TTL
	copy(b[9:15], l.Orig[:])
	copy(b[15:21], l.Target[:])
	if l.EgressID != nil {
		b = appendTLV(b, tlvLTMEgressID, l.EgressID)
	}
	return append(b, tlvEnd)
}

func parseLTM(h header, b []byte) (*ltm, error) {
	if h.TLVOffset < ltmTLVOffset {
		return nil, errShort
	}
	l := &ltm{Level: h.Level, Flags: h.Flags, TransID: binary.BigEndian.Uint32(b[4:]), TTL: b[8]}
	copy(l.Orig[:], b[9:15])
	copy(l.Target[:], b[15:21])
	tlvs, _ := parseTLVs(b[4+int(h.TLVOffset):])
	for _, t := range tlvs {
		if t.Type == tlvLTMEgressID && len(t.Value) == 8 {
			l.EgressID = append([]byte(nil), t.Value...)
		}
	}
	return l, nil
}

type ltr struct {
	Level       uint8
	Flags       uint8
	TransID     uint32
	ReplyTTL    uint8
	RelayAction uint8
	LastEgress  []byte
	NextEgress  []byte
	IngressAct  uint8
	IngressMAC  pkt.MAC
	EgressAct   uint8
	EgressMAC   pkt.MAC
}

func (l *ltr) encode() []byte {
	b := make([]byte, 4+ltrTLVOffset, 64)
	putHeader(b, header{Level: l.Level, Opcode: OpLTR, Flags: l.Flags, TLVOffset: ltrTLVOffset})
	binary.BigEndian.PutUint32(b[4:], l.TransID)
	b[8], b[9] = l.ReplyTTL, l.RelayAction
	eg := make([]byte, 16)
	copy(eg[0:8], l.LastEgress)
	copy(eg[8:16], l.NextEgress)
	b = appendTLV(b, tlvLTREgressID, eg)
	if l.IngressAct != 0 {
		b = appendTLV(b, tlvReplyIngress, append([]byte{l.IngressAct}, l.IngressMAC[:]...))
	}
	if l.EgressAct != 0 {
		b = appendTLV(b, tlvReplyEgress, append([]byte{l.EgressAct}, l.EgressMAC[:]...))
	}
	return append(b, tlvEnd)
}

func parseLTR(h header, b []byte) (*ltr, error) {
	if h.TLVOffset < ltrTLVOffset {
		return nil, errShort
	}
	l := &ltr{Level: h.Level, Flags: h.Flags, TransID: binary.BigEndian.Uint32(b[4:]), ReplyTTL: b[8], RelayAction: b[9]}
	tlvs, _ := parseTLVs(b[4+int(h.TLVOffset):])
	for _, t := range tlvs {
		switch {
		case t.Type == tlvLTREgressID && len(t.Value) == 16:
			l.LastEgress = append([]byte(nil), t.Value[:8]...)
			l.NextEgress = append([]byte(nil), t.Value[8:]...)
		case t.Type == tlvReplyIngress && len(t.Value) >= 7:
			l.IngressAct = t.Value[0]
			copy(l.IngressMAC[:], t.Value[1:7])
		case t.Type == tlvReplyEgress && len(t.Value) >= 7:
			l.EgressAct = t.Value[0]
			copy(l.EgressMAC[:], t.Value[1:7])
		}
	}
	return l, nil
}

// ---- DMM / DMR (Y.1731 9.15) ----

const dmTLVOffset = 32

func putTimestamp(b []byte, t time.Time) {
	if t.IsZero() {
		binary.BigEndian.PutUint64(b, 0)
		return
	}
	binary.BigEndian.PutUint32(b[0:], uint32(t.Unix()))
	binary.BigEndian.PutUint32(b[4:], uint32(t.Nanosecond()))
}

func getTimestamp(b []byte) time.Time {
	s, ns := binary.BigEndian.Uint32(b[0:]), binary.BigEndian.Uint32(b[4:])
	if s == 0 && ns == 0 {
		return time.Time{}
	}
	return time.Unix(int64(s), int64(ns))
}

func encodeDMM(level uint8, tx time.Time) []byte {
	b := make([]byte, 4+dmTLVOffset+1)
	putHeader(b, header{Level: level, Opcode: OpDMM, TLVOffset: dmTLVOffset})
	putTimestamp(b[4:], tx)
	return b
}

// ---- SLM / SLR (Y.1731 Amd.1, 9.22) ----

const slTLVOffset = 16

type slm struct {
	Level   uint8
	Opcode  uint8
	SrcMEP  uint16
	RespMEP uint16
	TestID  uint32
	TxFCf   uint32
	TxFCb   uint32
}

func (s *slm) encode() []byte {
	b := make([]byte, 4+slTLVOffset+1)
	putHeader(b, header{Level: s.Level, Opcode: s.Opcode, TLVOffset: slTLVOffset})
	binary.BigEndian.PutUint16(b[4:], s.SrcMEP)
	binary.BigEndian.PutUint16(b[6:], s.RespMEP)
	binary.BigEndian.PutUint32(b[8:], s.TestID)
	binary.BigEndian.PutUint32(b[12:], s.TxFCf)
	binary.BigEndian.PutUint32(b[16:], s.TxFCb)
	return b
}

func parseSL(h header, b []byte) (*slm, error) {
	if h.TLVOffset < slTLVOffset {
		return nil, errShort
	}
	return &slm{Level: h.Level, Opcode: h.Opcode, SrcMEP: binary.BigEndian.Uint16(b[4:]), RespMEP: binary.BigEndian.Uint16(b[6:]),
		TestID: binary.BigEndian.Uint32(b[8:]), TxFCf: binary.BigEndian.Uint32(b[12:]), TxFCb: binary.BigEndian.Uint32(b[16:])}, nil
}
