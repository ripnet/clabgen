package cfm

import (
	"encoding/binary"
	"math/rand/v2"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Proactive measurement sessions (Y.1731 / MEF 35 style): one DMM, SLM or
// LMM per interval toward a remote MEP; results summed in bins of bin_s
// seconds, the last few kept.

// Bin is one measurement interval's results.
type Bin struct {
	Start    string `json:"start"`
	End      string `json:"end,omitempty"` // unset: the current bin
	Sent     int    `json:"sent"`
	Received int    `json:"received"`
	// delay
	Delay    *rttStats `json:"delay,omitempty"`
	AvgFDVUS float64   `json:"avg_fdv_us,omitempty"`
	// synthetic-loss (SLM counts) and loss (data frames)
	FarEndTx       uint32  `json:"far_end_tx,omitempty"`
	FarEndLoss     int64   `json:"far_end_loss"`
	NearEndTx      uint32  `json:"near_end_tx,omitempty"`
	NearEndLoss    int64   `json:"near_end_loss"`
	FarEndLossPct  float64 `json:"far_end_loss_pct"`
	NearEndLossPct float64 `json:"near_end_loss_pct"`
}

type SessionStatus struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Target  string `json:"target"`
	Error   string `json:"error,omitempty"` // e.g. the target is unknown yet
	Current Bin    `json:"current"`
	History []Bin  `json:"history"` // newest first
}

type session struct {
	cfg  Session
	m    *mep
	quit chan struct{} // closed when a config change replaces the session
	dst  pkt.MAC
	err  string
	test uint32

	bin     Bin
	start   time.Time
	delays  []time.Duration
	history []Bin

	dm     chan dmReply
	dmKeys map[uint64]time.Time
	sl     chan *slm
	lm     chan lmReply
	maxFCb uint32 // SLM: the responder's count so far
	binFCb uint32 // at the start of the bin
	prevLM *lmReply
}

func (m *mep) startSessions() {
	for _, c := range m.c().Sessions {
		if c.IntervalMS == 0 {
			c.IntervalMS = 1000
		}
		if c.BinS == 0 {
			c.BinS = 60
		}
		if c.Bins == 0 {
			c.Bins = 10
		}
		x := &session{cfg: c, m: m, quit: make(chan struct{}), test: c.TestID, history: []Bin{}, dm: make(chan dmReply, 64), dmKeys: map[uint64]time.Time{},
			sl: make(chan *slm, 64), lm: make(chan lmReply, 64)}
		if x.test == 0 {
			x.test = rand.Uint32()
		}
		m.mu.Lock()
		m.sessions = append(m.sessions, x)
		m.mu.Unlock()
		go x.run()
	}
}

func (x *session) run() {
	m := x.m
	t := time.NewTicker(time.Duration(x.cfg.IntervalMS) * time.Millisecond)
	defer t.Stop()
	now := time.Now()
	m.mu.Lock()
	x.newBinLocked(now)
	m.mu.Unlock()
	defer x.unregister()
	for {
		select {
		case <-m.stop:
			return
		case <-x.quit:
			return
		case r := <-x.dm:
			d := max(r.rxb.Sub(r.txf), 0)
			if !r.txb.IsZero() && !r.rxf.IsZero() {
				d = max(d-r.txb.Sub(r.rxf), 0)
			}
			m.mu.Lock()
			delete(x.dmKeys, uint64(r.txf.Unix())<<32|uint64(uint32(r.txf.Nanosecond())))
			x.bin.Received++
			x.delays = append(x.delays, d)
			m.mu.Unlock()
		case r := <-x.sl:
			m.mu.Lock()
			x.bin.Received++
			x.maxFCb = max(x.maxFCb, r.TxFCb)
			m.mu.Unlock()
		case r := <-x.lm:
			m.mu.Lock()
			x.bin.Received++
			if p := x.prevLM; p != nil {
				ft, nt, f, n := lossBetween(*p, r)
				x.bin.FarEndTx += ft
				x.bin.NearEndTx += nt
				x.bin.FarEndLoss += f
				x.bin.NearEndLoss += n
			}
			x.prevLM = &r
			m.mu.Unlock()
		case now := <-t.C:
			x.tick(now)
		}
	}
}

func (x *session) unregister() {
	m := x.m
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range x.dmKeys {
		delete(m.dmWait, k)
	}
	if m.slWait[x.test] == x.sl {
		delete(m.slWait, x.test)
	}
	if !x.dst.IsZero() && m.lmWait[x.dst] == x.lm {
		delete(m.lmWait, x.dst)
	}
}

