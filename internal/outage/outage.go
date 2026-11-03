// Package outage records when a flow lost packets, for measuring what a
// failure cost it: loss duration (lost packets × send interval) and when
// the loss began and ended relative to a moment (the trigger). Senders and
// receivers keep a Log each.
package outage

import "time"

const (
	maxEntries = 1024
	// Runs of loss closer together than this (or ten send intervals) are
	// one outage in the log, so that partial loss (every fourth packet when
	// one of four ECMP paths fails) is one entry, not hundreds.
	minMergeGap = 50 * time.Millisecond
)

// Status of a flow over a measurement.
const (
	NoImpact    = "no_impact"
	Restored    = "restored"
	NotRestored = "not_restored"
	Stopped     = "stopped" // the flow isn't sending
)

// Outage is a burst of lost packets, by the time they were sent. End is
// when the first packet after them was sent.
type Outage struct {
	Start, End time.Time
	Lost       uint64
}

// Log is a bounded list of outages, oldest first. The zero value is ready;
// the owner provides locking.
type Log struct {
	entries   []Outage
	droppedAt time.Time // the newest outage no longer (or never) in the list
}

// Add appends an outage, or extends the last one if it is close enough.
func (l *Log) Add(o Outage, interval time.Duration) {
	if n := len(l.entries); n > 0 {
		last := &l.entries[n-1]
		if o.Start.Sub(last.End) <= max(10*interval, minMergeGap) {
			if o.Start.Before(last.Start) {
				last.Start = o.Start
			}
			if o.End.After(last.End) {
				last.End = o.End
			}
			last.Lost += o.Lost
			return
		}
	}
	if len(l.entries) == maxEntries {
		l.Dropped(l.entries[0].End)
		l.entries = append(l.entries[:0], l.entries[1:]...)
	}
	l.entries = append(l.entries, o)
}

// Dropped notes that an outage ending at the given time wasn't kept.
func (l *Log) Dropped(at time.Time) {
	if at.After(l.droppedAt) {
		l.droppedAt = at
	}
}

// Span is an outage relative to the start of a measurement.
type Span struct {
	StartMs float64 `json:"start_ms"`
	EndMs   float64 `json:"end_ms"`
	Lost    uint64  `json:"lost"`
}

// Report is what a flow lost since a moment.
type Report struct {
	Status string `json:"status"`
	// LossMs is the loss duration: lost packets × send interval. With
	// partial loss it is shorter than the time from first to last loss.
	LossMs float64 `json:"loss_ms"`
	Lost   uint64  `json:"lost"`
	// From the moment to the first lost packet and to the last one's end.
	FirstLossMs *float64 `json:"first_loss_ms,omitempty"`
	LastLossMs  *float64 `json:"last_loss_ms,omitempty"`
	// CleanMs is how long the flow has been known to be without loss.
	CleanMs float64 `json:"clean_ms"`
	// ResolutionMs is the send interval, the smallest loss it can show.
	ResolutionMs float64 `json:"resolution_ms"`
	// Truncated: the log overflowed since the moment, so this is a minimum.
	Truncated bool   `json:"truncated,omitempty"`
	Outages   []Span `json:"outages"`
	// A flow to many prefixes: how many, and those that lost traffic.
	Prefixes *PrefixLoss `json:"prefixes,omitempty"`
}

// PrefixLoss is a many-destination flow's loss by prefix. Each prefix is
// probed once every ProbeMs, so a failure shorter than that misses some.
type PrefixLoss struct {
	Total    int           `json:"total"`
	Impacted int           `json:"impacted"`
	ProbeMs  float64       `json:"probe_ms"`
	Measured bool          `json:"measured"` // an echo flow; a one-way flow cannot say by prefix
	List     []PrefixEntry `json:"list"`     // most lost first, up to MaxPrefixList
}

// PrefixEntry is what one prefix lost.
type PrefixEntry struct {
	Prefix      string  `json:"prefix"`
	Lost        uint64  `json:"lost"`
	LossMs      float64 `json:"loss_ms"`
	FirstLossMs float64 `json:"first_loss_ms"`
	LastLossMs  float64 `json:"last_loss_ms"`
	// Ongoing: its latest packet whose fate is known was lost.
	Ongoing bool `json:"ongoing,omitempty"`
}

// MaxPrefixList caps the prefixes listed in a report.
const MaxPrefixList = 1000

