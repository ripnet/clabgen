// Package cfm implements an IEEE 802.1Q CFM / ITU-T Y.1731 down-MEP:
// CCM with the remote-MEP and defect state machines, LBM/LBR, LTM/LTR,
// DMM/DMR, SLM/SLR, LMM/LMR, 1DM and TST (initiator and responder),
// AIS/LCK/CSF, proactive measurement sessions, plus fault injection.
package cfm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "cfm-mep"

func init() { protocol.Register(mepType{}) }

type mepType struct{}

func (mepType) Name() string { return TypeName }
func (mepType) Description() string {
	return "802.1ag/Y.1731 down MEP: CCM, loopback, linktrace, delay (DMM, 1DM), loss (SLM, LMM), test signal (TST), AIS/LCK/CSF, measurement sessions"
}
func (mepType) NewConfig() any       { return defaultConfig() }
func (mepType) EtherTypes() []uint16 { return []uint16{pkt.EtherTypeCFM} }
func (mepType) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "loopback", Description: "Send LBMs (to a MAC, a remote MEP, or multicast if no target) and report LBRs",
			NewArgs: func() any { return &LoopbackArgs{Count: 3, IntervalMS: 1000, TimeoutMS: 1000} }},
		{Name: "linktrace", Description: "Send an LTM and collect LTRs",
			NewArgs: func() any { return &LinktraceArgs{TTL: 64, TimeoutMS: 5000} }},
		{Name: "delay", Description: "Two-way delay measurement with DMM/DMR",
			NewArgs: func() any { return &DelayArgs{Count: 5, IntervalMS: 1000, TimeoutMS: 1000} }},
		{Name: "synthetic-loss", Description: "Synthetic loss measurement with SLM/SLR",
			NewArgs: func() any { return &SyntheticLossArgs{Count: 100, IntervalMS: 100, TimeoutMS: 1000} }},
		{Name: "one-way-delay", Description: "Send 1DMs; the receiving MEP computes one-way delay (clocks are shared in a lab)",
			NewArgs: func() any { return &OneWayDelayArgs{Count: 10, IntervalMS: 1000} }},
		{Name: "loss", Description: "Frame loss measurement with LMM/LMR over the devices' data-frame counters",
			NewArgs: func() any { return &LossArgs{Count: 10, IntervalMS: 1000, TimeoutMS: 1000} }},
		{Name: "test-signal", Description: "Send TST frames (sequence numbers, test pattern, optional CRC); the receiver counts loss and bit errors",
			NewArgs: func() any { return &TestSignalArgs{Count: 100, IntervalMS: 10, Size: 64} }},
		{Name: "fault", Description: "Inject faults into transmitted CCMs (stop CCM, RDI, status TLVs, sequence, MEPID/MAID/level) and send AIS, LCK or CSF",
			NewArgs: func() any { return &FaultArgs{} }},
		{Name: "clear", Description: "Clear counters and forget failed auto-discovered remote MEPs",
			NewArgs: func() any { return &ClearArgs{} }},
	}
}

const (
	rmepStart  = "start"
	rmepOK     = "ok"
	rmepFailed = "failed"
)

type rmep struct {
	id         uint16
	configured bool
	state      string
	since      time.Time
	mac        pkt.MAC
	lastRx     time.Time
	rdi        bool
	portStatus uint8
	intfStatus uint8
	interval   uint8
	senderID   string
	org        string
	padding    int
	ccmRx      uint64
	seqErrors  uint64
	lastSeq    uint32
	haveSeq    bool
}

type defects uint8

// In increasing priority order (802.1Q 20.1.2).
const (
	defRDICCM defects = 1 << iota
	defMACStatus
	defRemoteCCM
	defErrorCCM
	defXconCCM
)

var defectNames = []string{"DefRDICCM", "DefMACstatus", "DefRemoteCCM", "DefErrorCCM", "DefXconCCM"}

