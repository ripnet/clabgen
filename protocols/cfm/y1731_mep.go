package cfm

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// The MEP's Y.1731 functions beyond CCM, LB, LT, DM and SL: AIS / LCK
// toward the client level, CSF toward the peers, 1DM and TST (both ends)
// and LMM / LMR over the device's data-frame counters.

// signalsLocked sends AIS, LCK and CSF when due. Caller holds mu.
func (m *mep) signalsLocked(now time.Time) (out []func()) {
	period := time.Second
	if m.cfg.AIS.Interval == "1m" {
		period = time.Minute
	}
	cl := m.cfg.clientLevel()
	prio := m.cfg.CCMPriority
	if m.cfg.AIS.Priority != nil {
		prio = *m.cfg.AIS.Priority
	}
	// AIS while we have a signal-fail defect (or receive AIS ourselves),
	// unless locked.
	sf := m.cur&(defRemoteCCM|defErrorCCM|defXconCCM) != 0 || now.Before(m.aisUntil)
	ais := !m.lock && (m.forceAIS || m.cfg.AIS.Enabled && sf && m.cfg.Level < 7)
	if ais != m.aisOn {
		m.aisOn = ais
		msg := fmt.Sprintf("sending AIS to level %d", cl)
		if !ais {
			msg = "stopped sending AIS"
		}
		ev := event{"ais-tx", msg, map[string]any{"client_level": float64(cl), "on": ais}}
		out = append(out, func() { m.emit([]event{ev}) })
	}
	send := func(op, flags uint8, level uint8, p uint8) {
		pdu := encodeSignal(level, op, flags)
		out = append(out, func() {
			if m.send(ccmGroup(level), pdu, p) == nil {
				m.count(op)
			}
		})
	}
	if ais && !now.Before(m.nextAIS) {
		send(OpAIS, periodCode(period), cl, prio)
		m.nextAIS = now.Add(period)
	}
	if m.lock && !now.Before(m.nextLCK) {
		send(OpLCK, periodCode(period), cl, prio)
		m.nextLCK = now.Add(period)
	}
	if m.csf != "" && !now.Before(m.nextCSF) {
		send(OpCSF, csfTypes[m.csf]<<3|periodCode(time.Second), m.cfg.Level, m.cfg.CCMPriority)
		m.nextCSF = now.Add(time.Second)
	}
	return out
}

// ---- receive side ----

// owd is one sender's one-way delay statistics (1DM).
type owd struct {
	n              int
	last, min, max time.Duration
	sum, fdvSum    time.Duration
	lastRx         time.Time
}

// tstRx is one sender's test signal statistics.
type tstRx struct {
	n, bytes, bitErrors, lost, outOfOrder int
	unchecked                             int
	nextSeq                               uint32
	lastRx                                time.Time
}

func (m *mep) rx1DM(f *pkt.Frame, b []byte) {
	if len(b) < 4+oneDMTLVOffset {
		return
	}
	tx := getTimestamp(b[4:])
	if tx.IsZero() {
		return
	}
	rx := f.RxTime
	if rx.IsZero() {
		rx = time.Now()
	}
	d := max(rx.Sub(tx), 0)
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.owds[f.Src]
	if s == nil {
		s = &owd{min: d}
		m.owds[f.Src] = s
	}
	if s.n > 0 {
		s.fdvSum += max(d-s.last, s.last-d)
	}
	s.n++
	s.last, s.sum, s.lastRx = d, s.sum+d, rx
	s.min, s.max = min(s.min, d), max(s.max, d)
}

