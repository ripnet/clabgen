package isis

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
)

// Segment routing extensions: SRLB (in sr.go's SRCap), SID/Label Binding
// TLVs for a mapping server (RFC 8667 2.4), flex-algo definitions and
// application-specific link attributes (RFC 9350, 8919), node MSD (RFC
// 8491) and SRv6 (RFC 9352: capabilities, locators, End and End.X SIDs).

const (
	TLVSIDBinding   uint8 = 149
	TLVMTSIDBinding uint8 = 150
	TLVSRv6Locator  uint8 = 27

	subASLA        uint8 = 16 // in TLV 22/222
	subEndXSID     uint8 = 43
	subLANEndXSID  uint8 = 44
	subSRv6EndSID  uint8 = 5 // in TLV 27
	subSIDStruct   uint8 = 1 // sub-sub-TLV of SRv6 SIDs
	aslaAdminGroup uint8 = 3
	aslaTEMetric   uint8 = 18
	aslaMinMaxDly  uint8 = 34
	fadExclAny     uint8 = 1
	fadInclAny     uint8 = 2
	fadInclAll     uint8 = 3
	fadFlags       uint8 = 4

	sabmFlexAlgo byte = 0x10 // X: application flex-algo

	// Binding TLV flags.
	bindV6       byte = 0x80 // F
	bindMirror   byte = 0x40 // M
	bindFlood    byte = 0x20 // S
	bindDown     byte = 0x10 // D
	bindAttached byte = 0x08 // A
)

// Flex-algo metric types (RFC 9350).
var fadMetricTypes = map[string]byte{"igp": 0, "delay": 1, "te": 2}

// FAD is a flex-algo definition.
type FAD struct {
	Algo       byte
	MetricType byte
	Priority   byte
	ExclAny    uint32 // admin group bit masks (groups 0-31)
	InclAny    uint32
	InclAll    uint32
	PrefixMet  bool // M flag: use the prefix metric across areas
}

func (f FAD) encode() []byte {
	b := []byte{f.Algo, f.MetricType, 0, f.Priority} // calc type 0: SPF
	for _, g := range []struct {
		t byte
		m uint32
	}{{fadExclAny, f.ExclAny}, {fadInclAny, f.InclAny}, {fadInclAll, f.InclAll}} {
		if g.m != 0 {
			b = append(b, g.t, 4)
			b = binary.BigEndian.AppendUint32(b, g.m)
		}
	}
	if f.PrefixMet {
		b = append(b, fadFlags, 1, 0x80)
	}
	return b
}

// MSD is one maximum SID depth (type, value).
type MSD struct{ Type, Value byte }

// MSD types.
const (
	msdBaseMPLS   byte = 1
	msdSRHMaxSL   byte = 41
	msdSRHEndPop  byte = 42
	msdSRHHEncaps byte = 44
	msdSRHEndD    byte = 45
)

// LinkAttrs are the TE attributes of an adjacency, advertised as ASLA for
// flex-algo.
type LinkAttrs struct {
	TEMetric    uint32 // 0: none
	DelayUS     uint32 // min/max unidirectional delay, µs (0: none)
	AdminGroups uint32 // bit mask, groups 0-31
}

func (a *LinkAttrs) empty() bool {
	return a == nil || a.TEMetric == 0 && a.DelayUS == 0 && a.AdminGroups == 0
}

func (a *LinkAttrs) subTLV() []byte {
	if a.empty() {
		return nil
	}
	v := []byte{1, 0, sabmFlexAlgo} // SABM length 1, no UDABM, SABM
	if a.AdminGroups != 0 {
		v = append(v, aslaAdminGroup, 4)
		v = binary.BigEndian.AppendUint32(v, a.AdminGroups)
	}
	if a.TEMetric != 0 {
		v = append(v, aslaTEMetric, 3, byte(a.TEMetric>>16), byte(a.TEMetric>>8), byte(a.TEMetric))
	}
	if a.DelayUS != 0 {
		d := min(a.DelayUS, 0xffffff)
		v = append(v, aslaMinMaxDly, 8, 0, byte(d>>16), byte(d>>8), byte(d), 0, byte(d>>16), byte(d>>8), byte(d))
	}
	return append([]byte{subASLA, byte(len(v))}, v...)
}

