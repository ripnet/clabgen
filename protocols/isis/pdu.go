package isis

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/ripnet/clabgen/internal/pkt"
)

// ISO 10589 PDU types.
const (
	PDULANHelloL1 uint8 = 15
	PDULANHelloL2 uint8 = 16
	PDUP2PHello   uint8 = 17
	PDULSPL1      uint8 = 18
	PDULSPL2      uint8 = 20
	PDUCSNPL1     uint8 = 24
	PDUCSNPL2     uint8 = 25
	PDUPSNPL1     uint8 = 26
	PDUPSNPL2     uint8 = 27
)

// TLV types used here.
const (
	TLVAreaAddresses uint8 = 1
	TLVISNeighbors   uint8 = 2 // narrow IS reachability (decode only)
	TLVLANNeighbors  uint8 = 6
	TLVPadding       uint8 = 8
	TLVLSPEntries    uint8 = 9
	TLVAuth          uint8 = 10
	TLVExtISReach    uint8 = 22
	TLVIPIntReach    uint8 = 128 // narrow (decode only)
	TLVProtocols     uint8 = 129
	TLVIPExtReach    uint8 = 130 // narrow (decode only)
	TLVIPAddresses   uint8 = 132
	TLVTERouterID    uint8 = 134
	TLVExtIPReach    uint8 = 135
	TLVHostname      uint8 = 137
	TLVIPv6Addresses uint8 = 232
	TLVIPv6Reach     uint8 = 236
	TLVP2PAdjacency  uint8 = 240
	TLVRouterCapab   uint8 = 242
	TLVMTISReach     uint8 = 222
	TLVMultiTopology uint8 = 229
	TLVMTIPReach     uint8 = 235
	TLVMTIPv6Reach   uint8 = 237
	NLPIDIPv4        byte  = 0xcc
	NLPIDIPv6        byte  = 0x8e
	circuitL1        uint8 = 1
	circuitL2        uint8 = 2
	circuitL1L2      uint8 = 3
	maxLSPLifetime         = 1200
	irpd             byte  = 0x83
)

// Multi-topology IDs (RFC 5120).
const (
	MTIPv4Unicast   uint16 = 0
	MTIPv6Unicast   uint16 = 2
	MTIPv4Multicast uint16 = 3
	MTIPv6Multicast uint16 = 4
	mtOverload      uint16 = 0x8000 // O bit in TLV 229
	mtAttached      uint16 = 0x4000 // A bit
)

var mtNames = map[uint16]string{MTIPv4Unicast: "ipv4-unicast", MTIPv6Unicast: "ipv6-unicast", MTIPv4Multicast: "ipv4-multicast", MTIPv6Multicast: "ipv6-multicast"}

// MTName names an MT ID.
func MTName(id uint16) string {
	if n, ok := mtNames[id]; ok {
		return n
	}
	return fmt.Sprintf("mt-%d", id)
}

// Multicast destinations.
var (
	AllL1ISs = pkt.MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x14}
	AllL2ISs = pkt.MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x15}
	AllISs   = pkt.MAC{0x09, 0x00, 0x2b, 0x00, 0x00, 0x05}
)

var llc = []byte{0xfe, 0xfe, 0x03}

// SysID is a 6-byte system ID.
type SysID [6]byte

func (s SysID) String() string {
	h := hex.EncodeToString(s[:])
	return h[0:4] + "." + h[4:8] + "." + h[8:12]
}

// ParseSysID accepts 0000.0000.0001, 000000000001 or 00:00:00:00:00:01.
func ParseSysID(v string) (SysID, error) {
	var s SysID
	h := strings.NewReplacer(".", "", ":", "", "-", "").Replace(v)
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 6 {
		return s, fmt.Errorf("system ID %q: want 12 hex digits like 0000.0000.0001", v)
	}
	copy(s[:], b)
	return s, nil
}

// NodeID is a system ID plus pseudonode number.
type NodeID [7]byte

func (n NodeID) Sys() SysID { return SysID(n[:6]) }
func (n NodeID) String() string {
	return fmt.Sprintf("%s.%02x", n.Sys(), n[6])
}

// LSPID is a node ID plus fragment number.
type LSPID [8]byte

func (l LSPID) Node() NodeID { return NodeID(l[:7]) }
func (l LSPID) String() string {
	return fmt.Sprintf("%s-%02x", l.Node(), l[7])
}

func MakeLSPID(s SysID, pn, frag byte) LSPID {
	var l LSPID
	copy(l[:], s[:])
	l[6], l[7] = pn, frag
	return l
}

// ---- TLVs ----

