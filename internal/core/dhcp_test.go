package core_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
	"github.com/ripnet/clabgen/protocols/dhcp"
)

// A DHCPv4 and DHCPv6 server for the clients to talk to, on the other
// engine: v4 leases from 10.0.0.100, v6 addresses from 2001:db8::1000 and
// /56 prefixes from 2001:db8:100::, and router advertisements for
// 2001:db8::/64 in answer to solicitations.
func init() { protocol.Register(testServer{}) }

type testServerConfig struct {
	Lease  uint32 `json:"lease"`
	Nak    bool   `json:"nak"`    // NAK every Request
	NoPD   bool   `json:"no_pd"`  // NoPrefixAvail
	Silent bool   `json:"silent"` // answer nothing (a server gone)
}

type testServer struct{}

func (testServer) Name() string         { return "test-dhcp-server" }
func (testServer) Description() string  { return "test DHCP server" }
func (testServer) NewConfig() any       { return &testServerConfig{Lease: 3600} }
func (testServer) EtherTypes() []uint16 { return []uint16{pkt.EtherTypeIPv4, pkt.EtherTypeIPv6} }
func (testServer) Actions() []protocol.Action {
	return []protocol.Action{{Name: "silent"}, {Name: "talk"}}
}
func (testServer) Start(env protocol.Env, c any) (protocol.Session, error) {
	return &server{env: env, cfg: *c.(*testServerConfig), v4: map[pkt.MAC]int{}, v6: map[string]int{}, rx: map[string]int{}}, nil
}

type server struct {
	env protocol.Env
	cfg testServerConfig

	mu sync.Mutex
	v4 map[pkt.MAC]int // client → lease index
	v6 map[string]int  // DUID → binding index
	rx map[string]int  // messages received, by kind
}

var srvAddr = netip.MustParseAddr("10.0.0.1")

func (s *server) send(dstMAC pkt.MAC, src, dst netip.Addr, sport, dport uint16, data []byte) {
	u := make([]byte, 8+len(data))
	binary.BigEndian.PutUint16(u, sport)
	binary.BigEndian.PutUint16(u[2:], dport)
	binary.BigEndian.PutUint16(u[4:], uint16(len(u)))
	copy(u[8:], data)
	binary.BigEndian.PutUint16(u[6:], pkt.Checksum(u, pkt.PseudoSum(src, dst, pkt.ProtoUDP, len(u))))
	if src.Is4() {
		h := pkt.IPv4{TTL: 64, Protocol: pkt.ProtoUDP, Src: src, Dst: dst, Payload: u}
		s.env.Send(dstMAC, pkt.EtherTypeIPv4, h.Encode(), protocol.SendOpts{})
		return
	}
	h := pkt.IPv6{NextHeader: pkt.ProtoUDP, HopLimit: 64, Src: src, Dst: dst, Payload: u}
	s.env.Send(dstMAC, pkt.EtherTypeIPv6, h.Encode(), protocol.SendOpts{})
}

func (s *server) HandleRx(f *pkt.Frame) {
	s.mu.Lock()
	silent := s.cfg.Silent
	s.mu.Unlock()
	if silent {
		return
	}
	switch f.EtherType {
	case pkt.EtherTypeIPv4:
		h, err := pkt.DecodeIPv4(f.Payload)
		if err != nil || h.Protocol != pkt.ProtoUDP || len(h.Payload) < 8 || binary.BigEndian.Uint16(h.Payload[2:]) != dhcp.Port4Server {
			return
		}
		if m, err := dhcp.Decode4(h.Payload[8:]); err == nil && m.Op == dhcp.OpRequest {
			s.rx4(m)
		}
	case pkt.EtherTypeIPv6:
		h, err := pkt.DecodeIPv6(f.Payload)
		if err != nil {
			return
		}
		if h.NextHeader == pkt.ProtoICMPv6 && len(h.Payload) > 0 && h.Payload[0] == pkt.ICMPv6RouterSol {
			s.ra(f.Src, h.Src)
			return
		}
		if h.NextHeader != pkt.ProtoUDP || len(h.Payload) < 8 || binary.BigEndian.Uint16(h.Payload[2:]) != dhcp.Port6Server {
			return
		}
		if m, err := dhcp.Decode6(h.Payload[8:]); err == nil {
			s.rx6(f.Src, h.Src, m)
		}
	}
}