func (d defects) list() []string {
	out := []string{}
	for i, n := range defectNames {
		if d&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return out
}

func (d defects) highest() string {
	for i := len(defectNames) - 1; i >= 0; i-- {
		if d&(1<<i) != 0 {
			return defectNames[i]
		}
	}
	return "none"
}

type lbReply struct {
	from pkt.MAC
	rx   time.Time
}

type dmReply struct {
	txf, rxf, txb, rxb time.Time
}

type slKey struct {
	src  uint16
	test uint32
}

type event struct {
	kind, msg string
	data      map[string]any
}

type mep struct {
	env    protocol.Env
	cfg    Config
	maid   MAID
	ivCode uint8
	iv     time.Duration

	mu         sync.Mutex
	stopped    bool
	seq        uint32
	rmeps      map[uint16]*rmep
	xconUntil  time.Time
	xconReason string
	errUntil   time.Time
	errReason  string
	aisUntil   time.Time
	lckUntil   time.Time
	cur        defects
	txCnt      map[uint8]uint64
	rxCnt      map[uint8]uint64
	rxErrors   uint64

	// Fault injection state.
	ccmEnabled    bool
	forceRDI      bool
	portStatus    uint8
	intfStatus    uint8
	skipSeq       uint32
	mepIDOverride uint16
	maidOverride  *MAID
	maOverrideStr string
	levelOverride int

	transID uint32
	lbWait  map[uint32]chan lbReply
	ltWait  map[uint32]chan *ltr
	dmWait  map[uint64]chan dmReply
	slWait  map[uint32]chan *slm
	slrTx   map[slKey]uint32
	lmWait  map[pkt.MAC]chan lmReply

	// Y.1731 signals and receivers (y1731_mep.go).
	forceAIS, aisOn, lock     bool
	csf                       string
	nextAIS, nextLCK, nextCSF time.Time
	csfUntil                  time.Time
	csfRxType                 uint8
	csfFrom                   pkt.MAC
	owds                      map[pkt.MAC]*owd
	tsts                      map[pkt.MAC]*tstRx
	tstSeq                    uint32
	sessions                  []*session

	ccmTLVs    []byte // Sender ID, Organization-Specific, padding
	senderTLVs []byte // Sender ID for LBM / LTM

	stop chan struct{}
	done chan struct{}
}

func (mepType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	maid, err := encodeMAID(cfg.MDFormat, cfg.MDName, cfg.MAFormat, cfg.MAName)
	if err != nil {
		return nil, err
	}
	code, iv, _ := intervalByName(cfg.CCMInterval)
	now := time.Now()
	m := &mep{env: env, cfg: cfg, maid: maid, ivCode: code, iv: iv,
		seq: 1, rmeps: map[uint16]*rmep{}, txCnt: map[uint8]uint64{}, rxCnt: map[uint8]uint64{},
		ccmEnabled: cfg.CCMEnabled, portStatus: PortUp, intfStatus: 1, levelOverride: -1,
		transID: rand.Uint32(),
		lbWait:  map[uint32]chan lbReply{}, ltWait: map[uint32]chan *ltr{}, dmWait: map[uint64]chan dmReply{},
		slWait: map[uint32]chan *slm{}, slrTx: map[slKey]uint32{}, lmWait: map[pkt.MAC]chan lmReply{},
		owds: map[pkt.MAC]*owd{}, tsts: map[pkt.MAC]*tstRx{},
		stop: make(chan struct{}), done: make(chan struct{})}
	m.senderTLVs = senderIDTLV(cfg.SenderID)
	m.ccmTLVs = append([]byte{}, m.senderTLVs...)
	if cfg.OrgTLV != nil {
		o, _ := cfg.OrgTLV.encode()
		m.ccmTLVs = append(m.ccmTLVs, o...)
	}
	m.ccmTLVs = append(m.ccmTLVs, dataTLV(cfg.CCMPadding)...)
	for _, id := range cfg.RemoteMEPs {
		m.rmeps[id] = &rmep{id: id, configured: true, state: rmepStart, since: now}
	}
	go m.run()
	m.startSessions()
	return m, nil
}

func (m *mep) timeout() time.Duration { return m.iv * 7 / 2 }

func (m *mep) run() {
	defer close(m.done)
	// The first CCM goes out at a random point of the interval: MEPs
	// started together (a device group) would otherwise send in the same
	// instant forever, which remote MEPs at separate sites don't, and a
	// receiver's policer drops part of such bursts.
	first := time.NewTimer(rand.N(m.iv))
	defer first.Stop()
	var tx <-chan time.Time
	evalEvery := m.iv
	if evalEvery > 100*time.Millisecond {
		evalEvery = 100 * time.Millisecond
	}
	eval := time.NewTicker(evalEvery)
	defer eval.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-first.C:
			t := time.NewTicker(m.iv)
			defer t.Stop()
			tx = t.C
			m.sendCCM()
		case <-tx:
			m.sendCCM()
		case <-eval.C:
			m.evaluate()
		}
	}
}

func (m *mep) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	m.mu.Unlock()
	close(m.stop)
	<-m.done
}

func (m *mep) send(dst pkt.MAC, pdu []byte, pcp uint8) error {
	return m.env.Send(dst, pkt.EtherTypeCFM, pdu, protocol.SendOpts{SetPCP: true, PCP: pcp})
}

func (m *mep) count(op uint8) {
	m.mu.Lock()
	m.txCnt[op]++
	m.mu.Unlock()
}

func (m *mep) sendCCM() {
	m.mu.Lock()
	if !m.ccmEnabled {
		m.mu.Unlock()
		return
	}
	level := m.cfg.Level
	if m.levelOverride >= 0 {
		level = uint8(m.levelOverride)
	}
	c := &ccm{Level: level, Interval: m.ivCode, Seq: m.seq, MEPID: m.cfg.MEPID, MAID: m.maid,
		RDI: m.presentRDI()}
	m.seq += 1 + m.skipSeq
	m.skipSeq = 0
	if m.mepIDOverride != 0 {
		c.MEPID = m.mepIDOverride
	}
	if m.maidOverride != nil {
		c.MAID = *m.maidOverride
	}
	if m.cfg.PortStatusTLV {
		c.PortStatus = m.portStatus
	}
	if m.cfg.InterfaceStatusTLV {
		c.IntfStatus = m.intfStatus
	}
	m.txCnt[OpCCM]++
	m.mu.Unlock()
	if err := m.send(ccmGroup(level), withTLVs(c.encode(), m.ccmTLVs), m.cfg.CCMPriority); err != nil {
		m.mu.Lock()
		m.txCnt[OpCCM]--
		m.mu.Unlock()
	}
}

// presentRDI per 802.1Q 20.9.6. Caller holds mu.
func (m *mep) presentRDI() bool {
	return m.forceRDI || m.cur&(defMACStatus|defRemoteCCM|defErrorCCM|defXconCCM) != 0
}

// evaluate runs the remote MEP timers and recomputes defects, emitting
// events for any change.
func (m *mep) evaluate() {
	now := time.Now()
	m.mu.Lock()
	evs := m.evaluateLocked(now)
	sends := m.signalsLocked(now)
	m.mu.Unlock()
	m.emit(evs)
	for _, f := range sends {
		f()
	}
}