type TLV struct {
	Type  uint8
	Value []byte
}

type TLVs []TLV

func parseTLVs(b []byte) (TLVs, error) {
	var out TLVs
	for len(b) > 0 {
		if len(b) < 2 || int(b[1]) > len(b)-2 {
			return out, errors.New("truncated TLV")
		}
		out = append(out, TLV{b[0], b[2 : 2+int(b[1])]})
		b = b[2+int(b[1]):]
	}
	return out, nil
}

// All returns the values of every TLV of type t.
func (ts TLVs) All(t uint8) [][]byte {
	var out [][]byte
	for _, x := range ts {
		if x.Type == t {
			out = append(out, x.Value)
		}
	}
	return out
}

func (ts TLVs) First(t uint8) ([]byte, bool) {
	for _, x := range ts {
		if x.Type == t {
			return x.Value, true
		}
	}
	return nil, false
}

// tlvBuf appends TLVs, splitting values over 255 bytes for TLVs made of
// fixed-size or self-delimiting items (the caller adds items one by one).
type tlvBuf struct{ b []byte }

func (w *tlvBuf) add(t uint8, v []byte) {
	w.b = append(w.b, t, byte(len(v)))
	w.b = append(w.b, v...)
}

// addMTItems is addItems for MT TLVs, whose values start with the MT ID.
func (w *tlvBuf) addMTItems(t uint8, mt uint16, items [][]byte) {
	hdr := []byte{byte(mt >> 8 & 0x0f), byte(mt)}
	cur := append([]byte{}, hdr...)
	for _, it := range items {
		if len(cur)+len(it) > 255 {
			w.add(t, cur)
			cur = append([]byte{}, hdr...)
		}
		cur = append(cur, it...)
	}
	if len(cur) > 2 {
		w.add(t, cur)
	}
}

// mtTLV is TLV 229's value.
func mtTLV(mts []uint16, overload bool) []byte {
	var v []byte
	for _, m := range mts {
		x := m & 0x0fff
		if overload {
			x |= mtOverload
		}
		v = binary.BigEndian.AppendUint16(v, x)
	}
	return v
}

// parseMTs decodes TLV 229 values.
func parseMTs(vs [][]byte) (ids []uint16, overloaded []uint16) {
	for _, v := range vs {
		for ; len(v) >= 2; v = v[2:] {
			x := binary.BigEndian.Uint16(v)
			ids = append(ids, x&0x0fff)
			if x&mtOverload != 0 {
				overloaded = append(overloaded, x&0x0fff)
			}
		}
	}
	return
}

// addItems packs items into as few TLVs of type t as fit (255 bytes each).
func (w *tlvBuf) addItems(t uint8, items [][]byte) {
	var cur []byte
	for _, it := range items {
		if len(cur)+len(it) > 255 {
			w.add(t, cur)
			cur = nil
		}
		cur = append(cur, it...)
	}
	if len(cur) > 0 {
		w.add(t, cur)
	}
}

// ---- PDU framing ----

func header(typ, hdrLen uint8) []byte {
	return []byte{irpd, hdrLen, 1, 0, typ, 1, 0, 0}
}

// frame wraps a PDU in the LLC header for sending.
func frame(pdu []byte) []byte { return append(append([]byte{}, llc...), pdu...) }

// PDU is a decoded IS-IS PDU (one of the typed fields is set).
type PDU struct {
	Type  uint8
	P2P   *P2PHello
	LAN   *LANHello
	LSP   *LSP
	SNP   *SNP
	Raw   []byte // the PDU without LLC
	TLVs  TLVs
	Areas [][]byte
	// MTs from TLV 229 (hellos); HasMT is false without the TLV, which
	// means MT 0 only.
	MTs   []uint16
	HasMT bool
}

var ErrNotISIS = errors.New("not IS-IS")

