package dhcp

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// What the DHCPv4 and DHCPv6 servers share: a worker that takes requests
// off the receive path (replies to relays may need ARP/ND first), pause
// and resume, and counters.

// offerHold is how long an offered address or prefix is kept for the
// client's Request.
const offerHold = 30 * time.Second

// request is a received message for the worker.
type request struct {
	src, dst netip.Addr
	srcMAC   pkt.MAC
	data     []byte
}

type PauseArgs struct {
	Seconds uint32 `json:"seconds" jsonschema:"description=How long to stay silent (0: until resume)"`
}

type AddressArgs struct {
	Address string `json:"address" jsonschema:"required,description=The leased address or prefix to forget"`
}

func serverActions(what string) []protocol.Action {
	return []protocol.Action{
		{Name: "pause", Description: "Stop answering (a server gone): clients retry, then their " + what + " expire", NewArgs: func() any { return &PauseArgs{} }},
		{Name: "resume", Description: "Answer again"},
		{Name: "forget", Description: "Forget one lease: the client's next renewal is refused", NewArgs: func() any { return &AddressArgs{} }},
		{Name: "forget-all", Description: "Forget every lease"},
		{Name: "clear", Description: "Reset the message counters"},
	}
}

// worker runs handle for each request, in order, off the receive path.
type worker struct {
	env   protocol.Env
	in    chan request
	stop  chan struct{}
	ended chan struct{}

	mu          sync.Mutex
	pausedUntil time.Time // zero: answering; far future: until resume
	counters    map[string]uint64
}

func newWorker(env protocol.Env) *worker {
	return &worker{env: env, in: make(chan request, 256), stop: make(chan struct{}), ended: make(chan struct{}), counters: map[string]uint64{}}
}

func (w *worker) run(handle func(request)) {
	defer close(w.ended)
	for {
		select {
		case <-w.stop:
			return
		case r := <-w.in:
			if w.paused() {
				w.count("dropped_paused")
				continue
			}
			handle(r)
		}
	}
}

func (w *worker) queue(r request) {
	select {
	case w.in <- r:
	default:
		w.count("dropped_busy")
	}
}

func (w *worker) paused() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.pausedUntil.IsZero() && time.Now().Before(w.pausedUntil)
}

func (w *worker) count(k string) {
	w.mu.Lock()
	w.counters[k]++
	w.mu.Unlock()
}

func (w *worker) snapshot() (map[string]uint64, float64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := make(map[string]uint64, len(w.counters))
	for k, v := range w.counters {
		c[k] = v
	}
	if w.pausedUntil.IsZero() || !time.Now().Before(w.pausedUntil) {
		return c, 0, false
	}
	left := secs(time.Until(w.pausedUntil))
	if left > 365*24*3600 {
		left = 0 // until resume
	}
	return c, left, true
}

// action runs the shared actions; forget and forget-all go to the server.
func (w *worker) action(name string, args any, forget func(string) error, forgetAll func()) error {
	switch name {
	case "pause":
		d := 100 * 365 * 24 * time.Hour
		if a, ok := args.(*PauseArgs); ok && a.Seconds > 0 {
			d = time.Duration(a.Seconds) * time.Second
		}
		w.mu.Lock()
		w.pausedUntil = time.Now().Add(d)
		w.mu.Unlock()
		w.env.Emit("paused", "not answering", nil)
	case "resume":
		w.mu.Lock()
		w.pausedUntil = time.Time{}
		w.mu.Unlock()
		w.env.Emit("resumed", "answering", nil)
	case "forget":
		a, ok := args.(*AddressArgs)
		if !ok || a.Address == "" {
			return errors.New("address is required")
		}
		return forget(a.Address)
	case "forget-all":
		forgetAll()
	case "clear":
		w.mu.Lock()
		w.counters = map[string]uint64{}
		w.mu.Unlock()
	default:
		return fmt.Errorf("unknown action %q", name)
	}
	return nil
}

func (w *worker) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.ended
}

// ours reports whether a is one of the device's addresses.
func ours(ip protocol.IP, a netip.Addr) bool {
	for _, p := range ip.Addresses() {
		if p.Addr() == a {
			return true
		}
	}
	return false
}

// sendUDP sends a reply: to dstMAC when given (an on-link client), else
// to the next hop toward dst (a relay, or a client by its address).
func sendUDP(env protocol.Env, src, dst netip.Addr, dstMAC pkt.MAC, sport, dport uint16, data []byte) error {
	if dstMAC.IsZero() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, mac, err := env.IP().ResolveNextHop(ctx, dst)
		cancel()
		if err != nil {
			return err
		}
		dstMAC = mac
	}
	hops := uint8(64)
	if dst.IsLinkLocalUnicast() {
		hops = 255
	}
	et, b := udpPacket(src, dst, sport, dport, hops, data)
	return env.Send(dstMAC, et, b, protocol.SendOpts{})
}

// optText shows an option value as text when it is printable, else hex.
func optText(b []byte) string {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return "0x" + hex.EncodeToString(b)
		}
	}
	return string(b)
}

// nth returns the n-th address after a (n may be large for IPv6).
func nth(a netip.Addr, n *big.Int) (netip.Addr, bool) {
	x := new(big.Int).SetBytes(a.AsSlice())
	x.Add(x, n)
	b := x.Bytes()
	size := a.BitLen() / 8
	if len(b) > size {
		return netip.Addr{}, false
	}
	buf := make([]byte, size)
	copy(buf[size-len(b):], b)
	r, _ := netip.AddrFromSlice(buf)
	return r, true
}

// rangeSize is end-start+1.
func rangeSize(start, end netip.Addr) *big.Int {
	s, e := new(big.Int).SetBytes(start.AsSlice()), new(big.Int).SetBytes(end.AsSlice())
	return e.Sub(e, s).Add(e, big.NewInt(1))
}

// maxScan bounds how many candidates an allocation tries.
const maxScan = 1 << 16
