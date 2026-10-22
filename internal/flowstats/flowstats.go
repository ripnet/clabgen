// Package flowstats tracks request/response traffic flows: what was sent,
// which replies came back and how fast, what was lost, and for how long
// the path was down (outages). Flow plugins feed it sent and received
// sequence numbers; it produces a JSON-friendly snapshot with a per-second
// timeline for UIs.
package flowstats

import (
	"math"
	"sort"
	"sync"
	"time"
)

const (
	timelineLen = 600 // seconds kept
	recentLen   = 30  // result lines kept
)

// Result kinds for recent lines.
const (
	ResultReply   = "reply"
	ResultLost    = "timeout"
	ResultLate    = "late"
	ResultDup     = "duplicate"
	ResultError   = "error" // e.g. ICMP unreachable for a sent packet
	ResultTxError = "tx-error"
)

// Line is one per-packet result, like a line of ping output.
type Line struct {
	Seq    uint64  `json:"seq"`
	Time   string  `json:"time"`
	Result string  `json:"result"`
	RTTus  float64 `json:"rtt_us,omitempty"`
	Detail string  `json:"detail,omitempty"`
}

// Bucket counts packets by the second they were sent in, so that
// tx = rx + lost + still pending.
type Bucket struct {
	T        int64   `json:"t"` // unix seconds
	Tx       uint64  `json:"tx"`
	Rx       uint64  `json:"rx"`
	Lost     uint64  `json:"lost"`
	RTTAvgus float64 `json:"rtt_avg_us,omitempty"`
	RTTMaxus float64 `json:"rtt_max_us,omitempty"`
	rttSum   float64
}

type RTT struct {
	Minus    float64 `json:"min_us"`
	Avgus    float64 `json:"avg_us"`
	Maxus    float64 `json:"max_us"`
	Jitterus float64 `json:"jitter_us"` // mean difference between consecutive RTTs
	Lastus   float64 `json:"last_us"`
}

type Stats struct {
	Tx       uint64  `json:"tx"`
	TxErrors uint64  `json:"tx_errors"`
	Rx       uint64  `json:"rx"`
	Lost     uint64  `json:"lost"`
	Late     uint64  `json:"late"`
	Dup      uint64  `json:"duplicate"`
	Errors   uint64  `json:"errors"` // error responses (e.g. unreachable) for sent packets
	Pending  int     `json:"pending"`
	LossPct  float64 `json:"loss_pct"`
	RTT      *RTT    `json:"rtt,omitempty"`

	// An outage is a run of consecutive lost packets; its length is the
	// run's packet count times the send interval.
	Outages         uint64  `json:"outages"`
	OutageCurrentMs float64 `json:"outage_current_ms"`
	OutageLastMs    float64 `json:"outage_last_ms"`
	OutageMaxMs     float64 `json:"outage_max_ms"`
	OutageTotalMs   float64 `json:"outage_total_ms"`

	Recent   []Line   `json:"recent"`
	Timeline []Bucket `json:"timeline"`
}

type pending struct {
	at time.Time
}

// Tracker is safe for concurrent use.
type Tracker struct {
	mu       sync.Mutex
	interval time.Duration
	timeout  time.Duration

	pending map[uint64]pending
	// lost remembers recently lost sequences (and their send time) to tell
	// late replies from duplicates.
	lost map[uint64]time.Time

	// Outages are measured in sequence order: results are settled at the
	// cursor (the oldest unsettled sequence) so a reply that overtakes
	// still-pending packets doesn't end a run early.
	settled map[uint64]bool // seq → replied (true) or lost (false)
	cursor  uint64
	begun   bool
	maxSeq  uint64 // one past the highest sequence sent

	st      Stats
	run     uint64 // current consecutive-loss run
	rttN    uint64
	rttSum  float64
	prevRTT float64
	jitSum  float64
	jitN    uint64

	buckets [timelineLen]Bucket
	recent  [recentLen]entry // ring; formatted into Lines by Snapshot
	nRecent int
}

// New returns a tracker for a flow sending one packet per interval and
// waiting timeout for each reply.
func New(interval, timeout time.Duration) *Tracker {
	return &Tracker{interval: interval, timeout: timeout,
		pending: map[uint64]pending{}, lost: map[uint64]time.Time{}, settled: map[uint64]bool{}}
}

