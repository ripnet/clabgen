package ipflow

import (
	"sort"
	"sync"
	"time"
)

// The agent's paced senders, so a sender that starts can put its packets in
// the biggest gap between everyone else's. Without it, flows that start
// together (a scenario's auto_start, a start of many flows, flows waiting on
// the same retry) send every interval in one burst, which a router's
// receive path may not take in whole.

const (
	maxPlaceWait    = time.Second      // a sender waits at most this (and one interval) for its slot
	maxHorizon      = 10 * time.Second // others' packets are looked at this far ahead at most
	maxPointsPerRun = 4096             // and at most this many of each
)

type sender struct {
	anchor time.Time // first packet; then one every iv
	iv     time.Duration
}

type schedule struct {
	mu   sync.Mutex
	next uint64
	all  map[uint64]sender
}

var senders = &schedule{all: map[uint64]sender{}}

// place registers a sender of interval iv and returns its id and when it
// sends its first packet: now, or with spread the time (within one interval
// and maxPlaceWait) farthest from every other sender's packets.
func (s *schedule) place(now time.Time, iv time.Duration, spread bool) (uint64, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := now
	if spread {
		others := make([]sender, 0, len(s.all))
		for _, o := range s.all {
			others = append(others, o)
		}
		at = now.Add(bestOffset(now, iv, others))
	}
	s.next++
	s.all[s.next] = sender{anchor: at, iv: iv}
	return s.next, at
}

func (s *schedule) remove(id uint64) {
	s.mu.Lock()
	delete(s.all, id)
	s.mu.Unlock()
}

// bestOffset is how long after now a sender of interval iv should send its
// first packet so its packets (one every iv) fall as far as possible from the
// others' packets: the middle of the biggest gap between their packets, laid
// over one interval. Of gaps nearly as big, the earliest wins, so a slow
// sender among faster ones waits for the next gap, not for its own interval.
func bestOffset(now time.Time, iv time.Duration, others []sender) time.Duration {
	if iv <= 0 || len(others) == 0 {
		return 0
	}
	horizon := iv
	for _, o := range others {
		if o.iv > horizon {
			horizon = o.iv
		}
	}
	horizon = min(max(horizon, iv), max(iv, maxHorizon))
	end := now.Add(horizon)
	var pts []time.Duration // others' packets as offsets within one interval
	for _, o := range others {
		if o.iv <= 0 {
			continue
		}
		t := o.anchor
		if t.Before(now) {
			t = t.Add((now.Sub(t) + o.iv - 1) / o.iv * o.iv)
		}
		for n := 0; t.Before(end) && n < maxPointsPerRun; n++ {
			pts = append(pts, t.Sub(now)%iv)
			t = t.Add(o.iv)
		}
	}
	if len(pts) == 0 {
		return 0
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i] < pts[j] })
	wait := min(iv, maxPlaceWait)
	best := time.Duration(-1)
	type cand struct{ at, score time.Duration }
	var cands []cand
	for i, lo := range pts {
		hi := pts[0] + iv // the gap that wraps past the interval's end
		if i+1 < len(pts) {
			hi = pts[i+1]
		}
		if lo == hi {
			continue
		}
		// One gap, lo..hi; as offsets from now it may also start before 0
		// (the wrapping gap seen from the start of the interval).
		for _, shift := range []time.Duration{0, -iv} {
			l, h := lo+shift, hi+shift
			a, b := max(l, 0), min(h, wait)
			if a >= b {
				continue
			}
			at := min(max((l+h)/2, a), b-1)
			score := min(at-l, h-at)
			cands = append(cands, cand{at, score})
			if score > best {
				best = score
			}
		}
	}
	if best < 0 {
		return 0
	}
	bestAt := time.Duration(-1)
	for _, c := range cands {
		if c.score*100 >= best*99 && (bestAt < 0 || c.at < bestAt) {
			bestAt = c.at
		}
	}
	return bestAt
}
