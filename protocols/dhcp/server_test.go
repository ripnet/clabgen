package dhcp

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net/netip"
	"sync"
	"testing"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// A server's device, for driving handle directly: its addresses, and the
// frames it sends.
type ipStub = protocol.IP

type fakeEnv struct {
	protocol.Env // the rest panics: not used
	ipStub
	mac   pkt.MAC
	addrs []netip.Prefix

	mu   sync.Mutex
	sent []sentUDP
}

type sentUDP struct {
	mac      pkt.MAC
	src, dst netip.Addr
	dport    uint16
	data     []byte
}

type fakeDevice struct{ mac pkt.MAC }

func (d fakeDevice) Name() string         { return "srv" }
func (d fakeDevice) Port() string         { return "eth1" }
func (d fakeDevice) MAC() pkt.MAC         { return d.mac }
func (d fakeDevice) VLANs() []pkt.VLANTag { return nil }
func (d fakeDevice) PortUp() bool         { return true }

var relayMAC = pkt.MAC{2, 0, 0, 0, 0, 0xee}

func (e *fakeEnv) Device() protocol.Device             { return fakeDevice{e.mac} }
func (e *fakeEnv) IP() protocol.IP                     { return e }
func (e *fakeEnv) Emit(string, string, map[string]any) {}
func (e *fakeEnv) Log() *slog.Logger                   { return slog.Default() }
func (e *fakeEnv) Addresses() []netip.Prefix           { return e.addrs }
func (e *fakeEnv) ResolveNextHop(context.Context, netip.Addr) (netip.Addr, pkt.MAC, error) {
	return netip.Addr{}, relayMAC, nil
}
func (e *fakeEnv) Source(dst netip.Addr) (netip.Addr, bool) {
	for _, p := range e.addrs {
		if p.Addr().Is4() == dst.Is4() && p.Addr().IsLinkLocalUnicast() == dst.IsLinkLocalUnicast() {
			return p.Addr(), true
		}
	}
	return netip.Addr{}, false
}
func (e *fakeEnv) Send(dst pkt.MAC, et uint16, b []byte, _ protocol.SendOpts) error {
	f := &pkt.Frame{Dst: dst, EtherType: et, Payload: b}
	var dport uint16
	for _, port := range []uint16{Port4Server, Port4Client, Port6Server, Port6Client} {
		if src, d, data, ok := udpOf(f, port); ok {
			dport = port
			e.mu.Lock()
			e.sent = append(e.sent, sentUDP{dst, src, d, dport, append([]byte(nil), data...)})
			e.mu.Unlock()
			return nil
		}
	}
	return nil
}

func (e *fakeEnv) take(t *testing.T) sentUDP {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(e.sent))
	}
	s := e.sent[0]
	e.sent = nil
	return s
}

func newEnv(addrs ...string) *fakeEnv {
	e := &fakeEnv{mac: pkt.MAC{2, 0, 0, 0, 0, 1}}
	for _, a := range addrs {
		e.addrs = append(e.addrs, netip.MustParsePrefix(a))
	}
	return e
}

