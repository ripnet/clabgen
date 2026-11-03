package prefixset

import (
	"fmt"
	"net/netip"
	"testing"
)

func has(t *testing.T, s *Set, addr string, want bool) {
	t.Helper()
	if got := s.Contains(netip.MustParseAddr(addr)); got != want {
		t.Errorf("%s: %v", addr, got)
	}
}

func TestSet(t *testing.T) {
	var s Set
	has(t, &s, "10.0.0.1", false) // empty
	s.Update([]netip.Prefix{
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("198.51.101.0/24"),   // adjacent: merged
		netip.MustParsePrefix("198.51.100.128/25"), // inside
		netip.MustParsePrefix("203.0.113.5/32"),
		netip.MustParsePrefix("2001:db8:1::/48"),
	})
	for _, a := range []string{"198.51.100.0", "198.51.100.200", "198.51.101.255", "203.0.113.5", "2001:db8:1::1", "2001:db8:1:ffff:ffff:ffff:ffff:ffff"} {
		has(t, &s, a, true)
	}
	for _, a := range []string{"198.51.99.255", "198.51.102.0", "203.0.113.4", "203.0.113.6", "2001:db8:2::", "2001:db8::1", "::ffff:198.51.100.1"} {
		has(t, &s, a, false)
	}
	if n := len(*s.spans.Load()); n != 3 {
		t.Fatalf("%d spans", n)
	}
	s.Update(nil)
	has(t, &s, "198.51.100.1", false)
}

func TestLast(t *testing.T) {
	for p, want := range map[string]string{"10.1.2.0/24": "10.1.2.255", "10.1.2.3/32": "10.1.2.3", "0.0.0.0/0": "255.255.255.255", "2001:db8::/126": "2001:db8::3"} {
		if got := Last(netip.MustParsePrefix(p)).String(); got != want {
			t.Errorf("%s: %s", p, got)
		}
	}
}

func BenchmarkContains200k(b *testing.B) {
	ps := make([]netip.Prefix, 0, 200000)
	for i := 0; i < 200000; i += 2 { // every other /24: nothing merges
		ps = append(ps, netip.MustParsePrefix(fmt.Sprintf("%d.%d.%d.0/24", 10+i>>16, i>>8&255, i&255)))
	}
	var s Set
	s.Update(ps)
	a := netip.MustParseAddr("10.1.2.3")
	for i := 0; i < b.N; i++ {
		s.Contains(a)
	}
}