func (m *mep) evaluateLocked(now time.Time) []event {
	var evs []event
	to := m.timeout()
	var d defects
	for _, r := range m.rmeps {
		ref := r.lastRx
		if r.state == rmepStart {
			ref = r.since
		}
		if r.state != rmepFailed && now.Sub(ref) > to {
			r.state, r.since = rmepFailed, now
			evs = append(evs, event{"rmep-down", fmt.Sprintf("remote MEP %d failed (no CCM for %s)", r.id, to.Round(time.Millisecond)),
				map[string]any{"rmep": r.id}})
		}
		switch r.state {
		case rmepFailed:
			d |= defRemoteCCM
		case rmepOK:
			if r.rdi {
				d |= defRDICCM
			}
			if (r.portStatus != 0 && r.portStatus != PortUp) || (r.intfStatus != 0 && r.intfStatus != 1) {
				d |= defMACStatus
			}
		}
	}
	if now.Before(m.errUntil) {
		d |= defErrorCCM
	}
	if now.Before(m.xconUntil) {
		d |= defXconCCM
	}
	if d != m.cur {
		old := m.cur
		m.cur = d
		msg := "defects: " + strings.Join(d.list(), ",")
		if d == 0 {
			msg = "defects cleared"
		}
		evs = append(evs, event{"defects", msg, map[string]any{
			"defects": toAny(d.list()), "highest": d.highest(), "previous": toAny(old.list()),
			"error_reason": m.errReason, "xcon_reason": m.xconReason}})
	}
	return evs
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func (m *mep) emit(evs []event) {
	for _, e := range evs {
		m.env.Emit(e.kind, e.msg, e.data)
	}
}

func (m *mep) setError(now time.Time, reason string) {
	m.errUntil, m.errReason = now.Add(m.timeout()), reason
}

func (m *mep) setXcon(now time.Time, reason string) {
	m.xconUntil, m.xconReason = now.Add(m.timeout()), reason
}

func (m *mep) siblingAtLevel(level uint8) bool {
	for _, s := range m.env.Siblings() {
		if o, ok := s.(*mep); ok && o.cfg.Level == level {
			return true
		}
	}
	return false
}

// ---- receive ----

func (m *mep) HandleRx(f *pkt.Frame) {
	h, err := parseHeader(f.Payload)
	if err != nil {
		m.mu.Lock()
		m.rxErrors++
		m.mu.Unlock()
		return
	}
	if h.Level > m.cfg.Level {
		return // passes transparently through a MEP of a lower level
	}
	if h.Level < m.cfg.Level {
		if h.Opcode == OpCCM && !m.siblingAtLevel(h.Level) {
			m.mu.Lock()
			m.rxCnt[OpCCM]++
			m.setXcon(time.Now(), fmt.Sprintf("CCM at lower level %d from %s", h.Level, f.Src))
			evs := m.evaluateLocked(time.Now())
			m.mu.Unlock()
			m.emit(evs)
		}
		return
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.rxCnt[h.Opcode]++
	m.mu.Unlock()

	b := f.Payload
	switch h.Opcode {
	case OpCCM:
		m.rxCCM(h, f)
	case OpLBM:
		if m.cfg.DisableLBR || !m.forMe(f.Dst, ccmGroup(m.cfg.Level)) {
			return
		}
		r := append([]byte(nil), b...)
		r[1] = OpLBR
		if m.send(f.Src, r, rxPCP(f, m.cfg.CCMPriority)) == nil {
			m.count(OpLBR)
		}
	case OpLBR:
		if id, ok := transID(h, b); ok {
			m.mu.Lock()
			ch := m.lbWait[id]
			m.mu.Unlock()
			if ch != nil {
				select {
				case ch <- lbReply{f.Src, f.RxTime}:
				default:
				}
			}
		}
	case OpLTM:
		l, err := parseLTM(h, b)
		if err != nil || m.cfg.DisableLTR || l.Target != m.env.Device().MAC() || l.TTL == 0 {
			return
		}
		mac := m.env.Device().MAC()
		r := &ltr{Level: m.cfg.Level, Flags: l.Flags&flagUseFDBOnly | flagTerminalMEP, TransID: l.TransID,
			ReplyTTL: l.TTL - 1, RelayAction: RlyHit, LastEgress: l.EgressID,
			NextEgress: append([]byte{0, 0}, mac[:]...), IngressAct: 1, IngressMAC: mac}
		if m.send(l.Orig, r.encode(), rxPCP(f, m.cfg.CCMPriority)) == nil {
			m.count(OpLTR)
		}
	case OpLTR:
		l, err := parseLTR(h, b)
		if err != nil {
			return
		}
		m.mu.Lock()
		ch := m.ltWait[l.TransID]
		m.mu.Unlock()
		if ch != nil {
			select {
			case ch <- l:
			default:
			}
		}
	case OpDMM:
		if m.cfg.DisableDMR || len(b) < 4+dmTLVOffset || !m.forMe(f.Dst, ccmGroup(m.cfg.Level)) {
			return
		}
		r := append([]byte(nil), b...)
		r[1] = OpDMR
		putTimestamp(r[12:], f.RxTime)
		putTimestamp(r[20:], time.Now())
		if m.send(f.Src, r, rxPCP(f, m.cfg.CCMPriority)) == nil {
			m.count(OpDMR)
		}
	case OpDMR:
		if len(b) < 4+dmTLVOffset {
			return
		}
		key := binary.BigEndian.Uint64(b[4:12])
		m.mu.Lock()
		ch := m.dmWait[key]
		m.mu.Unlock()
		if ch != nil {
			select {
			case ch <- dmReply{getTimestamp(b[4:]), getTimestamp(b[12:]), getTimestamp(b[20:]), f.RxTime}:
			default:
			}
		}
	case OpSLM:
		s, err := parseSL(h, b)
		if err != nil || m.cfg.DisableSLR || !m.forMe(f.Dst, ccmGroup(m.cfg.Level)) {
			return
		}
		m.mu.Lock()
		k := slKey{s.SrcMEP, s.TestID}
		m.slrTx[k]++
		s.TxFCb = m.slrTx[k]
		m.mu.Unlock()
		s.Opcode, s.RespMEP, s.Level = OpSLR, m.cfg.MEPID, m.cfg.Level
		if m.send(f.Src, s.encode(), rxPCP(f, m.cfg.CCMPriority)) == nil {
			m.count(OpSLR)
		}
	case OpSLR:
		s, err := parseSL(h, b)
		if err != nil {
			return
		}
		m.mu.Lock()
		ch := m.slWait[s.TestID]
		m.mu.Unlock()
		if ch != nil {
			select {
			case ch <- s:
			default:
			}
		}
	case Op1DM:
		if m.forMe(f.Dst, ccmGroup(m.cfg.Level)) {
			m.rx1DM(f, b)
		}
	case OpTST:
		if m.forMe(f.Dst, ccmGroup(m.cfg.Level)) {
			m.rxTST(h, f)
		}
	case OpCSF:
		if m.forMe(f.Dst, ccmGroup(m.cfg.Level)) {
			m.rxCSF(h, f)
		}
	case OpLMM:
		m.rxLMM(h, f)
	case OpLMR:
		m.rxLMR(h, f)
	case OpAIS, OpLCK:
		period := time.Second
		if h.Flags&7 == 6 {
			period = time.Minute
		}
		now := time.Now()
		m.mu.Lock()
		var started bool
		if h.Opcode == OpAIS {
			started = !now.Before(m.aisUntil)
			m.aisUntil = now.Add(period * 7 / 2)
		} else {
			started = !now.Before(m.lckUntil)
			m.lckUntil = now.Add(period * 7 / 2)
		}
		m.mu.Unlock()
		if started {
			m.env.Emit(strings.ToLower(OpName(h.Opcode))+"-rx", OpName(h.Opcode)+" received from "+f.Src.String(), nil)
		}
	}
}

func (m *mep) forMe(dst, group pkt.MAC) bool {
	return dst == m.env.Device().MAC() || dst == group
}

func rxPCP(f *pkt.Frame, def uint8) uint8 {
	if len(f.Tags) > 0 {
		return f.Tags[0].PCP
	}
	return def
}

func (m *mep) rxCCM(h header, f *pkt.Frame) {
	c, err := parseCCM(h, f.Payload)
	if err != nil {
		m.mu.Lock()
		m.rxErrors++
		m.mu.Unlock()
		return
	}
	now := time.Now()
	var evs []event
	m.mu.Lock()
	defer func() {
		evs = append(evs, m.evaluateLocked(now)...)
		m.mu.Unlock()
		m.emit(evs)
	}()
	if c.MAID != m.maid {
		m.setXcon(now, fmt.Sprintf("MAID mismatch from %s: %s", f.Src, c.MAID))
		return
	}
	if c.MEPID == m.cfg.MEPID {
		m.setError(now, fmt.Sprintf("CCM with our own MEPID %d from %s", c.MEPID, f.Src))
		return
	}
	r := m.rmeps[c.MEPID]
	if r == nil {
		if len(m.cfg.RemoteMEPs) > 0 {
			m.setError(now, fmt.Sprintf("unexpected MEPID %d from %s", c.MEPID, f.Src))
			return
		}
		r = &rmep{id: c.MEPID, state: rmepStart, since: now}
		m.rmeps[c.MEPID] = r
		evs = append(evs, event{"rmep-discovered", fmt.Sprintf("discovered remote MEP %d (%s)", c.MEPID, f.Src),
			map[string]any{"rmep": c.MEPID, "mac": f.Src.String()}})
	}
	if c.Interval != m.ivCode {
		m.setError(now, fmt.Sprintf("interval mismatch from MEP %d: %s, expected %s", c.MEPID, intervalName(c.Interval), m.cfg.CCMInterval))
	}
	if r.haveSeq && c.Seq != 0 && c.Seq != r.lastSeq+1 {
		r.seqErrors++
	}
	r.lastSeq, r.haveSeq = c.Seq, true
	r.mac, r.lastRx, r.rdi, r.portStatus, r.intfStatus, r.interval = f.Src, now, c.RDI, c.PortStatus, c.IntfStatus, c.Interval
	r.senderID, r.org, r.padding = c.SenderID, c.Org, c.Padding
	r.ccmRx++
	if r.state != rmepOK {
		r.state, r.since = rmepOK, now
		evs = append(evs, event{"rmep-up", fmt.Sprintf("remote MEP %d up (%s)", r.id, f.Src),
			map[string]any{"rmep": r.id, "mac": f.Src.String()}})
	}
}

// ---- status ----

type RMEPStatus struct {
	MEPID           uint16    `json:"mep_id"`
	State           string    `json:"state"`
	Configured      bool      `json:"configured"`
	MAC             string    `json:"mac,omitempty"`
	RDI             bool      `json:"rdi"`
	PortStatus      string    `json:"port_status,omitempty"`
	InterfaceStatus string    `json:"interface_status,omitempty"`
	Interval        string    `json:"ccm_interval,omitempty"`
	CCMRx           uint64    `json:"ccm_rx"`
	SeqErrors       uint64    `json:"seq_errors"`
	LastRx          time.Time `json:"last_rx,omitempty"`
	StateSince      time.Time `json:"state_since"`
	SenderID        string    `json:"sender_id,omitempty"` // chassis ID from its Sender ID TLV
	OrgTLV          string    `json:"org_tlv,omitempty"`
	Padding         int       `json:"ccm_padding,omitempty"` // Data TLV bytes in its CCMs
}

type Status struct {
	MAC           string            `json:"mac"`
	Level         uint8             `json:"level"`
	MEPID         uint16            `json:"mep_id"`
	MAID          string            `json:"maid"`
	CCMInterval   string            `json:"ccm_interval"`
	CCMEnabled    bool              `json:"ccm_enabled"`
	PresentRDI    bool              `json:"present_rdi"`
	Defects       []string          `json:"defects"`
	HighestDefect string            `json:"highest_defect"`
	ErrorReason   string            `json:"error_reason,omitempty"`
	XconReason    string            `json:"xcon_reason,omitempty"`
	AISReceived   bool              `json:"ais_received"`
	LCKReceived   bool              `json:"lck_received"`
	RemoteMEPs    []RMEPStatus      `json:"remote_meps"`
	Tx            map[string]uint64 `json:"tx"`
	Rx            map[string]uint64 `json:"rx"`
	RxErrors      uint64            `json:"rx_errors"`
	Faults        map[string]any    `json:"faults,omitempty"`

	AISSending  bool            `json:"ais_sending"`
	LCKSending  bool            `json:"lck_sending"`
	CSFSending  string          `json:"csf_sending,omitempty"`
	CSFReceived *CSFStatus      `json:"csf_received,omitempty"`
	OneWayDelay []OWDStatus     `json:"one_way_delay"` // 1DMs received, per sender
	TestSignal  []TSTStatus     `json:"test_signal"`   // TSTs received, per sender
	Sessions    []SessionStatus `json:"sessions,omitempty"`
}

func (m *mep) Status() any {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{MAC: m.env.Device().MAC().String(), Level: m.cfg.Level, MEPID: m.cfg.MEPID, MAID: m.maid.String(),
		CCMInterval: m.cfg.CCMInterval, CCMEnabled: m.ccmEnabled, PresentRDI: m.presentRDI(),
		Defects: m.cur.list(), HighestDefect: m.cur.highest(),
		AISReceived: now.Before(m.aisUntil), LCKReceived: now.Before(m.lckUntil),
		Tx: map[string]uint64{}, Rx: map[string]uint64{}, RxErrors: m.rxErrors, RemoteMEPs: []RMEPStatus{}}
	if m.cur&defErrorCCM != 0 {
		s.ErrorReason = m.errReason
	}
	if m.cur&defXconCCM != 0 {
		s.XconReason = m.xconReason
	}
	for op, n := range m.txCnt {
		s.Tx[OpName(op)] = n
	}
	for op, n := range m.rxCnt {
		s.Rx[OpName(op)] = n
	}
	for _, r := range m.rmeps {
		rs := RMEPStatus{MEPID: r.id, State: r.state, Configured: r.configured, RDI: r.rdi,
			PortStatus: portStatusNames[r.portStatus], InterfaceStatus: intfStatusNames[r.intfStatus],
			CCMRx: r.ccmRx, SeqErrors: r.seqErrors, LastRx: r.lastRx, StateSince: r.since, SenderID: r.senderID, OrgTLV: r.org, Padding: r.padding}
		if !r.mac.IsZero() {
			rs.MAC = r.mac.String()
		}
		if r.interval != 0 {
			rs.Interval = intervalName(r.interval)
		}
		s.RemoteMEPs = append(s.RemoteMEPs, rs)
	}
	sort.Slice(s.RemoteMEPs, func(i, j int) bool { return s.RemoteMEPs[i].MEPID < s.RemoteMEPs[j].MEPID })
	f := map[string]any{}
	if m.forceRDI {
		f["force_rdi"] = true
	}
	if m.mepIDOverride != 0 {
		f["mep_id_override"] = m.mepIDOverride
	}
	if m.maidOverride != nil {
		f["ma_name_override"] = m.maOverrideStr
	}
	if m.levelOverride >= 0 {
		f["level_override"] = m.levelOverride
	}
	if m.portStatus != PortUp {
		f["port_status"] = portStatusNames[m.portStatus]
	}
	if m.intfStatus != 1 {
		f["interface_status"] = intfStatusNames[m.intfStatus]
	}
	if m.forceAIS {
		f["force_ais"] = true
	}
	if m.lock {
		f["lock"] = true
	}
	if m.csf != "" {
		f["csf"] = m.csf
	}
	if len(f) > 0 {
		s.Faults = f
	}
	m.y1731StatusLocked(&s, now)
	for _, x := range m.sessions {
		s.Sessions = append(s.Sessions, x.statusLocked())
	}
	return s
}

// ---- actions ----

func (m *mep) Action(ctx context.Context, name string, args any) (any, error) {
	switch name {
	case "loopback":
		return m.loopback(ctx, args.(*LoopbackArgs))
	case "linktrace":
		return m.linktrace(ctx, args.(*LinktraceArgs))
	case "delay":
		return m.delay(ctx, args.(*DelayArgs))
	case "synthetic-loss":
		return m.syntheticLoss(ctx, args.(*SyntheticLossArgs))
	case "one-way-delay":
		return m.oneWayDelay(ctx, args.(*OneWayDelayArgs))
	case "loss":
		return m.loss(ctx, args.(*LossArgs))
	case "test-signal":
		return m.testSignal(ctx, args.(*TestSignalArgs))
	case "fault":
		return m.fault(args.(*FaultArgs))
	case "clear":
		m.clear()
		return map[string]any{"cleared": true}, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

func (m *mep) resolve(t Target, allowGroup bool) (pkt.MAC, error) {
	switch {
	case t.TargetMAC != "":
		return pkt.ParseMAC(t.TargetMAC)
	case t.TargetMEPID != 0:
		m.mu.Lock()
		defer m.mu.Unlock()
		r := m.rmeps[t.TargetMEPID]
		if r == nil || r.mac.IsZero() {
			return pkt.MAC{}, fmt.Errorf("remote MEP %d has no known MAC (no CCM received yet)", t.TargetMEPID)
		}
		return r.mac, nil
	case allowGroup:
		return ccmGroup(m.cfg.Level), nil
	}
	return pkt.MAC{}, errors.New("target_mac or target_mep_id is required")
}

func (m *mep) prio(t Target) uint8 {
	if t.Priority != nil {
		return *t.Priority
	}
	return m.cfg.CCMPriority
}

func (m *mep) nextTransID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transID++
	return m.transID
}

// sleepCtx waits d or until ctx/stop ends, returning false if interrupted.
func (m *mep) sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
	case <-m.stop:
	}
	return false
}

type rttStats struct {
	MinUS float64 `json:"min_us"`
	AvgUS float64 `json:"avg_us"`
	MaxUS float64 `json:"max_us"`
}

func stats(samples []time.Duration) *rttStats {
	if len(samples) == 0 {
		return nil
	}
	s := &rttStats{MinUS: us(samples[0]), MaxUS: us(samples[0])}
	var sum float64
	for _, d := range samples {
		v := us(d)
		sum += v
		if v < s.MinUS {
			s.MinUS = v
		}
		if v > s.MaxUS {
			s.MaxUS = v
		}
	}
	s.AvgUS = sum / float64(len(samples))
	return s
}

func us(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1000 }

type LBReplyResult struct {
	Seq     int     `json:"seq"`
	TransID uint32  `json:"trans_id"`
	From    string  `json:"from"`
	RTTUS   float64 `json:"rtt_us"`
}

type LoopbackResult struct {
	Target     string          `json:"target"`
	Sent       int             `json:"sent"`
	Received   int             `json:"received"`
	LossPct    float64         `json:"loss_pct"`
	RTT        *rttStats       `json:"rtt,omitempty"`
	Responders []string        `json:"responders,omitempty"`
	Replies    []LBReplyResult `json:"replies"`
}

func (m *mep) loopback(ctx context.Context, a *LoopbackArgs) (any, error) {
	dst, err := m.resolve(a.Target, true)
	if err != nil {
		return nil, err
	}
	multicast := dst.IsMulticast()
	res := &LoopbackResult{Target: dst.String(), Replies: []LBReplyResult{}}
	var rtts []time.Duration
	responders := map[pkt.MAC]bool{}
	answered := 0
	timeout := time.Duration(a.TimeoutMS) * time.Millisecond
	interval := time.Duration(a.IntervalMS) * time.Millisecond
	for i := 1; i <= a.Count; i++ {
		id := m.nextTransID()
		ch := make(chan lbReply, 64)
		m.mu.Lock()
		m.lbWait[id] = ch
		m.mu.Unlock()
		start := time.Now()
		if err := m.send(dst, withTLVs(encodeLBM(m.cfg.Level, id, a.DataSize), m.senderTLVs), m.prio(a.Target)); err != nil {
			m.mu.Lock()
			delete(m.lbWait, id)
			m.mu.Unlock()
			return res, fmt.Errorf("send LBM: %w", err)
		}
		m.count(OpLBM)
		res.Sent++
		got := false
		deadline := time.NewTimer(timeout)
	wait:
		for {
			select {
			case r := <-ch:
				rtt := r.rx.Sub(start)
				if rtt < 0 {
					rtt = 0
				}
				res.Replies = append(res.Replies, LBReplyResult{i, id, r.from.String(), us(rtt)})
				responders[r.from] = true
				rtts = append(rtts, rtt)
				got = true
				if !multicast {
					break wait
				}
			case <-deadline.C:
				break wait
			case <-ctx.Done():
				break wait
			case <-m.stop:
				break wait
			}
		}
		deadline.Stop()
		m.mu.Lock()
		delete(m.lbWait, id)
		m.mu.Unlock()
		if got {
			answered++
		}
		if ctx.Err() != nil {
			break
		}
		if i < a.Count && !m.sleepCtx(ctx, interval-time.Since(start)) {
			break
		}
	}
	res.Received = answered
	if res.Sent > 0 {
		res.LossPct = 100 * float64(res.Sent-answered) / float64(res.Sent)
	}
	res.RTT = stats(rtts)
	for mac := range responders {
		res.Responders = append(res.Responders, mac.String())
	}
	sort.Strings(res.Responders)
	return res, ctx.Err()
}

type LTHop struct {
	Hop         int    `json:"hop"`
	ReplyTTL    uint8  `json:"reply_ttl"`
	RelayAction string `json:"relay_action"`
	FwdYes      bool   `json:"fwd_yes"`
	TerminalMEP bool   `json:"terminal_mep"`
	IngressMAC  string `json:"ingress_mac,omitempty"`
	EgressMAC   string `json:"egress_mac,omitempty"`
	LastEgress  string `json:"last_egress_id,omitempty"`
	NextEgress  string `json:"next_egress_id,omitempty"`
}

type LinktraceResult struct {
	Target  string  `json:"target"`
	TransID uint32  `json:"trans_id"`
	Reached bool    `json:"reached"`
	Hops    []LTHop `json:"hops"`
}

var relayNames = map[uint8]string{RlyHit: "RlyHit", RlyFDB: "RlyFDB", RlyMPDB: "RlyMPDB"}

func egressID(b []byte) string {
	if len(b) != 8 {
		return ""
	}
	return fmt.Sprintf("%d-%s", binary.BigEndian.Uint16(b), pkt.MAC(b[2:8]))
}

func (m *mep) linktrace(ctx context.Context, a *LinktraceArgs) (any, error) {
	dst, err := m.resolve(a.Target, false)
	if err != nil {
		return nil, err
	}
	mac := m.env.Device().MAC()
	id := m.nextTransID()
	ch := make(chan *ltr, 64)
	m.mu.Lock()
	m.ltWait[id] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.ltWait, id)
		m.mu.Unlock()
	}()
	l := &ltm{Level: m.cfg.Level, Flags: flagUseFDBOnly, TransID: id, TTL: a.TTL, Orig: mac, Target: dst,
		EgressID: append([]byte{0, 0}, mac[:]...)}
	if err := m.send(ltmGroup(m.cfg.Level), withTLVs(l.encode(), m.senderTLVs), m.prio(a.Target)); err != nil {
		return nil, fmt.Errorf("send LTM: %w", err)
	}
	m.count(OpLTM)
	res := &LinktraceResult{Target: dst.String(), TransID: id, Hops: []LTHop{}}
	deadline := time.NewTimer(time.Duration(a.TimeoutMS) * time.Millisecond)
	defer deadline.Stop()
