package ospf

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
)

// Areas. Every interface is in one area (the router's unless it names
// another); the LSDB keeps area-scoped LSAs per area (LSAKey.Area) and
// AS-scoped ones once. With interfaces (or the emulated topology) in more
// than one area we are an area border router: spf.go computes each area's
// routes and summary.go advertises them into the others.

const (
	areaNormal = "normal"
	areaStub   = "stub"
	areaNSSA   = "nssa"
)

type area struct {
	idx         int // LSAKey.Area is idx + 1
	id          netip.Addr
	kind        string
	noSummary   bool
	defCost     uint32
	nssaDefault bool
	translate   string
	ranges      []areaRange

	spf *spfResult // from the last origination (border routers)
}

type areaRange struct {
	prefix      netip.Prefix
	noAdvertise bool
	cost        uint32
}

func (a *area) key() int        { return a.idx + 1 }
func (a *area) backbone() bool  { return a.id == zero4 }
func (a *area) externals() bool { return a.kind == areaNormal }

// rangeFor is the range a prefix falls in (the longest), or nil.
func (a *area) rangeFor(p netip.Prefix) *areaRange {
	var best *areaRange
	for i := range a.ranges {
		r := &a.ranges[i]
		if r.prefix.Bits() <= p.Bits() && r.prefix.Contains(p.Addr()) && (best == nil || r.prefix.Bits() > best.prefix.Bits()) {
			best = r
		}
	}
	return best
}

// options are the area's bits in hellos, DBDs and LSAs: E in normal areas,
// N in an NSSA (OSPFv3 also V6 and R).
func (s *session) options(a *area) uint32 {
	o := uint32(0)
	if s.cd.v3 {
		o = OptV6 | OptR
	}
	switch a.kind {
	case areaNormal:
		o |= OptE
	case areaNSSA:
		o |= OptNP
	}
	if !s.cd.v3 {
		o |= OptO // we store and flood opaque LSAs (RFC 5250)
	}
	return o
}

// lsaOptions are the options in the header of an OSPFv2 LSA in the area.
func (s *session) lsaOptions(a *area) uint8 {
	if a.kind == areaNormal {
		return OptE
	}
	return 0
}

func parseAreas(c *Config, v6 bool) (map[netip.Addr]AreaConfig, error) {
	out := map[netip.Addr]AreaConfig{}
	for _, ac := range c.Areas {
		id, err := ParseArea(string(ac.ID))
		if err != nil {
			return nil, err
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("area %s is listed twice", id)
		}
		switch ac.Type {
		case "", areaNormal, areaStub, areaNSSA:
		default:
			return nil, fmt.Errorf("area %s: type must be normal, stub or nssa", id)
		}
		if ac.Type != areaNormal && ac.Type != "" && id == zero4 {
			return nil, errors.New("the backbone (area 0) can't be a stub or NSSA")
		}
		if (ac.NoSummary || ac.DefaultCost > 0) && ac.Type != areaStub && ac.Type != areaNSSA {
			return nil, fmt.Errorf("area %s: no_summary and default_cost are for stub and nssa areas", id)
		}
		if (ac.NSSADefault || ac.Translate != "") && ac.Type != areaNSSA {
			return nil, fmt.Errorf("area %s: nssa_default and nssa_translate are for nssa areas", id)
		}
		switch ac.Translate {
		case "", "candidate", "always", "never":
		default:
			return nil, fmt.Errorf("area %s: nssa_translate must be candidate, always or never", id)
		}
		if ac.DefaultCost >= LSInfinity {
			return nil, fmt.Errorf("area %s: default_cost too large", id)
		}
		for _, r := range ac.Ranges {
			p, err := netip.ParsePrefix(r.Prefix)
			if err != nil || p.Addr().Is4() == v6 {
				return nil, fmt.Errorf("area %s: range %q: want an %s prefix", id, r.Prefix, family(v6))
			}
			if r.Cost >= LSInfinity {
				return nil, fmt.Errorf("area %s: range %s: cost too large", id, r.Prefix)
			}
		}
		out[id] = ac
	}
	return out, nil
}

func validateAreas(c *Config) error {
	// The family of ranges is checked by the version's Validate.
	_, err4 := parseAreas(c, false)
	if err4 == nil {
		return nil
	}
	if _, err6 := parseAreas(c, true); err6 == nil {
		return nil
	}
	return err4
}

