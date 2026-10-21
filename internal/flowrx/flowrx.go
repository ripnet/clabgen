// Package flowrx counts signed flow packets arriving at a device: every
// clabgen flow can carry a signature (flow ID, sequence, send time), so a
// receiving clabgen device measures the flow one way without any setup:
// received, lost (sequence gaps), reordered, duplicates, one-way latency
// (agents on one host share its clock) and outages.
package flowrx

import (
	"encoding/binary"
	"math"
	"net/netip"
	"sort"
	"sync"
	"time"
)

const (
	window     = 1024 // sequences remembered for reorder/duplicate detection
	maxEntries = 1024
	// A gap is settled (its remaining missing packets become an outage)
	// once it is this old or this far behind the newest sequence, so
	// reordered packets can still fill it.
	settleAfter = 500 * time.Millisecond
	settleSeqs  = 256
)

// gap is a run of missing sequences [lo, hi].
type gap struct {
	lo, hi uint64
	opened time.Time
}

// Signature layout, at the start of a flow packet's payload: magic
// "CLGN", flow ID, sequence index, send time (unix ns).
const SigLen = 24

var SigMagic = [4]byte{'C', 'L', 'G', 'N'}

// ParseSignature decodes a signature at the start of data.
func ParseSignature(data []byte) (flowID uint32, seq uint64, sent time.Time, ok bool) {
	if len(data) < SigLen || [4]byte(data[0:4]) != SigMagic {
		return 0, 0, time.Time{}, false
	}
	return binary.BigEndian.Uint32(data[4:]), binary.BigEndian.Uint64(data[8:]), time.Unix(0, int64(binary.BigEndian.Uint64(data[16:]))), true
}

// Key identifies a received flow.
type Key struct {
	FlowID uint32
	Proto  string // "udp", "icmp"
}

// Packet is one received signed packet.
type Packet struct {
	FlowID   uint32
	Seq      uint64
	Sent, Rx time.Time
	Proto    string
	Src, Dst netip.Addr
	SrcPort  uint16
	DstPort  uint16
	Bytes    int
}

type entry struct {
	key       Key
	src, dst  netip.Addr
	sport     uint16
	dport     uint16
	first     time.Time
	last      time.Time
	lastSent  time.Time
	interval  float64 // EWMA of send-time spacing, seconds
	base, max uint64
	seen      [window]uint64 // seq+1 at seq%window
	rx, bytes uint64
	lost      uint64 // gaps not (yet) filled by reordered packets
	reordered uint64
	dup       uint64
	tooLate   uint64 // older than the window
	latN      uint64
	latSum    float64
	latMin    float64
	latMax    float64
	prevLat   float64
	jitSum    float64
	outages   uint64
	outMax    float64
	outLast   float64
	outTotal  float64
	recentRx  [10]uint64 // per-second ring for the current rate
	recentT   [10]int64
	gaps      []gap
}

// fill removes seq from the open gaps (a reordered packet arrived).
func (e *entry) fill(seq uint64) {
	for i, g := range e.gaps {
		if seq < g.lo || seq > g.hi {
			continue
		}
		switch {
		case g.lo == g.hi:
			e.gaps = append(e.gaps[:i], e.gaps[i+1:]...)
		case seq == g.lo:
			e.gaps[i].lo++
		case seq == g.hi:
			e.gaps[i].hi--
		default:
			e.gaps = append(e.gaps[:i+1], e.gaps[i:]...)
			e.gaps[i].hi, e.gaps[i+1].lo = seq-1, seq+1
		}
		return
	}
}

// settle turns old gaps into outages: missing count × send interval.
func (e *entry) settle(now time.Time) {
	keep := e.gaps[:0]
	for _, g := range e.gaps {
		if now.Sub(g.opened) < settleAfter && e.max-g.hi < settleSeqs {
			keep = append(keep, g)
			continue
		}
		ms := float64(g.hi-g.lo+1) * e.interval * 1000
		e.outages++
		e.outLast, e.outTotal = ms, e.outTotal+ms
		e.outMax = math.Max(e.outMax, ms)
	}
	e.gaps = keep
}

// Table is one device's received-flow table. Safe for concurrent use.
type Table struct {
	mu sync.Mutex
	m  map[Key]*entry
}

func New() *Table { return &Table{m: map[Key]*entry{}} }