// Report sums the outages that ended after since. Results are known up to
// settled (later packets may still be lost); open, if not nil, is an outage
// still going on.
func (l *Log) Report(since, settled time.Time, interval time.Duration, open *Outage) Report {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	r := Report{Status: NoImpact, ResolutionMs: ms(interval), Outages: []Span{}, Truncated: l.droppedAt.After(since)}
	last := since
	add := func(o Outage) {
		first := max(0, ms(o.Start.Sub(since))) // an outage already going on
		if r.FirstLossMs == nil {
			r.FirstLossMs = &first
		}
		end := ms(o.End.Sub(since))
		r.LastLossMs = &end
		lost := o.Lost
		if o.Start.Before(since) && interval > 0 {
			// It began earlier: only what it lost from the moment on.
			lost = min(lost, uint64(o.End.Sub(since).Round(interval)/interval))
		}
		r.Lost += lost
		r.Outages = append(r.Outages, Span{StartMs: first, EndMs: end, Lost: lost})
		last = o.End
	}
	for _, o := range l.entries {
		if o.End.After(since) {
			add(o)
			r.Status = Restored
		}
	}
	r.CleanMs = max(0, ms(settled.Sub(last)))
	if open != nil {
		add(*open)
		r.Status, r.CleanMs = NotRestored, 0
	}
	r.LossMs = float64(r.Lost) * ms(interval)
	return r
}

// Window is what one receiver saw of a flow, by send time.
type Window struct {
	First, Last time.Time // its first and latest packet
	Interval    time.Duration
	Outages     []Outage // its gaps, later than the moment asked about
}

// Since lists the outages that ended after a moment.
func (l *Log) Since(t time.Time) []Outage {
	var out []Outage
	for _, o := range l.entries {
		if o.End.After(t) {
			out = append(out, o)
		}
	}
	return out
}

// Combine reports one flow seen by several receivers whose traffic moved
// between them (a failover to another path): each has a hole where the other
// got the packets, so only time that all of them missed is lost. It returns
// false if they each got the same packets (a flooded flow), where each one's
// own report stands.
func Combine(ws []Window, since, now, settled time.Time) (Report, bool) {
	var iv time.Duration
	var last time.Time
	for _, w := range ws {
		// An estimate at each, too high if anything; the lowest is nearest.
		if iv == 0 || w.Interval > 0 && w.Interval < iv {
			iv = w.Interval
		}
		if w.Last.After(last) {
			last = w.Last
		}
	}
	if iv == 0 {
		return Report{}, false
	}
	// Packets each got over the time, against what was sent.
	got := 0.0
	for _, w := range ws {
		from := w.First
		if from.Before(since) {
			from = since
		}
		if w.Last.After(from) {
			got += float64(w.Last.Sub(from)) / float64(iv)
		}
		for _, o := range w.Outages {
			if o.Start.After(since) {
				got -= float64(o.Lost)
			}
		}
	}
	if got > 1.2*float64(last.Sub(since))/float64(iv)+20 {
		return Report{}, false
	}
	// What all of them missed: start from the whole time, cut by what each
	// received (everything but its holes).
	lost := []Outage{{Start: since, End: last}}
	for _, w := range ws {
		var holes []Outage
		if w.First.After(since) {
			holes = append(holes, Outage{Start: since, End: w.First})
		}
		holes = append(holes, w.Outages...)
		holes = append(holes, Outage{Start: w.Last.Add(iv), End: last.Add(iv)})
		lost = intersect(lost, holes)
	}
	var l Log
	for _, o := range lost {
		// Shorter than a packet (the handover itself) isn't a loss.
		if n := uint64(o.End.Sub(o.Start).Round(iv) / iv); n > 0 {
			l.Add(Outage{Start: o.Start, End: o.End, Lost: n}, iv)
		}
	}
	var open *Outage
	if idle := now.Sub(last); idle > max(5*iv, 200*time.Millisecond) {
		open = &Outage{Start: last.Add(iv), End: now, Lost: uint64(idle / iv)}
	}
	return l.Report(since, settled, iv, open), true
}

// intersect is the time in both lists, each sorted and without overlaps.
func intersect(a, b []Outage) []Outage {
	var out []Outage
	for i, j := 0, 0; i < len(a) && j < len(b); {
		start, end := a[i].Start, a[i].End
		if b[j].Start.After(start) {
			start = b[j].Start
		}
		if b[j].End.Before(end) {
			end = b[j].End
		}
		if end.After(start) {
			out = append(out, Outage{Start: start, End: end})
		}
		if a[i].End.Before(b[j].End) {
			i++
		} else {
			j++
		}
	}
	return out
}

// Flow is one flow's report, from its sender or a receiver.
type Flow struct {
	FlowID string `json:"flow_id"`
	// OneWay: the sender gets no replies, so the receivers measure the flow.
	OneWay  bool `json:"one_way"`
	Running bool `json:"running"`
	Report
}
