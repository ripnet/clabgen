package ldp

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Session states (section 2.5.4).
const (
	stInitialized = "initialized"
	stOpenSent    = "open sent"
	stOpenRec     = "open received"
	stOperational = "operational"
)

// conn is one TCP session with a neighbor.
type conn struct {
	s      *session
	n      *neighbor // nil on an accepted connection until its Init names it
	c      net.Conn
	active bool

	// Under s.mu.
	state     string
	keepalive int
	maxPDU    int
	upSince   time.Time
	peer      ID
	addresses map[netip.Addr]bool
	lib       map[netip.Prefix]uint32
	counters  map[string]uint64

	wmu        sync.Mutex
	silentTill time.Time
	once       sync.Once
	closed     chan struct{}
	why        string
}

func newConn(s *session, n *neighbor, c net.Conn) *conn {
	return &conn{s: s, n: n, c: c, active: n != nil, state: stInitialized, maxPDU: maxPDUDefault, addresses: map[netip.Addr]bool{},
		lib: map[netip.Prefix]uint32{}, counters: map[string]uint64{}, closed: make(chan struct{})}
}

func (c *conn) count(k string) {
	c.s.mu.Lock()
	c.counters[k]++
	c.s.mu.Unlock()
}

func (c *conn) setSilent(till time.Time) {
	c.wmu.Lock()
	c.silentTill = till
	c.wmu.Unlock()
}

func (c *conn) silent() bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return time.Now().Before(c.silentTill)
}

// write sends messages in PDUs of at most the session's size.
func (c *conn) write(msgs ...[]byte) error {
	c.s.mu.Lock()
	limit := c.maxPDU
	c.s.mu.Unlock()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if time.Now().Before(c.silentTill) {
		return nil
	}
	c.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	for len(msgs) > 0 {
		n, size := 0, pduHeaderLen
		for n < len(msgs) && (n == 0 || size+len(msgs[n]) <= limit) {
			size += len(msgs[n])
			n++
		}
		if _, err := c.c.Write(EncodePDU(c.s.id, msgs[:n]...)); err != nil {
			return err
		}
		msgs = msgs[n:]
	}
	return nil
}

func (c *conn) send(kind string, msgs ...[]byte) error {
	err := c.write(msgs...)
	if err == nil {
		c.s.mu.Lock()
		c.counters["tx_"+kind] += uint64(len(msgs))
		c.s.mu.Unlock()
	}
	return err
}

func (c *conn) close(why string) {
	c.once.Do(func() {
		c.why = why
		close(c.closed)
		c.c.Close()
	})
}

// notify sends a notification; a fatal one is the caller's to follow with close.
func (c *conn) notify(code, msgID uint32, msgType uint16) error {
	m := Message{Type: MsgNotification, ID: c.s.msgID(), TLVs: []TLV{StatusTLV{Code: code, MsgID: msgID, MsgType: msgType}.tlv()}}
	return c.send("notification", m.Encode())
}

// notifyClose sends a fatal notification and ends the session.
func (c *conn) notifyClose(code uint32, why string) {
	c.notify(code, 0, 0)
	c.close(why)
}

func (c *conn) sendInit(receiver ID) error {
	maxPDU := c.s.cfg.MaxPDU
	if maxPDU == 0 {
		maxPDU = maxPDUDefault
	}
	p := SessionParams{KeepAlive: uint16(c.s.cfg.KeepAlive), MaxPDU: uint16(maxPDU), Receiver: receiver}
	return c.send("init", Message{Type: MsgInit, ID: c.s.msgID(), TLVs: []TLV{p.tlv()}}.Encode())
}

func (c *conn) sendKeepAlive() error {
	return c.send("keepalive", Message{Type: MsgKeepAlive, ID: c.s.msgID()}.Encode())
}

