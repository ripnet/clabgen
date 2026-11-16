package core_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// A router that sends router advertisements from its device's link-local
// address: in answer to solicitations, every every_ms if set, and once with
// a zero lifetime on the action retire.
func init() { protocol.Register(testRouter{}) }

type testRouterConfig struct {
	Lifetime uint16 `json:"lifetime"`
	EveryMS  int    `json:"every_ms"`
}

type testRouter struct{}

func (testRouter) Name() string               { return "test-router" }
func (testRouter) Description() string        { return "test router advertisements" }
func (testRouter) NewConfig() any             { return &testRouterConfig{Lifetime: 1800} }
func (testRouter) EtherTypes() []uint16       { return []uint16{pkt.EtherTypeIPv6} }
func (testRouter) Actions() []protocol.Action { return []protocol.Action{{Name: "retire"}} }
func (testRouter) Start(env protocol.Env, c any) (protocol.Session, error) {
	r := &raSender{env: env, cfg: *c.(*testRouterConfig), stop: make(chan struct{})}
	if r.cfg.EveryMS > 0 {
		go func() {
			t := time.NewTicker(time.Duration(r.cfg.EveryMS) * time.Millisecond)
			defer t.Stop()
			for {
				r.send(r.cfg.Lifetime)
				select {
				case <-r.stop:
					return
				case <-t.C:
				}
			}
		}()
	}
	return r, nil
}

type raSender struct {
	env  protocol.Env
	cfg  testRouterConfig
	stop chan struct{}

	mu  sync.Mutex
	rs  int
	off bool // retired: advertises a zero lifetime
}

func (r *raSender) send(lifetime uint16) {
	r.mu.Lock()
	if r.off {
		lifetime = 0
	}
	r.mu.Unlock()
	m := make([]byte, 16, 48)
	m[0], m[4] = pkt.ICMPv6RouterAdv, 64
	binary.BigEndian.PutUint16(m[6:], lifetime)
	pi := make([]byte, 32)
	pi[0], pi[1], pi[2], pi[3] = 3, 4, 64, 0x80
	binary.BigEndian.PutUint32(pi[4:], 86400)
	binary.BigEndian.PutUint32(pi[8:], 14400)
	p := netip.MustParseAddr("2001:db8::").As16()
	copy(pi[16:], p[:])
	m = append(m, pi...)
	src, dst := pkt.LinkLocal(r.env.Device().MAC()), netip.MustParseAddr("ff02::1")
	pkt.ICMPv6Checksum(src, dst, m)
	h := pkt.IPv6{NextHeader: pkt.ProtoICMPv6, HopLimit: 255, Src: src, Dst: dst, Payload: m}
	r.env.Send(pkt.MulticastMAC(dst), pkt.EtherTypeIPv6, h.Encode(), protocol.SendOpts{})
}

func (r *raSender) HandleRx(f *pkt.Frame) {
	h, err := pkt.DecodeIPv6(f.Payload)
	if err == nil && h.NextHeader == pkt.ProtoICMPv6 && len(h.Payload) > 0 && h.Payload[0] == pkt.ICMPv6RouterSol {
		r.mu.Lock()
		r.rs++
		r.mu.Unlock()
		r.send(r.cfg.Lifetime)
	}
}

func (r *raSender) Action(_ context.Context, name string, _ any) (any, error) {
	r.mu.Lock()
	r.off = true
	r.mu.Unlock()
	r.send(0)
	return nil, nil
}

func (r *raSender) Status() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]int{"rs": r.rs}
}

func (r *raSender) Stop() { close(r.stop) }

