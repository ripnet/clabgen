package flowstats

import (
	"fmt"
	"net/netip"
	"testing"
	"time"
)

func BenchmarkReceived(b *testing.B) {
	t0 := time.Now()
	tr := New(time.Millisecond, time.Second)
	from := netip.MustParseAddr("10.0.4.1")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		at := t0.Add(time.Duration(i) * time.Millisecond)
		tr.Sent(uint64(i), at)
		tr.Received(uint64(i), at.Add(time.Millisecond), time.Time{}, func() string { return fmt.Sprintf("%d B from %s %s=%d", 56, from, "ttl", 64) })
	}
}
