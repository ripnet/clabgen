package ipflow

import (
	"context"
	"time"
)

// maxCatchUp is how much sending a run makes up after falling behind (the
// host stalled): packets missed beyond it are skipped, not sent in a burst.
const maxCatchUp = 10 * time.Millisecond

// Pace calls send at the run's rate until its mode is done (count sent,
// duration over; never for continuous), calling tick after each timer
// tick. It returns false when ctx ended first. The first packet goes out
// when the agent's schedule puts it (phase spread: in the biggest gap
// between the other runs' packets, at most one interval or a second away)
// or at once (phase now).
func (r Run) Pace(ctx context.Context, send func(), tick func(now time.Time)) bool {
	iv := r.Interval()
	id, begin := senders.place(time.Now(), iv, r.Phase != "now")
	defer senders.remove(id)
	if d := time.Until(begin); d > 0 {
		w := time.NewTimer(d)
		select {
		case <-ctx.Done():
			w.Stop()
			return false
		case <-w.C:
		}
	}
	t := time.NewTicker(max(iv, time.Millisecond))
	defer t.Stop()
	var end time.Time
	if r.Mode == "duration" {
		end = begin.Add(r.DurationD())
	}
	behind := max(uint64(maxCatchUp/iv), 1)
	var slot, sent uint64 // send times passed; packets sent
	countDone := func() bool { return r.Mode == "count" && sent >= r.Count }
	send()
	slot, sent = 1, 1
	for !countDone() {
		select {
		case <-ctx.Done():
			return false
		case now := <-t.C:
			due := uint64(now.Sub(begin)/iv) + 1
			if due > slot+behind {
				slot = due - 1
			}
			for slot < due && !countDone() {
				if !end.IsZero() && !now.Before(end) {
					tick(now)
					return true
				}
				send()
				slot++
				sent++
			}
			tick(now)
		}
	}
	return true
}