func (m *mep) rxTST(h header, f *pkt.Frame) {
	t, err := parseTST(h, f.Payload)
	if err != nil {
		m.mu.Lock()
		m.rxErrors++
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.tsts[f.Src]
	if s == nil {
		s = &tstRx{nextSeq: t.Seq}
		m.tsts[f.Src] = s
	}
	switch {
	case t.Seq > s.nextSeq:
		s.lost += int(t.Seq - s.nextSeq)
	case t.Seq < s.nextSeq:
		s.outOfOrder++
		if s.lost > 0 {
			s.lost--
		}
	}
	if t.Seq >= s.nextSeq {
		s.nextSeq = t.Seq + 1
	}
	s.n++
	s.bytes += t.Bytes
	s.lastRx = time.Now()
	if !t.Checked {
		s.unchecked++
	} else if t.BitError {
		s.bitErrors++
	}
}

func (m *mep) rxCSF(h header, f *pkt.Frame) {
	now := time.Now()
	typ := h.Flags >> 3 & 7
	m.mu.Lock()
	started := !now.Before(m.csfUntil) || m.csfRxType != typ
	m.csfUntil, m.csfRxType, m.csfFrom = now.Add(periodOf(h.Flags)*7/2), typ, f.Src
	m.mu.Unlock()
	if started {
		m.env.Emit("csf-rx", fmt.Sprintf("CSF %s received from %s", csfName(typ), f.Src), map[string]any{"type": csfName(typ)})
	}
}

// counters returns the device's data-frame counters.
func (m *mep) counters() (tx, rx uint32, ok bool) {
	c, ok := m.env.Device().(protocol.FrameCounter)
	if !ok {
		return 0, 0, false
	}
	t, r := c.DataFrames()
	return uint32(t), uint32(r), true
}

// rxLMM answers with LMR: the frames we received (at reception) and sent.
func (m *mep) rxLMM(h header, f *pkt.Frame) {
	l, err := parseLM(h, f.Payload)
	if err != nil || !m.forMe(f.Dst, ccmGroup(m.cfg.Level)) {
		return
	}
	tx, rx, ok := m.counters()
	if !ok {
		return
	}
	r := &lm{Level: m.cfg.Level, Opcode: OpLMR, TxFCf: l.TxFCf, RxFCf: rx, TxFCb: tx}
	if m.send(f.Src, r.encode(), rxPCP(f, m.cfg.CCMPriority)) == nil {
		m.count(OpLMR)
	}
}

type lmReply struct {
	lm
	rxFCl uint32 // our received count when the LMR arrived
}

func (m *mep) rxLMR(h header, f *pkt.Frame) {
	l, err := parseLM(h, f.Payload)
	if err != nil {
		return
	}
	_, rx, _ := m.counters()
	m.mu.Lock()
	ch := m.lmWait[f.Src]
	m.mu.Unlock()
	if ch != nil {
		select {
		case ch <- lmReply{*l, rx}:
		default:
		}
	}
}

// ---- actions ----

type OneWayDelayResult struct {
	Target string `json:"target"`
	Sent   int    `json:"sent"`
}

func (m *mep) oneWayDelay(ctx context.Context, a *OneWayDelayArgs) (any, error) {
	dst, err := m.resolve(a.Target, false)
	if err != nil {
		return nil, err
	}
	res := &OneWayDelayResult{Target: dst.String()}
	iv := time.Duration(a.IntervalMS) * time.Millisecond
	for i := 0; i < a.Count; i++ {
		start := time.Now()
		if err := m.send(dst, encode1DM(m.cfg.Level, start), m.prio(a.Target)); err != nil {
			return res, fmt.Errorf("send 1DM: %w", err)
		}
		m.count(Op1DM)
		res.Sent++
		if i < a.Count-1 && !m.sleepCtx(ctx, iv-time.Since(start)) {
			break
		}
	}
	return res, ctx.Err()
}

type TestSignalResult struct {
	Target string `json:"target"`
	Sent   int    `json:"sent"`
	Bytes  int    `json:"pattern_bytes"`
}

func (m *mep) testSignal(ctx context.Context, a *TestSignalArgs) (any, error) {
	dst, err := m.resolve(a.Target, true)
	if err != nil {
		return nil, err
	}
	res := &TestSignalResult{Target: dst.String()}
	iv := time.Duration(a.IntervalMS) * time.Millisecond
	for i := 0; i < a.Count; i++ {
		start := time.Now()
		m.mu.Lock()
		seq := m.tstSeq
		m.tstSeq++
		m.mu.Unlock()
		if err := m.send(dst, encodeTST(m.cfg.Level, seq, a.Size, a.Pattern == "prbs", a.CRC), m.prio(a.Target)); err != nil {
			return res, fmt.Errorf("send TST: %w", err)
		}
		m.count(OpTST)
		res.Sent++
		res.Bytes += a.Size
		if i < a.Count-1 && !m.sleepCtx(ctx, iv-time.Since(start)) {
			break
		}
	}
	return res, ctx.Err()
}

type LossResult struct {
	Target         string  `json:"target"`
	Sent           int     `json:"sent"`
	Received       int     `json:"received"`
	FarEndTx       uint32  `json:"far_end_tx"`   // data frames we sent over the measurement
	FarEndLoss     int64   `json:"far_end_loss"` // of those, not received by the peer
	NearEndTx      uint32  `json:"near_end_tx"`  // data frames the peer sent
	NearEndLoss    int64   `json:"near_end_loss"`
	FarEndLossPct  float64 `json:"far_end_loss_pct"`
	NearEndLossPct float64 `json:"near_end_loss_pct"`
}

// lossBetween is the frame loss between two LMRs (Y.1731 8.1.1.1).
func lossBetween(p, c lmReply) (farTx, nearTx uint32, far, near int64) {
	farTx, nearTx = c.TxFCf-p.TxFCf, c.TxFCb-p.TxFCb
	far = int64(farTx) - int64(c.RxFCf-p.RxFCf)
	near = int64(nearTx) - int64(c.rxFCl-p.rxFCl)
	return
}

func (m *mep) loss(ctx context.Context, a *LossArgs) (any, error) {
	dst, err := m.resolve(a.Target, false)
	if err != nil {
		return nil, err
	}
	if _, _, ok := m.counters(); !ok {
		return nil, errNoCounters
	}
	ch := make(chan lmReply, 16)
	m.mu.Lock()
	if m.lmWait[dst] != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("a loss measurement toward %s is already running", dst)
	}
	m.lmWait[dst] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.lmWait, dst)
		m.mu.Unlock()
	}()
	res := &LossResult{Target: dst.String()}
	var first, last *lmReply
	iv := time.Duration(a.IntervalMS) * time.Millisecond
	timeout := time.Duration(a.TimeoutMS) * time.Millisecond
	for i := 0; i < a.Count; i++ {
		start := time.Now()
		tx, _, _ := m.counters()
		if err := m.send(dst, (&lm{Level: m.cfg.Level, Opcode: OpLMM, TxFCf: tx}).encode(), m.prio(a.Target)); err != nil {
			return res, fmt.Errorf("send LMM: %w", err)
		}
		m.count(OpLMM)
		res.Sent++
		t := time.NewTimer(timeout)
		select {
		case r := <-ch:
			res.Received++
			if first == nil {
				first = &r
			}
			last = &r
		case <-t.C:
		case <-ctx.Done():
		case <-m.stop:
		}
		t.Stop()
		if ctx.Err() != nil {
			break
		}
		if i < a.Count-1 && !m.sleepCtx(ctx, iv-time.Since(start)) {
			break
		}
	}
	if first != nil && last != first {
		res.FarEndTx, res.NearEndTx, res.FarEndLoss, res.NearEndLoss = lossBetween(*first, *last)
		if res.FarEndTx > 0 {
			res.FarEndLossPct = 100 * float64(res.FarEndLoss) / float64(res.FarEndTx)
		}
		if res.NearEndTx > 0 {
			res.NearEndLossPct = 100 * float64(res.NearEndLoss) / float64(res.NearEndTx)
		}
	}
	return res, ctx.Err()
}

