package isis

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
)

// Segment routing for IS-IS (RFC 8667, MPLS data plane): SR capabilities
// in the Router Capability TLV, Prefix-SIDs on prefixes and Adj-SIDs /
// LAN-Adj-SIDs on adjacencies.

// Sub-TLV and TLV codes.
const (
	subPrefixSID  uint8 = 3  // in TLV 135/235/236/237
	subAdjSID     uint8 = 31 // in TLV 22/222
	subLANAdjSID  uint8 = 32
	capSRCap      uint8 = 2 // in TLV 242
	capSRAlg      uint8 = 19
	capSRLB       uint8 = 22
	capNodeMSD    uint8 = 23
	capSRv6       uint8 = 25
	capFAD        uint8 = 26
	capSIDLabel   uint8 = 1 // SID/Label sub-TLV in SR-Capabilities
	srCapMPLSIPv4 byte  = 0x80
	srCapMPLSIPv6 byte  = 0x40
)

// Prefix-SID flags.
const (
	PSIDReadvertised byte = 0x80 // R
	PSIDNode         byte = 0x40 // N
	PSIDNoPHP        byte = 0x20 // P
	PSIDExplicitNull byte = 0x10 // E
	PSIDValue        byte = 0x08 // V
	PSIDLocal        byte = 0x04 // L
)

// Adj-SID flags.
const (
	ASIDAddrFamily byte = 0x80 // F: IPv6 adjacency
	ASIDBackup     byte = 0x40 // B
	ASIDValue      byte = 0x20 // V
	ASIDLocal      byte = 0x10 // L
)

// PrefixSID is an index into the advertiser's SRGB.
type PrefixSID struct {
	Index uint32
	Flags byte // PSID*
	Algo  byte
}

func (p PrefixSID) subTLV() []byte {
	b := []byte{subPrefixSID, 6, p.Flags &^ (PSIDValue | PSIDLocal), p.Algo}
	return binary.BigEndian.AppendUint32(b, p.Index)
}

// AdjSID is a local label for an adjacency; on a LAN it names the neighbor.
type AdjSID struct {
	Label    uint32
	Flags    byte // ASID*; V and L are always set (labels)
	Weight   byte
	Neighbor *SysID // LAN-Adj-SID
}

func (a AdjSID) subTLV() []byte {
	fl := a.Flags | ASIDValue | ASIDLocal
	if a.Neighbor != nil {
		b := []byte{subLANAdjSID, 11, fl, a.Weight}
		b = append(b, a.Neighbor[:]...)
		return append(b, byte(a.Label>>16), byte(a.Label>>8), byte(a.Label))
	}
	return []byte{subAdjSID, 5, fl, a.Weight, byte(a.Label >> 16), byte(a.Label >> 8), byte(a.Label)}
}

// SRCap is what we put in the Router Capability TLV.
type SRCap struct {
	RouterID   netip.Addr
	SRGBBase   uint32
	SRGBSize   uint32
	IPv6       bool   // MPLS IPv6 capable
	SRLBBase   uint32 // local block (0: none)
	SRLBSize   uint32
	Algorithms []byte // flex-algos we take part in (algorithm 0 is implied)
	FADs       []FAD  // flex-algo definitions we advertise
	SRv6       bool   // SRv6 capabilities sub-TLV
	MSD        []MSD  // node maximum SID depths
}

// labelRange is a range (3 bytes) and a SID/Label sub-TLV with a label.
func labelRange(base, size uint32) []byte {
	return []byte{byte(size >> 16), byte(size >> 8), byte(size), capSIDLabel, 3, byte(base >> 16), byte(base >> 8), byte(base)}
}

