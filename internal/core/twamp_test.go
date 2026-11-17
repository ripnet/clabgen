package core_test

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
	_ "github.com/ripnet/clabgen/protocols/twamp"
)

// A test sender: one UDP payload (hex, the sequence number in its first 4
// bytes counting up) every every_ms to dst:862, keeping the replies.
func init() { protocol.Register(testSender{}) }

type testSenderConfig struct {
	Dst     string `json:"dst"`
	SrcPort uint16 `json:"src_port"`
	Payload string `json:"payload"`
	EveryMS int    `json:"every_ms"`
	TOS     uint8  `json:"tos"`
	TTL     uint8  `json:"ttl"`
}

type testSender struct{}

func (testSender) Name() string               { return "test-twamp-sender" }
func (testSender) Description() string        { return "test TWAMP sender" }
func (testSender) NewConfig() any             { return &testSenderConfig{SrcPort: 50000, EveryMS: 20, TTL: 255} }
func (testSender) EtherTypes() []uint16       { return []uint16{pkt.EtherTypeIPv4, pkt.EtherTypeIPv6} }
func (testSender) Actions() []protocol.Action { return nil }
func (testSender) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*testSenderConfig)
	p, err := hex.DecodeString(cfg.Payload)
	if err != nil {
		return nil, err
	}
	s := &tSender{env: env, cfg: cfg, payload: p, dst: netip.MustParseAddr(cfg.Dst), stop: make(chan struct{})}
	tSenders.Store(env.InstanceName(), s)
	go s.run()
	return s, nil
}

type tReply struct {
	b      []byte
	ttl    uint8
	tos    uint8
	rxTime time.Time
}

type tSender struct {
	env     protocol.Env
	cfg     testSenderConfig
	payload []byte
	dst     netip.Addr
	stop    chan struct{}

	mu      sync.Mutex
	seq     uint32
	sent    map[uint32]time.Time
	replies []tReply
}

func (s *tSender) run() {
	t := time.NewTicker(time.Duration(s.cfg.EveryMS) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		ip := s.env.IP()
		src, ok := ip.Source(s.dst)
		if !ok {
			continue
		}
		_, mac, err := ip.ResolveNextHop(context.Background(), s.dst)
		if err != nil {
			continue
		}
		s.mu.Lock()
		seq := s.seq
		s.seq++
		if s.sent == nil {
			s.sent = map[uint32]time.Time{}
		}
		s.sent[seq] = time.Now()
		s.mu.Unlock()
		u := make([]byte, 8+len(s.payload))
		copy(u[8:], s.payload)
		binary.BigEndian.PutUint32(u[8:], seq)
		binary.BigEndian.PutUint16(u[0:], s.cfg.SrcPort)
		binary.BigEndian.PutUint16(u[2:], 862)
		binary.BigEndian.PutUint16(u[4:], uint16(len(u)))
		cs := pkt.Checksum(u, pkt.PseudoSum(src, s.dst, pkt.ProtoUDP, len(u)))
		if cs == 0 {
			cs = 0xffff
		}
		binary.BigEndian.PutUint16(u[6:], cs)
		if s.dst.Is4() {
			h := pkt.IPv4{TOS: s.cfg.TOS, TTL: s.cfg.TTL, Protocol: pkt.ProtoUDP, Src: src, Dst: s.dst, Payload: u}
			s.env.Send(mac, pkt.EtherTypeIPv4, h.Encode(), protocol.SendOpts{})
		} else {
			h := pkt.IPv6{TrafficClass: s.cfg.TOS, NextHeader: pkt.ProtoUDP, HopLimit: s.cfg.TTL, Src: src, Dst: s.dst, Payload: u}
			s.env.Send(mac, pkt.EtherTypeIPv6, h.Encode(), protocol.SendOpts{})
		}
	}
}

func (s *tSender) HandleRx(f *pkt.Frame) {
	var b []byte
	var ttl, tos uint8
	switch f.EtherType {
	case pkt.EtherTypeIPv4:
		h, err := pkt.DecodeIPv4(f.Payload)
		if err != nil || h.Protocol != pkt.ProtoUDP {
			return
		}
		b, ttl, tos = h.Payload, h.TTL, h.TOS
	case pkt.EtherTypeIPv6:
		h, err := pkt.DecodeIPv6(f.Payload)
		if err != nil || h.NextHeader != pkt.ProtoUDP {
			return
		}
		b, ttl, tos = h.Payload, h.HopLimit, h.TrafficClass
	}
	if len(b) < 8 || binary.BigEndian.Uint16(b) != 862 || binary.BigEndian.Uint16(b[2:]) != s.cfg.SrcPort {
		return
	}
	s.mu.Lock()
	s.replies = append(s.replies, tReply{b: append([]byte(nil), b[8:]...), ttl: ttl, tos: tos, rxTime: f.RxTime})
	s.mu.Unlock()
}

func (s *tSender) Status() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"sent": s.seq, "replies": len(s.replies)}
}
func (s *tSender) Action(context.Context, string, any) (any, error) { return nil, nil }
func (s *tSender) Stop()                                            { close(s.stop) }

