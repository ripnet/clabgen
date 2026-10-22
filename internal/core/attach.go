package core

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// A tap is a session receiving on a device other than its own
// (protocol.Env.Attach), e.g. a router's further interfaces.
type tap struct {
	in     *instance
	dev    *device
	rx     func(*pkt.Frame)
	closed chan struct{}
	once   sync.Once
}

func (t *tap) close() { t.once.Do(func() { close(t.closed) }) }

var errDetached = errors.New("attachment closed")

type attachment struct {
	e *Engine
	t *tap
}

func (a *attachment) Device() protocol.Device { return a.t.dev }
func (a *attachment) IP() protocol.IP         { return a.t.dev.ip }
func (a *attachment) Closed() <-chan struct{} { return a.t.closed }
func (a *attachment) Close()                  { a.e.detach(a.t) }

func (a *attachment) Send(dst pkt.MAC, et uint16, payload []byte, opt protocol.SendOpts) error {
	select {
	case <-a.t.closed:
		return errDetached
	default:
	}
	return a.e.sendFrom(a.t.dev, dst, et, payload, opt)
}

func (v *env) Attach(name string, rx func(*pkt.Frame)) (protocol.Attachment, error) {
	e := v.e
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.devices[name]
	if d == nil {
		return nil, fmt.Errorf("device %s: %w", name, ErrNotFound)
	}
	if d == v.dev {
		return nil, fmt.Errorf("device %s is the instance's own device", name)
	}
	t := &tap{in: v.in, dev: d, rx: rx, closed: make(chan struct{})}
	d.taps = append(d.taps, t)
	v.in.taps = append(v.in.taps, t)
	return &attachment{e, t}, nil
}

// detach removes one tap.
func (e *Engine) detach(t *tap) {
	e.mu.Lock()
	t.dev.taps = without(t.dev.taps, t)
	t.in.taps = without(t.in.taps, t)
	e.mu.Unlock()
	t.close()
}

// closeTapsLocked ends the taps on a device that goes away (or changes its
// L2 identity). Caller holds mu.
func (e *Engine) closeDeviceTapsLocked(d *device) {
	for _, t := range d.taps {
		t.in.taps = without(t.in.taps, t)
		t.close()
	}
	d.taps = nil
}

// closeInstanceTapsLocked ends an instance's taps (it stops). Caller holds mu.
func (e *Engine) closeInstanceTapsLocked(in *instance) {
	for _, t := range in.taps {
		t.dev.taps = without(t.dev.taps, t)
		t.close()
	}
	in.taps = nil
}

func without(l []*tap, t *tap) []*tap {
	for i, x := range l {
		if x == t {
			return append(l[:i:i], l[i+1:]...)
		}
	}
	return l
}