// SIDStructure is an SRv6 SID's layout (bits).
type SIDStructure struct{ LB, LN, Fun, Arg byte }

func (st SIDStructure) subTLV() []byte {
	if st == (SIDStructure{}) {
		return nil
	}
	return []byte{subSIDStruct, 4, st.LB, st.LN, st.Fun, st.Arg}
}

// EndXSID is an SRv6 End.X SID on an adjacency (LAN: with the neighbor).
type EndXSID struct {
	SID      netip.Addr
	Behavior uint16
	Algo     byte
	Struct   SIDStructure
	Neighbor *SysID // LAN End.X
}

func (x EndXSID) subTLV() []byte {
	var v []byte
	t := subEndXSID
	if x.Neighbor != nil {
		t = subLANEndXSID
		v = append(v, x.Neighbor[:]...)
	}
	v = append(v, 0, x.Algo, 0) // flags, algorithm, weight
	v = binary.BigEndian.AppendUint16(v, x.Behavior)
	a := x.SID.As16()
	v = append(v, a[:]...)
	ss := x.Struct.subTLV()
	v = append(append(v, byte(len(ss))), ss...)
	return append([]byte{t, byte(len(v))}, v...)
}

// Locator is one SRv6 locator with its End SIDs (TLV 27).
type Locator struct {
	Prefix  netip.Prefix
	Metric  uint32
	Algo    byte
	MT      uint16
	EndSIDs []EndSID
}

// EndSID is an SRv6 End SID within a locator.
type EndSID struct {
	SID      netip.Addr
	Behavior uint16
	Struct   SIDStructure
}

func (l Locator) entry() []byte {
	b := binary.BigEndian.AppendUint32(nil, l.Metric)
	b = append(b, 0, l.Algo, byte(l.Prefix.Bits()))
	b = append(b, l.Prefix.Masked().Addr().AsSlice()[:(l.Prefix.Bits()+7)/8]...)
	var sub []byte
	for _, e := range l.EndSIDs {
		v := []byte{0}
		v = binary.BigEndian.AppendUint16(v, e.Behavior)
		a := e.SID.As16()
		v = append(v, a[:]...)
		ss := e.Struct.subTLV()
		v = append(append(v, byte(len(ss))), ss...)
		sub = append(append(sub, subSRv6EndSID, byte(len(v))), v...)
	}
	return append(append(b, byte(len(sub))), sub...)
}

// Binding is a mapping-server entry: a range of prefixes from Prefix with
// SIDs from Index (TLV 149, or 150 in a non-zero MT).
type Binding struct {
	Prefix   netip.Prefix
	Range    uint16
	Index    uint32
	Algo     byte
	MT       uint16
	Attached bool
}

func (b Binding) tlv() (byte, []byte) {
	var v []byte
	t := TLVSIDBinding
	if b.MT != 0 {
		t = TLVMTSIDBinding
		v = binary.BigEndian.AppendUint16(v, b.MT&0x0fff)
	}
	var fl byte
	if b.Prefix.Addr().Is6() {
		fl |= bindV6
	}
	if b.Attached {
		fl |= bindAttached
	}
	v = append(v, fl, 0)
	v = binary.BigEndian.AppendUint16(v, max(b.Range, 1))
	v = append(v, byte(b.Prefix.Bits()))
	v = append(v, b.Prefix.Masked().Addr().AsSlice()[:(b.Prefix.Bits()+7)/8]...)
	v = append(v, PrefixSID{Index: b.Index, Algo: b.Algo}.subTLV()...)
	return t, v
}

// SRv6 endpoint behaviors (IANA) by name.
var srv6Behaviors = map[string]uint16{
	"end": 1, "end-psp": 2, "end-usp": 3, "end-psp-usp": 4,
	"end.x": 5, "end.x-psp": 6, "end.x-usp": 7, "end.x-psp-usp": 8,
	"un": 43, "un-psp": 44, "un-usp": 45, "un-psp-usp": 46, // End with NEXT-CSID (uSID)
	"ua": 52, "ua-psp": 53, "ua-usp": 54, "ua-psp-usp": 55, // End.X with NEXT-CSID
}

// behaviorName names an endpoint behavior code.
func behaviorName(c uint16) string {
	for n, v := range srv6Behaviors {
		if v == c {
			return n
		}
	}
	return fmt.Sprint(c)
}