// The running test senders by instance name (tests using them don't run
// in parallel).
var tSenders sync.Map

func senderOf(t *testing.T, name string) *tSender {
	t.Helper()
	s, ok := tSenders.Load(name)
	if !ok {
		t.Fatalf("no sender %s", name)
	}
	return s.(*tSender)
}

func (s *tSender) got() []tReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tReply(nil), s.replies...)
}

func ntp(b []byte) time.Time {
	sec, frac := binary.BigEndian.Uint32(b), binary.BigEndian.Uint32(b[4:])
	return time.Unix(int64(sec)-2208988800, int64(uint64(frac)*1e9>>32))
}

func mkTWAMP(t *testing.T, e *core.Engine, name, typ, cfg string) {
	t.Helper()
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: typ, Device: "d", Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

func zhex(n int) string { return hex.EncodeToString(make([]byte, n)) }

// TWAMP Light (14 bytes) and STAMP (with TLVs) senders against the reflector:
// reply layout and sizes, the reflector's own sequence per session, TTL 1
// accepted, TLVs processed, sessions in status.
func TestTWAMPReflector(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mkTWAMP(t, b, "refl", "twamp-reflector", `{}`)
	t1 := "eea6f5cffde2bcef"
	// TWAMP Light: seq, T1, error estimate (mult 1).
	mkTWAMP(t, a, "tl", "test-twamp-sender", `{"dst":"10.0.0.2","src_port":50001,"payload":"00000000`+t1+`0001"}`)
	// STAMP over IPv6 with TTL 1, DSCP 48, SSID 7 and TLVs: extra padding,
	// class of service (asks for DSCP 10), direct measurement, an unknown
	// type 99, timestamp information.
	stamp := "00000000" + t1 + "0001" + "0007" + zhex(28) +
		"80010008" + zhex(8) +
		"80040004" + "28000000" +
		"8005000c" + "00000005" + zhex(8) +
		"80630004" + "deadbeef" +
		"80030004" + zhex(4)
	mkTWAMP(t, a, "st", "test-twamp-sender", `{"dst":"2001:db8::2","src_port":50002,"ttl":1,"tos":192,"payload":"`+stamp+`"}`)
	tl, st := senderOf(t, "tl"), senderOf(t, "st")
	eventually(t, "replies", func() bool { return len(tl.got()) >= 5 && len(st.got()) >= 5 })

	r := tl.got()
	for i, x := range r[:5] {
		m := x.b
		if len(m) != 41 {
			t.Fatalf("twamp light reply %d bytes", len(m))
		}
		if seq := binary.BigEndian.Uint32(m); seq != uint32(i) {
			t.Fatalf("reflector seq %d at %d", seq, i)
		}
		if binary.BigEndian.Uint32(m[24:]) != uint32(i) || hex.EncodeToString(m[28:36]) != t1 || m[40] != 255 || x.ttl != 255 {
			t.Fatalf("reply %d: %x ttl %d", i, m, x.ttl)
		}
		t2, t3 := ntp(m[16:24]), ntp(m[4:12])
		if t3.Before(t2) || time.Since(t2) > 5*time.Second || t3.Sub(t2) > 50*time.Millisecond {
			t.Fatalf("timestamps T2 %v T3 %v", t2, t3)
		}
	}

	m := st.got()[0]
	if len(m.b) != len(stamp)/2 {
		t.Fatalf("stamp reply %d bytes, sent %d", len(m.b), len(stamp)/2)
	}
	if ssid := binary.BigEndian.Uint16(m.b[14:]); ssid != 7 || m.b[40] != 1 {
		t.Fatalf("ssid %d sender ttl %d", ssid, m.b[40])
	}
	if m.tos>>2 != 10 {
		t.Fatalf("reply DSCP %d, the CoS TLV asked for 10", m.tos>>2)
	}
	tlv := m.b[44:]
	if tlv[0] != 0 || tlv[1] != 1 {
		t.Fatalf("padding TLV %x", tlv[:4])
	}
	cos := tlv[12:20]
	if cos[0] != 0 || cos[4]>>2 != 10 || (cos[4]&3)<<4|cos[5]>>4 != 48 {
		t.Fatalf("CoS TLV %x", cos)
	}
	dm := tlv[20:36]
	if dm[0] != 0 || binary.BigEndian.Uint32(dm[4:]) != 5 || binary.BigEndian.Uint32(dm[8:]) != 1 || binary.BigEndian.Uint32(dm[12:]) != 1 {
		t.Fatalf("direct measurement TLV %x", dm)
	}
	if unk := tlv[36:44]; unk[0] != 0x80 || hex.EncodeToString(unk[4:]) != "deadbeef" {
		t.Fatalf("unknown TLV %x", unk)
	}
	if ti := tlv[44:52]; ti[0] != 0 || ti[4] != 5 || ti[5] != 2 {
		t.Fatalf("timestamp info TLV %x", ti)
	}

	s := flowStatus(t, b, "refl")
	ss := s["sessions"].([]any)
	if len(ss) != 2 || num(s, "active") != 2 {
		t.Fatalf("sessions %v", s)
	}
	for _, x := range ss {
		x := x.(map[string]any)
		switch x["kind"] {
		case "twamp-light":
			if x["sender"] != "10.0.0.1:50001" || x["local"] != "10.0.0.2:862" || num(x, "ttl") != 255 {
				t.Fatalf("twamp session %v", x)
			}
		case "stamp":
			if x["sender"] != "[2001:db8::1]:50002" || num(x, "ssid") != 7 || num(x, "ttl") != 1 || num(x, "dscp") != 48 || x["tlvs"].(map[string]any)["type-99"] == nil {
				t.Fatalf("stamp session %v", x)
			}
		default:
			t.Fatalf("session %v", x)
		}
	}
}

// Faults: added delay is path delay (T2 moves, the round trip grows), mute
// and burst-loss drop replies, a clock offset moves both timestamps, and
// senders outside the allowed prefixes are refused.
func TestTWAMPReflectorFaults(t *testing.T) {
	a, b, _, _ := ipPair(t)
	mkTWAMP(t, b, "refl", "twamp-reflector", `{"delay_us":30000}`)
	mkTWAMP(t, a, "tl", "test-twamp-sender", `{"dst":"10.0.0.2","payload":"00000000eea6f5cffde2bcef0001"}`)
	tl := senderOf(t, "tl")
	eventually(t, "replies", func() bool { return len(tl.got()) >= 3 })
	for _, x := range tl.got()[:3] {
		seq := binary.BigEndian.Uint32(x.b[24:])
		tl.mu.Lock()
		sent := tl.sent[seq]
		tl.mu.Unlock()
		if rtt := x.rxTime.Sub(sent); rtt < 30*time.Millisecond {
			t.Fatalf("round trip %v with 30 ms delay", rtt)
		}
		if t2 := ntp(x.b[16:24]); t2.Sub(sent) < 29*time.Millisecond {
			t.Fatalf("T2 %v after sending: the delay isn't path delay", t2.Sub(sent))
		}
		if dwell := ntp(x.b[4:12]).Sub(ntp(x.b[16:24])); dwell > 10*time.Millisecond {
			t.Fatalf("dwell %v", dwell)
		}
	}

	if _, err := b.RunAction(context.Background(), "refl", "mute", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // replies already held go out
	n := len(tl.got())
	time.Sleep(200 * time.Millisecond)
	if len(tl.got()) != n {
		t.Fatalf("replies while muted: %d -> %d", n, len(tl.got()))
	}
	if h := flowStatus(t, b, "refl"); h["muted"] != true || num(h, "dropped") < 5 {
		t.Fatalf("muted status %v", h)
	}
	if _, err := b.RunAction(context.Background(), "refl", "unmute", nil); err != nil {
		t.Fatal(err)
	}
	n = len(tl.got())
	eventually(t, "answering after unmute", func() bool { return len(tl.got()) > n+1 })
	sess := func() map[string]any { return flowStatus(t, b, "refl")["sessions"].([]any)[0].(map[string]any) }
	d0 := num(sess(), "dropped")
	if _, err := b.RunAction(context.Background(), "refl", "burst-loss", json.RawMessage(`{"count":5}`)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "burst used up", func() bool { return num(sess(), "burst_remaining") == 0 })
	n = len(tl.got())
	eventually(t, "answering again", func() bool { return len(tl.got()) > n })
	if d := num(sess(), "dropped") - d0; d != 5 {
		t.Fatalf("burst dropped %v", d)
	}
	// Lost on the way back: the reflector's sequence numbers skip 5.
	skipped := false
	r := tl.got()
	for i := 1; i < len(r); i++ {
		if binary.BigEndian.Uint32(r[i].b)-binary.BigEndian.Uint32(r[i-1].b) == 5+1 {
			skipped = true
		}
	}
	if !skipped {
		t.Fatal("reflector sequence numbers didn't skip the burst")
	}

	// No delay, a clock 1 s ahead: T2 a second after the send time.
	if _, err := b.UpdateProtocol("refl", json.RawMessage(`{"clock_offset_ms":1000}`)); err != nil {
		t.Fatal(err)
	}
	n = len(tl.got())
	eventually(t, "offset replies", func() bool { return len(tl.got()) > n+2 })
	x := tl.got()[len(tl.got())-1]
	if d := ntp(x.b[16:24]).Sub(x.rxTime); d < 900*time.Millisecond || d > 1100*time.Millisecond {
		t.Fatalf("T2 - receive %v with a 1 s offset", d)
	}

	if _, err := b.UpdateProtocol("refl", json.RawMessage(`{"senders":["192.0.2.0/24"]}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	n = len(tl.got())
	time.Sleep(200 * time.Millisecond)
	if len(tl.got()) != n || num(flowStatus(t, b, "refl"), "refused") == 0 {
		t.Fatalf("answered a sender outside the allowed prefixes: %v", flowStatus(t, b, "refl"))
	}
}