func (c SRCap) tlv() []byte {
	rid := c.RouterID.As4()
	b := append([]byte{}, rid[:]...)
	b = append(b, 0) // flags: area scope, not leaked
	sub := func(t byte, v []byte) { b = append(append(b, t, byte(len(v))), v...) }
	if c.SRGBSize > 0 {
		fl := srCapMPLSIPv4
		if c.IPv6 {
			fl |= srCapMPLSIPv6
		}
		sub(capSRCap, append([]byte{fl}, labelRange(c.SRGBBase, c.SRGBSize)...))
	}
	sub(capSRAlg, append([]byte{0}, c.Algorithms...)) // 0: SPF
	if c.SRLBSize > 0 {
		sub(capSRLB, append([]byte{0}, labelRange(c.SRLBBase, c.SRLBSize)...))
	}
	if len(c.MSD) > 0 {
		var m []byte
		for _, x := range c.MSD {
			m = append(m, x.Type, x.Value)
		}
		sub(capNodeMSD, m)
	}
	if c.SRv6 {
		sub(capSRv6, []byte{0, 0}) // flags (O: no OAM)
	}
	for _, f := range c.FADs {
		sub(capFAD, f.encode())
	}
	return b
}

// isItem encodes one extended IS reachability entry with its Adj-SIDs.
func isItem(n ISReach) []byte {
	it := append([]byte{}, n.Neighbor[:]...)
	it = append(it, byte(n.Metric>>16), byte(n.Metric>>8), byte(n.Metric))
	// Sub-TLVs as fit in one TLV with the entry (a busy LAN's per-neighbor
	// SIDs could overflow it): attributes first, then SIDs.
	sub := n.Attrs.subTLV()
	add := func(b []byte) {
		if len(sub)+len(b) <= 255-11 {
			sub = append(sub, b...)
		}
	}
	for _, a := range n.AdjSIDs {
		add(a.subTLV())
	}
	for _, x := range n.EndX {
		add(x.subTLV())
	}
	it = append(it, byte(len(sub)))
	return append(it, sub...)
}

// ---- decoding for status ----

// SIDInfo is a decoded Prefix-SID.
type SIDInfo struct {
	Index uint32 `json:"index,omitempty"`
	Label uint32 `json:"label,omitempty"` // when the SID is a label (V/L)
	Flags string `json:"flags,omitempty"` // e.g. "N", "NP"
	Algo  byte   `json:"algorithm,omitempty"`
}

// AdjSIDInfo is a decoded Adj-SID or LAN-Adj-SID.
type AdjSIDInfo struct {
	Label    uint32 `json:"label"`
	Neighbor string `json:"neighbor,omitempty"` // LAN-Adj-SID
	IPv6     bool   `json:"ipv6,omitempty"`
	Backup   bool   `json:"backup,omitempty"`
}

// SRCapInfo is a decoded Router Capability TLV's SR part.
type SRCapInfo struct {
	RouterID   string         `json:"router_id"`
	SRGB       []SRGBInfo     `json:"srgb,omitempty"`
	SRLB       []SRGBInfo     `json:"srlb,omitempty"`
	MPLSIPv4   bool           `json:"mpls_ipv4"`
	MPLSIPv6   bool           `json:"mpls_ipv6"`
	Algorithms []int          `json:"algorithms,omitempty"`
	FADs       []FADInfo      `json:"flex_algo_definitions,omitempty"`
	SRv6       bool           `json:"srv6,omitempty"`
	MSD        map[string]int `json:"msd,omitempty"`
}

type SRGBInfo struct {
	Base uint32 `json:"base"`
	Size uint32 `json:"size"`
}

func flagString(fl byte, names string, bits []byte) string {
	var b strings.Builder
	for i, m := range bits {
		if fl&m != 0 {
			b.WriteByte(names[i])
		}
	}
	return b.String()
}

func parsePrefixSIDs(sub []byte) []SIDInfo {
	var out []SIDInfo
	for len(sub) >= 2 && int(sub[1]) <= len(sub)-2 {
		t, v := sub[0], sub[2:2+int(sub[1])]
		sub = sub[2+int(sub[1]):]
		if t != subPrefixSID || len(v) < 5 {
			continue
		}
		s := SIDInfo{Flags: flagString(v[0], "RNPEVL", []byte{PSIDReadvertised, PSIDNode, PSIDNoPHP, PSIDExplicitNull, PSIDValue, PSIDLocal}), Algo: v[1]}
		switch len(v) - 2 {
		case 3:
			s.Label = uint32(v[2])<<16 | uint32(v[3])<<8 | uint32(v[4])
		case 4:
			s.Index = binary.BigEndian.Uint32(v[2:])
		}
		out = append(out, s)
	}
	return out
}