collect:
	for {
		select {
		case r := <-ch:
			h := LTHop{Hop: int(a.TTL) - int(r.ReplyTTL), ReplyTTL: r.ReplyTTL, RelayAction: relayNames[r.RelayAction],
				FwdYes: r.Flags&flagFwdYes != 0, TerminalMEP: r.Flags&flagTerminalMEP != 0,
				LastEgress: egressID(r.LastEgress), NextEgress: egressID(r.NextEgress)}
			if h.RelayAction == "" {
				h.RelayAction = fmt.Sprintf("%d", r.RelayAction)
			}
			if r.IngressAct != 0 {
				h.IngressMAC = r.IngressMAC.String()
			}
			if r.EgressAct != 0 {
				h.EgressMAC = r.EgressMAC.String()
			}
			if r.RelayAction == RlyHit {
				res.Reached = true
			}
			res.Hops = append(res.Hops, h)
		case <-deadline.C:
			break collect
		case <-ctx.Done():
			break collect
		case <-m.stop:
			break collect
		}
	}
	sort.SliceStable(res.Hops, func(i, j int) bool { return res.Hops[i].Hop < res.Hops[j].Hop })
	return res, ctx.Err()
}

type DelaySample struct {
	Seq     int     `json:"seq"`
	DelayUS float64 `json:"delay_us"`
}