// Record counts one packet.
func (t *Table) Record(p Packet) {
	k := Key{p.FlowID, p.Proto}
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[k]
	if e == nil {
		if len(t.m) >= maxEntries {
			return
		}
		e = &entry{key: k, first: p.Rx, base: p.Seq, max: p.Seq, latMin: math.Inf(1)}
		t.m[k] = e
		e.seen[p.Seq%window] = p.Seq + 1
		e.lastSent = p.Sent
	} else {
		switch {
		case p.Seq > e.max:
			if missing := p.Seq - e.max - 1; missing > 0 {
				e.lost += missing
				if len(e.gaps) < 256 {
					e.gaps = append(e.gaps, gap{lo: e.max + 1, hi: p.Seq - 1, opened: p.Rx})
				}
			} else if d := p.Sent.Sub(e.lastSent).Seconds(); d > 0 {
				if e.interval == 0 {
					e.interval = d
				} else {
					e.interval += (d - e.interval) / 16
				}
			}
			e.max, e.lastSent = p.Seq, p.Sent
			e.seen[p.Seq%window] = p.Seq + 1
		case e.max-p.Seq >= window || p.Seq < e.base:
			e.tooLate++
			return
		case e.seen[p.Seq%window] == p.Seq+1:
			e.dup++
			return
		default:
			e.reordered++
			if e.lost > 0 {
				e.lost--
			}
			e.fill(p.Seq)
			e.seen[p.Seq%window] = p.Seq + 1
		}
	}
	e.settle(p.Rx)
	e.src, e.dst, e.sport, e.dport = p.Src, p.Dst, p.SrcPort, p.DstPort
	e.last = p.Rx
	e.rx++
	e.bytes += uint64(p.Bytes)
	sec := p.Rx.Unix()
	i := sec % int64(len(e.recentT))
	if e.recentT[i] != sec {
		e.recentT[i], e.recentRx[i] = sec, 0
	}
	e.recentRx[i]++
	if lat := float64(p.Rx.Sub(p.Sent).Nanoseconds()) / 1000; lat >= 0 {
		if e.latN > 0 {
			e.jitSum += math.Abs(lat - e.prevLat)
		}
		e.latN++
		e.latSum += lat
		e.latMin, e.latMax, e.prevLat = math.Min(e.latMin, lat), math.Max(e.latMax, lat), lat
	}
}

// Flow is the JSON-friendly view of one received flow.
type Flow struct {
	FlowID    string  `json:"flow_id"`
	Proto     string  `json:"proto"`
	Src       string  `json:"src"`
	Dst       string  `json:"dst"`
	SrcPort   uint16  `json:"src_port,omitempty"`
	DstPort   uint16  `json:"dst_port,omitempty"`
	First     string  `json:"first_seen"`
	Last      string  `json:"last_seen"`
	IdleMs    float64 `json:"idle_ms"` // since the last packet
	Rx        uint64  `json:"rx"`
	Bytes     uint64  `json:"bytes"`
	RatePPS   float64 `json:"rate_pps"` // over the last few seconds
	Lost      uint64  `json:"lost"`     // sequence gaps (packets in flight after the last one aren't counted)
	LossPct   float64 `json:"loss_pct"`
	Reordered uint64  `json:"reordered"`
	Duplicate uint64  `json:"duplicate"`
	TooLate   uint64  `json:"too_late"`
	// One-way latency; meaningful between agents on one host (shared clock).
	LatMinus    float64 `json:"latency_min_us"`
	LatAvgus    float64 `json:"latency_avg_us"`
	LatMaxus    float64 `json:"latency_max_us"`
	Jitterus    float64 `json:"jitter_us"`
	Outages     uint64  `json:"outages"`
	OutageMaxMs float64 `json:"outage_max_ms"`
	OutageLast  float64 `json:"outage_last_ms"`
	OutageTotal float64 `json:"outage_total_ms"`
}

// FlowID strings match ipflow's status.
func flowID(id uint32) string {
	const hexd = "0123456789abcdef"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = hexd[id&0xf]
		id >>= 4
	}
	return string(b)
}

// Snapshot returns all flows, most recently active first.
func (t *Table) Snapshot(now time.Time) []Flow {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Flow, 0, len(t.m))
	for _, e := range t.m {
		e.settle(now)
		f := Flow{FlowID: flowID(e.key.FlowID), Proto: e.key.Proto, Src: e.src.String(), Dst: e.dst.String(),
			SrcPort: e.sport, DstPort: e.dport, First: e.first.Format(time.RFC3339Nano), Last: e.last.Format(time.RFC3339Nano),
			IdleMs: float64(now.Sub(e.last)) / float64(time.Millisecond), Rx: e.rx, Bytes: e.bytes, Lost: e.lost,
			Reordered: e.reordered, Duplicate: e.dup, TooLate: e.tooLate, Outages: e.outages, OutageMaxMs: e.outMax,
			OutageLast: e.outLast, OutageTotal: e.outTotal}
		if total := e.rx + e.lost; total > 0 {
			f.LossPct = 100 * float64(e.lost) / float64(total)
		}
		if e.latN > 0 {
			f.LatMinus, f.LatMaxus, f.LatAvgus = e.latMin, e.latMax, e.latSum/float64(e.latN)
			if e.latN > 1 {
				f.Jitterus = e.jitSum / float64(e.latN-1)
			}
		}
		// Rate over the last 3 full seconds.
		var n uint64
		for i, ts := range e.recentT {
			if d := now.Unix() - ts; d >= 1 && d <= 3 {
				n += e.recentRx[i]
			}
		}
		f.RatePPS = float64(n) / 3
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	return out
}

// ClearFlow forgets one flow ID (all protocols).
func (t *Table) ClearFlow(id uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.m {
		if k.FlowID == id {
			delete(t.m, k)
		}
	}
}

// Clear forgets all flows.
func (t *Table) Clear() {
	t.mu.Lock()
	t.m = map[Key]*entry{}
	t.mu.Unlock()
}
