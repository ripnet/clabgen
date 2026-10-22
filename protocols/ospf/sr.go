package ospf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/ripnet/clabgen/internal/topogen"
)

// OSPFv2 opaque LSAs (RFC 5250, area scope): segment routing (RFC 8665)
// in Router Information (RFC 7770), Extended Prefix and Extended Link
// LSAs (RFC 7684); flex-algo definitions (RFC 9350) and application-
// specific link attributes (RFC 8920); TE LSAs (RFC 3630, RFC 7471 delay).

const (
	LSAOpaqueLink = 9
	LSAOpaqueArea = 10
	LSAOpaqueAS   = 11

	opqTE      = 1
	opqRI      = 4
	opqExtPfx  = 7
	opqExtLink = 8

	// Router Information TLVs.
	riSRAlgo  = 8
	riSRRange = 9
	riNodeMSD = 12
	riSRLB    = 14
	riFAD     = 16
	// Extended Prefix / Link TLVs and sub-TLVs.
	tlvExtPrefix  = 1
	tlvExtLink    = 1
	subSIDLabel   = 1
	subPrefixSID  = 2
	subFAPM       = 3 // flex-algo prefix metric (RFC 9350), in the Extended Prefix TLV
	subAdjSID     = 2
	subLANAdjSID  = 3
	subASLA       = 10
	subMinMaxDly  = 13
	subAdminGroup = 19
	subTEMetric   = 22
	// FAD sub-TLVs.
	fadExclAny = 1
	fadInclAny = 2
	fadInclAll = 3
	// TE LSA TLVs and link sub-TLVs.
	teRouterAddr  = 1
	teLink        = 2
	teLinkType    = 1
	teLinkID      = 2
	teLocalAddr   = 3
	teRemoteAddr  = 4
	teMetric      = 5
	teMaxBW       = 6
	teMaxResBW    = 7
	teUnresBW     = 8
	teAdminGroup  = 9
	teLinkDelay   = 27
	teMinMaxDelay = 28

	// Flags.
	extPfxAttach = 0x80 // A
	extPfxNode   = 0x40 // N
	psidNP       = 0x40
	psidE        = 0x10
	adjV         = 0x40
	adjL         = 0x20

	routeIntra = 1
	routeInter = 3

	sabmFlexAlgo = 0x10 // X (RFC 9350), the first SABM byte
	msdBaseMPLS  = 1
)

var fadMetricTypes = map[string]byte{"igp": 0, "delay": 1, "te": 2}

