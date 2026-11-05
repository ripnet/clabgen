// Package vpnrib is a device's table of received VPN routes: what a flow or
// a reply looks up to find the label for a destination in a VPN. Routes
// belong to route targets, as in a VRF's import policy; a route distinguisher
// only tells routes apart.
package vpnrib

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Path is one way to a prefix, as a BGP peer advertised it.
type Path struct {
	Source    string // who says so: the BGP instance and peer
	RD        string
	Prefix    netip.Prefix
	Labels    []uint32
	NextHop   netip.Addr
	Targets   []string
	LocalPref uint32
	ASPathLen int
	MED       uint32
}

// better reports whether a beats b: local preference, AS path, MED, then
// anything stable, so that all flows on a device agree.
func better(a, b *Path) bool {
	if a.LocalPref != b.LocalPref {
		return a.LocalPref > b.LocalPref
	}
	if a.ASPathLen != b.ASPathLen {
		return a.ASPathLen < b.ASPathLen
	}
	if a.MED != b.MED {
		return a.MED < b.MED
	}
	if c := a.NextHop.Compare(b.NextHop); c != 0 {
		return c < 0
	}
	if a.RD != b.RD {
		return a.RD < b.RD
	}
	return a.Source < b.Source
}

// NormalizeTarget gives a route target one spelling: "65100:500",
// "192.0.2.1:500", and four-byte ASNs as a plain number ("1.1:500" is
// "65537:500"). It reports whether s is a route target at all.
func NormalizeTarget(s string) (string, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "rt:")
	i := strings.LastIndexByte(s, ':')
	if i <= 0 {
		return "", false
	}
	admin, num := s[:i], s[i+1:]
	if _, err := strconv.ParseUint(num, 10, 32); err != nil {
		return "", false
	}
	if a, err := netip.ParseAddr(admin); err == nil && a.Is4() {
		return s, true
	}
	if hi, lo, dotted := strings.Cut(admin, "."); dotted {
		h, err1 := strconv.ParseUint(hi, 10, 16)
		l, err2 := strconv.ParseUint(lo, 10, 16)
		if err1 != nil || err2 != nil {
			return "", false
		}
		return fmt.Sprintf("%d:%s", h<<16|l, num), true
	}
	if _, err := strconv.ParseUint(admin, 10, 32); err != nil {
		return "", false
	}
	return s, true
}

type key struct {
	source, rd string
	prefix     netip.Prefix
}

// targetTable is one route target's routes: for each prefix the paths in
// preference order, and the prefix lengths in use, longest first.
type targetTable struct {
	paths map[netip.Prefix][]*Path
	bits  [2][]int // [0] IPv4, [1] IPv6
	count [2]map[int]int
}

func newTarget() *targetTable {
	return &targetTable{paths: map[netip.Prefix][]*Path{}, count: [2]map[int]int{{}, {}}}
}

func family(a netip.Addr) int {
	if a.Is4() {
		return 0
	}
	return 1
}

func (t *targetTable) add(p *Path) {
	ps, have := t.paths[p.Prefix]
	i := slices.IndexFunc(ps, func(x *Path) bool { return better(p, x) })
	if i < 0 {
		i = len(ps)
	}
	t.paths[p.Prefix] = slices.Insert(ps, i, p)
	if !have {
		f := family(p.Prefix.Addr())
		if t.count[f][p.Prefix.Bits()]++; t.count[f][p.Prefix.Bits()] == 1 {
			t.bits[f] = append(t.bits[f], p.Prefix.Bits())
			slices.SortFunc(t.bits[f], func(a, b int) int { return cmp.Compare(b, a) })
		}
	}
}

func (t *targetTable) remove(p *Path) {
	ps := slices.DeleteFunc(t.paths[p.Prefix], func(x *Path) bool { return x == p })
	if len(ps) > 0 {
		t.paths[p.Prefix] = ps
		return
	}
	delete(t.paths, p.Prefix)
	f, n := family(p.Prefix.Addr()), p.Prefix.Bits()
	if t.count[f][n]--; t.count[f][n] <= 0 {
		delete(t.count[f], n)
		t.bits[f] = slices.DeleteFunc(t.bits[f], func(x int) bool { return x == n })
	}
}

func (t *targetTable) lookup(a netip.Addr) *Path {
	for _, n := range t.bits[family(a)] {
		p, err := a.Prefix(n)
		if err != nil {
			continue
		}
		if ps := t.paths[p]; len(ps) > 0 {
			return ps[0]
		}
	}
	return nil
}

// Table is safe for concurrent use. The zero value is not: use New.
type Table struct {
	mu      sync.RWMutex
	all     map[key]*Path
	sources map[string]map[key]*Path
	targets map[string]*targetTable
}

// New returns an empty table.
func New() *Table {
	return &Table{all: map[key]*Path{}, sources: map[string]map[key]*Path{}, targets: map[string]*targetTable{}}
}

// Add adds a path, replacing the one with the same source, RD and prefix.
func (t *Table) Add(p Path) {
	p.Prefix = p.Prefix.Masked()
	p.NextHop = p.NextHop.Unmap()
	norm := make([]string, 0, len(p.Targets))
	for _, x := range p.Targets {
		if n, ok := NormalizeTarget(x); ok {
			norm = append(norm, n)
		}
	}
	p.Targets = norm
	k := key{p.Source, p.RD, p.Prefix}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeLocked(k)
	t.all[k] = &p
	if t.sources[p.Source] == nil {
		t.sources[p.Source] = map[key]*Path{}
	}
	t.sources[p.Source][k] = &p
	for _, x := range p.Targets {
		tt := t.targets[x]
		if tt == nil {
			tt = newTarget()
			t.targets[x] = tt
		}
		tt.add(&p)
	}
}

func (t *Table) removeLocked(k key) {
	p := t.all[k]
	if p == nil {
		return
	}
	delete(t.all, k)
	delete(t.sources[k.source], k)
	if len(t.sources[k.source]) == 0 {
		delete(t.sources, k.source)
	}
	for _, x := range p.Targets {
		if tt := t.targets[x]; tt != nil {
			tt.remove(p)
			if len(tt.paths) == 0 {
				delete(t.targets, x)
			}
		}
	}
}

// Remove takes a path away.
func (t *Table) Remove(source, rd string, prefix netip.Prefix) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeLocked(key{source, rd, prefix.Masked()})
}

// RemoveSource takes away everything from a source: a peer that went down.
func (t *Table) RemoveSource(source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.sources[source] {
		t.removeLocked(k)
	}
}

// Lookup finds the best path to a under a route target, by longest match.
// The path is not to be changed.
func (t *Table) Lookup(target string, a netip.Addr) *Path {
	t.mu.RLock()
	defer t.mu.RUnlock()
	tt := t.targets[target]
	if tt == nil {
		return nil
	}
	return tt.lookup(a.Unmap())
}

// LookupAny is Lookup over several route targets: the longest match, then
// the better path.
func (t *Table) LookupAny(targets []string, a netip.Addr) *Path {
	var best *Path
	for _, x := range targets {
		p := t.Lookup(x, a)
		if p == nil {
			continue
		}
		if best == nil || p.Prefix.Bits() > best.Prefix.Bits() || p.Prefix.Bits() == best.Prefix.Bits() && better(p, best) {
			best = p
		}
	}
	return best
}

// Len is the number of paths in the table.
func (t *Table) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.all)
}
