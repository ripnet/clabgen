package ipstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/ripnet/clabgen/internal/pkt"
)

// TCP comes from gVisor's netstack, one per device, created on first use.
// We feed it the TCP packets addressed to the device (or its local
// addresses) and send the IP packets it emits ourselves, resolving the next
// hop with our ARP/ND cache. Everything else (ARP, ND, ICMP, UDP) stays in
// this package.

const nicID tcpip.NICID = 1

type tcpStack struct {
	st     *stack.Stack
	ep     *channel.Endpoint
	cancel context.CancelFunc
	addrs  map[netip.Addr]bool // addresses given to the netstack
}

// tcpLocked returns the device's netstack, creating it. Caller holds mu.
func (s *Stack) tcpLocked() *tcpStack {
	if s.tcp != nil {
		return s.tcp
	}
	st := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	ep := channel.New(256, 1500, "")
	if err := st.CreateNIC(nicID, ep); err != nil {
		panic("ipstack: netstack NIC: " + err.String())
	}
	// Routing is ours: the netstack sends everything to its one NIC.
	st.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}, {Destination: header.IPv6EmptySubnet, NIC: nicID}})
	sack := tcpip.TCPSACKEnabled(true)
	st.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)
	ctx, cancel := context.WithCancel(context.Background())
	s.tcp = &tcpStack{st: st, ep: ep, cancel: cancel, addrs: map[netip.Addr]bool{}}
	s.syncTCPAddrsLocked()
	go s.tcpOut(ctx, ep)
	return s.tcp
}

// syncTCPAddrsLocked gives the netstack the device's addresses and local
// addresses. Caller holds mu.
func (s *Stack) syncTCPAddrsLocked() {
	t := s.tcp
	if t == nil {
		return
	}
	want := map[netip.Addr]netip.Prefix{}
	for _, p := range s.addrs {
		want[p.Addr()] = p
	}
	for a := range s.locals {
		if _, ok := want[a]; !ok {
			want[a] = netip.PrefixFrom(a, a.BitLen())
		}
	}
	for a := range t.addrs {
		if _, ok := want[a]; !ok {
			t.st.RemoveAddress(nicID, tcpip.AddrFromSlice(a.AsSlice()))
			delete(t.addrs, a)
		}
	}
	for a, p := range want {
		if t.addrs[a] {
			continue
		}
		proto := ipv4.ProtocolNumber
		if a.Is6() {
			proto = ipv6.ProtocolNumber
		}
		pa := tcpip.ProtocolAddress{Protocol: proto, AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFromSlice(a.AsSlice()), PrefixLen: p.Bits()}}
		if err := t.st.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err == nil {
			t.addrs[a] = true
		}
	}
}

func (t *tcpStack) close() {
	t.cancel()
	t.ep.Close()
	t.st.Close()
}

// rxTCP hands a TCP packet (the whole IP packet) to the netstack, if the
// device has one. It reports whether it took the packet.
func (s *Stack) rxTCP(v6 bool, ip []byte) bool {
	s.mu.Lock()
	t := s.tcp
	s.mu.Unlock()
	if t == nil {
		return false
	}
	proto := ipv4.ProtocolNumber
	if v6 {
		proto = ipv6.ProtocolNumber
	}
	p := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), ip...))})
	t.ep.InjectInbound(proto, p)
	p.DecRef()
	s.counters.tcpRx.Add(1)
	return true
}

// tcpOut sends what the netstack emits. Only TCP leaves (the netstack's own
// ND/MLD/ICMP stays inside: this package does those).
func (s *Stack) tcpOut(ctx context.Context, ep *channel.Endpoint) {
	for {
		p := ep.ReadContext(ctx)
		if p == nil {
			return
		}
		v := p.ToView()
		b := append([]byte(nil), v.AsSlice()...)
		v.Release()
		p.DecRef()
		var dst netip.Addr
		et := pkt.EtherTypeIPv4
		switch {
		case len(b) >= 20 && b[0]>>4 == 4 && b[9] == pkt.ProtoTCP:
			dst = netip.AddrFrom4([4]byte(b[16:20]))
		case len(b) >= 40 && b[0]>>4 == 6 && b[6] == pkt.ProtoTCP:
			dst, et = netip.AddrFrom16([16]byte(b[24:40])), pkt.EtherTypeIPv6
		default:
			continue
		}
		s.sendIP(dst, et, b)
	}
}

// sendIP sends an IP packet toward dst: at once when the next hop is
// known, else after resolving it (in the background; TCP retransmits).
func (s *Stack) sendIP(dst netip.Addr, et uint16, b []byte) {
	nh, err := s.NextHop(dst)
	if err != nil {
		s.counters.tcpTxErrors.Add(1)
		return
	}
	if mac, ok := s.cachedMAC(nh); ok {
		s.txTCP(mac, et, b)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), resolveWait*resolveTries)
		defer cancel()
		mac, _, err := s.Resolve(ctx, nh)
		if err != nil {
			s.counters.tcpTxErrors.Add(1)
			return
		}
		s.txTCP(mac, et, b)
	}()
}

