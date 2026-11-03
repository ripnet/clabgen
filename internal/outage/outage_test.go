package outage

import (
	"testing"
	"time"
)

var (
	t0 = time.Unix(1_000_000, 0)
	iv = time.Millisecond
)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

func TestReportOutagesSinceTheMoment(t *testing.T) {
	var l Log
	l.Add(Outage{at(-5000), at(-4900), 100}, iv) // before: not counted
	l.Add(Outage{at(20), at(220), 200}, iv)
	l.Add(Outage{at(1000), at(1100), 100}, iv)
	r := l.Report(t0, at(3100), iv, nil)
	if r.Status != Restored || r.Lost != 300 || r.LossMs != 300 || r.ResolutionMs != 1 {
		t.Fatalf("%+v", r)
	}
	if *r.FirstLossMs != 20 || *r.LastLossMs != 1100 || r.CleanMs != 2000 || len(r.Outages) != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestReportNoImpact(t *testing.T) {
	var l Log
	r := l.Report(t0, at(1500), iv, nil)
	if r.Status != NoImpact || r.LossMs != 0 || r.CleanMs != 1500 || r.FirstLossMs != nil || r.Outages == nil {
		t.Fatalf("%+v", r)
	}
	// Results not settled yet: nothing is known to be clean.
	if r := l.Report(t0, at(-500), iv, nil); r.CleanMs != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestReportOpenOutage(t *testing.T) {
	var l Log
	r := l.Report(t0, at(900), iv, &Outage{Start: at(100), End: at(900), Lost: 800})
	if r.Status != NotRestored || r.LossMs != 800 || r.CleanMs != 0 || *r.FirstLossMs != 100 || *r.LastLossMs != 900 {
		t.Fatalf("%+v", r)
	}
}

// Every fourth packet lost (one of four ECMP paths down for 400 ms): one
// entry whose span shows the whole failure, the loss duration a quarter of it.
func TestPartialLossMerges(t *testing.T) {
	var l Log
	for ms := 0; ms < 400; ms += 4 {
		l.Add(Outage{at(ms), at(ms + 1), 1}, iv)
	}
	r := l.Report(t0.Add(-time.Second), at(2000), iv, nil)
	if len(r.Outages) != 1 || r.Lost != 100 || r.LossMs != 100 {
		t.Fatalf("%+v", r)
	}
	if span := *r.LastLossMs - *r.FirstLossMs; span != 397 {
		t.Fatalf("span %v", span)
	}
}

func TestLogBoundedAndTruncationReported(t *testing.T) {
	var l Log
	for i := 0; i < maxEntries+10; i++ {
		l.Add(Outage{at(i * 1000), at(i*1000 + 1), 1}, iv)
	}
	if len(l.entries) != maxEntries {
		t.Fatal(len(l.entries))
	}
	if r := l.Report(t0, at(maxEntries*2000), iv, nil); !r.Truncated || r.Lost != maxEntries {
		t.Fatalf("truncated=%v lost=%d", r.Truncated, r.Lost)
	}
	// Dropped entries were before this moment: the report is complete.
	if r := l.Report(at(100_000), at(maxEntries*2000), iv, nil); r.Truncated {
		t.Fatal("truncated")
	}
}

// Traffic moves from receiver a to b at +100 ms after a loss of 400 ms, and
// back at +2000 ms without loss: only the 400 ms is lost, not what each
// missed while the other had it.
func TestCombineMoved(t *testing.T) {
	since := at(0)
	a := Window{First: at(-5000), Last: at(3000), Interval: iv, Outages: []Outage{{at(100), at(2000), 1900}}}
	b := Window{First: at(500), Last: at(1999), Interval: iv}
	r, moved := Combine([]Window{a, b}, since, at(3000), at(2500))
	if !moved || r.Status != Restored || r.Lost != 400 || r.LossMs != 400 || *r.FirstLossMs != 100 || *r.LastLossMs != 500 {
		t.Fatalf("moved=%v %+v", moved, r)
	}
}

// Still on b, all clean since it moved.
func TestCombineStaysMoved(t *testing.T) {
	a := Window{First: at(-5000), Last: at(99), Interval: iv}
	b := Window{First: at(500), Last: at(3000), Interval: iv}
	r, moved := Combine([]Window{a, b}, at(0), at(3000), at(2500))
	if !moved || r.Status != Restored || r.Lost != 400 || *r.LastLossMs != 500 || r.CleanMs != 2000 {
		t.Fatalf("moved=%v %+v", moved, r)
	}
}

func TestCombineAllSilentIsOpen(t *testing.T) {
	a := Window{First: at(-5000), Last: at(99), Interval: iv}
	b := Window{First: at(-5000), Last: at(-3000), Interval: iv}
	r, moved := Combine([]Window{a, b}, at(0), at(3000), at(2500))
	if !moved || r.Status != NotRestored || r.CleanMs != 0 {
		t.Fatalf("moved=%v %+v", moved, r)
	}
}

// Flooded: both got every packet, so each one's own report stands.
func TestCombineFlooded(t *testing.T) {
	a := Window{First: at(-5000), Last: at(3000), Interval: iv}
	b := Window{First: at(-5000), Last: at(3000), Interval: iv, Outages: []Outage{{at(100), at(500), 400}}}
	if _, moved := Combine([]Window{a, b}, at(0), at(3000), at(2500)); moved {
		t.Fatal("flooded taken for moved")
	}
}

// The handover itself leaves less than a packet's time between them.
func TestCombineHandoverNoLoss(t *testing.T) {
	a := Window{First: at(-5000), Last: at(1000), Interval: 5 * iv}
	b := Window{First: at(1006), Last: at(3000), Interval: 5 * iv}
	r, moved := Combine([]Window{a, b}, at(0), at(3000), at(2500))
	if !moved || r.Status != NoImpact || r.Lost != 0 || len(r.Outages) != 0 {
		t.Fatalf("moved=%v %+v", moved, r)
	}
}

// An outage already going on counts only from the moment.
func TestReportOutageAlreadyGoingOn(t *testing.T) {
	var l Log
	l.Add(Outage{at(-60000), at(200), 60200}, iv)
	r := l.Report(t0, at(3000), iv, nil)
	if r.Lost != 200 || r.LossMs != 200 || *r.FirstLossMs != 0 || *r.LastLossMs != 200 {
		t.Fatalf("%+v", r)
	}
}