// run is the session: handshake, then messages until it ends.
func (c *conn) run() {
	defer c.s.wg.Done()
	defer c.ended()
	if c.active {
		c.s.mu.Lock()
		lsr := c.n.lsr
		c.s.mu.Unlock()
		if err := c.sendInit(ID{LSR: lsr}); err != nil {
			c.close("init: " + err.Error())
			return
		}
		c.setState(stOpenSent)
	}
	stopKA := make(chan struct{})
	defer close(stopKA)
	go c.keepaliveLoop(stopKA)
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 8192)
	for {
		c.s.mu.Lock()
		ka := c.keepalive
		c.s.mu.Unlock()
		if ka == 0 {
			ka = max(c.s.cfg.KeepAlive, 15) // the handshake's limit
		}
		// Silent: we've stopped listening too, and the peer's timers may end
		// it. What it sends meanwhile waits in TCP, so nothing is lost if not.
		if c.silent() {
			select {
			case <-c.closed:
				return
			case <-c.s.stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		c.c.SetReadDeadline(time.Now().Add(time.Duration(ka) * time.Second))
		n, err := c.c.Read(tmp)
		if err != nil {
			var ne net.Error
			switch {
			case errors.As(err, &ne) && ne.Timeout():
				if c.silent() {
					continue // went silent during the read
				}
				c.notifyClose(StatusKeepAliveExpired, "keepalive timer expired")
			case errors.Is(err, io.EOF):
				c.close("peer closed the connection")
			default:
				c.close(err.Error())
			}
			return
		}
		buf = append(buf, tmp[:n]...)
		for len(buf) >= 4 {
			size := PDULen(buf)
			if size < pduHeaderLen || size > 65535 {
				c.notifyClose(StatusBadPDULen, fmt.Sprintf("PDU length %d", size))
				return
			}
			if len(buf) < size {
				break
			}
			p, err := DecodePDU(buf[:size])
			if err != nil {
				c.notifyClose(StatusMalformedTLV, err.Error())
				return
			}
			if !c.handlePDU(p) {
				return
			}
			buf = append(buf[:0], buf[size:]...)
		}
	}
}

func (c *conn) setState(st string) {
	c.s.mu.Lock()
	c.state = st
	c.s.mu.Unlock()
}

func (c *conn) keepaliveLoop(stop chan struct{}) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-stop:
			return
		case <-c.closed:
			return
		case now := <-t.C:
			c.s.mu.Lock()
			ka, st := c.keepalive, c.state
			c.s.mu.Unlock()
			if st != stOperational || ka == 0 {
				continue
			}
			if now.Sub(last) >= time.Duration(ka)*time.Second/3 {
				last = now
				if c.sendKeepAlive() != nil {
					c.close("keepalive: write failed")
					return
				}
			}
		}
	}
}

// handlePDU handles a PDU's messages; false means the session ended.
func (c *conn) handlePDU(p *PDU) bool {
	c.s.mu.Lock()
	state, peer := c.state, c.peer
	c.s.mu.Unlock()
	if state == stOperational || state == stOpenRec {
		if p.ID.LSR != peer.LSR {
			c.notifyClose(StatusBadLDPID, fmt.Sprintf("PDU from %s on the session with %s", p.ID, peer))
			return false
		}
	}
	for i := range p.Messages {
		if !c.handle(p.ID, &p.Messages[i]) {
			return false
		}
	}
	return true
}

func (c *conn) handle(id ID, m *Message) bool {
	c.s.mu.Lock()
	state := c.state
	c.counters["rx_"+counterName(m.Type)]++
	c.s.mu.Unlock()
	switch m.Type {
	case MsgInit:
		return c.rxInit(id, m, state)
	case MsgKeepAlive:
		if state == stOpenRec {
			c.operational()
		}
		return true
	case MsgNotification:
		return c.rxNotification(m)
	}
	if state != stOperational {
		c.notifyClose(StatusShutdown, MessageName(m.Type)+" before the session was up")
		return false
	}
	switch m.Type {
	case MsgAddress, MsgAddressWithdraw:
		c.rxAddress(m)
	case MsgLabelMapping:
		c.rxMapping(m)
	case MsgLabelWithdraw:
		c.rxWithdraw(m)
	case MsgLabelRequest:
		c.rxRequest(m)
	case MsgLabelRelease, MsgLabelAbort:
		// Noted in the counters: we hold no state per peer to give back.
	default:
		if !m.Unknown {
			c.notify(StatusUnknownMsg, m.ID, m.Type)
		}
	}
	return true
}

func counterName(t uint16) string {
	switch t {
	case MsgNotification:
		return "notification"
	case MsgInit:
		return "init"
	case MsgKeepAlive:
		return "keepalive"
	case MsgAddress:
		return "address"
	case MsgAddressWithdraw:
		return "address_withdraw"
	case MsgLabelMapping:
		return "mapping"
	case MsgLabelRequest:
		return "request"
	case MsgLabelWithdraw:
		return "withdraw"
	case MsgLabelRelease:
		return "release"
	case MsgLabelAbort:
		return "abort"
	}
	return "unknown"
}