// Decode parses an LLC payload carrying an IS-IS PDU.
func Decode(b []byte) (*PDU, error) {
	if len(b) < 3+8 || b[0] != 0xfe || b[1] != 0xfe || b[2] != 0x03 || b[3] != irpd {
		return nil, ErrNotISIS
	}
	b = b[3:]
	if b[3] != 0 && b[3] != 6 || b[2] != 1 || b[5] != 1 {
		return nil, errors.New("unsupported IS-IS header (ID length or version)")
	}
	hl := int(b[1])
	p := &PDU{Type: b[4] & 0x1f}
	need := map[uint8]int{PDUP2PHello: 20, PDULANHelloL1: 27, PDULANHelloL2: 27, PDULSPL1: 27, PDULSPL2: 27,
		PDUCSNPL1: 33, PDUCSNPL2: 33, PDUPSNPL1: 17, PDUPSNPL2: 17}[p.Type]
	if need == 0 {
		return nil, fmt.Errorf("unsupported PDU type %d", p.Type)
	}
	if hl != need || len(b) < hl {
		return nil, fmt.Errorf("PDU type %d: bad header length %d", p.Type, hl)
	}
	var plen int
	switch p.Type {
	case PDUP2PHello, PDULANHelloL1, PDULANHelloL2:
		plen = int(binary.BigEndian.Uint16(b[17:]))
	default:
		plen = int(binary.BigEndian.Uint16(b[8:]))
	}
	if plen < hl || plen > len(b) {
		return nil, fmt.Errorf("PDU length %d out of range", plen)
	}
	b = b[:plen]
	p.Raw = b
	tl, err := parseTLVs(b[hl:])
	if err != nil {
		return nil, err
	}
	p.TLVs = tl
	if vs := tl.All(TLVMultiTopology); len(vs) > 0 {
		p.MTs, _ = parseMTs(vs)
		p.HasMT = true
	}
	for _, v := range tl.All(TLVAreaAddresses) {
		for len(v) > 0 && int(v[0]) < len(v) {
			p.Areas = append(p.Areas, v[1:1+v[0]])
			v = v[1+v[0]:]
		}
	}
	switch p.Type {
	case PDUP2PHello:
		p.P2P = &P2PHello{Circuit: b[8] & 3, Source: SysID(b[9:15]), Hold: binary.BigEndian.Uint16(b[15:]), LocalCircuit: b[19]}
		if v, ok := tl.First(TLVP2PAdjacency); ok && len(v) >= 1 {
			a := &ThreeWay{State: v[0]}
			if len(v) >= 5 {
				a.ExtCircuit = binary.BigEndian.Uint32(v[1:])
			}
			if len(v) >= 11 {
				a.NeighborSys, a.HasNeighbor = SysID(v[5:11]), true
			}
			if len(v) >= 15 {
				a.NeighborExtCircuit = binary.BigEndian.Uint32(v[11:])
			}
			p.P2P.ThreeWay = a
		}
	case PDULANHelloL1, PDULANHelloL2:
		h := &LANHello{Level: 1, Circuit: b[8] & 3, Source: SysID(b[9:15]), Hold: binary.BigEndian.Uint16(b[15:]), Priority: b[19] & 0x7f, LANID: NodeID(b[20:27])}
		if p.Type == PDULANHelloL2 {
			h.Level = 2
		}
		for _, v := range tl.All(TLVLANNeighbors) {
			for len(v) >= 6 {
				h.Neighbors = append(h.Neighbors, pkt.MAC(v[:6]))
				v = v[6:]
			}
		}
		p.LAN = h
	case PDULSPL1, PDULSPL2:
		l := &LSP{Level: 1, Lifetime: binary.BigEndian.Uint16(b[10:]), ID: LSPID(b[12:20]), Seq: binary.BigEndian.Uint32(b[20:]),
			Checksum: binary.BigEndian.Uint16(b[24:]), Flags: b[26], TLVs: tl, Raw: b}
		if p.Type == PDULSPL2 {
			l.Level = 2
		}
		p.LSP = l
	case PDUCSNPL1, PDUCSNPL2, PDUPSNPL1, PDUPSNPL2:
		s := &SNP{Level: 1, Complete: p.Type == PDUCSNPL1 || p.Type == PDUCSNPL2, Source: NodeID(b[10:17])}
		if p.Type == PDUCSNPL2 || p.Type == PDUPSNPL2 {
			s.Level = 2
		}
		if s.Complete {
			s.Start, s.End = LSPID(b[17:25]), LSPID(b[25:33])
		}
		for _, v := range tl.All(TLVLSPEntries) {
			for len(v) >= 16 {
				s.Entries = append(s.Entries, LSPEntry{Lifetime: binary.BigEndian.Uint16(v[0:]), ID: LSPID(v[2:10]),
					Seq: binary.BigEndian.Uint32(v[10:]), Checksum: binary.BigEndian.Uint16(v[14:])})
				v = v[16:]
			}
		}
		p.SNP = s
	}
	return p, nil
}

// ---- hellos ----

// 3-way adjacency states (RFC 5303).
const (
	AdjUp           uint8 = 0
	AdjInitializing uint8 = 1
	AdjDown         uint8 = 2
)

type ThreeWay struct {
	State              uint8
	ExtCircuit         uint32
	NeighborSys        SysID
	HasNeighbor        bool
	NeighborExtCircuit uint32
}

