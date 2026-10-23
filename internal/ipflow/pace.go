package ipflow

import (
	"context"
	"time"
)

// Pace calls send at the run's rate until its mode is done (count sent,
// duration over; never for continuous), calling tick after each timer
// tick. It returns false when ctx ended first. Sends that fall far behind
// (the host stalled) are skipped instead of sent as a burst.
func (r Run) Pace(ctx context.Context, send func(), tick func(now time.Time)) bool {
	iv := r.Interval()
	t := time.NewTicker(max(iv, time.Millisecond))
	defer t.Stop()
	begin := time.Now()
	var sent uint64
	var end time.Time
	if r.Mode == "duration" {
		end = begin.Add(r.DurationD())
	}
	countDone := func() bool { return r.Mode == "count" && sent >= r.Count }
	send()
	sent++
	for !countDone() {
		select {
		case <-ctx.Done():
			return false
		case now := <-t.C:
			due := uint64(now.Sub(begin)/iv) + 1
			if due > sent+1000 {
				sent = due - 1
			}
			for sent < due && !countDone() {
				if !end.IsZero() && !now.Before(end) {
					tick(now)
					return true
				}
				send()
				sent++
			}
			tick(now)
		}
	}
	return true
}