func (s *server) rx4(m *dhcp.Msg4) {
	names := map[uint8]string{dhcp.MsgDiscover: "discover", dhcp.MsgRequest: "request", dhcp.MsgRelease: "release"}
	s.mu.Lock()
	s.rx["v4_"+names[m.Type()]]++
	i, ok := s.v4[m.CHAddr]
	if !ok {
		i = len(s.v4)
		s.v4[m.CHAddr] = i
	}
	cfg := s.cfg
	s.mu.Unlock()
	reply := &dhcp.Msg4{Op: dhcp.OpReply, XID: m.XID, CHAddr: m.CHAddr, YIAddr: netip.AddrFrom4([4]byte{10, 0, 0, byte(100 + i)})}
	switch m.Type() {
	case dhcp.MsgDiscover:
		reply.Add(dhcp.OptMsgType, []byte{dhcp.MsgOffer})
	case dhcp.MsgRequest:
		if cfg.Nak {
			reply.YIAddr = netip.Addr{}
			reply.Add(dhcp.OptMsgType, []byte{dhcp.MsgNak})
			reply.Add(dhcp.OptMessage, []byte("no"))
			reply.Add(dhcp.OptServerID, srvAddr.AsSlice())
			s.send(m.CHAddr, srvAddr, netip.AddrFrom4([4]byte{255, 255, 255, 255}), 67, 68, reply.Encode())
			return
		}
		reply.Add(dhcp.OptMsgType, []byte{dhcp.MsgAck})
	default:
		return
	}
	reply.Add(dhcp.OptServerID, srvAddr.AsSlice())
	reply.Add(dhcp.OptLeaseTime, binary.BigEndian.AppendUint32(nil, cfg.Lease))
	reply.Add(dhcp.OptSubnetMask, []byte{255, 255, 255, 0})
	reply.Add(dhcp.OptRouter, srvAddr.AsSlice())
	reply.Add(dhcp.OptDNS, []byte{10, 0, 0, 53})
	s.send(m.CHAddr, srvAddr, reply.YIAddr, 67, 68, reply.Encode())
}

func (s *server) srcLL() netip.Addr { return pkt.LinkLocal(s.env.Device().MAC()) }

func (s *server) ra(mac pkt.MAC, dst netip.Addr) {
	m := make([]byte, 16, 48)
	m[0], m[4] = pkt.ICMPv6RouterAdv, 64
	m[5] = 0x80 // managed
	binary.BigEndian.PutUint16(m[6:], 1800)
	pi := make([]byte, 32)
	pi[0], pi[1], pi[2], pi[3] = 3, 4, 64, 0x80 // on-link, not autonomous
	binary.BigEndian.PutUint32(pi[4:], 86400)
	binary.BigEndian.PutUint32(pi[8:], 14400)
	p := netip.MustParseAddr("2001:db8::").As16()
	copy(pi[16:], p[:])
	m = append(m, pi...)
	if !dst.IsValid() || dst.IsUnspecified() {
		dst, mac = netip.MustParseAddr("ff02::1"), pkt.MAC{0x33, 0x33, 0, 0, 0, 1}
	}
	pkt.ICMPv6Checksum(s.srcLL(), dst, m)
	h := pkt.IPv6{NextHeader: pkt.ProtoICMPv6, HopLimit: 255, Src: s.srcLL(), Dst: dst, Payload: m}
	s.env.Send(mac, pkt.EtherTypeIPv6, h.Encode(), protocol.SendOpts{})
}

