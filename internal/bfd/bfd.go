// Package bfd is Bidirectional Forwarding Detection (RFC 5880) in
// asynchronous mode, without authentication or echo: control packets,
// the session state machine, timer negotiation (with poll sequences) and
// detection. A protocol owns its sessions and their transport (single hop,
// RFC 5881: UDP port 3784, TTL 255), calling Tick and Receive.
package bfd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"time"
)

const (
	Port       = 3784 // single-hop control packets (RFC 5881)
	PacketLen  = 24
	slowTx     = time.Second // while not Up (RFC 5880 6.8.3)
	SrcPortMin = 49152
)

// States.
const (
	AdminDown byte = 0
	Down      byte = 1
	Init      byte = 2
	Up        byte = 3
)

var StateNames = []string{"admin-down", "down", "init", "up"}

// Diagnostics.
const (
	DiagNone       byte = 0
	DiagTimeExpire byte = 1 // control detection time expired
	DiagNbrDown    byte = 3 // neighbor signaled session down
	DiagAdminDown  byte = 7
)

var DiagNames = map[byte]string{0: "none", 1: "detection time expired", 2: "echo failed", 3: "neighbor signaled down",
	4: "forwarding plane reset", 5: "path down", 6: "concatenated path down", 7: "administratively down"}

// Flags.
const (
	flagPoll  = 0x20
	flagFinal = 0x10
)

// Packet is a control packet.
type Packet struct {
	Diag        byte
	State       byte
	Poll, Final bool
	Mult        byte
	MyDisc      uint32
	YourDisc    uint32
	MinTx       uint32 // desired min TX interval, µs
	MinRx       uint32 // required min RX interval, µs
}

func (p Packet) Encode() []byte {
	b := make([]byte, PacketLen)
	b[0] = 1<<5 | p.Diag&0x1f
	b[1] = p.State << 6
	if p.Poll {
		b[1] |= flagPoll
	}
	if p.Final {
		b[1] |= flagFinal
	}
	b[2], b[3] = p.Mult, PacketLen
	binary.BigEndian.PutUint32(b[4:], p.MyDisc)
	binary.BigEndian.PutUint32(b[8:], p.YourDisc)
	binary.BigEndian.PutUint32(b[12:], p.MinTx)
	binary.BigEndian.PutUint32(b[16:], p.MinRx)
	return b // required min echo RX: 0
}

// Decode parses and checks a control packet (RFC 5880 6.8.6, the
// session-independent checks).
func Decode(b []byte) (*Packet, error) {
	if len(b) < PacketLen {
		return nil, errors.New("short packet")
	}
	if b[0]>>5 != 1 {
		return nil, fmt.Errorf("version %d", b[0]>>5)
	}
	if int(b[3]) < PacketLen || int(b[3]) > len(b) {
		return nil, fmt.Errorf("bad length %d", b[3])
	}
	p := &Packet{Diag: b[0] & 0x1f, State: b[1] >> 6, Poll: b[1]&flagPoll != 0, Final: b[1]&flagFinal != 0, Mult: b[2],
		MyDisc: binary.BigEndian.Uint32(b[4:]), YourDisc: binary.BigEndian.Uint32(b[8:]),
		MinTx: binary.BigEndian.Uint32(b[12:]), MinRx: binary.BigEndian.Uint32(b[16:])}
	switch {
	case p.Mult == 0:
		return nil, errors.New("detect multiplier 0")
	case p.MyDisc == 0:
		return nil, errors.New("my discriminator 0")
	case b[1]&0x01 != 0:
		return nil, errors.New("multipoint bit set")
	case b[1]&0x04 != 0:
		return nil, errors.New("authentication not supported")
	case p.YourDisc == 0 && p.State != Down && p.State != AdminDown:
		return nil, errors.New("your discriminator 0 outside down")
	}
	return p, nil
}

// Config is a session's timers.
type Config struct {
	MinTx time.Duration // desired min TX interval
	MinRx time.Duration // required min RX interval
	Mult  byte
}

// Session is one BFD session.
type Session struct {
	cfg          Config
	MyDisc       uint32
	yourDisc     uint32
	state        byte
	remoteState  byte
	diag         byte
	remoteMinRx  time.Duration
	remoteMinTx  time.Duration
	remoteMult   byte
	lastRx       time.Time
	nextTx       time.Time
	poll         bool // poll sequence in progress (our timers changed)
	sentMinTx    time.Duration
	sendFinal    bool
	since        time.Time
	upCount      int
	lastDownDiag byte
	silent       bool // fault injection: send nothing
	peerAdmin    bool // the last down was the peer going AdminDown
}

// New starts a session in Down.
func New(cfg Config, disc uint32, now time.Time) *Session {
	return &Session{cfg: cfg, MyDisc: disc, state: Down, remoteMinRx: time.Microsecond, since: now, nextTx: now}
}

func (s *Session) State() byte { return s.state }

// PeerAdminDown reports whether the session is down because the peer took
// it administratively down: not a path failure, so clients keep their
// adjacencies (RFC 5882 3.2).
func (s *Session) PeerAdminDown() bool { return s.state == Down && s.peerAdmin }