// buildAreas sets up the areas the configuration uses: the router's
// first, then the interfaces' and the topology's in order.
func (s *session) buildAreas() error {
	acs, err := parseAreas(&s.cfg, s.cd.v3)
	if err != nil {
		return err
	}
	byID := map[netip.Addr]*area{}
	get := func(name AreaID) *area {
		id, _ := ParseArea(string(name))
		if a := byID[id]; a != nil {
			return a
		}
		ac := acs[id]
		a := &area{idx: len(s.areas), id: id, kind: ac.Type, noSummary: ac.NoSummary, defCost: ac.DefaultCost,
			nssaDefault: ac.NSSADefault, translate: ac.Translate}
		if a.kind == "" {
			a.kind = areaNormal
		}
		if a.defCost == 0 {
			a.defCost = 1
		}
		if a.translate == "" {
			a.translate = "candidate"
		}
		for _, r := range ac.Ranges {
			p, _ := netip.ParsePrefix(r.Prefix)
			a.ranges = append(a.ranges, areaRange{prefix: p.Masked(), noAdvertise: r.NoAdvertise, cost: r.Cost})
		}
		sort.Slice(a.ranges, func(i, j int) bool { return prefixLess(a.ranges[i].prefix, a.ranges[j].prefix) })
		byID[id] = a
		s.areas = append(s.areas, a)
		return a
	}
	s.home = get(s.cfg.Area)
	s.ifArea = []*area{s.home}
	for _, ic := range s.cfg.Interfaces {
		if ic.Area == "" {
			s.ifArea = append(s.ifArea, s.home)
		} else {
			s.ifArea = append(s.ifArea, get(ic.Area))
		}
	}
	s.topoArea = s.home
	if s.cfg.Topology.Area != "" {
		s.topoArea = get(s.cfg.Topology.Area)
	}
	if len(s.cfg.VirtualLinks) > 0 {
		get("0")
		for _, v := range s.cfg.VirtualLinks {
			if id, _ := ParseArea(string(v.TransitArea)); byID[id] == nil {
				return fmt.Errorf("virtual link to %s: no interface in transit area %s", v.RouterID, id)
			}
		}
	}
	s.areaByID = func(name AreaID) *area {
		id, _ := ParseArea(string(name))
		return byID[id]
	}
	for id := range acs {
		if byID[id] == nil {
			return fmt.Errorf("area %s has settings but no interface (or topology) in it", id)
		}
	}
	return nil
}

// activeAreasLocked are the areas with an interface that is up (or the
// emulated topology), in order; the home area always counts.
func (s *session) activeAreasLocked() []*area {
	on := map[*area]bool{s.home: true}
	if s.home.backbone() && len(s.cfg.VirtualLinks) > 0 {
		on[s.home] = false // a router with only virtual links to the backbone
	}
	for _, f := range s.ifs {
		if f.state != ifDown {
			on[f.area] = true
		}
	}
	if s.topo != nil {
		on[s.topoArea] = true
	}
	var out []*area
	for _, a := range s.areas {
		if on[a] {
			out = append(out, a)
		}
	}
	return out
}

// abrLocked: attached to more than one area.
func (s *session) abrLocked() bool { return len(s.activeAreasLocked()) > 1 }

// areaOf is the area an LSA key belongs to (nil: AS scope).
func (s *session) areaOf(k LSAKey) *area {
	if k.Area == 0 {
		return nil
	}
	return s.areas[k.Area-1]
}

// inScope: an LSA with key k is flooded and described on interface f.
func (s *session) inScope(k LSAKey, f *iface) bool {
	if k.Link != 0 && k.Link != f.idx+1 {
		return false
	}
	if k.Area == 0 {
		return f.area.externals() && f.vl == nil // not over virtual links (RFC 2328 13.3)
	}
	return k.Area == f.area.key()
}

// asScope: an AS-scoped LSA type (AS-external, opaque type 11; OSPFv3 by
// its scope bits, unknown types without the U bit being link-scoped).
func (c codec) asScope(t uint16) bool {
	if !c.v3 {
		return t == LSAExternal || t == 11
	}
	if t&0x8000 == 0 && !known3(t) {
		return false
	}
	return t&0x6000 == 0x4000
}

// nssaType is the type-7 LSA type of the version.
func (c codec) nssaType() uint16 {
	if c.v3 {
		return LSA3NSSA
	}
	return LSANSSA
}

// ---- status ----

type AreaStatus struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	NoSummary  bool     `json:"no_summary,omitempty"`
	Interfaces []string `json:"interfaces"`
	Topology   bool     `json:"topology,omitempty"`
	LSAs       int      `json:"lsas"`
	Ranges     []string `json:"ranges,omitempty"`
	Routers    int      `json:"reachable_routers,omitempty"` // from our last SPF (border routers)
	Prefixes   int      `json:"intra_area_prefixes,omitempty"`
	Translator bool     `json:"nssa_translator,omitempty"`
}

func (s *session) areaStatusLocked() []AreaStatus {
	var out []AreaStatus
	count := map[int]int{}
	for k := range s.db {
		count[k.Area]++
	}
	for _, a := range s.areas {
		st := AreaStatus{ID: a.id.String(), Type: a.kind, NoSummary: a.noSummary, Interfaces: []string{}, LSAs: count[a.key()],
			Topology: s.topo != nil && s.topoArea == a, Translator: s.translating[a]}
		for _, f := range s.ifs {
			if f.area == a {
				st.Interfaces = append(st.Interfaces, f.device)
			}
		}
		for _, r := range a.ranges {
			t := r.prefix.String()
			if r.noAdvertise {
				t += " (not advertised)"
			}
			st.Ranges = append(st.Ranges, t)
		}
		if a.spf != nil {
			st.Routers, st.Prefixes = len(a.spf.dist), len(a.spf.intra)
		}
		out = append(out, st)
	}
	return out
}

// needsSPFLocked: LSDB changes matter to our origination (border routers'
// summaries, virtual links' state).
func (s *session) needsSPFLocked() bool {
	return len(s.cfg.VirtualLinks) > 0 || len(s.activeAreasLocked()) > 1
}