type DelayResult struct {
	Target   string        `json:"target"`
	Sent     int           `json:"sent"`
	Received int           `json:"received"`
	Delay    *rttStats     `json:"delay,omitempty"`
	AvgFDVUS float64       `json:"avg_fdv_us"`
	Samples  []DelaySample `json:"samples"`
}

func (m *mep) delay(ctx context.Context, a *DelayArgs) (any, error) {
	dst, err := m.resolve(a.Target, false)
	if err != nil {
		return nil, err
	}
	res := &DelayResult{Target: dst.String(), Samples: []DelaySample{}}
	var ds []time.Duration
	timeout := time.Duration(a.TimeoutMS) * time.Millisecond
	interval := time.Duration(a.IntervalMS) * time.Millisecond
	for i := 1; i <= a.Count; i++ {
		start := time.Now()
		pdu := encodeDMM(m.cfg.Level, start)
		key := binary.BigEndian.Uint64(pdu[4:12])
		ch := make(chan dmReply, 1)
		m.mu.Lock()
		m.dmWait[key] = ch
		m.mu.Unlock()
		err := m.send(dst, pdu, m.prio(a.Target))
		if err == nil {
			m.count(OpDMM)
			res.Sent++
			t := time.NewTimer(timeout)
			select {
			case r := <-ch:
				d := r.rxb.Sub(r.txf)
				if !r.txb.IsZero() && !r.rxf.IsZero() {
					d -= r.txb.Sub(r.rxf)
				}
				if d < 0 {
					d = 0
				}
				ds = append(ds, d)
				res.Samples = append(res.Samples, DelaySample{i, us(d)})
			case <-t.C:
			case <-ctx.Done():
			case <-m.stop:
			}
			t.Stop()
		}
		m.mu.Lock()
		delete(m.dmWait, key)
		m.mu.Unlock()
		if err != nil {
			return res, fmt.Errorf("send DMM: %w", err)
		}
		if ctx.Err() != nil {
			break
		}
		if i < a.Count && !m.sleepCtx(ctx, interval-time.Since(start)) {
			break
		}
	}
	res.Received = len(ds)
	res.Delay = stats(ds)
	if len(ds) > 1 {
		var sum float64
		for i := 1; i < len(ds); i++ {
			d := ds[i] - ds[i-1]
			if d < 0 {
				d = -d
			}
			sum += us(d)
		}
		res.AvgFDVUS = sum / float64(len(ds)-1)
	}
	return res, ctx.Err()
}