type P2PHello struct {
	Circuit      uint8
	Source       SysID
	Hold         uint16
	LocalCircuit uint8
	ThreeWay     *ThreeWay
}

type LANHello struct {
	Level     int
	Circuit   uint8
	Source    SysID
	Hold      uint16
	Priority  uint8
	LANID     NodeID
	Neighbors []pkt.MAC
}

// helloTLVs are the TLVs every hello carries.
func helloTLVs(w *tlvBuf, areas [][]byte, v4, v6 []netip.Addr, mts []uint16) {
	var a []byte
	for _, x := range areas {
		a = append(a, byte(len(x)))
		a = append(a, x...)
	}
	w.add(TLVAreaAddresses, a)
	var nl []byte
	if len(v4) > 0 {
		nl = append(nl, NLPIDIPv4)
	}
	if len(v6) > 0 {
		nl = append(nl, NLPIDIPv6)
	}
	if len(nl) > 0 {
		w.add(TLVProtocols, nl)
	}
	if len(v4) > 0 {
		var b []byte
		for _, x := range v4 {
			a4 := x.As4()
			b = append(b, a4[:]...)
		}
		w.add(TLVIPAddresses, b[:min(len(b), 252)])
	}
	if len(v6) > 0 {
		// Hellos carry link-local IPv6 addresses (RFC 5308).
		var b []byte
		for _, x := range v6 {
			a6 := x.As16()
			b = append(b, a6[:]...)
		}
		w.add(TLVIPv6Addresses, b[:min(len(b), 240)])
	}
	if len(mts) > 0 {
		w.add(TLVMultiTopology, mtTLV(mts, false))
	}
}

// pad adds padding TLVs until the PDU is size bytes (hello padding).
func pad(w *tlvBuf, have, size int) {
	for left := size - have - len(w.b); left >= 2; left = size - have - len(w.b) {
		n := min(left-2, 255)
		w.add(TLVPadding, make([]byte, n))
	}
}

func setLen(b []byte, off int) { binary.BigEndian.PutUint16(b[off:], uint16(len(b))) }

// EncodeP2PHello builds a point-to-point IIH.
func EncodeP2PHello(circuit uint8, src SysID, hold uint16, localCircuit uint8, areas [][]byte, v4, v6 []netip.Addr, mts []uint16, tw ThreeWay, padTo int) []byte {
	b := header(PDUP2PHello, 20)
	b = append(b, circuit)
	b = append(b, src[:]...)
	b = binary.BigEndian.AppendUint16(b, hold)
	b = append(b, 0, 0, localCircuit)
	var w tlvBuf
	helloTLVs(&w, areas, v4, v6, mts)
	a := []byte{tw.State}
	a = binary.BigEndian.AppendUint32(a, tw.ExtCircuit)
	if tw.HasNeighbor {
		a = append(a, tw.NeighborSys[:]...)
		a = binary.BigEndian.AppendUint32(a, tw.NeighborExtCircuit)
	}
	w.add(TLVP2PAdjacency, a)
	if padTo > 0 {
		pad(&w, len(b), padTo)
	}
	b = append(b, w.b...)
	setLen(b, 17)
	return b
}

// EncodeLANHello builds a LAN IIH for level 1 or 2.
func EncodeLANHello(level int, circuit uint8, src SysID, hold uint16, prio uint8, lanID NodeID, areas [][]byte, v4, v6 []netip.Addr, mts []uint16, neighbors []pkt.MAC, padTo int) []byte {
	typ := PDULANHelloL1
	if level == 2 {
		typ = PDULANHelloL2
	}
	b := header(typ, 27)
	b = append(b, circuit)
	b = append(b, src[:]...)
	b = binary.BigEndian.AppendUint16(b, hold)
	b = append(b, 0, 0, prio&0x7f)
	b = append(b, lanID[:]...)
	var w tlvBuf
	helloTLVs(&w, areas, v4, v6, mts)
	var items [][]byte
	for _, m := range neighbors {
		items = append(items, append([]byte{}, m[:]...))
	}
	w.addItems(TLVLANNeighbors, items)
	if padTo > 0 {
		pad(&w, len(b), padTo)
	}
	b = append(b, w.b...)
	setLen(b, 17)
	return b
}

// ---- LSPs ----

type LSP struct {
	Level    int
	Lifetime uint16
	ID       LSPID
	Seq      uint32
	Checksum uint16
	Flags    byte // P | ATT(4) | OL | IS type(2)
	TLVs     TLVs
	Raw      []byte
}

const (
	lspFlagOL     byte = 0x04
	lspTypeL1     byte = 0x01
	lspTypeL1L2   byte = 0x03
	lspRawLifeOff      = 10
)