func parseAdjSIDs(sub []byte) []AdjSIDInfo {
	var out []AdjSIDInfo
	for len(sub) >= 2 && int(sub[1]) <= len(sub)-2 {
		t, v := sub[0], sub[2:2+int(sub[1])]
		sub = sub[2+int(sub[1]):]
		var a AdjSIDInfo
		var sid []byte
		switch {
		case t == subAdjSID && len(v) >= 5:
			sid = v[2:]
		case t == subLANAdjSID && len(v) >= 11:
			a.Neighbor = SysID(v[2:8]).String()
			sid = v[8:]
		default:
			continue
		}
		a.IPv6, a.Backup = v[0]&ASIDAddrFamily != 0, v[0]&ASIDBackup != 0
		if len(sid) == 3 {
			a.Label = uint32(sid[0])<<16 | uint32(sid[1])<<8 | uint32(sid[2])
		} else if len(sid) >= 4 {
			a.Label = binary.BigEndian.Uint32(sid)
		}
		out = append(out, a)
	}
	return out
}

func parseRouterCap(v []byte) *SRCapInfo {
	if len(v) < 5 {
		return nil
	}
	c := &SRCapInfo{RouterID: netip.AddrFrom4([4]byte(v[:4])).String()}
	sub := v[5:]
	found := false
	for len(sub) >= 2 && int(sub[1]) <= len(sub)-2 {
		t, x := sub[0], sub[2:2+int(sub[1])]
		sub = sub[2+int(sub[1]):]
		switch t {
		case capSRCap, capSRLB:
			if len(x) < 1 {
				continue
			}
			found = true
			if t == capSRCap {
				c.MPLSIPv4, c.MPLSIPv6 = x[0]&srCapMPLSIPv4 != 0, x[0]&srCapMPLSIPv6 != 0
			}
			r := x[1:]
			for len(r) >= 5 {
				size := uint32(r[0])<<16 | uint32(r[1])<<8 | uint32(r[2])
				st, l := r[3], int(r[4])
				if len(r) < 5+l {
					break
				}
				g := SRGBInfo{Size: size}
				if st == capSIDLabel && l == 3 {
					g.Base = uint32(r[5])<<16 | uint32(r[6])<<8 | uint32(r[7])
				} else if st == capSIDLabel && l == 4 {
					g.Base = binary.BigEndian.Uint32(r[5:])
				}
				if t == capSRCap {
					c.SRGB = append(c.SRGB, g)
				} else {
					c.SRLB = append(c.SRLB, g)
				}
				r = r[5+l:]
			}
		case capSRAlg:
			found = true
			for _, a := range x {
				c.Algorithms = append(c.Algorithms, int(a))
			}
		case capFAD:
			found = true
			if f, ok := parseFAD(x); ok {
				c.FADs = append(c.FADs, f)
			}
		case capSRv6:
			found, c.SRv6 = true, true
		case capNodeMSD:
			if c.MSD == nil {
				c.MSD = map[string]int{}
			}
			for ; len(x) >= 2; x = x[2:] {
				c.MSD[msdName(x[0])] = int(x[1])
			}
		}
	}
	if !found {
		return nil
	}
	return c
}

func (c *SRCapInfo) String() string {
	return fmt.Sprintf("SRGB %v algorithms %v", c.SRGB, c.Algorithms)
}

// ---- origination ----

// srState is what we last advertised, for status.
type srState struct {
	NodeSIDs []SRNodeSID     `json:"node_sids"`
	AdjSIDs  []SRAdjSID      `json:"adj_sids"`
	SRv6SIDs []SRv6SIDStatus `json:"srv6_sids,omitempty"`
}