func (s *server) rx6(mac pkt.MAC, src netip.Addr, m *dhcp.Msg6) {
	names := map[uint8]string{dhcp.MsgSolicit: "solicit", dhcp.MsgRequest6: "request", dhcp.MsgRenew: "renew", dhcp.MsgRebind: "rebind", dhcp.MsgRelease6: "release"}
	duid := string(m.Get(dhcp.Opt6ClientID))
	s.mu.Lock()
	s.rx["v6_"+names[m.Type]]++
	i, ok := s.v6[duid]
	if !ok {
		i = len(s.v6)
		s.v6[duid] = i
	}
	cfg := s.cfg
	s.mu.Unlock()
	t := dhcp.MsgReply
	rapid := false
	switch m.Type {
	case dhcp.MsgSolicit:
		t, rapid = dhcp.MsgAdvertise, m.Has(dhcp.Opt6RapidCommit)
		if rapid {
			t = dhcp.MsgReply
		}
	case dhcp.MsgRequest6, dhcp.MsgRenew, dhcp.MsgRebind, dhcp.MsgRelease6:
	default:
		return
	}
	reply := &dhcp.Msg6{Type: t, XID: m.XID}
	reply.Add(dhcp.Opt6ClientID, []byte(duid))
	reply.Add(dhcp.Opt6ServerID, dhcp.DUIDLL(s.env.Device().MAC()))
	if rapid {
		reply.Add(dhcp.Opt6RapidCommit, nil)
	}
	if m.Type == dhcp.MsgRelease6 {
		reply.Add(dhcp.Opt6StatusCode, dhcp.Status6{Code: dhcp.StatusSuccess}.Encode())
	} else {
		for _, ia := range m.IAs() {
			out := dhcp.IA{Kind: ia.Kind, IAID: ia.IAID, T1: cfg.Lease / 2, T2: cfg.Lease * 4 / 5}
			if ia.Kind == dhcp.Opt6IANA {
				a := netip.MustParseAddr("2001:db8::1000").As16()
				a[15] += byte(i)
				out.Addrs = []dhcp.IAAddr{{Addr: netip.AddrFrom16(a), Preferred: cfg.Lease, Valid: cfg.Lease * 2}}
			} else if cfg.NoPD {
				out.T1, out.T2 = 0, 0
				out.Status = &dhcp.Status6{Code: dhcp.StatusNoPrefixAvail, Message: "none left"}
			} else {
				p := netip.MustParseAddr("2001:db8:100::").As16()
				p[5] = byte(i)
				out.Prefixes = []dhcp.IAPrefix{{Prefix: netip.PrefixFrom(netip.AddrFrom16(p), 56), Preferred: cfg.Lease, Valid: cfg.Lease * 2}}
			}
			reply.Options = append(reply.Options, out.Encode())
		}
		reply.Add(dhcp.Opt6DNS, netip.MustParseAddr("2001:db8::53").AsSlice())
		reply.Add(dhcp.Opt6DomainList, []byte("\x03lab\x00"))
	}
	s.send(mac, s.srcLL(), src, dhcp.Port6Server, dhcp.Port6Client, reply.Encode())
}

func (s *server) Status() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.rx {
		out[k] = v
	}
	return out
}

func (s *server) Action(_ context.Context, name string, _ any) (any, error) {
	s.mu.Lock()
	s.cfg.Silent = name == "silent"
	s.mu.Unlock()
	return nil, nil
}
func (s *server) Stop() {}