// Overload reports the LSP's overload bit.
func (l *LSP) Overload() bool { return l.Flags&lspFlagOL != 0 }

// Fletcher computes the ISO 8473 checksum of b with the checksum field at
// off (the two bytes there are treated as zero).
func Fletcher(b []byte, off int) uint16 { return pkt.Fletcher(b, off) }

func fletcherOK(b []byte) bool { return pkt.FletcherOK(b) }

// lspBody is everything an LSP carries apart from its header fields.
type lspBody struct {
	Areas     [][]byte
	Hostname  string
	RouterID  netip.Addr
	V4Addrs   []netip.Addr
	V6Addrs   []netip.Addr
	Protocols []byte
	Neighbors []ISReach // MT 0 (TLV 22)
	Prefixes  []IPReach
	Overload  bool
	Attached  bool
	// Multi-topology: the MTs we take part in (TLV 229, fragment 0) and
	// IS reachability per non-zero MT (TLV 222).
	MTs         []uint16
	MTNeighbors map[uint16][]ISReach
	SR          *SRCap    // Router Capability TLV with SR capabilities
	Locators    []Locator // SRv6 (TLV 27, per MT)
	Bindings    []Binding // mapping server (TLV 149/150)
}

type ISReach struct {
	Neighbor NodeID
	Metric   uint32 // 24 bits
	AdjSIDs  []AdjSID
	Attrs    *LinkAttrs // ASLA for flex-algo
	EndX     []EndXSID  // SRv6
}

type IPReach struct {
	Prefix netip.Prefix
	Metric uint32
	Down   bool        // up/down bit (leaked from L2 to L1)
	MT     uint16      // 0: standard TLVs 135/236; else TLV 235/237 for that MT
	SIDs   []PrefixSID // Prefix-SIDs, one per algorithm
}

// EncodeLSP builds one LSP fragment. The checksum is computed.
func EncodeLSP(level int, id LSPID, seq uint32, lifetime uint16, isType byte, body lspBody) []byte {
	typ := PDULSPL1
	if level == 2 {
		typ = PDULSPL2
	}
	b := header(typ, 27)
	b = append(b, 0, 0) // length
	b = binary.BigEndian.AppendUint16(b, lifetime)
	b = append(b, id[:]...)
	b = binary.BigEndian.AppendUint32(b, seq)
	b = append(b, 0, 0) // checksum
	flags := isType
	if body.Overload {
		flags |= lspFlagOL
	}
	if body.Attached {
		flags |= 0x08 // ATT default metric
	}
	b = append(b, flags)
	var w tlvBuf
	if len(body.Areas) > 0 {
		var a []byte
		for _, x := range body.Areas {
			a = append(a, byte(len(x)))
			a = append(a, x...)
		}
		w.add(TLVAreaAddresses, a)
	}
	if len(body.Protocols) > 0 {
		w.add(TLVProtocols, body.Protocols)
	}
	if body.Hostname != "" {
		w.add(TLVHostname, []byte(body.Hostname[:min(len(body.Hostname), 255)]))
	}
	if body.RouterID.IsValid() {
		r := body.RouterID.As4()
		w.add(TLVTERouterID, r[:])
	}
	if len(body.MTs) > 0 {
		w.add(TLVMultiTopology, mtTLV(body.MTs, body.Overload))
	}
	if body.SR != nil {
		w.add(TLVRouterCapab, body.SR.tlv())
	}
	for _, b := range body.Bindings {
		w.add(b.tlv())
	}
	locs := map[uint16][][]byte{}
	var locMTs []uint16
	for _, l := range body.Locators {
		if _, ok := locs[l.MT]; !ok {
			locMTs = append(locMTs, l.MT)
		}
		locs[l.MT] = append(locs[l.MT], l.entry())
	}
	sort.Slice(locMTs, func(i, j int) bool { return locMTs[i] < locMTs[j] })
	for _, mt := range locMTs {
		w.addMTItems(TLVSRv6Locator, mt, locs[mt])
	}
	var items [][]byte
	for _, a := range body.V4Addrs {
		x := a.As4()
		items = append(items, x[:])
	}
	w.addItems(TLVIPAddresses, items)
	items = nil
	for _, a := range body.V6Addrs {
		x := a.As16()
		items = append(items, x[:])
	}
	w.addItems(TLVIPv6Addresses, items)
	items = nil
	for _, n := range body.Neighbors {
		items = append(items, isItem(n))
	}
	w.addItems(TLVExtISReach, items)
	var mtIDs []uint16
	for mt := range body.MTNeighbors {
		mtIDs = append(mtIDs, mt)
	}
	sort.Slice(mtIDs, func(i, j int) bool { return mtIDs[i] < mtIDs[j] })
	for _, mt := range mtIDs {
		items = nil
		for _, n := range body.MTNeighbors[mt] {
			items = append(items, isItem(n))
		}
		w.addMTItems(TLVMTISReach, mt, items)
	}
	var v4, v6 [][]byte
	mt4, mt6 := map[uint16][][]byte{}, map[uint16][][]byte{}
	for _, p := range body.Prefixes {
		bits := p.Prefix.Bits()
		addr := p.Prefix.Masked().Addr().AsSlice()[:(bits+7)/8]
		var sub []byte
		for _, x := range p.SIDs {
			sub = append(sub, x.subTLV()...)
		}
		if p.Prefix.Addr().Is4() {
			ctl := byte(bits)
			if p.Down {
				ctl |= 0x80
			}
			if sub != nil {
				ctl |= 0x40 // sub-TLVs present
			}
			it := append(append(binary.BigEndian.AppendUint32(nil, p.Metric), ctl), addr...)
			if sub != nil {
				it = append(append(it, byte(len(sub))), sub...)
			}
			if p.MT != 0 {
				mt4[p.MT] = append(mt4[p.MT], it)
			} else {
				v4 = append(v4, it)
			}
		} else {
			fl := byte(0)
			if p.Down {
				fl |= 0x80
			}
			if sub != nil {
				fl |= 0x20 // sub-TLVs present
			}
			it := append(append(binary.BigEndian.AppendUint32(nil, p.Metric), fl, byte(bits)), addr...)
			if sub != nil {
				it = append(append(it, byte(len(sub))), sub...)
			}
			if p.MT != 0 {
				mt6[p.MT] = append(mt6[p.MT], it)
			} else {
				v6 = append(v6, it)
			}
		}
	}
	w.addItems(TLVExtIPReach, v4)
	w.addItems(TLVIPv6Reach, v6)
	for _, m := range []struct {
		t  uint8
		by map[uint16][][]byte
	}{{TLVMTIPReach, mt4}, {TLVMTIPv6Reach, mt6}} {
		ids := make([]uint16, 0, len(m.by))
		for id := range m.by {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			w.addMTItems(m.t, id, m.by[id])
		}
	}
	b = append(b, w.b...)
	setLen(b, 8)
	cs := Fletcher(b[12:], 24-12)
	binary.BigEndian.PutUint16(b[24:], cs)
	return b
}

