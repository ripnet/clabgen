// Package prefixset is a set of IP prefixes, kept as sorted address ranges,
// that one goroutine replaces and others read without a lock: a router
// answering for its many advertised routes on every received packet.
package prefixset

import (
	"net/netip"
	"slices"
	"sync/atomic"
)

type span struct{ first, last netip.Addr }

// Set is safe for concurrent use. The zero value is empty.
type Set struct {
	spans atomic.Pointer[[]span]
}

// Update replaces the set's contents.
func (s *Set) Update(prefixes []netip.Prefix) {
	spans := make([]span, 0, len(prefixes))
	for _, p := range prefixes {
		p = p.Masked()
		spans = append(spans, span{p.Addr(), Last(p)})
	}
	slices.SortFunc(spans, func(a, b span) int { return a.first.Compare(b.first) })
	merged := spans[:0]
	for _, x := range spans {
		if n := len(merged); n > 0 && merged[n-1].last.Is4() == x.first.Is4() {
			prev := &merged[n-1]
			next := prev.last.Next()
			if x.first.Compare(prev.last) <= 0 || next == x.first {
				if x.last.Compare(prev.last) > 0 {
					prev.last = x.last
				}
				continue
			}
		}
		merged = append(merged, x)
	}
	s.spans.Store(&merged)
}

// Contains reports whether a is in the set.
func (s *Set) Contains(a netip.Addr) bool {
	p := s.spans.Load()
	if p == nil {
		return false
	}
	spans := *p
	lo, hi := 0, len(spans)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case a.Compare(spans[mid].first) < 0:
			hi = mid
		case a.Compare(spans[mid].last) > 0:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// Last is the prefix's last address.
func Last(p netip.Prefix) netip.Addr {
	p = p.Masked()
	if p.Addr().Is4() {
		b := p.Addr().As4()
		for i := p.Bits(); i < 32; i++ {
			b[i/8] |= 1 << (7 - i%8)
		}
		return netip.AddrFrom4(b)
	}
	b := p.Addr().As16()
	for i := p.Bits(); i < 128; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	return netip.AddrFrom16(b)
}
