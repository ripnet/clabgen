package ipflow

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

// minGap returns the sorted offsets (mod iv) of the senders' packets and the
// smallest gap between neighbours, round the interval.
func minGap(s *schedule, now time.Time, iv time.Duration) (time.Duration, []time.Duration) {
	var pts []time.Duration
	for _, o := range s.all {
		for t := o.anchor; t.Before(o.anchor.Add(max(iv, o.iv))); t = t.Add(o.iv) {
			pts = append(pts, t.Sub(now)%iv)
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i] < pts[j] })
	g := pts[0] + iv - pts[len(pts)-1]
	for i := 1; i < len(pts); i++ {
		g = min(g, pts[i]-pts[i-1])
	}
	return g, pts
}

func TestPlaceSpreadsFlowsStartedTogether(t *testing.T) {
	s := &schedule{all: map[uint64]sender{}}
	now := time.Unix(1000, 0)
	const n = 84
	for i := 0; i < n; i++ {
		_, at := s.place(now, time.Second, true)
		if w := at.Sub(now); w < 0 || w >= time.Second {
			t.Fatalf("flow %d waits %v, want within one interval", i, w)
		}
	}
	g, _ := minGap(s, now, time.Second)
	// Greedy halving gives at least interval/2^ceil(log2 n) (here 1s/128).
	if g < time.Second/128 {
		t.Fatalf("closest packets %v apart, want >= %v", g, time.Second/128)
	}
}

func TestPlaceSlowFlowWaitsForNextGapNotItsInterval(t *testing.T) {
	s := &schedule{all: map[uint64]sender{}}
	now := time.Unix(1000, 0)
	for i := 0; i < 10; i++ {
		s.place(now, time.Second, true)
	}
	_, at := s.place(now, 10*time.Second, true) // 0.1 pps
	w := at.Sub(now)
	if w >= time.Second {
		t.Fatalf("0.1 pps flow waits %v, want under a second", w)
	}
	// It sits mid-gap: 1 s / 10 flows = 100 ms gaps, so 50 ms from the nearest.
	off := w % time.Second
	for _, o := range s.all {
		if o.iv != time.Second {
			continue
		}
		d := (o.anchor.Sub(now)%time.Second - off + time.Second) % time.Second
		d = min(d, time.Second-d)
		if d < 45*time.Millisecond {
			t.Fatalf("0.1 pps flow is %v from a 1 pps flow's packets, want ~50ms", d)
		}
	}
}

func TestPlaceFastFlowAmongSlowOnes(t *testing.T) {
	s := &schedule{all: map[uint64]sender{}}
	now := time.Unix(1000, 0)
	for i := 0; i < 20; i++ {
		s.place(now, time.Second, true) // 20 x 1 pps: one packet every 50 ms
	}
	iv := 10 * time.Millisecond // 100 pps
	_, at := s.place(now, iv, true)
	if at.Sub(now) >= iv {
		t.Fatalf("100 pps flow waits %v, want under one interval", at.Sub(now))
	}
	// Over a second its 100 packets keep clear of the 20 others': laid over
	// 10 ms those are 20 points, 0.5 ms apart, so 0.25 ms is the most possible.
	off := at.Sub(now) % iv
	for id, o := range s.all {
		if o.iv != time.Second {
			continue
		}
		d := (o.anchor.Sub(now)%iv - off + iv) % iv
		d = min(d, iv-d)
		if d < 200*time.Microsecond {
			t.Fatalf("sender %d's packets %v from the 100 pps flow's, want >= 200µs", id, d)
		}
	}
}

func TestPlaceNowAndAlone(t *testing.T) {
	s := &schedule{all: map[uint64]sender{}}
	now := time.Unix(1000, 0)
	if _, at := s.place(now, time.Second, true); !at.Equal(now) {
		t.Fatalf("first flow starts at %v, want now", at.Sub(now))
	}
	if _, at := s.place(now, time.Second, false); !at.Equal(now) {
		t.Fatalf("phase now starts at %v, want now", at.Sub(now))
	}
	id, _ := s.place(now, time.Second, true)
	s.remove(id)
	if len(s.all) != 2 {
		t.Fatalf("%d senders after remove, want 2", len(s.all))
	}
}

func TestPhaseValidate(t *testing.T) {
	r := DefaultRun
	for _, p := range []string{"", "spread", "now"} {
		r.Phase = p
		if err := r.Validate(); err != nil {
			t.Fatalf("phase %q: %v", p, err)
		}
	}
	r.Phase = "later"
	if r.Validate() == nil {
		t.Fatal("phase later accepted")
	}
}

// Two runs started together send half an interval apart.
func TestPaceSpreadsConcurrentRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Two packets each, so both runs are registered while the other places.
	r := Run{Mode: "count", Count: 2, Rate: 5, Timeout: "1s"} // 200 ms interval
	var mu sync.Mutex
	var firsts []time.Time
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			r.Pace(ctx, func() {
				if first {
					mu.Lock()
					firsts = append(firsts, time.Now())
					mu.Unlock()
					first = false
				}
			}, func(time.Time) {})
		}()
	}
	wg.Wait()
	d := firsts[1].Sub(firsts[0])
	if d < 0 {
		d = -d
	}
	if d < 70*time.Millisecond || d > 130*time.Millisecond {
		t.Fatalf("first packets %v apart, want ~100ms", d)
	}
}

// After a stall a run makes up at most maxCatchUp of packets, and count
// mode still sends exactly count.
func TestPaceCatchUpCapped(t *testing.T) {
	r := Run{Mode: "count", Count: 120, Rate: 1000, Timeout: "1s", Phase: "now"}
	var times []time.Time
	n := 0
	r.Pace(context.Background(), func() {
		times = append(times, time.Now())
		n++
		if n == 10 {
			time.Sleep(60 * time.Millisecond)
		}
	}, func(time.Time) {})
	if n != 120 {
		t.Fatalf("sent %d, want 120", n)
	}
	// Right after the stall (packet 10) at most ~10 ms worth go out at once.
	burst := 0
	for _, ts := range times[10:] {
		if ts.Sub(times[10]) < 2*time.Millisecond {
			burst++
		}
	}
	if burst > 15 {
		t.Fatalf("%d packets within 2 ms after a 60 ms stall, want <= 15 (catch-up capped at 10 ms)", burst)
	}
}