// routerPair: a DHCPv6 client (no server: only its routers matter) on a,
// and router devices r1..rn on b, each with an address so it answers
// neighbor solicitations for its link-local address.
func routerPair(t *testing.T, n int) (*core.Engine, *core.Engine) {
	t.Helper()
	a, b, _, _ := newPair()
	for i := 1; i <= n; i++ {
		if _, err := b.CreateDevice(core.DeviceSpec{Name: fmt.Sprintf("r%d", i), Port: "eth1", MAC: fmt.Sprintf("02:00:00:00:0b:%02x", i),
			Addresses: []string{fmt.Sprintf("2001:db8::%d/64", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.CreateDevice(core.DeviceSpec{Name: "c0", Port: "eth1", MAC: "02:00:00:00:0a:01"}); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func routerLL(i int) string { return pkt.LinkLocal(pkt.MAC{2, 0, 0, 0, 0x0b, byte(i)}).String() }

// The client keeps a default router list (RFC 4861 6.3.4) and picks its
// gateway from it (6.3.6): it stays on the router in use while that one
// answers, moves at once when it retires (a zero router lifetime), and
// moves when it stops answering neighbor solicitations while another
// router advertises.
func TestDHCPv6DefaultRouterList(t *testing.T) {
	a, b := routerPair(t, 3)
	mkProto(t, b, "ra1", "test-router", "r1", `{}`)
	mkProto(t, a, "v6", "dhcpv6", "c0", `{}`)
	gw := func() string { return device(t, a, "c0").DynamicGateway6 }
	eventually(t, "r1 is the gateway", func() bool { return gw() == routerLL(1) })

	mkProto(t, b, "ra2", "test-router", "r2", `{"every_ms": 300}`)
	eventually(t, "r2 listed", func() bool { return len(flowStatus(t, a, "v6")["routers"].([]any)) == 2 })
	time.Sleep(time.Second)
	if gw() != routerLL(1) {
		t.Fatalf("moved to %s while r1 answers", gw())
	}

	if _, err := b.RunAction(context.Background(), "ra1", "retire", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "r2 after r1 retired", func() bool { return gw() == routerLL(2) })

	// r2 goes away without a word (its device deleted, as a router
	// interface readdressed); r3 advertises.
	mkProto(t, b, "ra3", "test-router", "r3", `{"every_ms": 300}`)
	eventually(t, "r3 listed", func() bool { return len(flowStatus(t, a, "v6")["routers"].([]any)) == 2 })
	if err := b.DeleteDevice("r2"); err != nil {
		t.Fatal(err)
	}
	// What r2 said last is good for 30 s (the reachable time); emptying
	// the cache makes the next check (every 10 s) solicit it.
	if err := a.ClearNeighbors("c0"); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(20 * time.Second); gw() != routerLL(3); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("still on %s after r2 stopped answering", gw())
		}
	}
	st := flowStatus(t, a, "v6")
	if st["gateway"] != routerLL(3) {
		t.Fatalf("status gateway %v", st["gateway"])
	}
	for _, x := range st["routers"].([]any) {
		r := x.(map[string]any)
		if r["router"] == routerLL(2) && r["unreachable"] != true {
			t.Fatalf("r2 not marked unreachable: %v", st["routers"])
		}
	}
}

// When the last router's lifetime ends, the client solicits advertisements
// again (RFC 7559) instead of waiting for the next periodic one.
func TestDHCPv6SolicitsAfterRouterExpires(t *testing.T) {
	a, b := routerPair(t, 1)
	mkProto(t, b, "ra1", "test-router", "r1", `{"lifetime": 2}`)
	mkProto(t, a, "v6", "dhcpv6", "c0", `{}`)
	rs := func() float64 { return num(flowStatus(t, b, "ra1"), "rs") }
	eventually(t, "gateway", func() bool { return device(t, a, "c0").DynamicGateway6 == routerLL(1) })
	first := rs()
	// Each expiry (2 s) brings a solicitation, which the router answers.
	eventuallyLong(t, "solicited again", func() bool { return rs() >= first+2 })
	eventually(t, "gateway back", func() bool { return device(t, a, "c0").DynamicGateway6 == routerLL(1) })
}

// A lone router readdressed (its old link-local gone, the new one only
// answering solicitations): the client finds the old one doesn't answer
// and solicits, so the new one takes over without waiting for a periodic
// advertisement. The solicit action asks at once.
func TestDHCPv6RouterReaddressed(t *testing.T) {
	a, b := routerPair(t, 2)
	if err := b.DeleteDevice("r2"); err != nil {
		t.Fatal(err)
	}
	mkProto(t, b, "ra1", "test-router", "r1", `{}`)
	mkProto(t, a, "v6", "dhcpv6", "c0", `{}`)
	gw := func() string { return device(t, a, "c0").DynamicGateway6 }
	eventually(t, "r1 is the gateway", func() bool { return gw() == routerLL(1) })

	rs := num(flowStatus(t, b, "ra1"), "rs")
	if _, err := a.RunAction(context.Background(), "v6", "solicit", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "solicit action", func() bool { return num(flowStatus(t, b, "ra1"), "rs") > rs })

	if err := b.DeleteDevice("r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateDevice(core.DeviceSpec{Name: "r2", Port: "eth1", MAC: "02:00:00:00:0b:02", Addresses: []string{"2001:db8::2/64"}}); err != nil {
		t.Fatal(err)
	}
	mkProto(t, b, "ra2", "test-router", "r2", `{}`)
	if err := a.ClearNeighbors("c0"); err != nil { // r1 last heard over 30 s ago
		t.Fatal(err)
	}
	for end := time.Now().Add(20 * time.Second); gw() != routerLL(2); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("still on %s after r1 went away", gw())
		}
	}
}