// endXFor maps an End behavior to the End.X behavior of the same flavor.
func endXFor(end uint16) uint16 {
	switch {
	case end >= 1 && end <= 4:
		return end + 4
	case end >= 43 && end <= 46:
		return end + 9
	}
	return 5
}

// ---- decoding for status ----

type FADInfo struct {
	Algorithm  byte   `json:"algorithm"`
	MetricType string `json:"metric_type"`
	Priority   byte   `json:"priority"`
	ExcludeAny uint32 `json:"exclude_any,omitempty"`
	IncludeAny uint32 `json:"include_any,omitempty"`
	IncludeAll uint32 `json:"include_all,omitempty"`
	// Our status only: we take part without advertising the definition.
	ParticipateOnly bool `json:"participate_only,omitempty"`
}

type LinkAttrInfo struct {
	FlexAlgo    bool   `json:"flex_algo"` // ASLA for the flex-algo application
	TEMetric    uint32 `json:"te_metric,omitempty"`
	DelayUS     uint32 `json:"delay_us,omitempty"`
	AdminGroups uint32 `json:"admin_groups,omitempty"`
}

type SRv6SIDInfo struct {
	SID      string `json:"sid"`
	Behavior string `json:"behavior"`
	Algo     byte   `json:"algorithm,omitempty"`
	Neighbor string `json:"neighbor,omitempty"` // LAN End.X
}

type LocatorInfo struct {
	Locator string        `json:"locator"`
	Metric  uint32        `json:"metric"`
	Algo    byte          `json:"algorithm,omitempty"`
	MT      uint16        `json:"mt,omitempty"`
	EndSIDs []SRv6SIDInfo `json:"end_sids,omitempty"`
}

type BindingInfo struct {
	Prefix string `json:"prefix"`
	Range  uint16 `json:"range"`
	Index  uint32 `json:"index"`
	Algo   byte   `json:"algorithm,omitempty"`
	MT     uint16 `json:"mt,omitempty"`
}

func metricTypeName(t byte) string {
	for n, v := range fadMetricTypes {
		if v == t {
			return n
		}
	}
	return fmt.Sprint(t)
}

func parseFAD(x []byte) (FADInfo, bool) {
	if len(x) < 4 {
		return FADInfo{}, false
	}
	f := FADInfo{Algorithm: x[0], MetricType: metricTypeName(x[1]), Priority: x[3]}
	for s := x[4:]; len(s) >= 2 && int(s[1]) <= len(s)-2; s = s[2+int(s[1]):] {
		v := s[2 : 2+int(s[1])]
		if len(v) < 4 {
			continue
		}
		switch s[0] {
		case fadExclAny:
			f.ExcludeAny = binary.BigEndian.Uint32(v)
		case fadInclAny:
			f.IncludeAny = binary.BigEndian.Uint32(v)
		case fadInclAll:
			f.IncludeAll = binary.BigEndian.Uint32(v)
		}
	}
	return f, true
}

func parseASLA(v []byte) *LinkAttrInfo {
	if len(v) < 2 {
		return nil
	}
	sl, ul := int(v[0]&0x7f), int(v[1]&0x7f)
	if len(v) < 2+sl+ul {
		return nil
	}
	a := &LinkAttrInfo{FlexAlgo: sl > 0 && v[2]&sabmFlexAlgo != 0}
	for s := v[2+sl+ul:]; len(s) >= 2 && int(s[1]) <= len(s)-2; s = s[2+int(s[1]):] {
		x := s[2 : 2+int(s[1])]
		switch {
		case s[0] == aslaAdminGroup && len(x) == 4:
			a.AdminGroups = binary.BigEndian.Uint32(x)
		case s[0] == aslaTEMetric && len(x) == 3:
			a.TEMetric = uint32(x[0])<<16 | uint32(x[1])<<8 | uint32(x[2])
		case s[0] == aslaMinMaxDly && len(x) == 8:
			a.DelayUS = uint32(x[1])<<16 | uint32(x[2])<<8 | uint32(x[3])
		}
	}
	return a
}