// txInterval is the desired TX interval we advertise: slow until Up.
func (s *Session) txInterval() time.Duration {
	if s.state != Up {
		return max(slowTx, s.cfg.MinTx)
	}
	return s.cfg.MinTx
}

// detectTime is how long without packets before the session goes down.
func (s *Session) detectTime() time.Duration {
	return time.Duration(s.remoteMult) * max(s.cfg.MinRx, s.remoteMinTx)
}

func (s *Session) setState(st, diag byte, now time.Time) {
	if st == s.state {
		return
	}
	if s.state == Up {
		s.lastDownDiag = diag
	}
	s.state, s.diag, s.since = st, diag, now
	if st == Up {
		s.upCount++
		s.peerAdmin = false
	}
	if st == Down {
		s.yourDisc = 0 // RFC 5880 6.8.1: learn it again
	}
	// Advertised timers change between slow and configured: poll.
	if s.txInterval() != s.sentMinTx {
		s.poll = true
	}
	s.nextTx = now // tell the peer at once
}

// Receive handles a control packet for this session and reports whether
// its state changed.
func (s *Session) Receive(p *Packet, now time.Time) (changed bool) {
	if p.YourDisc != 0 && p.YourDisc != s.MyDisc {
		return false
	}
	old := s.state
	s.yourDisc = p.MyDisc
	s.remoteState, s.remoteMult = p.State, p.Mult
	s.remoteMinRx = time.Duration(p.MinRx) * time.Microsecond
	s.remoteMinTx = time.Duration(p.MinTx) * time.Microsecond
	s.lastRx = now
	if p.Final {
		s.poll = false
	}
	if p.Poll {
		s.sendFinal = true
		s.nextTx = now
	}
	if s.state == AdminDown {
		return false
	}
	switch {
	case p.State == AdminDown:
		if s.state != Down {
			s.setState(Down, DiagNbrDown, now)
			s.peerAdmin = true
		}
	case s.state == Down:
		if p.State == Down {
			s.setState(Init, DiagNone, now)
		} else if p.State == Init {
			s.setState(Up, DiagNone, now)
		}
	case s.state == Init:
		if p.State == Init || p.State == Up {
			s.setState(Up, DiagNone, now)
		}
	case s.state == Up:
		if p.State == Down {
			s.setState(Down, DiagNbrDown, now)
		}
	}
	return s.state != old
}

// Tick runs the timers: detection, then a control packet when one is due
// (nil otherwise). changed reports a state change (to Down).
func (s *Session) Tick(now time.Time) (out []byte, changed bool) {
	if (s.state == Init || s.state == Up) && !s.lastRx.IsZero() && now.Sub(s.lastRx) > s.detectTime() {
		s.setState(Down, DiagTimeExpire, now)
		changed = true
	}
	if now.Before(s.nextTx) {
		return nil, changed
	}
	// The interval: our advertised one or the peer's required RX,
	// whichever is longer, less 0-25% jitter (RFC 5880 6.8.7).
	iv := max(s.txInterval(), s.remoteMinRx)
	s.nextTx = now.Add(iv - time.Duration(rand.Int63n(int64(iv)/4+1)))
	if s.silent {
		return nil, changed
	}
	p := Packet{Diag: s.diag, State: s.state, Poll: s.poll && !s.sendFinal, Final: s.sendFinal, Mult: s.cfg.Mult, MyDisc: s.MyDisc,
		YourDisc: s.yourDisc, MinTx: uint32(s.txInterval() / time.Microsecond), MinRx: uint32(s.cfg.MinRx / time.Microsecond)}
	s.sendFinal = false
	s.sentMinTx = s.txInterval()
	return p.Encode(), changed
}

// SetSilent stops (or resumes) sending, for fault injection.
func (s *Session) SetSilent(v bool) { s.silent = v }

// Status is a session's state for display.
type Status struct {
	State       string  `json:"state"`
	RemoteState string  `json:"remote_state,omitempty"`
	Diag        string  `json:"diag,omitempty"`
	LastDown    string  `json:"last_down,omitempty"`
	MyDisc      uint32  `json:"my_disc"`
	YourDisc    uint32  `json:"your_disc,omitempty"`
	TxMs        float64 `json:"tx_ms"`
	DetectMs    float64 `json:"detect_ms,omitempty"`
	Ups         int     `json:"ups,omitempty"`
	Since       string  `json:"since"`
	Silent      bool    `json:"silent,omitempty"`
}

func (s *Session) Status() Status {
	st := Status{State: StateNames[s.state], MyDisc: s.MyDisc, YourDisc: s.yourDisc, Ups: s.upCount, Since: s.since.Format(time.RFC3339), Silent: s.silent,
		TxMs: float64(max(s.txInterval(), s.remoteMinRx)) / 1e6}
	if !s.lastRx.IsZero() {
		st.RemoteState, st.DetectMs = StateNames[s.remoteState], float64(s.detectTime())/1e6
	}
	if s.diag != 0 {
		st.Diag = DiagNames[s.diag]
	}
	if s.lastDownDiag != 0 {
		st.LastDown = DiagNames[s.lastDownDiag]
	}
	return st
}