func (c *conn) rxInit(id ID, m *Message, state string) bool {
	t := m.tlv(TLVCommonSession)
	if t == nil {
		c.notifyClose(StatusMissingParam|0x80000000, "init without session parameters")
		return false
	}
	p, err := decodeSessionParams(t.Value)
	if err != nil {
		c.notifyClose(StatusMalformedTLV, err.Error())
		return false
	}
	if p.Receiver.LSR != c.s.rid || p.Receiver.Space != 0 {
		c.notifyClose(StatusRejectedNoHello, fmt.Sprintf("init for %s, we are %s", p.Receiver, c.s.id))
		return false
	}
	if p.KeepAlive == 0 {
		c.notifyClose(StatusRejectedKeepAlive, "keepalive time 0")
		return false
	}
	if p.DoD {
		c.s.count("rx_dod_session")
	}
	ka := min(int(p.KeepAlive), c.s.cfg.KeepAlive)
	maxPDU := int(p.MaxPDU)
	if maxPDU == 0 {
		maxPDU = maxPDUDefault
	}
	if maxPDU < 256 {
		c.notifyClose(StatusRejectedMaxPDU, fmt.Sprintf("max PDU %d", maxPDU))
		return false
	}
	c.s.mu.Lock()
	c.peer, c.keepalive, c.maxPDU = id, ka, maxPDU
	if !c.active {
		// A session must follow hellos (section 2.5.3).
		n := c.s.neighbors[id.LSR]
		if n == nil || len(n.adjs) == 0 {
			c.s.mu.Unlock()
			c.s.count("rx_session_no_hello")
			c.notifyClose(StatusRejectedNoHello, "no hello from "+id.String())
			return false
		}
		if n.conn != nil {
			c.s.mu.Unlock()
			c.notifyClose(StatusShutdown, "already have a session with "+id.String())
			return false
		}
		c.n, n.conn = n, c
	}
	c.s.mu.Unlock()
	if !c.active {
		if err := c.sendInit(id); err != nil {
			c.close(err.Error())
			return false
		}
	}
	if err := c.sendKeepAlive(); err != nil {
		c.close(err.Error())
		return false
	}
	c.setState(stOpenRec)
	return true
}

// operational: exchange addresses and labels.
func (c *conn) operational() {
	c.s.mu.Lock()
	c.state = stOperational
	c.upSince = time.Now()
	n := c.n
	n.sessions++
	n.lastErr, n.backoff = "", 0
	lsr := n.lsr
	bs := make([]binding, 0, len(c.s.fecs))
	for p, l := range c.s.fecs {
		bs = append(bs, binding{p, l})
	}
	c.s.mu.Unlock()
	c.s.event("session-up", "session with "+lsr.String()+" operational")
	var addrs []netip.Addr
	seen := map[netip.Addr]bool{}
	add := func(a netip.Addr) {
		if a.Is4() && !seen[a] {
			seen[a] = true
			addrs = append(addrs, a)
		}
	}
	add(c.s.transport)
	add(c.s.rid)
	for _, p := range c.s.env.IP().Addresses() {
		add(p.Addr())
	}
	c.send("address", Message{Type: MsgAddress, ID: c.s.msgID(), TLVs: []TLV{addressTLV(addrs)}}.Encode())
	c.sendBindings(bs, false)
}

// sendBindings sends a mapping per FEC, or a withdraw.
func (c *conn) sendBindings(bs []binding, withdraw bool) {
	if len(bs) == 0 {
		return
	}
	t, kind := MsgLabelMapping, "mapping"
	if withdraw {
		t, kind = MsgLabelWithdraw, "withdraw"
	}
	msgs := make([][]byte, 0, len(bs))
	for _, b := range bs {
		msgs = append(msgs, Message{Type: t, ID: c.s.msgID(), TLVs: []TLV{fecTLV(b.prefix), labelTLV(b.label)}}.Encode())
	}
	if err := c.send(kind, msgs...); err != nil {
		c.close(err.Error())
	}
}