func TestServer4Relayed(t *testing.T) {
	env := newEnv("10.9.0.1/24")
	cfg := &ServerConfig4{LeaseTime: 600, Pools: []Pool4{
		{Subnet: "10.1.0.0/24", Start: "10.1.0.10", End: "10.1.0.20", Router: "10.1.0.1"},
		{Subnet: "10.2.0.0/24", Start: "10.2.0.10", End: "10.2.0.11", Router: "10.2.0.1", DNS: []string{"10.9.0.53"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sess, err := server4Type{}.Start(env, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Stop()
	s := sess.(*server4)
	client := pkt.MAC{2, 0, 0, 0, 0xc, 1}
	ri := []byte{SubCircuitID, 8, 'e', 't', 'h', '1', ':', '1', '0', '0', SubRemoteID, 4, 'b', 'n', 'g', '1'}
	msg := func(t uint8, mod func(*Msg4)) []byte {
		m := &Msg4{Op: OpRequest, XID: 7, Hops: 2, GIAddr: netip.MustParseAddr("10.2.0.1"), CHAddr: client}
		m.Add(OptMsgType, []byte{t})
		if mod != nil {
			mod(m)
		}
		m.Add(OptRelayInfo, ri)
		return m.Encode()
	}
	relay := netip.MustParseAddr("10.2.0.1")

	// A relayed Discover: an Offer from the giaddr's pool, to the relay's
	// server port, with option 82 echoed.
	s.handle(request{src: relay, dst: netip.MustParseAddr("10.9.0.1"), data: msg(MsgDiscover, nil)})
	out := env.take(t)
	if out.dst != relay || out.dport != Port4Server || out.mac != relayMAC || out.src != netip.MustParseAddr("10.9.0.1") {
		t.Fatalf("sent %+v", out)
	}
	offer, _ := Decode4(out.data)
	if offer.Type() != MsgOffer || offer.YIAddr != netip.MustParseAddr("10.2.0.10") || offer.GIAddr != relay ||
		string(offer.Get(OptRelayInfo)) != string(ri) || offer.Addrs(OptDNS)[0] != netip.MustParseAddr("10.9.0.53") {
		t.Fatalf("offer %+v", offer)
	}

	// Request → Ack; the lease shows how it was relayed.
	s.handle(request{src: relay, data: msg(MsgRequest, func(m *Msg4) {
		m.Add(OptServerID, offer.Addr(OptServerID).AsSlice())
		m.Add(OptRequestedIP, offer.YIAddr.AsSlice())
	})})
	if ack, _ := Decode4(env.take(t).data); ack.Type() != MsgAck || ack.YIAddr != offer.YIAddr {
		t.Fatalf("ack %+v", ack)
	}
	st := s.status(true)
	if len(st.Leases) != 1 || st.Leases[0].Relay == nil || st.Leases[0].Relay.CircuitID != "eth1:100" || st.Leases[0].Relay.RemoteID != "bng1" ||
		st.Leases[0].Relay.Hops != 2 || st.Pools[1].Bound != 1 {
		t.Fatalf("status %+v", st)
	}

	// A renewal unicast straight to the server: answered to the client
	// (through the router it came from), the relay chain kept.
	renew := &Msg4{Op: OpRequest, XID: 8, CIAddr: offer.YIAddr, CHAddr: client}
	renew.Add(OptMsgType, []byte{MsgRequest})
	s.handle(request{src: offer.YIAddr, srcMAC: relayMAC, data: renew.Encode()})
	out = env.take(t)
	if ack, _ := Decode4(out.data); ack.Type() != MsgAck || out.dst != offer.YIAddr || out.dport != Port4Client {
		t.Fatalf("renewal answer %+v", out)
	}
	if r := s.status(true).Leases[0].Relay; r == nil || r.CircuitID != "eth1:100" {
		t.Fatalf("relay after renewal %+v", r)
	}

	// Link selection overrides giaddr.
	other := pkt.MAC{2, 0, 0, 0, 0xc, 2}
	m := &Msg4{Op: OpRequest, XID: 9, GIAddr: relay, CHAddr: other}
	m.Add(OptMsgType, []byte{MsgDiscover})
	m.Add(OptRelayInfo, []byte{SubLinkSel, 4, 10, 1, 0, 0})
	s.handle(request{src: relay, data: m.Encode()})
	if o, _ := Decode4(env.take(t).data); o.YIAddr != netip.MustParseAddr("10.1.0.10") || o.Addrs(OptRouter)[0] != netip.MustParseAddr("10.1.0.1") {
		t.Fatalf("link selection: %+v", o)
	}

	// The 10.2 pool has two addresses: a third client gets the second,
	// a fourth nothing.
	for i, want := range []string{"10.2.0.11", ""} {
		m := &Msg4{Op: OpRequest, XID: 10, GIAddr: relay, CHAddr: pkt.MAC{2, 0, 0, 0, 0xd, byte(i)}}
		m.Add(OptMsgType, []byte{MsgDiscover})
		s.handle(request{src: relay, data: m.Encode()})
		env.mu.Lock()
		n := len(env.sent)
		env.mu.Unlock()
		if want == "" {
			if n != 0 || s.status(false).Counters["pool_exhausted"] != 1 {
				t.Fatalf("exhausted pool answered")
			}
			continue
		}
		if o, _ := Decode4(env.take(t).data); o.YIAddr.String() != want {
			t.Fatalf("got %s", o.YIAddr)
		}
	}

	// A relay from a subnet without a pool is not answered.
	m = &Msg4{Op: OpRequest, XID: 11, GIAddr: netip.MustParseAddr("10.3.0.1"), CHAddr: other}
	m.Add(OptMsgType, []byte{MsgDiscover})
	s.handle(request{src: m.GIAddr, data: m.Encode()})
	if len(env.sent) != 0 || s.status(false).Counters["no_pool"] != 1 {
		t.Fatal("answered without a pool")
	}
}

func TestServer6TwoRelays(t *testing.T) {
	env := newEnv("2001:db8:ff::1/64", "fe80::1/64")
	cfg := &ServerConfig6{Preferred: 600, Valid: 900, Pools: []Pool6{
		{Link: "2001:db8:1::/64", Start: "2001:db8:1::100", End: "2001:db8:1::1ff"},
		{Link: "2001:db8:2::/64", Start: "2001:db8:2::100", End: "2001:db8:2::1ff", PDPrefix: "2001:db8:200::/40", PDLength: 56,
			DNS: []string{"2001:db8::53"}, Domains: []string{"lab.example"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sess, err := server6Type{}.Start(env, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Stop()
	s := sess.(*server6)
	duid := DUIDLL(pkt.MAC{2, 0, 0, 0, 0xc, 1})
	outer := netip.MustParseAddr("2001:db8:ff::2")
	remote := binary.BigEndian.AppendUint32(nil, 6527)
	remote = append(remote, "cpe-7"...)
	// The client's message, relayed by the access relay (link 2001:db8:2::1)
	// and then by an aggregation relay toward the server.
	wrap := func(m *Msg6) []byte {
		inner := &Relay6{Type: MsgRelayForw, Hops: 0, Link: netip.MustParseAddr("2001:db8:2::1"), Peer: netip.MustParseAddr("fe80::c1"),
			Options: []Option6{{Opt6InterfaceID, []byte("ge-0/0/1.100")}, {Opt6RemoteID, remote},
				{Opt6ClientLLAddr, []byte{0, 1, 2, 0, 0, 0, 0xc, 1}}, {Opt6RelayMsg, m.Encode()}}}
		out := &Relay6{Type: MsgRelayForw, Hops: 1, Link: netip.IPv6Unspecified(), Peer: netip.MustParseAddr("2001:db8:2::1"),
			Options: []Option6{{Opt6InterfaceID, []byte("uplink")}, {Opt6RelayMsg, inner.Encode()}}}
		return out.Encode()
	}
	sol := &Msg6{Type: MsgSolicit, XID: 0x123}
	sol.Add(Opt6ClientID, duid)
	sol.Options = append(sol.Options, IA{Kind: Opt6IANA, IAID: 1}.Encode(), IA{Kind: Opt6IAPD, IAID: 1}.Encode())
	s.handle(request{src: outer, dst: netip.MustParseAddr("2001:db8:ff::1"), data: wrap(sol)})
	out := env.take(t)
	if out.dst != outer || out.dport != Port6Server || out.src != netip.MustParseAddr("2001:db8:ff::1") {
		t.Fatalf("sent %+v", out)
	}
	relays, adv, err := Unwrap6(out.data)
	if err != nil || len(relays) != 2 || relays[0].Type != MsgRelayRepl {
		t.Fatalf("reply %v %v", relays, err)
	}
	if string(relays[0].Get(Opt6InterfaceID)) != "uplink" || string(relays[1].Get(Opt6InterfaceID)) != "ge-0/0/1.100" ||
		relays[1].Peer != netip.MustParseAddr("fe80::c1") || relays[1].Get(Opt6RemoteID) != nil {
		t.Fatalf("relay-replies %+v %+v", relays[0], relays[1])
	}
	ias := adv.IAs()
	if adv.Type != MsgAdvertise || len(ias) != 2 || ias[0].Addrs[0].Addr != netip.MustParseAddr("2001:db8:2::100") ||
		ias[1].Prefixes[0].Prefix != netip.MustParsePrefix("2001:db8:200::/56") || ias[0].T1 != 300 {
		t.Fatalf("advertise %+v", ias)
	}

	req := &Msg6{Type: MsgRequest6, XID: 0x124}
	req.Add(Opt6ClientID, duid)
	req.Add(Opt6ServerID, adv.Get(Opt6ServerID))
	req.Options = append(req.Options, ias[0].Encode(), ias[1].Encode())
	s.handle(request{src: outer, data: wrap(req)})
	_, reply, _ := Unwrap6(env.take(t).data)
	if reply.Type != MsgReply || len(reply.IAs()) != 2 || string(reply.Get(Opt6DomainList)) != "\x03lab\x07example\x00" {
		t.Fatalf("reply %+v", reply)
	}
	st := s.status(true)
	if len(st.Bindings) != 2 || st.Pools[1].Bound != 1 || st.Pools[1].Delegated != 1 {
		t.Fatalf("status %+v", st)
	}
	hops := st.Bindings[1].Relays
	if len(hops) != 2 || hops[1].InterfaceID != "ge-0/0/1.100" || hops[1].RemoteID != "6527:cpe-7" || hops[1].Link != "2001:db8:2::1" || hops[0].InterfaceID != "uplink" ||
		hops[1].ClientLLAddr != "02:00:00:00:0c:01" || len(hops[1].Other) != 0 {
		t.Fatalf("relays %+v", hops)
	}

	// Renew keeps them; a Request for another server is ignored.
	ren := &Msg6{Type: MsgRenew, XID: 0x125}
	ren.Add(Opt6ClientID, duid)
	ren.Add(Opt6ServerID, adv.Get(Opt6ServerID))
	ren.Options = append(ren.Options, ias[0].Encode())
	s.handle(request{src: outer, data: wrap(ren)})
	if _, r, _ := Unwrap6(env.take(t).data); r.IAs()[0].Addrs[0].Addr != netip.MustParseAddr("2001:db8:2::100") {
		t.Fatalf("renew %+v", r.IAs())
	}
	req.Options[1] = Option6{Opt6ServerID, []byte("someone else")}
	s.handle(request{src: outer, data: wrap(req)})
	if len(env.sent) != 0 {
		t.Fatal("answered a Request for another server")
	}

	// The other link's pool gives addresses but no prefixes.
	sol2 := &Msg6{Type: MsgSolicit, XID: 0x200}
	sol2.Add(Opt6ClientID, DUIDLL(pkt.MAC{2, 0, 0, 0, 0xc, 2}))
	sol2.Options = append(sol2.Options, IA{Kind: Opt6IAPD, IAID: 1}.Encode())
	r1 := &Relay6{Type: MsgRelayForw, Link: netip.MustParseAddr("2001:db8:1::1"), Peer: netip.MustParseAddr("fe80::c2"), Options: []Option6{{Opt6RelayMsg, sol2.Encode()}}}
	s.handle(request{src: netip.MustParseAddr("2001:db8:1::1"), data: r1.Encode()})
	relays, adv2, _ := Unwrap6(env.take(t).data)
	if len(relays) != 1 || relays[0].Get(Opt6InterfaceID) != nil || adv2.IAs()[0].Status == nil || adv2.IAs()[0].Status.Code != StatusNoPrefixAvail {
		t.Fatalf("no-PD pool: %+v", adv2.IAs())
	}
}