func (t *Tracker) bucket(at time.Time) *Bucket {
	sec := at.Unix()
	b := &t.buckets[sec%timelineLen]
	if b.T != sec {
		*b = Bucket{T: sec}
	}
	return b
}

// bucketIfKept returns the bucket for at only if it is still in the ring.
func (t *Tracker) bucketIfKept(at time.Time) *Bucket {
	sec := at.Unix()
	b := &t.buckets[sec%timelineLen]
	if b.T != sec {
		return nil
	}
	return b
}

// entry is a result line as recorded: formatting (the time, the detail)
// waits until a snapshot asks for it, since most lines are overwritten
// unread at high rates.
type entry struct {
	seq    uint64
	at     time.Time
	result string
	rtt    float64
	detail string
	lazy   func() string // detail, when set
}

func (t *Tracker) line(e entry) {
	t.recent[t.nRecent%recentLen] = e
	t.nRecent++
}

func (e entry) line() Line {
	l := Line{Seq: e.seq, Time: stamp(e.at), Result: e.result, RTTus: e.rtt, Detail: e.detail}
	if e.lazy != nil {
		l.Detail = e.lazy()
	}
	return l
}

func stamp(at time.Time) string { return at.Format("15:04:05.000") }

// settle records seq's outcome and advances the cursor over settled
// sequences, extending or ending the current loss run.
func (t *Tracker) settle(seq uint64, replied bool) {
	if !t.begun || seq < t.cursor {
		return
	}
	t.settled[seq] = replied
	for {
		r, ok := t.settled[t.cursor]
		if !ok {
			if _, waiting := t.pending[t.cursor]; waiting || t.cursor >= t.maxSeq {
				return
			}
			t.cursor++ // never sent (e.g. a flow without reply tracking)
			continue
		}
		delete(t.settled, t.cursor)
		if r {
			t.endOutage()
		} else {
			t.run++
		}
		t.cursor++
	}
}

// Sent records a transmitted packet that expects a reply.
func (t *Tracker) Sent(seq uint64, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun {
		t.begun, t.cursor = true, seq
	}
	if seq+1 > t.maxSeq {
		t.maxSeq = seq + 1
	}
	t.st.Tx++
	t.bucket(at).Tx++
	t.pending[seq] = pending{at: at}
}

// SentNoReply records a transmitted packet that expects no reply. For
// such flows only failed sends count as lost, so a successful send ends
// the current outage.
func (t *Tracker) SentNoReply(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.st.Tx++
	t.bucket(at).Tx++
	t.endOutage()
}

// TxError records a failed send (e.g. the port is down). It counts as
// sent and lost, so a local link failure shows up as an outage.
// seq may already have been recorded with Sent (flows record before
// sending, so that a very fast reply always finds it pending).
func (t *Tracker) TxError(seq uint64, at time.Time, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun {
		t.begun, t.cursor = true, seq
	}
	if seq+1 > t.maxSeq {
		t.maxSeq = seq + 1
	}
	b := t.bucket(at)
	if _, ok := t.pending[seq]; ok {
		delete(t.pending, seq)
	} else {
		t.st.Tx++
		b.Tx++
	}
	t.st.TxErrors++
	t.st.Lost++
	b.Lost++
	t.settle(seq, false)
	t.line(entry{seq: seq, at: at, result: ResultTxError, detail: err.Error()})
}