func (c *conn) rxNotification(m *Message) bool {
	t := m.tlv(TLVStatus)
	if t == nil {
		return true
	}
	st, err := decodeStatus(t.Value)
	if err != nil {
		c.notifyClose(StatusMalformedTLV, err.Error())
		return false
	}
	text := StatusText(st.Code)
	if st.MsgType != 0 {
		text += " (" + MessageName(st.MsgType) + ")"
	}
	if Fatal(st.Code) {
		c.close("peer: " + text)
		return false
	}
	c.s.env.Emit("notification", "from "+c.peerName()+": "+text, nil)
	return true
}

func (c *conn) peerName() string {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return c.peer.LSR.String()
}

func (c *conn) rxAddress(m *Message) {
	t := m.tlv(TLVAddressList)
	if t == nil {
		c.notify(StatusMissingParam, m.ID, m.Type)
		return
	}
	as, err := decodeAddresses(t.Value)
	if err != nil {
		c.notifyClose(StatusMalformedTLV, err.Error())
		return
	}
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	for _, a := range as {
		if m.Type == MsgAddress {
			c.addresses[a] = true
		} else {
			delete(c.addresses, a)
		}
	}
}

func fecOf(m *Message) (FECValue, bool) {
	t := m.tlv(TLVFEC)
	if t == nil {
		return FECValue{}, false
	}
	f, err := decodeFEC(t.Value)
	return f, err == nil
}

func (c *conn) rxMapping(m *Message) {
	f, ok := fecOf(m)
	lt := m.tlv(TLVGenericLabel)
	if !ok || lt == nil {
		c.notify(StatusMissingParam, m.ID, m.Type)
		return
	}
	label, err := decodeLabel(lt.Value)
	if err != nil {
		c.notify(StatusMissingParam, m.ID, m.Type)
		return
	}
	var release []binding
	c.s.mu.Lock()
	c.counters["rx_other_fec"] += uint64(f.Other)
	for _, p := range f.Prefixes {
		if old, have := c.lib[p]; have && old != label {
			release = append(release, binding{p, old})
		}
		c.lib[p] = label
	}
	c.s.mu.Unlock()
	c.release(release)
}

// release gives labels back to the peer.
func (c *conn) release(bs []binding) {
	if len(bs) == 0 {
		return
	}
	msgs := make([][]byte, 0, len(bs))
	for _, b := range bs {
		msgs = append(msgs, Message{Type: MsgLabelRelease, ID: c.s.msgID(), TLVs: []TLV{fecTLV(b.prefix), labelTLV(b.label)}}.Encode())
	}
	c.send("release", msgs...)
}

func (c *conn) rxWithdraw(m *Message) {
	f, ok := fecOf(m)
	if !ok {
		c.notify(StatusMissingParam, m.ID, m.Type)
		return
	}
	var release []binding
	c.s.mu.Lock()
	if f.Wildcard {
		for p, l := range c.lib {
			release = append(release, binding{p, l})
			delete(c.lib, p)
		}
	}
	for _, p := range f.Prefixes {
		if l, have := c.lib[p]; have {
			release = append(release, binding{p, l})
			delete(c.lib, p)
		}
	}
	c.s.mu.Unlock()
	c.release(release)
}

// rxRequest answers a label request with our mapping, or says there's none.
func (c *conn) rxRequest(m *Message) {
	f, ok := fecOf(m)
	if !ok {
		c.notify(StatusMissingParam, m.ID, m.Type)
		return
	}
	var bs []binding
	missing := false
	c.s.mu.Lock()
	for _, p := range f.Prefixes {
		if l, have := c.s.fecs[p]; have {
			bs = append(bs, binding{p, l})
		} else {
			missing = true
		}
	}
	c.s.mu.Unlock()
	c.sendBindings(bs, false)
	if missing {
		c.notify(StatusNoRoute, m.ID, m.Type)
	}
}

// ended cleans up after the session.
func (c *conn) ended() {
	c.close("ended")
	c.s.mu.Lock()
	n, st := c.n, c.state
	lost := len(c.lib)
	if n != nil && n.conn == c {
		n.conn = nil
		n.lastErr = c.why
		if st == stOperational {
			n.backoff = 0
			n.retryAt = time.Now().Add(time.Second)
		} else {
			c.s.retryLocked(n)
		}
	}
	c.s.mu.Unlock()
	select {
	case <-c.s.stop:
		return
	default:
	}
	if st == stOperational && n != nil {
		c.s.event("session-down", fmt.Sprintf("session with %s down: %s (%d labels dropped)", n.lsr, c.why, lost))
	}
}