// lspFitsMTU reports whether an LSP of n bytes fits (with LLC) in mtu.
func lspFitsMTU(n, mtu int) bool { return n+3 <= mtu }

// LSPInfo is the decoded content of an LSP, for status.
type LSPInfo struct {
	SR         *SRCapInfo    `json:"sr,omitempty"` // Router Capability TLV, SR part
	Hostname   string        `json:"hostname,omitempty"`
	Topologies []string      `json:"topologies,omitempty"` // TLV 229 (MT 0 alone when absent)
	Areas      []string      `json:"areas,omitempty"`
	Overload   bool          `json:"overload,omitempty"`
	Attached   bool          `json:"attached,omitempty"`
	RouterID   string        `json:"router_id,omitempty"`
	Addresses  []string      `json:"addresses,omitempty"`
	Neighbors  []NbrInfo     `json:"neighbors,omitempty"`
	Prefixes   []PfxInfo     `json:"prefixes,omitempty"`
	Locators   []LocatorInfo `json:"srv6_locators,omitempty"`
	Bindings   []BindingInfo `json:"sid_bindings,omitempty"`
}

type NbrInfo struct {
	ID      string        `json:"id"`
	Metric  uint32        `json:"metric"`
	MT      uint16        `json:"mt,omitempty"`
	AdjSIDs []AdjSIDInfo  `json:"adj_sids,omitempty"`
	Attrs   *LinkAttrInfo `json:"link_attributes,omitempty"`
	EndX    []SRv6SIDInfo `json:"end_x_sids,omitempty"`
}

type PfxInfo struct {
	Prefix string    `json:"prefix"`
	Metric uint32    `json:"metric"`
	Down   bool      `json:"down,omitempty"`
	MT     uint16    `json:"mt,omitempty"`
	SID    *SIDInfo  `json:"sid,omitempty"`       // algorithm 0
	Flex   []SIDInfo `json:"flex_sids,omitempty"` // other algorithms (flex-algo)
}