func (s *Stack) txTCP(mac pkt.MAC, et uint16, b []byte) {
	if s.send(mac, et, b) != nil {
		s.counters.tcpTxErrors.Add(1)
		return
	}
	s.counters.tcpTx.Add(1)
}

// cachedMAC is a resolved neighbor's MAC, without resolving.
func (s *Stack) cachedMAC(ip netip.Addr) (pkt.MAC, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.neighbors[ip]; n != nil && !n.mac.IsZero() {
		return n.mac, true
	}
	return pkt.MAC{}, false
}

func fullAddr(ap netip.AddrPort) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFromSlice(ap.Addr().AsSlice()), Port: ap.Port()}
}

func netProto(a netip.Addr) tcpip.NetworkProtocolNumber {
	if a.Is4() {
		return ipv4.ProtocolNumber
	}
	return ipv6.ProtocolNumber
}

// DialTCP opens a TCP connection from the device (protocol.IP), from local
// (default: chosen by the netstack) with the given TTL / hop limit (0: 64).
func (s *Stack) DialTCP(ctx context.Context, remote netip.AddrPort, local netip.Addr, ttl int) (net.Conn, error) {
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	s.mu.Lock()
	t := s.tcpLocked()
	s.mu.Unlock()
	var wq waiter.Queue
	ep, terr := t.st.NewEndpoint(tcp.ProtocolNumber, netProto(remote.Addr()), &wq)
	if terr != nil {
		return nil, errors.New(terr.String())
	}
	setTTL(ep, remote.Addr(), ttl)
	entry, ready := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)
	if local.IsValid() {
		if terr := ep.Bind(fullAddr(netip.AddrPortFrom(local.Unmap(), 0))); terr != nil {
			ep.Close()
			return nil, fmt.Errorf("bind %s: %s", local, terr)
		}
	}
	terr = ep.Connect(fullAddr(remote))
	if _, ok := terr.(*tcpip.ErrConnectStarted); ok {
		select {
		case <-ctx.Done():
			ep.Close()
			return nil, ctx.Err()
		case <-ready:
		}
		terr = ep.LastError()
	}
	if terr != nil {
		ep.Close()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Addr: net.TCPAddrFromAddrPort(remote), Err: errors.New(terr.String())}
	}
	return gonet.NewTCPConn(&wq, ep), nil
}

func setTTL(ep tcpip.Endpoint, a netip.Addr, ttl int) {
	if ttl <= 0 {
		return
	}
	if a.Is4() {
		ep.SetSockOptInt(tcpip.IPv4TTLOption, ttl)
	} else {
		ep.SetSockOptInt(tcpip.IPv6HopLimitOption, ttl)
	}
}

// ListenTCP accepts TCP connections on the device (protocol.IP): on local
// (an address of the device) or, if invalid, on every address of the
// family given by v6.
func (s *Stack) ListenTCP(local netip.Addr, port uint16, v6 bool, ttl int) (net.Listener, error) {
	s.mu.Lock()
	t := s.tcpLocked()
	s.mu.Unlock()
	proto := ipv4.ProtocolNumber
	addr := tcpip.FullAddress{NIC: nicID, Port: port}
	if local.IsValid() {
		local = local.Unmap()
		proto, addr = netProto(local), fullAddr(netip.AddrPortFrom(local, port))
	} else if v6 {
		proto = ipv6.ProtocolNumber
	}
	var wq waiter.Queue
	ep, terr := t.st.NewEndpoint(tcp.ProtocolNumber, proto, &wq)
	if terr != nil {
		return nil, errors.New(terr.String())
	}
	if proto == ipv6.ProtocolNumber {
		ep.SocketOptions().SetV6Only(true)
	}
	ep.SocketOptions().SetReuseAddress(true)
	if ttl > 0 {
		a := netip.IPv4Unspecified()
		if proto == ipv6.ProtocolNumber {
			a = netip.IPv6Unspecified()
		}
		setTTL(ep, a, ttl)
	}
	if terr := ep.Bind(addr); terr != nil {
		ep.Close()
		return nil, fmt.Errorf("bind port %d: %s", port, terr)
	}
	if terr := ep.Listen(64); terr != nil {
		ep.Close()
		return nil, fmt.Errorf("listen port %d: %s", port, terr)
	}
	return gonet.NewTCPListener(t.st, &wq, ep), nil
}

// AddrPortOf converts a connection's net.Addr.
func AddrPortOf(a net.Addr) netip.AddrPort {
	if t, ok := a.(*net.TCPAddr); ok {
		return t.AddrPort()
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return ap
}