type SyntheticLossResult struct {
	Target         string  `json:"target"`
	TestID         uint32  `json:"test_id"`
	Sent           int     `json:"sent"`
	Received       int     `json:"received"`
	FarEndRx       uint32  `json:"far_end_rx"`
	FarEndLoss     int     `json:"far_end_loss"`
	NearEndLoss    int     `json:"near_end_loss"`
	FarEndLossPct  float64 `json:"far_end_loss_pct"`
	NearEndLossPct float64 `json:"near_end_loss_pct"`
	ResponderMEPID uint16  `json:"responder_mep_id,omitempty"`
}

func (m *mep) syntheticLoss(ctx context.Context, a *SyntheticLossArgs) (any, error) {
	dst, err := m.resolve(a.Target, false)
	if err != nil {
		return nil, err
	}
	test := a.TestID
	if test == 0 {
		test = rand.Uint32()
	}
	ch := make(chan *slm, 1024)
	m.mu.Lock()
	if _, busy := m.slWait[test]; busy {
		m.mu.Unlock()
		return nil, fmt.Errorf("test_id %d already running", test)
	}
	m.slWait[test] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.slWait, test)
		m.mu.Unlock()
	}()
	res := &SyntheticLossResult{Target: dst.String(), TestID: test}
	var minFCb, maxFCb uint32
	collect := func(s *slm) {
		res.Received++
		res.ResponderMEPID = s.RespMEP
		if minFCb == 0 || s.TxFCb < minFCb {
			minFCb = s.TxFCb
		}
		if s.TxFCb > maxFCb {
			maxFCb = s.TxFCb
		}
	}
	interval := time.Duration(a.IntervalMS) * time.Millisecond
	tick := time.NewTicker(interval)
	defer tick.Stop()