type SRNodeSID struct {
	Prefix    string `json:"prefix"`
	Index     uint32 `json:"index"`
	Label     uint32 `json:"label"` // SRGB base + index (in our SRGB)
	Algorithm byte   `json:"algorithm,omitempty"`
	Flags     string `json:"flags,omitempty"`
}

type SRAdjSID struct {
	Neighbor string `json:"neighbor"`
	Label    uint32 `json:"label"`
	IPv6     bool   `json:"ipv6,omitempty"`
	MT       uint16 `json:"mt,omitempty"`
	LAN      bool   `json:"lan,omitempty"`
}

// srCap is the capability we (or a simulated router) advertise.
// own is false for simulated routers (no SRLB, flex-algo definitions or
// mapping server of their own).
func (s *session) srCap(rid netip.Addr, v6, own bool) *SRCap {
	sr := s.cfg.SegmentRouting
	if !sr.Enabled && !s.cfg.SRv6.Enabled {
		return nil
	}
	if !rid.IsValid() || !rid.Is4() {
		rid = netip.IPv4Unspecified()
	}
	c := &SRCap{RouterID: rid, IPv6: v6, Algorithms: s.algorithms(), SRv6: s.cfg.SRv6.Enabled}
	if sr.Enabled {
		c.SRGBBase, c.SRGBSize = sr.SRGBBase, sr.SRGBSize
		c.MSD = append(c.MSD, MSD{msdBaseMPLS, 10})
	}
	if s.cfg.SRv6.Enabled {
		c.MSD = append(c.MSD, MSD{msdSRHMaxSL, 8}, MSD{msdSRHEndPop, 8}, MSD{msdSRHHEncaps, 8}, MSD{msdSRHEndD, 8})
	}
	if own {
		c.SRLBBase, c.SRLBSize = sr.SRLBBase, sr.SRLBSize
		if !sr.Enabled {
			c.SRLBSize = 0
		}
		c.FADs = s.fads()
	}
	return c
}

// adjLabel allocates a stable local label for an adjacency of owner (""
// for us; simulated routers have label spaces of their own).
func (s *session) adjLabel(owner, key string) uint32 {
	if s.adjLabels == nil {
		s.adjLabels = map[string]map[string]uint32{}
	}
	m := s.adjLabels[owner]
	if m == nil {
		m = map[string]uint32{}
		s.adjLabels[owner] = m
	}
	if l, ok := m[key]; ok {
		return l
	}
	l := s.cfg.SegmentRouting.AdjSIDBase + uint32(len(m))
	m[key] = l
	return l
}

func psidFlags(p netip.Prefix, n NodeSID) byte {
	var f byte
	if p.Bits() == p.Addr().BitLen() {
		f |= PSIDNode
	}
	if n.NoPHP {
		f |= PSIDNoPHP
	}
	if n.ExplicitNull {
		f |= PSIDExplicitNull
	}
	return f
}

// srPrefixes makes sure every node-SID prefix is advertised (before
// mtify) and returns the SIDs by prefix.
func srPrefixes(nodeSIDs []NodeSID, pfx []IPReach) ([]IPReach, map[netip.Prefix][]PrefixSID) {
	sids := map[netip.Prefix][]PrefixSID{}
	for _, n := range nodeSIDs {
		p := netip.MustParsePrefix(n.Prefix).Masked()
		sids[p] = append(sids[p], PrefixSID{Index: n.Index, Flags: psidFlags(p, n), Algo: n.Algorithm})
		found := false
		for _, x := range pfx {
			if x.Prefix == p {
				found = true
			}
		}
		if !found {
			pfx = append(pfx, IPReach{Prefix: p, Metric: 0})
		}
	}
	return pfx, sids
}

