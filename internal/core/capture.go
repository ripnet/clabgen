package core

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Packet capture: every frame a port receives or sends, as on the wire
// (device tags included; frames between devices on one port are seen once,
// when sent), copied to the captures whose filter matches.

// CaptureFilter picks frames. Zero values match everything.
type CaptureFilter struct {
	Ports      []string // empty: every port
	Device     string   // frames to or from the device's MAC (or multicast) on its port and VLANs
	Direction  string   // "", "rx" or "tx"
	EtherTypes []uint16 // the ethertype after any VLAN tags
	Snaplen    int      // bytes kept per frame (0: all)
}

// CapturedFrame is one frame of a capture.
type CapturedFrame struct {
	Time   time.Time
	Port   string
	Dir    string // rx or tx
	Data   []byte // at most Snaplen bytes
	Length int    // the frame's whole length
}

// Capture is a running capture: read frames from C, Close when done.
type Capture struct {
	C       <-chan CapturedFrame
	e       *Engine
	c       chan CapturedFrame
	f       CaptureFilter
	mac     pkt.MAC
	tags    []pkt.VLANTag
	dropped atomic.Uint64
	once    sync.Once
}

// Dropped counts frames lost because the reader fell behind.
func (c *Capture) Dropped() uint64 { return c.dropped.Load() }

// Close stops the capture and closes C.
func (c *Capture) Close() {
	c.once.Do(func() {
		c.e.capMu.Lock()
		c.e.caps = slices.DeleteFunc(c.e.caps, func(x *Capture) bool { return x == c })
		c.e.capN.Store(int32(len(c.e.caps)))
		close(c.c) // no tap is sending: they hold capMu's read lock
		c.e.capMu.Unlock()
	})
}

// StartCapture begins capturing; buffer frames wait for the reader before
// further ones are dropped (counted in Dropped).
func (e *Engine) StartCapture(f CaptureFilter, buffer int) (*Capture, error) {
	switch f.Direction {
	case "", "rx", "tx":
	default:
		return nil, fmt.Errorf("capture direction %q: rx, tx or empty for both", f.Direction)
	}
	if f.Snaplen < 0 {
		return nil, errors.New("capture snaplen must not be negative")
	}
	c := &Capture{e: e, c: make(chan CapturedFrame, max(buffer, 16)), f: f}
	c.C = c.c
	if f.Device != "" {
		e.mu.RLock()
		d := e.devices[f.Device]
		e.mu.RUnlock()
		if d == nil {
			return nil, fmt.Errorf("device %s: %w", f.Device, ErrNotFound)
		}
		c.mac, c.tags = d.mac, d.vlans
		c.f.Ports = []string{d.port}
	}
	e.capMu.Lock()
	e.caps = append(e.caps, c)
	e.capN.Store(int32(len(e.caps)))
	e.capMu.Unlock()
	return c, nil
}

// tap hands a frame to the captures that want it. b may be reused by the
// caller once tap returns.
func (e *Engine) tap(port, dir string, b []byte, ts time.Time) {
	if e.capN.Load() == 0 {
		return
	}
	e.capMu.RLock()
	defer e.capMu.RUnlock()
	var f *pkt.Frame
	for _, c := range e.caps {
		if !c.match(port, dir, b, &f) {
			continue
		}
		n := len(b)
		if c.f.Snaplen > 0 {
			n = min(n, c.f.Snaplen)
		}
		select {
		case c.c <- CapturedFrame{Time: ts, Port: port, Dir: dir, Data: append([]byte(nil), b[:n]...), Length: len(b)}:
		default:
			c.dropped.Add(1)
		}
	}
}

func (c *Capture) match(port, dir string, b []byte, f **pkt.Frame) bool {
	if c.f.Direction != "" && c.f.Direction != dir {
		return false
	}
	if len(c.f.Ports) > 0 && !slices.Contains(c.f.Ports, port) {
		return false
	}
	if c.f.Device == "" && len(c.f.EtherTypes) == 0 {
		return true
	}
	if *f == nil {
		d, err := pkt.Decode(b)
		if err != nil {
			return false
		}
		*f = d
	}
	fr := *f
	if len(c.f.EtherTypes) > 0 && !slices.Contains(c.f.EtherTypes, fr.EtherType) {
		return false
	}
	if c.f.Device != "" {
		if !pkt.SameVLANs(fr.Tags, c.tags) {
			return false
		}
		if fr.Src != c.mac && fr.Dst != c.mac && !fr.Dst.IsMulticast() {
			return false
		}
	}
	return true
}
