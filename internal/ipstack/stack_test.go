package ipstack

import (
	"context"
	"encoding/binary"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// wire connects stacks as if they were on one L2 segment and records every
// frame sent.
type wire struct {
	mu     sync.Mutex
	stacks []*Stack
	sent   []*pkt.Frame
	drop   bool
}

func (w *wire) sender(src pkt.MAC) SendFunc {
	return func(dst pkt.MAC, et uint16, payload []byte) error {
		f := &pkt.Frame{Dst: dst, Src: src, EtherType: et, Payload: append([]byte(nil), payload...)}
		w.mu.Lock()
		w.sent = append(w.sent, f)
		peers, drop := append([]*Stack(nil), w.stacks...), w.drop
		w.mu.Unlock()
		if drop {
			return nil
		}
		for _, s := range peers {
			if s.mac != src && (dst == s.mac || dst.IsMulticast()) {
				s.HandleRx(f)
			}
		}
		return nil
	}
}

type evlog struct {
	mu    sync.Mutex
	kinds []string
}

func (e *evlog) emit(kind, msg string, _ map[string]any) {
	e.mu.Lock()
	e.kinds = append(e.kinds, kind)
	e.mu.Unlock()
}

func (e *evlog) has(kind string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, k := range e.kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func mustCfg(t *testing.T, gw4, gw6 string, addrs ...string) Config {
	t.Helper()
	c, err := ParseConfig(addrs, gw4, gw6)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (w *wire) add(t *testing.T, mac pkt.MAC, cfg Config) (*Stack, *evlog) {
	ev := &evlog{}
	s := New(mac, cfg, w.sender(mac), ev.emit)
	w.mu.Lock()
	w.stacks = append(w.stacks, s)
	w.mu.Unlock()
	return s, ev
}

var (
	macA = pkt.MAC{0x02, 0, 0, 0, 0, 0x0a}
	macB = pkt.MAC{0x02, 0, 0, 0, 0, 0x0b}
)

func TestParseConfig(t *testing.T) {
	ok := []struct {
		addrs    []string
		gw4, gw6 string
	}{
		{[]string{"10.0.0.2/24"}, "10.0.0.1", ""},
		{[]string{"10.0.0.0/31"}, "10.0.0.1", ""},
		{[]string{"10.0.0.2/32"}, "", ""},
		{[]string{"2001:db8::2/64"}, "", "fe80::1"},
		{[]string{"2001:db8::2/64", "10.0.0.2/24"}, "10.0.0.1", "2001:db8::1"},
		{nil, "", ""},
	}
	for _, c := range ok {
		if _, err := ParseConfig(c.addrs, c.gw4, c.gw6); err != nil {
			t.Errorf("%v: %v", c, err)
		}
	}
	bad := []struct {
		addrs    []string
		gw4, gw6 string
		want     string
	}{
		{[]string{"10.0.0.2"}, "", "", "address/prefix"},
		{[]string{"10.0.0.0/24"}, "", "", "network address"},
		{[]string{"10.0.0.255/24"}, "", "", "broadcast"},
		{[]string{"224.0.0.1/4"}, "", "", "unicast"},
		{[]string{"10.0.0.2/24", "10.0.0.2/16"}, "", "", "twice"},
		{[]string{"10.0.0.2/24"}, "10.0.1.1", "", "not in any"},
		{[]string{"10.0.0.2/24"}, "10.0.0.2", "", "own address"},
		{[]string{"10.0.0.2/24"}, "2001:db8::1", "", "not an IPv4"},
		{[]string{"10.0.0.2/24"}, "", "fe80::1", "needs an IPv6"},
	}
	for _, c := range bad {
		_, err := ParseConfig(c.addrs, c.gw4, c.gw6)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want %q", c, err, c.want)
		}
	}
}

func TestARPResolveAndReply(t *testing.T) {
	w := &wire{}
	a, evA := w.add(t, macA, mustCfg(t, "", "", "10.0.0.1/24"))
	b, _ := w.add(t, macB, mustCfg(t, "", "", "10.0.0.2/24"))
	mac, cached, err := a.Resolve(context.Background(), netip.MustParseAddr("10.0.0.2"))
	if err != nil || mac != macB || cached {
		t.Fatalf("resolve: %s cached=%v %v", mac, cached, err)
	}
	if _, cached, _ := a.Resolve(context.Background(), netip.MustParseAddr("10.0.0.2")); !cached {
		t.Fatal("second resolve should hit the cache")
	}
	// B learned A from the request aimed at it.
	if st := b.Status(); len(st.Neighbors) != 1 || st.Neighbors[0].MAC != macA.String() || st.Neighbors[0].State != "reachable" {
		t.Fatalf("b neighbors %+v", st.Neighbors)
	}
	if !evA.has("neighbor-added") {
		t.Fatal("no neighbor-added event")
	}
	c := a.Status().Counters
	if c["arp_tx_requests"] != 1 || c["arp_rx_replies"] != 1 {
		t.Fatalf("counters %v", c)
	}
	if _, _, err := a.Resolve(context.Background(), netip.MustParseAddr("10.9.9.9")); err == nil || !strings.Contains(err.Error(), "not in any") {
		t.Fatalf("off-link resolve: %v", err)
	}
}

func TestResolveTimeoutAndCancel(t *testing.T) {
	w := &wire{}
	a, _ := w.add(t, macA, mustCfg(t, "", "", "10.0.0.1/24"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := a.Resolve(ctx, netip.MustParseAddr("10.0.0.3")); err != context.DeadlineExceeded {
		t.Fatalf("want deadline, got %v", err)
	}
	if st := a.Status(); len(st.Neighbors) != 1 || st.Neighbors[0].State != "incomplete" {
		t.Fatalf("neighbors %+v", st.Neighbors)
	}
	start := time.Now()
	if _, _, err := a.Resolve(context.Background(), netip.MustParseAddr("10.0.0.4")); err == nil {
		t.Fatal("resolved a missing host")
	}
	if d := time.Since(start); d < 2900*time.Millisecond {
		t.Fatalf("gave up after %v", d)
	}
	if a.Status().Counters["resolve_failures"] != 1 {
		t.Fatal("resolve failure not counted")
	}
}

func TestStaleAndChanged(t *testing.T) {
	w := &wire{}
	a, ev := w.add(t, macA, mustCfg(t, "", "", "10.0.0.1/24"))
	now := time.Now()
	a.now = func() time.Time { return now }
	w.add(t, macB, mustCfg(t, "", "", "10.0.0.2/24"))
	a.Resolve(context.Background(), netip.MustParseAddr("10.0.0.2"))
	now = now.Add(time.Minute)
	if st := a.Status(); st.Neighbors[0].State != "stale" {
		t.Fatalf("want stale, got %+v", st.Neighbors)
	}
	// A gratuitous ARP from a new MAC for a known neighbor updates it.
	g := pkt.ARP{Op: pkt.ARPRequest, SHA: pkt.MAC{2, 0, 0, 0, 0, 0xcc}, SPA: netip.MustParseAddr("10.0.0.2"), TPA: netip.MustParseAddr("10.0.0.2")}
	a.HandleRx(&pkt.Frame{Dst: pkt.Broadcast, Src: g.SHA, EtherType: pkt.EtherTypeARP, Payload: g.Encode()})
	if st := a.Status(); st.Neighbors[0].MAC != "02:00:00:00:00:cc" || st.Neighbors[0].State != "reachable" {
		t.Fatalf("after GARP %+v", st.Neighbors)
	}
	if !ev.has("neighbor-changed") {
		t.Fatal("no neighbor-changed event")
	}
	a.ClearNeighbors()
	if len(a.Status().Neighbors) != 0 {
		t.Fatal("clear failed")
	}
}

func TestAddressConflict(t *testing.T) {
	w := &wire{}
	a, _ := w.add(t, macA, mustCfg(t, "", "", "10.0.0.1/24", "2001:db8::1/64"))
	b, ev := w.add(t, macB, mustCfg(t, "", "", "10.0.0.1/24", "2001:db8::1/64"))
	a.Announce() // b sees a's GARP and unsolicited NA for its own addresses
	if !ev.has("address-conflict") || b.Status().Counters["address_conflicts"] != 2 {
		t.Fatalf("conflict not detected: %v", b.Status().Counters)
	}
}

func icmpEchoFrame(src, dst netip.Addr, id, seq uint16, data []byte) *pkt.Frame {
	ip := pkt.IPv4{TTL: 64, Protocol: pkt.ProtoICMP, Src: src, Dst: dst, Payload: pkt.ICMPEcho(pkt.ICMPEchoRequest, id, seq, data)}
	return &pkt.Frame{Dst: macA, Src: macB, EtherType: pkt.EtherTypeIPv4, Payload: append(ip.Encode(), 0, 0, 0)}
}

func TestICMPEcho(t *testing.T) {
	w := &wire{}
	a, _ := w.add(t, macA, mustCfg(t, "", "", "10.0.0.1/24"))
	src, dst := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.1")
	if !a.HandleRx(icmpEchoFrame(src, dst, 0x1234, 7, []byte("payload!"))) {
		t.Fatal("echo not consumed")
	}
	if len(w.sent) != 1 {
		t.Fatalf("sent %d frames", len(w.sent))
	}
	r := w.sent[0]
	h, err := pkt.DecodeIPv4(r.Payload)
	if err != nil || r.Dst != macB || h.Src != dst || h.Dst != src || h.Protocol != pkt.ProtoICMP {
		t.Fatalf("reply %+v %v", h, err)
	}
	m := h.Payload
	if m[0] != pkt.ICMPEchoReply || pkt.Checksum(m, 0) != 0 || binary.BigEndian.Uint16(m[4:]) != 0x1234 || binary.BigEndian.Uint16(m[6:]) != 7 || string(m[8:]) != "payload!" {
		t.Fatalf("icmp reply % x", m)
	}
	// Not ours, fragments and other protocols are not answered.
	if a.HandleRx(icmpEchoFrame(src, netip.MustParseAddr("10.0.0.9"), 1, 1, nil)) {
		t.Fatal("consumed echo for another address")
	}
	frag := pkt.IPv4{TTL: 64, Protocol: pkt.ProtoICMP, Flags: pkt.IPv4MF, Src: src, Dst: dst, Payload: pkt.ICMPEcho(pkt.ICMPEchoRequest, 1, 1, nil)}
	a.HandleRx(&pkt.Frame{Dst: macA, Src: macB, EtherType: pkt.EtherTypeIPv4, Payload: frag.Encode()})
	bad := pkt.IPv4{TTL: 64, Protocol: pkt.ProtoICMP, Src: src, Dst: dst, Payload: pkt.ICMPEcho(pkt.ICMPEchoRequest, 1, 1, nil), Checksum: 1, ChecksumSet: true}
	a.HandleRx(&pkt.Frame{Dst: macA, Src: macB, EtherType: pkt.EtherTypeIPv4, Payload: bad.Encode()})
	c := a.Status().Counters
	if len(w.sent) != 1 || c["icmp_echo_requests"] != 1 || c["icmp_echo_replies"] != 1 || c["ipv4_fragments"] != 1 || c["ipv4_errors"] != 1 {
		t.Fatalf("sent %d, counters %v", len(w.sent), c)
	}
}

func TestIPv6NDAndEcho(t *testing.T) {
	w := &wire{}
	a, _ := w.add(t, macA, mustCfg(t, "", "", "2001:db8::1/64"))
	b, _ := w.add(t, macB, mustCfg(t, "", "fe80::a", "2001:db8::2/64", "fe80::b/64"))
	// a gets an automatic link-local; b's explicit one is used as is.
	if st := a.Status(); len(st.Addresses) != 2 || st.Addresses[1] != pkt.LinkLocal(macA).String()+"/64" {
		t.Fatalf("a addresses %v", st.Addresses)
	}
	if st := b.Status(); len(st.Addresses) != 2 {
		t.Fatalf("b addresses %v", st.Addresses)
	}
	mac, _, err := a.Resolve(context.Background(), netip.MustParseAddr("2001:db8::2"))
	if err != nil || mac != macB {
		t.Fatalf("resolve global: %s %v", mac, err)
	}
	mac, _, err = a.Resolve(context.Background(), netip.MustParseAddr("fe80::b"))
	if err != nil || mac != macB {
		t.Fatalf("resolve link-local: %s %v", mac, err)
	}
	if n := len(b.Status().Neighbors); n != 2 {
		t.Fatalf("b learned %d neighbors", n)
	}

	src, dst := netip.MustParseAddr("2001:db8::2"), netip.MustParseAddr("2001:db8::1")
	req := append([]byte{pkt.ICMPv6EchoRequest, 0, 0, 0, 0xab, 0xcd, 0, 3}, "data"...)
	pkt.ICMPv6Checksum(src, dst, req)
	ip := pkt.IPv6{NextHeader: pkt.ProtoICMPv6, HopLimit: 64, Src: src, Dst: dst, Payload: req}
	w.mu.Lock()
	w.sent, w.drop = nil, true
	w.mu.Unlock()
	if !a.HandleRx(&pkt.Frame{Dst: macA, Src: macB, EtherType: pkt.EtherTypeIPv6, Payload: ip.Encode()}) {
		t.Fatal("echo not consumed")
	}
	if len(w.sent) != 1 {
		t.Fatalf("sent %d", len(w.sent))
	}
	h, err := pkt.DecodeIPv6(w.sent[0].Payload)
	if err != nil || h.Src != dst || h.Dst != src || h.Payload[0] != pkt.ICMPv6EchoReply || !pkt.ICMPv6Valid(h.Src, h.Dst, h.Payload) || string(h.Payload[8:]) != "data" {
		t.Fatalf("reply %+v %v", h, err)
	}

	// Gateway selection.
	if nh, err := b.NextHop(netip.MustParseAddr("2001:db8:ffff::1")); err != nil || nh.String() != "fe80::a" {
		t.Fatalf("next hop %s %v", nh, err)
	}
	if _, err := a.NextHop(netip.MustParseAddr("2001:db8:ffff::1")); err == nil {
		t.Fatal("next hop without gateway")
	}
}

func TestDADDefended(t *testing.T) {
	w := &wire{}
	a, ev := w.add(t, macA, mustCfg(t, "", "", "2001:db8::1/64"))
	target := netip.MustParseAddr("2001:db8::1")
	sn := pkt.SolicitedNode(target)
	msg := pkt.NDMessage(pkt.ICMPv6NeighborSol, 0, target, 0, pkt.MAC{})[:24] // no SLLA in DAD
	src := netip.IPv6Unspecified()
	pkt.ICMPv6Checksum(src, sn, msg)
	ip := pkt.IPv6{NextHeader: pkt.ProtoICMPv6, HopLimit: 255, Src: src, Dst: sn, Payload: msg}
	w.drop = true
	a.HandleRx(&pkt.Frame{Dst: pkt.MulticastMAC(sn), Src: macB, EtherType: pkt.EtherTypeIPv6, Payload: ip.Encode()})
	if len(w.sent) != 1 || w.sent[0].Dst != pkt.MulticastMAC(allNodes) || !ev.has("address-conflict") {
		t.Fatalf("sent %+v", w.sent)
	}
}

func TestDisabledStackIgnoresEverything(t *testing.T) {
	w := &wire{}
	a, _ := w.add(t, macA, Config{})
	r := pkt.ARP{Op: pkt.ARPRequest, SHA: macB, SPA: netip.MustParseAddr("10.0.0.2"), TPA: netip.MustParseAddr("10.0.0.1")}
	if a.HandleRx(&pkt.Frame{Dst: pkt.Broadcast, Src: macB, EtherType: pkt.EtherTypeARP, Payload: r.Encode()}) || len(w.sent) != 0 {
		t.Fatal("disabled stack answered")
	}
}

func signedUDP(src, dst netip.Addr, id uint32, seq uint64) *pkt.Frame {
	data := make([]byte, 24)
	copy(data, []byte("CLGN"))
	binary.BigEndian.PutUint32(data[4:], id)
	binary.BigEndian.PutUint64(data[8:], seq)
	binary.BigEndian.PutUint64(data[16:], uint64(time.Now().UnixNano()))
	u := make([]byte, 8+len(data))
	binary.BigEndian.PutUint16(u[0:], 1000)
	binary.BigEndian.PutUint16(u[2:], 2000)
	binary.BigEndian.PutUint16(u[4:], uint16(len(u)))
	copy(u[8:], data)
	binary.BigEndian.PutUint16(u[6:], pkt.Checksum(u, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(u))))
	ip := pkt.IPv4{TTL: 64, Protocol: pkt.ProtoUDP, Src: src, Dst: dst, Payload: u}
	return &pkt.Frame{Dst: macA, Src: macB, EtherType: pkt.EtherTypeIPv4, Payload: ip.Encode()}
}

func TestClaimedFlowsNotRecorded(t *testing.T) {
	w := &wire{}
	a, _ := w.add(t, macA, mustCfg(t, "", "", "10.0.0.1/24"))
	src, dst := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.1")
	release := a.ClaimFlow(7)
	release2 := a.ClaimFlow(7) // claims are counted
	a.HandleRx(signedUDP(src, dst, 7, 0))
	a.HandleRx(signedUDP(src, dst, 8, 0))
	if f := a.ReceivedFlows(); len(f) != 1 || f[0].FlowID != "00000008" {
		t.Fatalf("claimed flow recorded: %+v", f)
	}
	release()
	release() // idempotent
	a.HandleRx(signedUDP(src, dst, 7, 1))
	if len(a.ReceivedFlows()) != 1 {
		t.Fatal("released too early")
	}
	release2()
	a.HandleRx(signedUDP(src, dst, 7, 2))
	if len(a.ReceivedFlows()) != 2 {
		t.Fatal("released flow not recorded")
	}
}