// Received records a reply for seq that arrived at rx. sentAt, when
// non-zero, is the send time carried in the packet (more precise than the
// recorded one). detail, if not nil, gives the text shown on the result
// line (e.g. "ttl=63"); it is called only if the line is read.
// It returns the result kind.
func (t *Tracker) Received(seq uint64, rx, sentAt time.Time, detail func() string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.pending[seq]
	if !ok {
		if at, wasLost := t.lost[seq]; wasLost {
			delete(t.lost, seq)
			t.st.Late++
			t.line(entry{seq: seq, at: rx, result: ResultLate, rtt: us(rx.Sub(at)), lazy: detail})
			return ResultLate
		}
		t.st.Dup++
		t.line(entry{seq: seq, at: rx, result: ResultDup, lazy: detail})
		return ResultDup
	}
	delete(t.pending, seq)
	if sentAt.IsZero() {
		sentAt = p.at
	}
	rtt := us(rx.Sub(sentAt))
	if rtt < 0 {
		rtt = 0
	}
	t.st.Rx++
	if b := t.bucketIfKept(p.at); b != nil {
		b.Rx++
		b.rttSum += rtt
		b.RTTAvgus = b.rttSum / float64(b.Rx)
		b.RTTMaxus = math.Max(b.RTTMaxus, rtt)
	}
	if t.st.RTT == nil {
		t.st.RTT = &RTT{Minus: rtt, Maxus: rtt}
	}
	r := t.st.RTT
	r.Minus, r.Maxus, r.Lastus = math.Min(r.Minus, rtt), math.Max(r.Maxus, rtt), rtt
	t.rttN++
	t.rttSum += rtt
	r.Avgus = t.rttSum / float64(t.rttN)
	if t.rttN > 1 {
		t.jitN++
		t.jitSum += math.Abs(rtt - t.prevRTT)
		r.Jitterus = t.jitSum / float64(t.jitN)
	}
	t.prevRTT = rtt
	t.settle(seq, true)
	t.line(entry{seq: seq, at: rx, result: ResultReply, rtt: rtt, lazy: detail})
	return ResultReply
}

// ErrorReceived records an error response (such as ICMP unreachable) for
// seq. The packet counts as answered, not lost, but not as a reply.
func (t *Tracker) ErrorReceived(seq uint64, rx time.Time, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.st.Errors++
	if p, ok := t.pending[seq]; ok {
		delete(t.pending, seq)
		// The path is broken even though something answered.
		t.st.Lost++
		if b := t.bucketIfKept(p.at); b != nil {
			b.Lost++
		}
		t.settle(seq, false)
	}
	t.line(entry{seq: seq, at: rx, result: ResultError, detail: detail})
}

func (t *Tracker) endOutage() {
	if t.run == 0 {
		return
	}
	ms := float64(t.run) * float64(t.interval) / float64(time.Millisecond)
	t.st.Outages++
	t.st.OutageLastMs = ms
	t.st.OutageMaxMs = math.Max(t.st.OutageMaxMs, ms)
	t.st.OutageTotalMs += ms
	t.run = 0
}

// Expire counts pending packets older than the timeout as lost. Call it
// periodically and once more when the flow stops (with force) to settle
// everything still pending.
func (t *Tracker) Expire(now time.Time, force bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var due []uint64
	for seq, p := range t.pending {
		if force || now.Sub(p.at) >= t.timeout {
			due = append(due, seq)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i] < due[j] })
	for _, seq := range due {
		p := t.pending[seq]
		delete(t.pending, seq)
		t.lost[seq] = p.at
		t.st.Lost++
		t.settle(seq, false)
		if b := t.bucketIfKept(p.at); b != nil {
			b.Lost++
		}
		t.line(entry{seq: seq, at: p.at.Add(t.timeout), result: ResultLost})
	}
	// Forget old losses; replies later than this count as duplicates.
	keep := 10 * t.timeout
	for seq, at := range t.lost {
		if now.Sub(at) > keep {
			delete(t.lost, seq)
		}
	}
}

// Snapshot returns the statistics and the last `seconds` of timeline
// (only seconds in which something was sent).
func (t *Tracker) Snapshot(now time.Time, seconds int) Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.st
	if st.RTT != nil {
		r := *st.RTT
		st.RTT = &r
	}
	st.Pending = len(t.pending)
	if done := st.Rx + st.Lost; done > 0 {
		st.LossPct = 100 * float64(st.Lost) / float64(done)
	}
	if t.run > 0 {
		st.OutageCurrentMs = float64(t.run) * float64(t.interval) / float64(time.Millisecond)
	}
	st.Recent = make([]Line, 0, recentLen)
	for i := max(0, t.nRecent-recentLen); i < t.nRecent; i++ {
		st.Recent = append(st.Recent, t.recent[i%recentLen].line())
	}
	if seconds > timelineLen {
		seconds = timelineLen
	}
	st.Timeline = []Bucket{}
	for sec := now.Unix() - int64(seconds) + 1; sec <= now.Unix(); sec++ {
		b := t.buckets[sec%timelineLen]
		if b.T == sec && b.Tx > 0 {
			st.Timeline = append(st.Timeline, b)
		}
	}
	return st
}

func us(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1000 }