send:
	for i := 1; i <= a.Count; i++ {
		p := &slm{Level: m.cfg.Level, Opcode: OpSLM, SrcMEP: m.cfg.MEPID, TestID: test, TxFCf: uint32(i)}
		if err := m.send(dst, p.encode(), m.prio(a.Target)); err != nil {
			return res, fmt.Errorf("send SLM: %w", err)
		}
		m.count(OpSLM)
		res.Sent++
		if i == a.Count {
			break
		}
		for {
			select {
			case s := <-ch:
				collect(s)
				continue
			case <-tick.C:
			case <-ctx.Done():
				break send
			case <-m.stop:
				break send
			}
			break
		}
	}
	deadline := time.NewTimer(time.Duration(a.TimeoutMS) * time.Millisecond)
	defer deadline.Stop()
drain:
	for {
		select {
		case s := <-ch:
			collect(s)
		case <-deadline.C:
			break drain
		case <-ctx.Done():
			break drain
		}
	}
	// The responder's TxFCb counts SLMs it received for this test, so the
	// highest value seen is the far-end receive count.
	res.FarEndRx = maxFCb
	res.FarEndLoss = res.Sent - int(maxFCb)
	res.NearEndLoss = int(maxFCb) - res.Received
	if res.Sent > 0 {
		res.FarEndLossPct = 100 * float64(res.FarEndLoss) / float64(res.Sent)
	}
	if maxFCb > 0 {
		res.NearEndLossPct = 100 * float64(res.NearEndLoss) / float64(maxFCb)
	}
	return res, ctx.Err()
}

