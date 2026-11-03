package flowrx

import (
	"net/netip"
	"testing"
	"time"
)

func TestTable(t *testing.T) {
	tb := New()
	t0 := time.Unix(1_000_000, 0)
	src, dst := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2")
	rec := func(seq uint64) {
		sent := t0.Add(time.Duration(seq) * 10 * time.Millisecond)
		tb.Record(Packet{FlowID: 0xabcd, Seq: seq, Sent: sent, Rx: sent.Add(time.Duration(100+seq) * time.Microsecond),
			Proto: "udp", Src: src, Dst: dst, SrcPort: 1000, DstPort: 2000, Bytes: 100})
	}
	// 0..49, then a gap 50..59 (100 ms), 60..69, 75 before 70..74 (reordered), 72 twice.
	for i := uint64(0); i < 50; i++ {
		rec(i)
	}
	for i := uint64(60); i < 70; i++ {
		rec(i)
	}
	rec(75)
	for i := uint64(70); i < 75; i++ {
		rec(i)
	}
	rec(72)
	rec(1)                                    // within the window but long seen: duplicate
	f := tb.Snapshot(t0.Add(2 * time.Second)) // after the gaps settle
	if len(f) != 1 {
		t.Fatalf("%d flows", len(f))
	}
	x := f[0]
	if x.FlowID != "0000abcd" || x.Rx != 66 || x.Lost != 10 || x.Reordered != 5 || x.Duplicate != 2 {
		t.Fatalf("%+v", x)
	}
	// The 50..59 hole (10 × 10 ms) settles as an outage; the gap 75 opened
	// was filled by the reordered packets, so it isn't one.
	if x.Outages != 1 || x.OutageMaxMs < 99 || x.OutageMaxMs > 101 {
		t.Fatalf("outages %+v", x)
	}
	if x.LatMinus != 100 || x.LatMaxus != 175 || x.SrcPort != 1000 {
		t.Fatalf("latency %+v", x)
	}
	tb.Clear()
	if len(tb.Snapshot(t0)) != 0 {
		t.Fatal("clear")
	}
}

func TestConvergence(t *testing.T) {
	tb := New()
	t0 := time.Unix(1_000_000, 0)
	at := func(seq uint64) time.Time { return t0.Add(time.Duration(seq) * 10 * time.Millisecond) }
	rec := func(from, to uint64) {
		for s := from; s < to; s++ {
			tb.Record(Packet{FlowID: 1, Seq: s, Sent: at(s), Rx: at(s), Proto: "udp"})
		}
	}
	rec(0, 50)
	rec(80, 200) // 50..79 missing: 300 ms from +500 ms, then 1.2 s clean
	f := tb.Convergence(t0, at(200))
	if len(f) != 1 || !f[0].OneWay || f[0].FlowID != "00000001" || !f[0].Window.First.Equal(t0) || !f[0].Window.Last.Equal(at(199)) || len(f[0].Window.Outages) != 1 {
		t.Fatalf("%+v", f)
	}
	r := f[0].Flow.Report
	if r.Status != "restored" || r.Lost != 30 || *r.FirstLossMs != 500 || *r.LastLossMs != 800 {
		t.Fatalf("%+v", r)
	}
	if ms := r.LossMs; ms < 299 || ms > 301 {
		t.Fatalf("loss %v", ms)
	}
	if r.CleanMs != 700 { // now (2000) - settling (500) - last loss (800)
		t.Fatalf("clean %v", r.CleanMs)
	}

	// Gone quiet while (as far as it knows) the sender still sends.
	r = tb.Convergence(t0, at(199).Add(100*time.Millisecond))[0].Flow.Report
	if r.Status != "restored" {
		t.Fatalf("not quiet long enough: %+v", r)
	}
	r = tb.Convergence(t0, at(300))[0].Flow.Report
	if r.Status != "not_restored" || r.CleanMs != 0 || r.Lost < 30+90 {
		t.Fatalf("quiet: %+v", r)
	}

	// Quiet well before the moment: not part of this measurement.
	if f := tb.Convergence(at(300), at(310)); len(f) != 0 {
		t.Fatalf("stale: %+v", f)
	}
}