// srDecorate adds Prefix-SIDs and Adj-SIDs to an LSP body after mtify.
// lanNbrs are the Up neighbors on each LAN (by pseudonode) body points at.
func (s *session) srDecorate(owner string, body *lspBody, pfx []IPReach, sids map[netip.Prefix][]PrefixSID, lanNbrs map[NodeID][]SysID, st *srState) []IPReach {
	sr := s.cfg.SegmentRouting
	if !sr.Enabled {
		return pfx
	}
	for i := range pfx {
		ss, ok := sids[pfx[i].Prefix]
		if !ok {
			continue
		}
		pfx[i].SIDs = append([]PrefixSID(nil), ss...)
		if st != nil && pfx[i].MT != MTIPv4Multicast && pfx[i].MT != MTIPv6Multicast {
			for _, sid := range ss {
				st.NodeSIDs = append(st.NodeSIDs, SRNodeSID{Prefix: pfx[i].Prefix.String(), Index: sid.Index, Label: sr.SRGBBase + sid.Index, Algorithm: sid.Algo,
					Flags: flagString(sid.Flags, "RNPE", []byte{PSIDReadvertised, PSIDNode, PSIDNoPHP, PSIDExplicitNull})})
			}
		}
	}
	if sr.AdjSIDBase == 0 {
		return pfx
	}
	hasV6 := false
	for _, p := range pfx {
		if p.Prefix.Addr().Is6() {
			hasV6 = true
		}
	}
	singleV6 := len(s.mts) == 0 && hasV6 // one topology carrying both families
	decorate := func(list []ISReach, mt uint16) []ISReach {
		out := make([]ISReach, len(list))
		dup := map[NodeID]int{} // parallel adjacencies to one neighbor get their own labels
		for i, n := range list {
			n.AdjSIDs = nil
			key := n.Neighbor.String()
			if k := dup[n.Neighbor]; k > 0 {
				key += fmt.Sprintf("#%d", k)
			}
			dup[n.Neighbor]++
			fams := []bool{false}
			switch {
			case mt == MTIPv6Unicast || mt == MTIPv6Multicast:
				fams = []bool{true}
			case singleV6:
				fams = []bool{false, true}
			}
			for _, v6 := range fams {
				var fl byte
				if v6 {
					fl = ASIDAddrFamily
				}
				if nbrs, ok := lanNbrs[n.Neighbor]; ok && n.Neighbor[6] != 0 {
					// Pseudonode: one LAN-Adj-SID per neighbor on the LAN.
					for _, nb := range nbrs {
						nb := nb
						l := s.adjLabel(owner, fmt.Sprintf("lan/%s/%s/%d/%v", key, nb, mt, v6))
						n.AdjSIDs = append(n.AdjSIDs, AdjSID{Label: l, Flags: fl, Neighbor: &nb})
						if st != nil {
							st.AdjSIDs = append(st.AdjSIDs, SRAdjSID{Neighbor: nb.String(), Label: l, IPv6: v6, MT: mt, LAN: true})
						}
					}
					continue
				}
				l := s.adjLabel(owner, fmt.Sprintf("%s/%d/%v", key, mt, v6))
				n.AdjSIDs = append(n.AdjSIDs, AdjSID{Label: l, Flags: fl})
				if st != nil {
					st.AdjSIDs = append(st.AdjSIDs, SRAdjSID{Neighbor: n.Neighbor.String(), Label: l, IPv6: v6, MT: mt})
				}
			}
			out[i] = n
		}
		return out
	}
	body.Neighbors = decorate(body.Neighbors, 0)
	for mt, list := range body.MTNeighbors {
		body.MTNeighbors[mt] = decorate(list, mt)
	}
	return pfx
}

// SRStatus is our SR configuration and what we advertise.
type SRStatus struct {
	Enabled   bool            `json:"mpls"` // SR-MPLS (else SRv6 only)
	SRGBBase  uint32          `json:"srgb_base"`
	SRGBSize  uint32          `json:"srgb_size"`
	SRLB      *SRGBInfo       `json:"srlb,omitempty"`
	NodeSIDs  []SRNodeSID     `json:"node_sids"`
	AdjSIDs   []SRAdjSID      `json:"adj_sids"`
	Mappings  []BindingInfo   `json:"mappings,omitempty"`
	FlexAlgos []FADInfo       `json:"flex_algos,omitempty"`
	SRv6SIDs  []SRv6SIDStatus `json:"srv6_sids,omitempty"`
}