func (m *mep) fault(a *FaultArgs) (any, error) {
	var override *MAID
	if a.MAIDOverride != nil && *a.MAIDOverride != "" {
		v, err := encodeMAID(m.cfg.MDFormat, m.cfg.MDName, m.cfg.MAFormat, *a.MAIDOverride)
		if err != nil {
			return nil, err
		}
		override = &v
	}
	m.mu.Lock()
	if a.CCMEnabled != nil {
		m.ccmEnabled = *a.CCMEnabled
	}
	if a.ForceRDI != nil {
		m.forceRDI = *a.ForceRDI
	}
	if a.PortStatus != nil {
		m.portStatus, _ = lookupName(portStatusNames, *a.PortStatus)
	}
	if a.InterfaceStatus != nil {
		m.intfStatus, _ = lookupName(intfStatusNames, *a.InterfaceStatus)
	}
	m.skipSeq += a.SkipSequence
	if a.MEPIDOverride != nil {
		m.mepIDOverride = *a.MEPIDOverride
	}
	if a.MAIDOverride != nil {
		m.maidOverride, m.maOverrideStr = override, *a.MAIDOverride
	}
	if a.LevelOverride != nil {
		m.levelOverride = *a.LevelOverride
	}
	if a.ForceAIS != nil {
		m.forceAIS = *a.ForceAIS
	}
	if a.Lock != nil {
		m.lock = *a.Lock
		go m.env.Emit("lck-tx", map[bool]string{true: "locked: sending LCK", false: "unlocked"}[m.lock], nil)
	}
	if a.CSF != nil {
		m.csf = *a.CSF
		if m.csf == "none" {
			m.csf = ""
		}
	}
	m.mu.Unlock()
	st := m.Status().(Status)
	return map[string]any{"ccm_enabled": st.CCMEnabled, "present_rdi": st.PresentRDI, "faults": st.Faults, "ais_sending": st.AISSending}, nil
}

func (m *mep) clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.txCnt, m.rxCnt, m.rxErrors = map[uint8]uint64{}, map[uint8]uint64{}, 0
	m.owds, m.tsts = map[pkt.MAC]*owd{}, map[pkt.MAC]*tstRx{}
	for id, r := range m.rmeps {
		if !r.configured && r.state == rmepFailed {
			delete(m.rmeps, id)
			continue
		}
		r.ccmRx, r.seqErrors = 0, 0
	}
}

// Health is the highest defect, or ok (protocol.Healther).
func (m *mep) Health() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.cur.highest(); h != "none" {
		return h
	}
	return "ok"
}
