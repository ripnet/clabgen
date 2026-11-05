package vpnrib

import (
	"net/netip"
	"sync"
	"testing"
)

func path(src, rd, prefix, nh string, label uint32, targets ...string) Path {
	return Path{Source: src, RD: rd, Prefix: netip.MustParsePrefix(prefix), NextHop: netip.MustParseAddr(nh), Labels: []uint32{label}, Targets: targets, LocalPref: 100}
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestLongestMatch(t *testing.T) {
	tb := New()
	tb.Add(path("a", "1:1", "198.51.100.0/24", "192.0.2.1", 100, "65000:1"))
	tb.Add(path("a", "1:1", "198.51.100.128/25", "192.0.2.1", 200, "65000:1"))
	tb.Add(path("a", "1:1", "2001:db8::/32", "::ffff:192.0.2.1", 300, "65000:1"))
	for dst, want := range map[string]uint32{"198.51.100.1": 100, "198.51.100.200": 200, "2001:db8::1": 300} {
		p := tb.Lookup("65000:1", addr(dst))
		if p == nil || p.Labels[0] != want {
			t.Fatalf("%s: %+v", dst, p)
		}
	}
	if p := tb.Lookup("65000:1", addr("2001:db8::1")); p.NextHop != addr("192.0.2.1") {
		t.Fatalf("mapped next hop: %s", p.NextHop)
	}
	if p := tb.Lookup("65000:1", addr("203.0.113.1")); p != nil {
		t.Fatalf("no route expected: %+v", p)
	}
	tb.Remove("a", "1:1", netip.MustParsePrefix("198.51.100.128/25"))
	if p := tb.Lookup("65000:1", addr("198.51.100.200")); p == nil || p.Labels[0] != 100 {
		t.Fatalf("after remove: %+v", p)
	}
}

func TestOverlapByTarget(t *testing.T) {
	tb := New()
	tb.Add(path("a", "1:1", "10.0.0.0/24", "192.0.2.1", 100, "65000:1"))
	tb.Add(path("a", "1:2", "10.0.0.0/24", "192.0.2.1", 200, "65000:2"))
	if tb.Lookup("65000:1", addr("10.0.0.1")).Labels[0] != 100 || tb.Lookup("65000:2", addr("10.0.0.1")).Labels[0] != 200 {
		t.Fatal("route targets must keep overlapping VPNs apart")
	}
	if tb.Lookup("65000:3", addr("10.0.0.1")) != nil {
		t.Fatal("unknown target")
	}
}

func TestBestPath(t *testing.T) {
	tb := New()
	low := path("a", "1:1", "10.0.0.0/24", "192.0.2.9", 100, "65000:1")
	high := path("b", "1:2", "10.0.0.0/24", "192.0.2.8", 200, "65000:1")
	high.LocalPref = 200
	tb.Add(low)
	tb.Add(high)
	if got := tb.Lookup("65000:1", addr("10.0.0.1")).Labels[0]; got != 200 {
		t.Fatalf("local pref: %d", got)
	}
	tb.RemoveSource("b")
	if got := tb.Lookup("65000:1", addr("10.0.0.1")).Labels[0]; got != 100 {
		t.Fatalf("fall back after the peer goes: %d", got)
	}
	long := path("c", "1:3", "10.0.0.0/24", "192.0.2.1", 300, "65000:1")
	long.ASPathLen = 2
	tb.Add(long)
	if got := tb.Lookup("65000:1", addr("10.0.0.1")).Labels[0]; got != 100 {
		t.Fatalf("AS path: %d", got)
	}
}

func TestReplaceFollowsNewLabelAndTargets(t *testing.T) {
	tb := New()
	tb.Add(path("a", "1:1", "10.0.0.0/24", "192.0.2.1", 100, "65000:1"))
	tb.Add(path("a", "1:1", "10.0.0.0/24", "192.0.2.1", 101, "65000:2"))
	if tb.Lookup("65000:1", addr("10.0.0.1")) != nil {
		t.Fatal("the old target must lose the route")
	}
	if got := tb.Lookup("65000:2", addr("10.0.0.1")).Labels[0]; got != 101 {
		t.Fatalf("label %d", got)
	}
	if tb.Len() != 1 {
		t.Fatalf("len %d", tb.Len())
	}
	tb.RemoveSource("a")
	if tb.Len() != 0 || len(tb.targets) != 0 || len(tb.sources) != 0 {
		t.Fatalf("left behind: %d %v %v", tb.Len(), tb.targets, tb.sources)
	}
}

func TestLookupAny(t *testing.T) {
	tb := New()
	tb.Add(path("a", "1:1", "10.0.0.0/16", "192.0.2.1", 100, "65000:1"))
	tb.Add(path("a", "1:2", "10.0.0.0/24", "192.0.2.1", 200, "65000:2"))
	if got := tb.LookupAny([]string{"65000:1", "65000:2"}, addr("10.0.0.1")).Labels[0]; got != 200 {
		t.Fatalf("longest over targets: %d", got)
	}
	if tb.LookupAny([]string{"9:9"}, addr("10.0.0.1")) != nil {
		t.Fatal("unknown target")
	}
}

func TestNormalizeTarget(t *testing.T) {
	for in, want := range map[string]string{"65100:500": "65100:500", " rt:65100:500": "65100:500", "192.0.2.1:7": "192.0.2.1:7", "1.1:500": "65537:500", "4200000000:1": "4200000000:1"} {
		if got, ok := NormalizeTarget(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "65100", ":5", "a:b", "65100:x", "1.2.3:4"} {
		if got, ok := NormalizeTarget(in); ok {
			t.Errorf("%q should not parse: %q", in, got)
		}
	}
}

func TestConcurrent(t *testing.T) {
	tb := New()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				tb.Add(path("a", "1:1", "10.0.0.0/24", "192.0.2.1", uint32(j), "65000:1"))
				tb.Remove("a", "1:1", netip.MustParsePrefix("10.0.0.0/24"))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				tb.Lookup("65000:1", addr("10.0.0.1"))
			}
		}()
	}
	wg.Wait()
}