// setSIDs files decoded Prefix-SIDs by algorithm.
func (p *PfxInfo) setSIDs(sub []byte) {
	for _, s := range parsePrefixSIDs(sub) {
		if s.Algo == 0 && p.SID == nil {
			c := s
			p.SID = &c
		} else {
			p.Flex = append(p.Flex, s)
		}
	}
}

// Info decodes the TLVs operators care about.
func (l *LSP) Info() LSPInfo {
	var in LSPInfo
	in.Overload, in.Attached = l.Overload(), l.Flags&0x78 != 0
	if v, ok := l.TLVs.First(TLVHostname); ok {
		in.Hostname = string(v)
	}
	for _, v := range l.TLVs.All(TLVAreaAddresses) {
		for len(v) > 0 && int(v[0]) < len(v) {
			in.Areas = append(in.Areas, AreaString(v[1:1+v[0]]))
			v = v[1+v[0]:]
		}
	}
	if v, ok := l.TLVs.First(TLVRouterCapab); ok {
		in.SR = parseRouterCap(v)
	}
	for _, v := range l.TLVs.All(TLVSRv6Locator) {
		in.Locators = append(in.Locators, parseLocators(v)...)
	}
	for _, t := range []uint8{TLVSIDBinding, TLVMTSIDBinding} {
		for _, v := range l.TLVs.All(t) {
			if b, ok := parseBinding(v, t == TLVMTSIDBinding); ok {
				in.Bindings = append(in.Bindings, b)
			}
		}
	}
	if v, ok := l.TLVs.First(TLVTERouterID); ok && len(v) == 4 {
		in.RouterID = netip.AddrFrom4([4]byte(v)).String()
	}
	for _, v := range l.TLVs.All(TLVIPAddresses) {
		for ; len(v) >= 4; v = v[4:] {
			in.Addresses = append(in.Addresses, netip.AddrFrom4([4]byte(v[:4])).String())
		}
	}
	for _, v := range l.TLVs.All(TLVIPv6Addresses) {
		for ; len(v) >= 16; v = v[16:] {
			in.Addresses = append(in.Addresses, netip.AddrFrom16([16]byte(v[:16])).String())
		}
	}
	if vs := l.TLVs.All(TLVMultiTopology); len(vs) > 0 {
		ids, ol := parseMTs(vs)
		for _, id := range ids {
			n := MTName(id)
			for _, o := range ol {
				if o == id {
					n += " (overload)"
				}
			}
			in.Topologies = append(in.Topologies, n)
		}
	}
	isReach := func(v []byte, mt uint16) {
		for len(v) >= 11 {
			sl := int(v[10])
			if len(v) < 11+sl {
				break
			}
			n := NbrInfo{ID: NodeID(v[:7]).String(), Metric: uint32(v[7])<<16 | uint32(v[8])<<8 | uint32(v[9]), MT: mt, AdjSIDs: parseAdjSIDs(v[11 : 11+sl])}
			parseNbrSubTLVs(v[11:11+sl], &n)
			in.Neighbors = append(in.Neighbors, n)
			v = v[11+sl:]
		}
	}
	for _, v := range l.TLVs.All(TLVExtISReach) {
		isReach(v, 0)
	}
	for _, v := range l.TLVs.All(TLVMTISReach) {
		if len(v) >= 2 {
			isReach(v[2:], binary.BigEndian.Uint16(v)&0x0fff)
		}
	}
	for _, v := range l.TLVs.All(TLVISNeighbors) {
		if len(v) > 0 {
			v = v[1:] // virtual flag
		}
		for ; len(v) >= 11; v = v[11:] {
			in.Neighbors = append(in.Neighbors, NbrInfo{ID: NodeID(v[4:11]).String(), Metric: uint32(v[0] & 0x3f)})
		}
	}
	ip4 := func(v []byte, mt uint16) {
		for len(v) >= 5 {
			ctl := v[4]
			bits := int(ctl & 0x3f)
			n := (bits + 7) / 8
			if bits > 32 || len(v) < 5+n {
				break
			}
			var a [4]byte
			copy(a[:], v[5:5+n])
			p := PfxInfo{Prefix: netip.PrefixFrom(netip.AddrFrom4(a), bits).String(), Metric: binary.BigEndian.Uint32(v), Down: ctl&0x80 != 0, MT: mt}
			off := 5 + n
			if ctl&0x40 != 0 && len(v) > off {
				if end := off + 1 + int(v[off]); end <= len(v) {
					p.setSIDs(v[off+1 : end])
				}
				off += 1 + int(v[off])
			}
			in.Prefixes = append(in.Prefixes, p)
			if off > len(v) {
				break
			}
			v = v[off:]
		}
	}
	for _, v := range l.TLVs.All(TLVExtIPReach) {
		ip4(v, 0)
	}
	for _, v := range l.TLVs.All(TLVMTIPReach) {
		if len(v) >= 2 {
			ip4(v[2:], binary.BigEndian.Uint16(v)&0x0fff)
		}
	}
	for _, t := range []uint8{TLVIPIntReach, TLVIPExtReach} {
		for _, v := range l.TLVs.All(t) {
			for ; len(v) >= 12; v = v[12:] {
				m := netip.AddrFrom4([4]byte(v[8:12]))
				bits := 0
				for _, x := range m.AsSlice() {
					for ; x&0x80 != 0; x <<= 1 {
						bits++
					}
				}
				in.Prefixes = append(in.Prefixes, PfxInfo{Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte(v[4:8])), bits).String(), Metric: uint32(v[0] & 0x3f)})
			}
		}
	}
	ip6 := func(v []byte, mt uint16) {
		for len(v) >= 6 {
			fl, bits := v[4], int(v[5])
			n := (bits + 7) / 8
			if bits > 128 || len(v) < 6+n {
				break
			}
			var a [16]byte
			copy(a[:], v[6:6+n])
			p := PfxInfo{Prefix: netip.PrefixFrom(netip.AddrFrom16(a), bits).String(), Metric: binary.BigEndian.Uint32(v), Down: fl&0x80 != 0, MT: mt}
			off := 6 + n
			if fl&0x20 != 0 && len(v) > off {
				if end := off + 1 + int(v[off]); end <= len(v) {
					p.setSIDs(v[off+1 : end])
				}
				off += 1 + int(v[off])
			}
			in.Prefixes = append(in.Prefixes, p)
			if off > len(v) {
				break
			}
			v = v[off:]
		}
	}
	for _, v := range l.TLVs.All(TLVIPv6Reach) {
		ip6(v, 0)
	}
	for _, v := range l.TLVs.All(TLVMTIPv6Reach) {
		if len(v) >= 2 {
			ip6(v[2:], binary.BigEndian.Uint16(v)&0x0fff)
		}
	}
	return in
}