// tick rolls the bin when due and sends the next PDU.
func (x *session) tick(now time.Time) {
	m := x.m
	if now.Sub(x.start) >= time.Duration(x.cfg.BinS)*time.Second {
		m.mu.Lock()
		x.closeBinLocked(now)
		x.newBinLocked(now)
		m.mu.Unlock()
	}
	dst, err := m.resolve(x.cfg.Target, false)
	m.mu.Lock()
	if err != nil {
		x.err = err.Error()
		m.mu.Unlock()
		return
	}
	x.err = ""
	if dst != x.dst {
		if !x.dst.IsZero() && m.lmWait[x.dst] == x.lm {
			delete(m.lmWait, x.dst)
		}
		x.dst, x.prevLM = dst, nil
	}
	var pdu []byte
	var op uint8
	switch x.cfg.Type {
	case "delay":
		pdu, op = encodeDMM(m.c().Level, now), OpDMM
		key := binary.BigEndian.Uint64(pdu[4:12])
		m.dmWait[key] = x.dm
		x.dmKeys[key] = now
		for k, at := range x.dmKeys { // unanswered
			if now.Sub(at) > 5*time.Second {
				delete(x.dmKeys, k)
				delete(m.dmWait, k)
			}
		}
	case "synthetic-loss":
		m.slWait[x.test] = x.sl
		x.bin.FarEndTx++ // SLMs sent in the bin
		pdu, op = (&slm{Level: m.c().Level, Opcode: OpSLM, SrcMEP: m.c().MEPID, TestID: x.test, TxFCf: x.bin.FarEndTx}).encode(), OpSLM
	case "loss":
		if w := m.lmWait[dst]; w != nil && w != x.lm {
			x.err = "another loss measurement toward the target is running"
			m.mu.Unlock()
			return
		}
		m.lmWait[dst] = x.lm
		m.mu.Unlock()
		tx, _, ok := m.counters()
		m.mu.Lock()
		if !ok {
			x.err = errNoCounters.Error()
			m.mu.Unlock()
			return
		}
		pdu, op = (&lm{Level: m.c().Level, Opcode: OpLMM, TxFCf: tx}).encode(), OpLMM
	}
	x.bin.Sent++
	m.mu.Unlock()
	if m.send(dst, pdu, m.prio(x.cfg.Target)) == nil {
		m.count(op)
	}
}

func (x *session) newBinLocked(now time.Time) {
	x.start, x.delays = now, nil
	x.bin = Bin{Start: now.Format(time.RFC3339)}
	x.binFCb = x.maxFCb
}

// summaryLocked fills in a bin's derived fields.
func (x *session) summaryLocked(b Bin) Bin {
	switch x.cfg.Type {
	case "delay":
		b.Delay = stats(x.delays)
		if len(x.delays) > 1 {
			var sum time.Duration
			for i := 1; i < len(x.delays); i++ {
				sum += max(x.delays[i]-x.delays[i-1], x.delays[i-1]-x.delays[i])
			}
			b.AvgFDVUS = us(sum) / float64(len(x.delays)-1)
		}
	case "synthetic-loss":
		// The responder's count tells how many of this bin's SLMs it got.
		farRx := int64(x.maxFCb - x.binFCb)
		b.FarEndLoss = max(int64(b.FarEndTx)-farRx, 0)
		b.NearEndTx = uint32(farRx)
		b.NearEndLoss = max(farRx-int64(b.Received), 0)
	}
	if b.FarEndTx > 0 {
		b.FarEndLossPct = 100 * float64(b.FarEndLoss) / float64(b.FarEndTx)
	}
	if b.NearEndTx > 0 {
		b.NearEndLossPct = 100 * float64(b.NearEndLoss) / float64(b.NearEndTx)
	}
	return b
}

func (x *session) closeBinLocked(now time.Time) {
	b := x.summaryLocked(x.bin)
	b.End = now.Format(time.RFC3339)
	x.history = append([]Bin{b}, x.history...)
	if len(x.history) > x.cfg.Bins {
		x.history = x.history[:x.cfg.Bins]
	}
}

func (x *session) statusLocked() SessionStatus {
	st := SessionStatus{Name: x.cfg.Name, Type: x.cfg.Type, Error: x.err, Current: x.summaryLocked(x.bin), History: append([]Bin{}, x.history...)}
	if !x.dst.IsZero() {
		st.Target = x.dst.String()
	}
	return st
}