// ---- status ----

type OWDStatus struct {
	From     string  `json:"from"`
	Count    int     `json:"count"`
	LastUS   float64 `json:"last_us"`
	MinUS    float64 `json:"min_us"`
	AvgUS    float64 `json:"avg_us"`
	MaxUS    float64 `json:"max_us"`
	AvgFDVUS float64 `json:"avg_fdv_us"`
	LastRx   string  `json:"last_rx"`
}

type TSTStatus struct {
	From       string `json:"from"`
	Received   int    `json:"received"`
	Bytes      int    `json:"pattern_bytes"`
	Lost       int    `json:"lost"`
	OutOfOrder int    `json:"out_of_order"`
	BitErrors  int    `json:"bit_errors"`
	Unchecked  int    `json:"unchecked,omitempty"` // PRBS without CRC
	LastRx     string `json:"last_rx"`
}

type CSFStatus struct {
	Type string `json:"type"`
	From string `json:"from"`
}

func (m *mep) y1731StatusLocked(s *Status, now time.Time) {
	s.AISSending, s.LCKSending, s.CSFSending = m.aisOn, m.lock, m.csf
	if now.Before(m.csfUntil) {
		s.CSFReceived = &CSFStatus{Type: csfName(m.csfRxType), From: m.csfFrom.String()}
	}
	s.OneWayDelay, s.TestSignal = []OWDStatus{}, []TSTStatus{}
	for mac, o := range m.owds {
		x := OWDStatus{From: mac.String(), Count: o.n, LastUS: us(o.last), MinUS: us(o.min), MaxUS: us(o.max), AvgUS: us(o.sum) / float64(o.n), LastRx: o.lastRx.Format(time.RFC3339)}
		if o.n > 1 {
			x.AvgFDVUS = us(o.fdvSum) / float64(o.n-1)
		}
		s.OneWayDelay = append(s.OneWayDelay, x)
	}
	sort.Slice(s.OneWayDelay, func(i, j int) bool { return s.OneWayDelay[i].From < s.OneWayDelay[j].From })
	for mac, t := range m.tsts {
		s.TestSignal = append(s.TestSignal, TSTStatus{From: mac.String(), Received: t.n, Bytes: t.bytes, Lost: t.lost, OutOfOrder: t.outOfOrder,
			BitErrors: t.bitErrors, Unchecked: t.unchecked, LastRx: t.lastRx.Format(time.RFC3339)})
	}
	sort.Slice(s.TestSignal, func(i, j int) bool { return s.TestSignal[i].From < s.TestSignal[j].From })
}