// dhcpPair: engine b runs the server on device "srv" (VLAN 10), engine a
// has n client devices c0… on the same VLAN, without addresses.
func dhcpPair(t *testing.T, n int, srvCfg string) (*core.Engine, *core.Engine) {
	t.Helper()
	a, b, _, _ := newPair()
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "srv", Port: "eth1", MAC: "02:00:00:00:0b:01", VLANs: []pkt.VLANTag{{VID: 10}},
		Addresses: []string{"10.0.0.1/24", "2001:db8::1/64"}, Gateway6: "2001:db8::1000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateProtocol(core.ProtocolSpec{Name: "server", Type: "test-dhcp-server", Device: "srv", Config: json.RawMessage(srvCfg)}); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if _, err := a.CreateDevice(core.DeviceSpec{Name: fmt.Sprintf("c%d", i), Port: "eth1", MAC: fmt.Sprintf("02:00:00:00:0a:%02x", i+1), VLANs: []pkt.VLANTag{{VID: 10}}}); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

func mkProto(t *testing.T, e *core.Engine, name, typ, dev, cfg string) {
	t.Helper()
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: typ, Device: dev, Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

func device(t *testing.T, e *core.Engine, name string) core.DeviceSpec {
	t.Helper()
	for _, d := range e.Devices() {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no device %s", name)
	return core.DeviceSpec{}
}

// v6Bound: an address and a prefix, the state settled.
func v6Bound(t *testing.T, e *core.Engine, name string) bool {
	t.Helper()
	st := flowStatus(t, e, name)
	return st["state"] == "bound" && st["addresses"] != nil && st["prefixes"] != nil
}

func TestDHCPv4(t *testing.T) {
	a, b := dhcpPair(t, 3, `{"lease": 4}`)
	for i := range 3 {
		mkProto(t, a, fmt.Sprintf("dhcp%d", i), "dhcp", fmt.Sprintf("c%d", i), `{"hostname": "cpe"}`)
	}
	eventually(t, "all bound", func() bool {
		for i := range 3 {
			if flowStatus(t, a, fmt.Sprintf("dhcp%d", i))["state"] != "bound" {
				return false
			}
		}
		return true
	})
	var got []string
	for i := range 3 {
		st := flowStatus(t, a, fmt.Sprintf("dhcp%d", i))
		got = append(got, st["address"].(string))
		if st["gateway"] != "10.0.0.1" || st["server"] != "10.0.0.1" || fmt.Sprint(st["dns"]) != "[10.0.0.53]" {
			t.Fatalf("dhcp%d: %v", i, st)
		}
		d := device(t, a, fmt.Sprintf("c%d", i))
		if !slices.Equal(d.DynamicAddresses, []string{st["address"].(string)}) || d.DynamicGateway4 != "10.0.0.1" || len(d.Addresses) != 0 {
			t.Fatalf("device c%d: %+v", i, d)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"10.0.0.100/24", "10.0.0.101/24", "10.0.0.102/24"}) {
		t.Fatalf("leases %v", got)
	}

	// The device answers on its lease: the server side resolves and pings it.
	addr := flowStatus(t, a, "dhcp0")["address"].(string)
	addr = netip.MustParsePrefix(addr).Addr().String()
	mkProto(t, b, "ping", "icmp", "srv", `{"destination": "`+addr+`", "rate": 50, "auto_start": true}`)
	defer func() {
		if t.Failed() {
			t.Logf("ping: %v", flowStatus(t, b, "ping"))
		}
	}()
	eventually(t, "ping answered", func() bool { return num(flowStatus(t, b, "ping"), "rx") >= 10 })

	// A 4 s lease renews at 2 s, unicast to the server.
	eventuallyLong(t, "renewed", func() bool { return num(flowStatus(t, a, "dhcp0")["counters"].(map[string]any), "rx_ack") >= 2 })
	if st := flowStatus(t, a, "dhcp0"); st["address"] != addr+"/24" {
		t.Fatalf("after renewal: %v", st)
	}

	// Release takes the address off the device and tells the server.
	if _, err := a.RunAction(context.Background(), "dhcp1", "release", nil); err != nil {
		t.Fatal(err)
	}
	if d := device(t, a, "c1"); len(d.DynamicAddresses) != 0 || d.DynamicGateway4 != "" {
		t.Fatalf("released: %+v", d)
	}
	eventually(t, "server got the release", func() bool { return flowStatus(t, b, "server")["v4_release"] == 1.0 })
	if st := flowStatus(t, a, "dhcp1"); st["state"] != "released" {
		t.Fatalf("%v", st)
	}

	// Without the server the lease runs out at 4 s and the address goes.
	if _, err := b.RunAction(context.Background(), "server", "silent", nil); err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "expired", func() bool { return len(device(t, a, "c2").DynamicAddresses) == 0 })
	if st := flowStatus(t, a, "dhcp2"); st["state"] != "selecting" || st["address"] != nil {
		t.Fatalf("%v", st)
	}
}

func TestDHCPv4NakAndRapidCommit(t *testing.T) {
	a, _ := dhcpPair(t, 2, `{"nak": true}`)
	mkProto(t, a, "dhcp", "dhcp", "c0", `{}`)
	eventually(t, "nak", func() bool { return num(flowStatus(t, a, "dhcp")["counters"].(map[string]any), "rx_nak") >= 1 })
	if st := flowStatus(t, a, "dhcp"); st["last_nak"] != "no" || st["address"] != nil {
		t.Fatalf("%v", st)
	}
	// The test server doesn't do rapid commit, so the client falls back to
	// the four-message exchange... and is NAKed again.
	mkProto(t, a, "rc", "dhcp", "c1", `{"rapid_commit": true}`)
	eventually(t, "offered", func() bool { return num(flowStatus(t, a, "rc")["counters"].(map[string]any), "rx_offer") >= 1 })
}

func TestDHCPv6(t *testing.T) {
	a, b := dhcpPair(t, 3, `{"lease": 4}`)
	for i := range 3 {
		mkProto(t, a, fmt.Sprintf("v6-%d", i), "dhcpv6", fmt.Sprintf("c%d", i), `{"ia_pd": true, "pd_length": 56, "pd_addresses": [{"subnet": 1, "host": 1}]}`)
	}
	eventually(t, "all bound", func() bool {
		for i := range 3 {
			if !v6Bound(t, a, fmt.Sprintf("v6-%d", i)) {
				return false
			}
		}
		return true
	})
	srvLL := pkt.LinkLocal(pkt.MAC{2, 0, 0, 0, 0x0b, 1}).String()
	seen := map[string]bool{}
	for i := range 3 {
		st := flowStatus(t, a, fmt.Sprintf("v6-%d", i))
		addrs, prefixes := st["addresses"].([]any), st["prefixes"].([]any)
		if len(addrs) != 1 || len(prefixes) != 1 || st["gateway"] != srvLL || fmt.Sprint(st["domains"]) != "[lab]" {
			t.Fatalf("v6-%d: %v", i, st)
		}
		addr, prefix := addrs[0].(map[string]any)["prefix"].(string), prefixes[0].(map[string]any)["prefix"].(string)
		seen[addr], seen[prefix] = true, true
		d := device(t, a, fmt.Sprintf("c%d", i))
		// The address takes the RA's on-link /64; the link-local is there too.
		if !slices.Contains(d.DynamicAddresses, addr+"/64") || d.DynamicGateway6 != srvLL {
			t.Fatalf("device c%d: %+v", i, d)
		}
	}
	if len(seen) != 6 || !seen["2001:db8::1002"] || !seen["2001:db8:102::/56"] {
		t.Fatalf("bindings %v", seen)
	}

	// A host in the first client's delegated prefix answers, routed via
	// the client (the server's gateway6 is that client's address).
	var host string
	for i := range 3 {
		st := flowStatus(t, a, fmt.Sprintf("v6-%d", i))
		if st["addresses"].([]any)[0].(map[string]any)["prefix"] == "2001:db8::1000" {
			host = st["pd_addresses"].([]any)[0].(string)
		}
	}
	if host == "" || !netip.MustParsePrefix("2001:db8:100::/40").Contains(netip.MustParseAddr(host)) {
		t.Fatalf("pd host %q", host)
	}
	mkProto(t, b, "ping", "icmp", "srv", `{"destination": "`+host+`", "rate": 50, "auto_start": true}`)
	eventually(t, "pd host answers", func() bool { return num(flowStatus(t, b, "ping"), "rx") >= 10 })

	// T1 is 2 s: renewed with the same bindings.
	eventuallyLong(t, "renewed", func() bool { return num(flowStatus(t, b, "server"), "v6_renew") >= 3 })
	eventually(t, "replies taken", func() bool { return v6Bound(t, a, "v6-0") })

	if _, err := a.RunAction(context.Background(), "v6-1", "release", nil); err != nil {
		t.Fatal(err)
	}
	d := device(t, a, "c1")
	for _, x := range d.DynamicAddresses {
		if !netip.MustParsePrefix(x).Addr().IsLinkLocalUnicast() {
			t.Fatalf("released, still has %v", d.DynamicAddresses)
		}
	}
	eventually(t, "server got the release", func() bool { return flowStatus(t, b, "server")["v6_release"] == 1.0 })

	// Deleting the protocol leaves the device as configured: no addresses.
	if err := a.DeleteProtocol("v6-2"); err != nil {
		t.Fatal(err)
	}
	if d := device(t, a, "c2"); len(d.DynamicAddresses) != 0 || d.DynamicGateway6 != "" {
		t.Fatalf("deleted: %+v", d)
	}
}

func TestDHCPv6NoPrefix(t *testing.T) {
	a, _ := dhcpPair(t, 1, `{"no_pd": true}`)
	mkProto(t, a, "v6", "dhcpv6", "c0", `{"ia_pd": true, "rapid_commit": true}`)
	eventually(t, "bound", func() bool { return flowStatus(t, a, "v6")["state"] == "bound" })
	st := flowStatus(t, a, "v6")
	if st["ia_pd_status"] != "NoPrefixAvail none left" || len(st["addresses"].([]any)) != 1 || st["prefixes"] != nil {
		t.Fatalf("%v", st)
	}
	// Rapid commit: a Solicit and a Reply, no Request.
	c := st["counters"].(map[string]any)
	if num(c, "tx_request") != 0 || num(c, "rx_reply") != 1 {
		t.Fatalf("counters %v", c)
	}
}

// clabgen's own servers with its clients, on one link.
func TestDHCPServers(t *testing.T) {
	a, b, _, _ := newPair()
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "srv", Port: "eth1", MAC: "02:00:00:00:0b:01", VLANs: []pkt.VLANTag{{VID: 10}},
		Addresses: []string{"10.0.0.1/24", "2001:db8::1/64"}}); err != nil {
		t.Fatal(err)
	}
	mkProto(t, b, "srv4", "dhcp-server", "srv", `{"lease_time": 4, "pools": [{"subnet": "10.0.0.0/24", "start": "10.0.0.100", "end": "10.0.0.199", "router": "10.0.0.1"}]}`)
	mkProto(t, b, "srv6", "dhcpv6-server", "srv", `{"preferred": 4, "valid": 8, "pools": [{"link": "2001:db8::/64", "start": "2001:db8::1000", "end": "2001:db8::1fff",
		"pd_prefix": "2001:db8:100::/40", "pd_length": 56}]}`)
	for i := range 3 {
		dev := fmt.Sprintf("c%d", i)
		if _, err := a.CreateDevice(core.DeviceSpec{Name: dev, Port: "eth1", MAC: fmt.Sprintf("02:00:00:00:0a:%02x", i+1), VLANs: []pkt.VLANTag{{VID: 10}}}); err != nil {
			t.Fatal(err)
		}
		mkProto(t, a, dev+"-v4", "dhcp", dev, `{}`)
		mkProto(t, a, dev+"-v6", "dhcpv6", dev, `{"ia_pd": true, "ra": false}`)
	}
	defer func() {
		if t.Failed() {
			t.Logf("c0-v4 %v", flowStatus(t, a, "c0-v4"))
			t.Logf("c0-v6 %v", flowStatus(t, a, "c0-v6"))
			t.Logf("srv4 %v", flowStatus(t, b, "srv4")["counters"])
			t.Logf("srv6 %v", flowStatus(t, b, "srv6")["counters"])
		}
	}()
	eventually(t, "all bound", func() bool {
		for i := range 3 {
			if flowStatus(t, a, fmt.Sprintf("c%d-v4", i))["state"] != "bound" || !v6Bound(t, a, fmt.Sprintf("c%d-v6", i)) {
				return false
			}
		}
		return true
	})
	pools := func(name string) map[string]any { return flowStatus(t, b, name)["pools"].([]any)[0].(map[string]any) }
	if p := pools("srv4"); p["bound"] != 3.0 {
		t.Fatalf("v4 pool %v", p)
	}
	if p := pools("srv6"); p["addresses_bound"] != 3.0 || p["prefixes_bound"] != 3.0 {
		t.Fatalf("v6 pool %v", p)
	}
	if st := flowStatus(t, a, "c0-v4"); st["gateway"] != "10.0.0.1" || st["server"] != "10.0.0.1" {
		t.Fatalf("c0: %v", st)
	}

	// Renewals at T1 (2 s) keep the same leases.
	before := flowStatus(t, a, "c1-v6")["prefixes"]
	eventuallyLong(t, "renewed", func() bool {
		return num(flowStatus(t, b, "srv4")["counters"].(map[string]any), "rx_request") >= 6 &&
			num(flowStatus(t, b, "srv6")["counters"].(map[string]any), "rx_renew") >= 3
	})
	eventually(t, "still bound", func() bool { return v6Bound(t, a, "c1-v6") })
	if after := flowStatus(t, a, "c1-v6")["prefixes"]; fmt.Sprint(after.([]any)[0].(map[string]any)["prefix"]) != fmt.Sprint(before.([]any)[0].(map[string]any)["prefix"]) {
		t.Fatalf("prefix changed on renewal: %v → %v", before, after)
	}

	// A release frees the lease on the server.
	if _, err := a.RunAction(context.Background(), "c2-v4", "release", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "server freed it", func() bool { return pools("srv4")["bound"] == 2.0 })

	// Paused, the server lets the 4 s leases run out.
	if _, err := b.RunAction(context.Background(), "srv4", "pause", nil); err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "expired", func() bool { return flowStatus(t, a, "c0-v4")["address"] == nil })
	if _, err := b.RunAction(context.Background(), "srv4", "resume", nil); err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "bound again", func() bool { return flowStatus(t, a, "c0-v4")["state"] == "bound" })
}
