package ipstack

import (
	"context"
	"io"
	"net/netip"
	"testing"
	"time"
)

// TCP between two stacks over IPv4, IPv6 and to a local (loopback)
// address routed via a gateway; data flows both ways.
func TestTCP(t *testing.T) {
	w := &wire{}
	a, _ := w.add(t, macA, mustCfg(t, "10.0.0.2", "", "10.0.0.1/24", "2001:db8::1/64"))
	b, _ := w.add(t, macB, mustCfg(t, "", "", "10.0.0.2/24", "2001:db8::2/64"))
	defer a.Close()
	defer b.Close()
	release := b.AddLocal(netip.MustParseAddr("192.0.2.9"))
	defer release()
	for _, dst := range []string{"10.0.0.2", "2001:db8::2", "192.0.2.9"} {
		d := netip.MustParseAddr(dst)
		l, err := b.ListenTCP(netip.Addr{}, 179, d.Is6(), 0)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			c, err := l.Accept()
			if err != nil {
				return
			}
			io.Copy(c, c) // echo
			c.Close()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := a.DialTCP(ctx, netip.AddrPortFrom(d, 179), netip.Addr{}, 1)
		cancel()
		if err != nil {
			t.Fatalf("%s: dial: %v", dst, err)
		}
		msg := make([]byte, 10000) // several segments
		for i := range msg {
			msg[i] = byte(i)
		}
		go c.Write(msg)
		got := make([]byte, len(msg))
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatalf("%s: read: %v", dst, err)
		}
		for i := range got {
			if got[i] != msg[i] {
				t.Fatalf("%s: byte %d differs", dst, i)
			}
		}
		if r := AddrPortOf(c.RemoteAddr()); r.Addr() != d || r.Port() != 179 {
			t.Fatalf("remote %v", r)
		}
		c.Close()
		l.Close()
	}
	if c := a.Status().Counters; c["tcp_tx"] == 0 || c["tcp_rx"] == 0 {
		t.Fatalf("counters %v", c)
	}
	// Nobody listening: the dial is refused.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.DialTCP(ctx, netip.MustParseAddrPort("10.0.0.2:180"), netip.Addr{}, 0); err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
}