// tlv encodes an OSPF TLV (16-bit type and length), padded to 4 bytes.
func tlv(t uint16, v []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, t)
	b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
	b = append(b, v...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func opaqueID(otype byte, id uint32) netip.Addr { return id4(uint32(otype)<<24 | id&0xffffff) }

func label3(l uint32) []byte { return []byte{byte(l >> 16), byte(l >> 8), byte(l)} }

func groupMask(gs []int) uint32 {
	var m uint32
	for _, g := range gs {
		m |= 1 << uint(g)
	}
	return m
}

func validGroups(scope string, gs []int) error {
	for _, g := range gs {
		if g < 0 || g > 31 {
			return fmt.Errorf("%s: admin groups are 0-31", scope)
		}
	}
	return nil
}

// ---- config ----

func (c *Config) validateSR(v6 bool) error {
	algos := map[byte]bool{}
	for _, f := range c.FlexAlgos {
		if f.Algorithm < 128 {
			return fmt.Errorf("flex_algos: algorithm %d: flex-algos are 128-255", f.Algorithm)
		}
		if algos[f.Algorithm] {
			return fmt.Errorf("flex_algos: algorithm %d twice", f.Algorithm)
		}
		algos[f.Algorithm] = true
		if _, ok := fadMetricTypes[f.MetricType]; f.MetricType != "" && !ok {
			return fmt.Errorf("flex_algos: algorithm %d: metric_type must be igp, delay or te", f.Algorithm)
		}
		for _, gs := range [][]int{f.ExcludeAny, f.IncludeAny, f.IncludeAll} {
			if err := validGroups(fmt.Sprintf("flex_algos %d", f.Algorithm), gs); err != nil {
				return err
			}
		}
	}
	if err := validGroups("admin_groups", c.AdminGroups); err != nil {
		return err
	}
	for _, f := range c.Interfaces {
		if err := validGroups("interface "+f.Device, f.AdminGroups); err != nil {
			return err
		}
	}
	for _, n := range c.Topology.Custom {
		for _, l := range n.Links {
			if err := validGroups("topology link "+n.Name+"-"+l.To, l.AdminGroups); err != nil {
				return err
			}
		}
	}
	sr := c.SegmentRouting
	if !sr.Enabled {
		return nil
	}
	if sr.SRGBBase < 16 || sr.SRGBSize == 0 || sr.SRGBBase+sr.SRGBSize > 1<<20 {
		return errors.New("segment_routing: SRGB must lie within labels 16-1048575")
	}
	if sr.SRLBBase != 0 || sr.SRLBSize != 0 {
		if sr.SRLBBase < 16 || sr.SRLBSize == 0 || sr.SRLBBase+sr.SRLBSize > 1<<20 {
			return errors.New("segment_routing: SRLB must lie within labels 16-1048575")
		}
		if sr.SRLBBase < sr.SRGBBase+sr.SRGBSize && sr.SRLBBase+sr.SRLBSize > sr.SRGBBase {
			return errors.New("segment_routing: the SRLB overlaps the SRGB")
		}
	}
	if sr.AdjSIDBase != 0 && (sr.AdjSIDBase < 16 || sr.AdjSIDBase+1000 > 1<<20) {
		return errors.New("segment_routing: adj_sid_base must leave room for labels below 1048576")
	}
	if sr.AdjSIDBase != 0 && sr.AdjSIDBase < sr.SRGBBase+sr.SRGBSize && sr.AdjSIDBase+1000 > sr.SRGBBase {
		return errors.New("segment_routing: adjacency labels overlap the SRGB")
	}
	seen := map[uint32]bool{}
	use := func(i uint32, what string) error {
		if i >= sr.SRGBSize {
			return fmt.Errorf("segment_routing: %s: index %d outside the SRGB (size %d)", what, i, sr.SRGBSize)
		}
		if seen[i] {
			return fmt.Errorf("segment_routing: %s: index %d used twice", what, i)
		}
		seen[i] = true
		return nil
	}
	for _, n := range sr.NodeSIDs {
		p, err := netip.ParsePrefix(n.Prefix)
		if err != nil || p.Addr().Is4() == v6 {
			return fmt.Errorf("segment_routing: node SID prefix %q: want an %s prefix", n.Prefix, family(v6))
		}
		if err := use(n.Index, "node SID "+n.Prefix); err != nil {
			return err
		}
		if n.Algorithm != 0 && !algos[n.Algorithm] {
			return fmt.Errorf("segment_routing: node SID %s: algorithm %d is not in flex_algos", n.Prefix, n.Algorithm)
		}
	}
	t := c.Topology
	if t.Kind == "" || t.Kind == "none" {
		return nil
	}
	topo, err := buildTopology(t, zero4, v6)
	if err != nil {
		return nil // reported by the topology check
	}
	bases := []*uint32{t.NodeSIDBase}
	for _, f := range t.FlexAlgoSIDs {
		if !algos[f.Algorithm] {
			return fmt.Errorf("topology: flex_algo_sids: algorithm %d is not in flex_algos", f.Algorithm)
		}
		bases = append(bases, f.NodeSIDBase)
	}
	for _, b := range bases {
		if b == nil {
			continue
		}
		for i := range topo.nodes {
			if err := use(*b+uint32(i), "simulated node SID"); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- origination ----

// prefixSID is a SID we advertise (for us or a simulated router).
type prefixSID struct {
	prefix netip.Prefix
	index  uint32
	algo   byte
	flags  byte // psid*
	adv    netip.Addr
	area   *area
	node   string // a simulated router's (for flex-algo costs)
}

// sidTableLocked lists the Prefix-SIDs we advertise inside areas, by
// prefix.
func (s *session) sidTableLocked() map[netip.Prefix][]prefixSID {
	out := map[netip.Prefix][]prefixSID{}
	sr := s.cfg.SegmentRouting
	if !sr.Enabled {
		return out
	}
	for _, n := range sr.NodeSIDs {
		p := netip.MustParsePrefix(n.Prefix).Masked()
		a := s.home
		for _, f := range s.ifs {
			if pf, ok := s.ifAddrLocked(f); ok && !s.cd.v3 && pf.Masked() == p {
				a = f.area
			}
			for _, pf := range s.ifPrefixes6(f) {
				if s.cd.v3 && pf.Masked() == p {
					a = f.area
				}
			}
		}
		x := prefixSID{prefix: p, index: n.Index, algo: n.Algorithm, adv: s.rid, area: a}
		if n.NoPHP {
			x.flags |= psidNP
		}
		if n.ExplicitNull {
			x.flags |= psidE
		}
		out[p] = append(out[p], x)
	}
	if t := s.topo; t != nil {
		bases := []AlgoSIDBase{{NodeSIDBase: s.cfg.Topology.NodeSIDBase}}
		bases = append(bases, s.cfg.Topology.FlexAlgoSIDs...)
		for _, b := range bases {
			if b.NodeSIDBase == nil {
				continue
			}
			for i, n := range t.nodes {
				if n.v.Down || !n.loopback.IsValid() {
					continue
				}
				p := n.loopback.Masked()
				out[p] = append(out[p], prefixSID{prefix: p, index: *b.NodeSIDBase + uint32(i), algo: b.Algorithm, adv: n.rid, area: s.topoArea, node: n.v.Name})
			}
		}
	}
	return out
}

// opqID is a stable opaque ID for an item of an advertising router's
// opaque LSAs of one type in one area.
func (s *session) opqID(a *area, otype byte, adv netip.Addr, item string) netip.Addr {
	k := fmt.Sprintf("%d/%d/%s/%s", a.key(), otype, adv, item)
	id, ok := s.opqIDs[k]
	if !ok {
		c := fmt.Sprintf("%d/%d/%s", a.key(), otype, adv)
		s.opqNext[c]++
		id = s.opqNext[c]
		s.opqIDs[k] = id
	}
	return opaqueID(otype, id)
}

func (s *session) srAlgos() []byte {
	out := []byte{0}
	for _, f := range s.cfg.FlexAlgos {
		out = append(out, f.Algorithm)
	}
	return out
}

// riBody is a Router Information LSA: SR algorithms, SRGB, SRLB, node
// MSD and our flex-algo definitions.
func (s *session) riBody(defs bool) []byte {
	sr := s.cfg.SegmentRouting
	var b []byte
	b = append(b, tlv(riSRAlgo, s.srAlgos())...)
	if sr.Enabled {
		r := append(label3(sr.SRGBSize), 0)
		b = append(b, tlv(riSRRange, append(r, tlv(subSIDLabel, label3(sr.SRGBBase))...))...)
		if sr.SRLBSize > 0 {
			r := append(label3(sr.SRLBSize), 0)
			b = append(b, tlv(riSRLB, append(r, tlv(subSIDLabel, label3(sr.SRLBBase))...))...)
		}
		b = append(b, tlv(riNodeMSD, []byte{msdBaseMPLS, 10})...)
	}
	if defs {
		for _, f := range s.cfg.FlexAlgos {
			if f.ParticipateOnly {
				continue
			}
			prio := f.Priority
			if prio == 0 {
				prio = 128
			}
			v := []byte{f.Algorithm, fadMetricTypes[f.MetricType], 0, prio}
			for _, g := range []struct {
				t  uint16
				gs []int
			}{{fadExclAny, f.ExcludeAny}, {fadInclAny, f.IncludeAny}, {fadInclAll, f.IncludeAll}} {
				if len(g.gs) > 0 {
					v = append(v, tlv(g.t, binary.BigEndian.AppendUint32(nil, groupMask(g.gs)))...)
				}
			}
			b = append(b, tlv(riFAD, v)...)
		}
	}
	return b
}

// extPrefixBody is an Extended Prefix TLV with Prefix-SIDs and, for
// inter-area flex-algo SIDs, their prefix metrics (fapm by algorithm).
func extPrefixBody(route byte, p netip.Prefix, flags byte, sids []prefixSID, fapm map[byte]uint32) []byte {
	v := []byte{route, byte(p.Bits()), 0, flags}
	v = append(v, p.Masked().Addr().AsSlice()[:(p.Bits()+31)/32*4]...)
	for _, x := range sids {
		sub := []byte{x.flags, 0, 0, x.algo}
		v = append(v, tlv(subPrefixSID, binary.BigEndian.AppendUint32(sub, x.index))...)
	}
	var algos []byte
	for a := range fapm {
		algos = append(algos, a)
	}
	sort.Slice(algos, func(i, j int) bool { return algos[i] < algos[j] })
	for _, a := range algos {
		v = append(v, tlv(subFAPM, binary.BigEndian.AppendUint32([]byte{a, 0, 0, 0}, fapm[a]))...)
	}
	return tlv(tlvExtPrefix, v)
}

// algoCosts are the costs from us to the simulated routers in a
// flex-algo: shortest paths over the emulated graph with the algorithm's
// metric (delay or TE metric, links without one left out; igp).
func (t *topology) algoCosts(s *session, algo byte) map[string]uint32 {
	mt := "igp"
	for _, f := range s.cfg.FlexAlgos {
		if f.Algorithm == algo {
			mt = f.MetricType
		}
	}
	weight := func(a, b string) (uint32, bool) {
		switch mt {
		case "delay":
			d := t.attrs(s, a, b).delay
			return d, d > 0
		case "te":
			te := t.attrs(s, a, b).te
			return te, te > 0
		}
		return uint32(t.linkMetric(s, a, b)), true
	}
	dist := map[string]uint32{topogen.Us: 0}
	done := map[string]bool{}
	for {
		cur, best := "", uint32(math.MaxUint32)
		for n, d := range dist {
			if !done[n] && d < best {
				cur, best = n, d
			}
		}
		if cur == "" {
			break
		}
		done[cur] = true
		for _, x := range t.g.Neighbors(cur) {
			if x == topogen.Us || !t.g.Up(cur, x) || t.byName[x].v.Down {
				continue
			}
			if w, ok := weight(cur, x); ok {
				if d, seen := dist[x]; !seen || best+w < d {
					dist[x] = best + w
				}
			}
		}
	}
	return dist
}

// linkAttrs are one link's attributes, resolved.
type linkAttrs struct {
	te, delay, groups uint32
	bw                uint32 // Mbit/s
}

func (a linkAttrs) asla() []byte {
	if a.te == 0 && a.delay == 0 && a.groups == 0 {
		return nil
	}
	v := []byte{4, 0, 0, 0, sabmFlexAlgo, 0, 0, 0}
	if a.groups != 0 {
		v = append(v, tlv(subAdminGroup, binary.BigEndian.AppendUint32(nil, a.groups))...)
	}
	if a.te != 0 {
		v = append(v, tlv(subTEMetric, binary.BigEndian.AppendUint32(nil, a.te))...)
	}
	if a.delay != 0 {
		d := min(a.delay, 0xffffff)
		v = append(v, tlv(subMinMaxDly, append(append([]byte{0}, label3(d)...), append([]byte{0}, label3(d)...)...))...)
	}
	return tlv(subASLA, v)
}

func attrsOf(l LinkAttrs) linkAttrs {
	return linkAttrs{te: l.TEMetric, delay: l.DelayUS, groups: groupMask(l.AdminGroups), bw: l.BandwidthMbps}
}

func (s *session) ifAttrs(f *iface) linkAttrs {
	if f.idx == 0 {
		return attrsOf(s.cfg.LinkAttrs)
	}
	return attrsOf(s.cfg.Interfaces[f.idx-1].LinkAttrs)
}

// adjLabel is a stable local label for the adjacency to rid on f.
func (s *session) adjLabel(f *iface, rid netip.Addr) uint32 {
	k := fmt.Sprintf("%d/%s", f.idx, rid)
	l, ok := s.adjLabels[k]
	if !ok {
		l = s.cfg.SegmentRouting.AdjSIDBase + uint32(len(s.adjLabels))
		s.adjLabels[k] = l
	}
	return l
}

// floatBW is a bandwidth in Mbit/s as the IEEE float of bytes per second.
func floatBW(mbps uint32) []byte {
	return binary.BigEndian.AppendUint32(nil, math.Float32bits(float32(mbps)*1e6/8))
}

func teLinkBody(typ byte, id, local, remote netip.Addr, metric uint32, a linkAttrs) []byte {
	bw := a.bw
	if bw == 0 {
		bw = 1000
	}
	v := tlv(teLinkType, []byte{typ})
	v = append(v, tlv(teLinkID, id.AsSlice())...)
	v = append(v, tlv(teLocalAddr, local.AsSlice())...)
	if remote.IsValid() {
		v = append(v, tlv(teRemoteAddr, remote.AsSlice())...)
	}
	if a.te != 0 {
		metric = a.te
	}
	v = append(v, tlv(teMetric, binary.BigEndian.AppendUint32(nil, metric))...)
	v = append(v, tlv(teMaxBW, floatBW(bw))...)
	v = append(v, tlv(teMaxResBW, floatBW(bw))...)
	var un []byte
	for i := 0; i < 8; i++ {
		un = append(un, floatBW(bw)...)
	}
	v = append(v, tlv(teUnresBW, un)...)
	v = append(v, tlv(teAdminGroup, binary.BigEndian.AppendUint32(nil, a.groups))...)
	if a.delay != 0 {
		v = append(v, tlv(teLinkDelay, append([]byte{0}, label3(min(a.delay, 0xffffff))...))...)
	}
	return tlv(teLink, v)
}

func (s *session) flexAlgo() bool { return len(s.cfg.FlexAlgos) > 0 }

// opaqueLocked adds our (and the simulated routers') opaque LSAs in area
// a: Router Information, Extended Prefix for the Prefix-SIDs, Extended
// Link with adjacency SIDs and link attributes, TE LSAs.
func (s *session) opaqueLocked(out map[LSAKey]want, a *area, sids map[netip.Prefix][]prefixSID) {
	if s.cd.v3 {
		return
	}
	sr := s.cfg.SegmentRouting
	te := s.cfg.TrafficEngineering
	now := time.Now()
	if !sr.Enabled && !s.flexAlgo() && !te {
		return
	}
	put := func(adv netip.Addr, id netip.Addr, body []byte) {
		s.put(out, a, LSAHeader{Type: LSAOpaqueArea, ID: id, AdvRtr: adv}, body)
	}
	if sr.Enabled || s.flexAlgo() {
		put(s.rid, opaqueID(opqRI, 0), s.riBody(true))
	}
	// Prefix-SIDs of prefixes in this area, ours and the simulated
	// routers', one prefix per LSA.
	var ps []netip.Prefix
	for p := range sids {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return prefixLess(ps[i], ps[j]) })
	for _, p := range ps {
		byAdv := map[netip.Addr][]prefixSID{}
		for _, x := range sids[p] {
			if x.area == a {
				byAdv[x.adv] = append(byAdv[x.adv], x)
			}
		}
		for adv, xs := range byAdv {
			flags := byte(0)
			if p.Bits() == 32 {
				flags |= extPfxNode
			}
			put(adv, s.opqID(a, opqExtPfx, adv, p.String()), extPrefixBody(routeIntra, p, flags, xs, nil))
		}
	}
	// Our links.
	teAddr := s.rid
	if len(s.loopbacks) > 0 {
		teAddr = s.loopbacks[0].Addr()
	}
	if te {
		put(s.rid, opaqueID(opqTE, 0), tlv(teRouterAddr, teAddr.AsSlice()))
	}
	for _, f := range s.ifs {
		if f.area != a || f.state == ifDown || f.passive || f.vl != nil {
			continue
		}
		mine, ok := s.ifAddrLocked(f)
		if !ok {
			continue
		}
		attrs := s.ifAttrs(f)
		if f.noDR() {
			for _, n := range f.sortedNbrs() {
				if n.state != nFull {
					continue
				}
				v := []byte{LinkP2P, 0, 0, 0}
				v = append(append(v, n.rid.AsSlice()...), mine.Addr().AsSlice()...)
				if sr.Enabled && sr.AdjSIDBase != 0 {
					v = append(v, tlv(subAdjSID, append([]byte{adjV | adjL, 0, 0, 0}, label3(s.adjLabel(f, n.rid))...))...)
				}
				if s.flexAlgo() {
					v = append(v, attrs.asla()...)
				}
				item := fmt.Sprintf("p2p/%d/%s", f.idx, n.rid)
				put(s.rid, s.opqID(a, opqExtLink, s.rid, item), tlv(tlvExtLink, v))
				if te {
					put(s.rid, s.opqID(a, opqTE, s.rid, item), teLinkBody(1, n.rid, mine.Addr(), n.addr, uint32(f.cost), attrs))
				}
			}
			continue
		}
		var dr *nbr
		full := false
		for _, n := range f.nbrs {
			if n.adjacent(now) {
				full = true
				if n.id == f.dr {
					dr = n
				}
			}
		}
		if f.state == ifWaiting || !(f.state == ifDR && full || dr != nil) {
			continue
		}
		v := []byte{LinkTransit, 0, 0, 0}
		v = append(append(v, f.dr.AsSlice()...), mine.Addr().AsSlice()...)
		if sr.Enabled && sr.AdjSIDBase != 0 {
			for _, n := range f.sortedNbrs() {
				if n.adjacent(now) {
					sub := append([]byte{adjV | adjL, 0, 0, 0}, n.rid.AsSlice()...)
					v = append(v, tlv(subLANAdjSID, append(sub, label3(s.adjLabel(f, n.rid))...))...)
				}
			}
		}
		if s.flexAlgo() {
			v = append(v, attrs.asla()...)
		}
		item := fmt.Sprintf("lan/%d", f.idx)
		put(s.rid, s.opqID(a, opqExtLink, s.rid, item), tlv(tlvExtLink, v))
		if te {
			put(s.rid, s.opqID(a, opqTE, s.rid, item), teLinkBody(2, f.dr, mine.Addr(), netip.Addr{}, uint32(f.cost), attrs))
		}
	}
	if s.topo != nil && a == s.topoArea {
		s.topo.opaqueLocked(s, a, put)
	}
}

// opaqueLocked adds the simulated routers' Router Information and
// Extended Link LSAs (link attributes for flex-algo), and those for our
// links to them.
func (t *topology) opaqueLocked(s *session, a *area, put func(adv, id netip.Addr, body []byte)) {
	sr := s.cfg.SegmentRouting.Enabled
	if !sr && !s.flexAlgo() {
		return
	}
	link := func(from, to string) []byte {
		v := []byte{LinkP2P, 0, 0, 0}
		v = append(append(v, t.ridOf(s, to).AsSlice()...), id4(t.ifID(from, to)).AsSlice()...)
		return tlv(tlvExtLink, append(v, t.attrs(s, from, to).asla()...))
	}
	if s.flexAlgo() {
		for _, x := range t.g.Neighbors(topogen.Us) {
			if t.g.Up(topogen.Us, x) {
				put(s.rid, s.opqID(a, opqExtLink, s.rid, "sim/"+x), link(topogen.Us, x))
			}
		}
	}
	for _, n := range t.nodes {
		if n.v.Down {
			continue
		}
		put(n.rid, opaqueID(opqRI, 0), s.riBody(false))
		if !s.flexAlgo() {
			continue
		}
		for _, x := range t.g.Neighbors(n.v.Name) {
			if t.g.Up(n.v.Name, x) {
				put(n.rid, s.opqID(a, opqExtLink, n.rid, x), link(n.v.Name, x))
			}
		}
	}
}

// attrs are a simulated link's attributes: the custom link's, else the
// topology's delay.
func (t *topology) attrs(s *session, a, b string) linkAttrs {
	out := linkAttrs{delay: s.cfg.Topology.LinkDelayUS}
	for _, cn := range s.cfg.Topology.Custom {
		for _, l := range cn.Links {
			if cn.Name == a && l.To == b || cn.Name == b && l.To == a {
				if l.DelayUS != 0 {
					out.delay = l.DelayUS
				}
				out.te, out.groups = l.TEMetric, groupMask(l.AdminGroups)
			}
		}
	}
	return out
}

// interSIDsLocked adds Extended Prefix LSAs in area b for summarised
// prefixes that have Prefix-SIDs in other areas (RFC 8665 7.2): route type
// inter-area, NP set unless the prefix is ours.
func (s *session) interSIDsLocked(out map[LSAKey]want, b *area, summarised map[netip.Prefix]bool, sids map[netip.Prefix][]prefixSID) {
	var ps []netip.Prefix
	for p := range summarised {
		if len(sids[p]) > 0 {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool { return prefixLess(ps[i], ps[j]) })
	costs := map[byte]map[string]uint32{}
	for _, p := range ps {
		var xs []prefixSID
		fapm := map[byte]uint32{}
		for _, x := range sids[p] {
			if x.area == b {
				xs = nil
				break
			}
			if x.adv != s.rid {
				x.flags = x.flags&^psidE | psidNP
			}
			if x.algo != 0 {
				c := uint32(0)
				if x.node != "" && s.topo != nil {
					if costs[x.algo] == nil {
						costs[x.algo] = s.topo.algoCosts(s, x.algo)
					}
					d, ok := costs[x.algo][x.node]
					if !ok {
						continue // not reachable in this algorithm
					}
					c = d
				}
				fapm[x.algo] = c
			}
			xs = append(xs, x)
		}
		if len(xs) == 0 {
			continue
		}
		flags := byte(extPfxAttach)
		if p.Bits() == 32 && xs[0].adv == s.rid {
			flags |= extPfxNode
		}
		h := LSAHeader{Type: LSAOpaqueArea, ID: s.opqID(b, opqExtPfx, s.rid, "inter/"+p.String()), AdvRtr: s.rid}
		s.put(out, b, h, extPrefixBody(routeInter, p, flags, xs, fapm))
	}
}

// ---- status ----

type SRStatus struct {
	SRGB       string        `json:"srgb,omitempty"`
	SRLB       string        `json:"srlb,omitempty"`
	Algorithms []byte        `json:"algorithms"`
	NodeSIDs   []SIDStatus   `json:"node_sids,omitempty"`
	AdjSIDs    []AdjSIDState `json:"adj_sids,omitempty"`
	TE         bool          `json:"traffic_engineering,omitempty"`
}

type SIDStatus struct {
	Prefix    string `json:"prefix"`
	Index     uint32 `json:"index"`
	Label     uint32 `json:"label"`
	Algorithm byte   `json:"algorithm,omitempty"`
	Router    string `json:"router,omitempty"` // a simulated router's
}

type AdjSIDState struct {
	Interface string `json:"interface"`
	Neighbor  string `json:"neighbor"`
	Label     uint32 `json:"label"`
	LAN       bool   `json:"lan,omitempty"`
}

func (s *session) srStatusLocked() *SRStatus {
	sr := s.cfg.SegmentRouting
	if !sr.Enabled && !s.flexAlgo() && !s.cfg.TrafficEngineering {
		return nil
	}
	st := &SRStatus{Algorithms: s.srAlgos(), TE: s.cfg.TrafficEngineering}
	if !sr.Enabled {
		return st
	}
	st.SRGB = fmt.Sprintf("%d-%d", sr.SRGBBase, sr.SRGBBase+sr.SRGBSize-1)
	if sr.SRLBSize > 0 {
		st.SRLB = fmt.Sprintf("%d-%d", sr.SRLBBase, sr.SRLBBase+sr.SRLBSize-1)
	}
	tab := s.sidTableLocked()
	var ps []netip.Prefix
	for p := range tab {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return prefixLess(ps[i], ps[j]) })
	for _, p := range ps {
		for _, x := range tab[p] {
			y := SIDStatus{Prefix: p.String(), Index: x.index, Label: sr.SRGBBase + x.index, Algorithm: x.algo}
			if x.adv != s.rid {
				y.Router = x.adv.String()
			}
			st.NodeSIDs = append(st.NodeSIDs, y)
		}
	}
	if sr.AdjSIDBase != 0 {
		now := time.Now()
		for _, f := range s.ifs {
			for _, n := range f.sortedNbrs() {
				if n.adjacent(now) {
					st.AdjSIDs = append(st.AdjSIDs, AdjSIDState{Interface: f.device, Neighbor: n.rid.String(), Label: s.adjLabel(f, n.rid), LAN: !f.noDR()})
				}
			}
		}
	}
	return st
}

// ---- decoding (status, LSA detail) ----

type tlvItem struct {
	t uint16
	v []byte
}

func readTLVs(b []byte) []tlvItem {
	var out []tlvItem
	for len(b) >= 4 {
		t, l := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
		if 4+l > len(b) {
			break
		}
		out = append(out, tlvItem{t, b[4 : 4+l]})
		n := 4 + (l+3)/4*4
		if n > len(b) {
			break
		}
		b = b[n:]
	}
	return out
}

func u24(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }

// sidValue reads a SID/Label sub-TLV value: a label (3 bytes) or index.
func sidValue(v []byte) string {
	switch len(v) {
	case 3:
		return fmt.Sprintf("label %d", u24(v)&0xfffff)
	case 4:
		return fmt.Sprintf("index %d", binary.BigEndian.Uint32(v))
	}
	return "?"
}

func rangeText(v []byte) string {
	if len(v) < 4 {
		return "?"
	}
	size := u24(v)
	for _, s := range readTLVs(v[4:]) {
		if s.t == subSIDLabel && len(s.v) == 3 {
			base := u24(s.v) & 0xfffff
			return fmt.Sprintf("%d-%d", base, base+size-1)
		}
	}
	return fmt.Sprintf("size %d", size)
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

// opaqueDetail decodes an OSPFv2 opaque LSA (by its link state ID) into a
// one-line summary and a detail map.
func opaqueDetail(id netip.Addr, b []byte) (string, map[string]any) {
	otype := byte(u32(id) >> 24)
	out := map[string]any{"opaque_type": otype, "opaque_id": u32(id) & 0xffffff}
	var parts []string
	switch otype {
	case opqRI:
		out["kind"] = "router-information"
		for _, t := range readTLVs(b) {
			switch t.t {
			case riSRAlgo:
				out["sr_algorithms"] = append([]byte(nil), t.v...)
				var as []string
				for _, x := range t.v {
					as = append(as, fmt.Sprint(x))
				}
				parts = append(parts, "algos "+strings.Join(as, ","))
			case riSRRange:
				out["srgb"] = rangeText(t.v)
				parts = append(parts, "SRGB "+rangeText(t.v))
			case riSRLB:
				out["srlb"] = rangeText(t.v)
				parts = append(parts, "SRLB "+rangeText(t.v))
			case riNodeMSD:
				out["node_msd"] = fmt.Sprintf("% x", t.v)
			case riFAD:
				if len(t.v) >= 4 {
					f := map[string]any{"algorithm": t.v[0], "metric_type": t.v[1], "priority": t.v[3]}
					for _, sub := range readTLVs(t.v[4:]) {
						if len(sub.v) >= 4 {
							g := groupList(binary.BigEndian.Uint32(sub.v))
							switch sub.t {
							case fadExclAny:
								f["exclude_any"] = g
							case fadInclAny:
								f["include_any"] = g
							case fadInclAll:
								f["include_all"] = g
							}
						}
					}
					fads, _ := out["flex_algo_definitions"].([]map[string]any)
					out["flex_algo_definitions"] = append(fads, f)
					parts = append(parts, fmt.Sprintf("FAD %d", t.v[0]))
				}
			default:
				parts = append(parts, fmt.Sprintf("tlv %d", t.t))
			}
		}
	case opqExtPfx:
		out["kind"] = "extended-prefix"
		var pfx []map[string]any
		for _, t := range readTLVs(b) {
			if t.t != tlvExtPrefix || len(t.v) < 4 {
				continue
			}
			bits := int(t.v[1])
			n := (bits + 31) / 32 * 4
			if len(t.v) < 4+n || bits > 32 {
				continue
			}
			var a [4]byte
			copy(a[:], t.v[4:4+n])
			p := netip.PrefixFrom(netip.AddrFrom4(a), bits)
			e := map[string]any{"prefix": p.String(), "route_type": t.v[0], "flags": fmt.Sprintf("%#02x", t.v[3])}
			var sids []string
			for _, sub := range readTLVs(t.v[4+n:]) {
				if sub.t == subPrefixSID && len(sub.v) >= 7 {
					x := fmt.Sprintf("%s algo %d flags %#02x", sidValue(sub.v[4:]), sub.v[3], sub.v[0])
					sids = append(sids, x)
				}
			}
			for _, sub := range readTLVs(t.v[4+n:]) {
				if sub.t == subFAPM && len(sub.v) >= 8 {
					sids = append(sids, fmt.Sprintf("algo %d metric %d", sub.v[0], binary.BigEndian.Uint32(sub.v[4:])))
				}
			}
			e["prefix_sids"] = sids
			pfx = append(pfx, e)
			parts = append(parts, p.String()+" "+strings.Join(sids, "; "))
		}
		out["prefixes"] = pfx
	case opqExtLink:
		out["kind"] = "extended-link"
		for _, t := range readTLVs(b) {
			if t.t != tlvExtLink || len(t.v) < 12 {
				continue
			}
			out["link_type"], out["link_id"], out["link_data"] = linkTypeNames[t.v[0]], addr4(t.v[4:]).String(), addr4(t.v[8:]).String()
			parts = append(parts, fmt.Sprintf("%s %s", linkTypeNames[t.v[0]], addr4(t.v[4:])))
			var adj []string
			for _, sub := range readTLVs(t.v[12:]) {
				switch {
				case sub.t == subAdjSID && len(sub.v) >= 7:
					adj = append(adj, "adj "+sidValue(sub.v[4:]))
				case sub.t == subLANAdjSID && len(sub.v) >= 11:
					adj = append(adj, fmt.Sprintf("lan-adj %s %s", addr4(sub.v[4:]), sidValue(sub.v[8:])))
				case sub.t == subASLA && len(sub.v) >= 4:
					adj = append(adj, aslaText(sub.v))
				}
			}
			out["sub_tlvs"] = adj
			parts = append(parts, adj...)
		}
	case opqTE:
		out["kind"] = "traffic-engineering"
		for _, t := range readTLVs(b) {
			switch t.t {
			case teRouterAddr:
				if len(t.v) >= 4 {
					out["router_address"] = addr4(t.v).String()
					parts = append(parts, "router address "+addr4(t.v).String())
				}
			case teLink:
				l := map[string]any{}
				for _, sub := range readTLVs(t.v) {
					switch {
					case sub.t == teLinkType && len(sub.v) >= 1:
						l["type"] = sub.v[0]
					case sub.t == teLinkID && len(sub.v) >= 4:
						l["link_id"] = addr4(sub.v).String()
					case sub.t == teLocalAddr && len(sub.v) >= 4:
						l["local"] = addr4(sub.v).String()
					case sub.t == teRemoteAddr && len(sub.v) >= 4:
						l["remote"] = addr4(sub.v).String()
					case sub.t == teMetric && len(sub.v) >= 4:
						l["te_metric"] = binary.BigEndian.Uint32(sub.v)
					case sub.t == teMaxBW && len(sub.v) >= 4:
						l["max_bw_mbps"] = math.Float32frombits(binary.BigEndian.Uint32(sub.v)) * 8 / 1e6
					case sub.t == teAdminGroup && len(sub.v) >= 4:
						l["admin_groups"] = groupList(binary.BigEndian.Uint32(sub.v))
					case sub.t == teLinkDelay && len(sub.v) >= 4:
						l["delay_us"] = u24(sub.v[1:])
					}
				}
				out["link"] = l
				parts = append(parts, fmt.Sprintf("link %v metric %v", l["link_id"], l["te_metric"]))
			}
		}
	default:
		out["kind"] = fmt.Sprintf("opaque type %d", otype)
	}
	return strings.Join(parts, " · "), out
}

func aslaText(v []byte) string {
	sl, ul := int(v[0]), int(v[1])
	if len(v) < 4+sl+ul {
		return "asla ?"
	}
	var parts []string
	if sl > 0 && v[4]&sabmFlexAlgo != 0 {
		parts = append(parts, "X")
	}
	for _, s := range readTLVs(v[4+sl+ul:]) {
		switch {
		case s.t == subTEMetric && len(s.v) >= 4:
			parts = append(parts, fmt.Sprintf("te %d", binary.BigEndian.Uint32(s.v)))
		case s.t == subMinMaxDly && len(s.v) >= 8:
			parts = append(parts, fmt.Sprintf("delay %d/%d µs", u24(s.v[1:]), u24(s.v[5:])))
		case s.t == subAdminGroup && len(s.v) >= 4:
			parts = append(parts, "groups "+groupList(binary.BigEndian.Uint32(s.v)))
		}
	}
	return "asla " + strings.Join(parts, " ")
}