// parseNbrSubTLVs decodes an IS reachability entry's ASLA and End.X SIDs.
func parseNbrSubTLVs(sub []byte, n *NbrInfo) {
	for len(sub) >= 2 && int(sub[1]) <= len(sub)-2 {
		t, v := sub[0], sub[2:2+int(sub[1])]
		sub = sub[2+int(sub[1]):]
		switch t {
		case subASLA:
			n.Attrs = parseASLA(v)
		case subEndXSID, subLANEndXSID:
			var x SRv6SIDInfo
			if t == subLANEndXSID {
				if len(v) < 6 {
					continue
				}
				x.Neighbor, v = SysID(v[:6]).String(), v[6:]
			}
			if len(v) < 21 {
				continue
			}
			x.Algo, x.Behavior, x.SID = v[1], behaviorName(binary.BigEndian.Uint16(v[3:])), netip.AddrFrom16([16]byte(v[5:21])).String()
			n.EndX = append(n.EndX, x)
		}
	}
}

func parseLocators(v []byte) []LocatorInfo {
	if len(v) < 2 {
		return nil
	}
	mt := binary.BigEndian.Uint16(v) & 0x0fff
	var out []LocatorInfo
	for v = v[2:]; len(v) >= 7; {
		bits := int(v[6])
		n := (bits + 7) / 8
		if bits > 128 || len(v) < 7+n+1 {
			break
		}
		var a [16]byte
		copy(a[:], v[7:7+n])
		l := LocatorInfo{Locator: netip.PrefixFrom(netip.AddrFrom16(a), bits).String(), Metric: binary.BigEndian.Uint32(v), Algo: v[5], MT: mt}
		sl := int(v[7+n])
		end := 8 + n + sl
		if end > len(v) {
			break
		}
		for s := v[8+n : end]; len(s) >= 2 && int(s[1]) <= len(s)-2; s = s[2+int(s[1]):] {
			x := s[2 : 2+int(s[1])]
			if s[0] == subSRv6EndSID && len(x) >= 19 {
				l.EndSIDs = append(l.EndSIDs, SRv6SIDInfo{SID: netip.AddrFrom16([16]byte(x[3:19])).String(), Behavior: behaviorName(binary.BigEndian.Uint16(x[1:]))})
			}
		}
		out = append(out, l)
		v = v[end:]
	}
	return out
}

func parseBinding(v []byte, mt bool) (BindingInfo, bool) {
	var b BindingInfo
	if mt {
		if len(v) < 2 {
			return b, false
		}
		b.MT, v = binary.BigEndian.Uint16(v)&0x0fff, v[2:]
	}
	if len(v) < 5 {
		return b, false
	}
	v6 := v[0]&bindV6 != 0
	b.Range = binary.BigEndian.Uint16(v[2:])
	bits := int(v[4])
	n := (bits + 7) / 8
	if len(v) < 5+n || bits > 128 || !v6 && bits > 32 {
		return b, false
	}
	if v6 {
		var a [16]byte
		copy(a[:], v[5:5+n])
		b.Prefix = netip.PrefixFrom(netip.AddrFrom16(a), bits).String()
	} else {
		var a [4]byte
		copy(a[:], v[5:5+n])
		b.Prefix = netip.PrefixFrom(netip.AddrFrom4(a), bits).String()
	}
	if s := parsePrefixSIDs(v[5+n:]); len(s) > 0 {
		b.Index, b.Algo = s[0].Index, s[0].Algo
	}
	return b, true
}

// groupMask turns admin group numbers (0-31) into a bit mask.
func groupMask(gs []int) uint32 {
	var m uint32
	for _, g := range gs {
		m |= 1 << uint(g)
	}
	return m
}

func groupList(m uint32) string {
	var out []string
	for i := 0; i < 32; i++ {
		if m&(1<<uint(i)) != 0 {
			out = append(out, fmt.Sprint(i))
		}
	}
	return strings.Join(out, ",")
}

func msdName(t byte) string {
	switch t {
	case msdBaseMPLS:
		return "base-mpls-imposition"
	case 2:
		return "erld"
	case msdSRHMaxSL:
		return "srh-max-sl"
	case msdSRHEndPop:
		return "srh-max-end-pop"
	case msdSRHHEncaps:
		return "srh-max-h-encaps"
	case msdSRHEndD:
		return "srh-max-end-d"
	}
	return fmt.Sprint(t)
}