// AreaString formats an area address like 49.0001.
func AreaString(a []byte) string {
	h := hex.EncodeToString(a)
	if len(h) <= 2 {
		return h
	}
	out := h[:2]
	for i := 2; i < len(h); i += 4 {
		out += "." + h[i:min(i+4, len(h))]
	}
	return out
}

// ParseArea accepts 49.0001 or 490001.
func ParseArea(v string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ReplaceAll(v, ".", ""))
	if err != nil || len(b) == 0 || len(b) > 13 {
		return nil, fmt.Errorf("area %q: want hex like 49.0001", v)
	}
	return b, nil
}

// ---- SNPs ----

type LSPEntry struct {
	Lifetime uint16
	ID       LSPID
	Seq      uint32
	Checksum uint16
}

type SNP struct {
	Level      int
	Complete   bool
	Source     NodeID
	Start, End LSPID
	Entries    []LSPEntry
}

func entryBytes(e LSPEntry) []byte {
	b := binary.BigEndian.AppendUint16(nil, e.Lifetime)
	b = append(b, e.ID[:]...)
	b = binary.BigEndian.AppendUint32(b, e.Seq)
	return binary.BigEndian.AppendUint16(b, e.Checksum)
}

// EncodeCSNP builds a complete SNP covering [start, end].
func EncodeCSNP(level int, src NodeID, start, end LSPID, entries []LSPEntry) []byte {
	typ := PDUCSNPL1
	if level == 2 {
		typ = PDUCSNPL2
	}
	b := header(typ, 33)
	b = append(b, 0, 0)
	b = append(b, src[:]...)
	b = append(b, start[:]...)
	b = append(b, end[:]...)
	var w tlvBuf
	var items [][]byte
	for _, e := range entries {
		items = append(items, entryBytes(e))
	}
	w.addItems(TLVLSPEntries, items)
	b = append(b, w.b...)
	setLen(b, 8)
	return b
}

// EncodePSNP builds a partial SNP (acknowledgements and requests).
func EncodePSNP(level int, src NodeID, entries []LSPEntry) []byte {
	typ := PDUPSNPL1
	if level == 2 {
		typ = PDUPSNPL2
	}
	b := header(typ, 17)
	b = append(b, 0, 0)
	b = append(b, src[:]...)
	var w tlvBuf
	var items [][]byte
	for _, e := range entries {
		items = append(items, entryBytes(e))
	}
	w.addItems(TLVLSPEntries, items)
	b = append(b, w.b...)
	setLen(b, 8)
	return b
}
