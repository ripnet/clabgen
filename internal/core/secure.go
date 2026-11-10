package core

import (
	"fmt"
	"sync"

	"github.com/ripnet/clabgen/internal/protocol"
)

// Secure implements protocol.Securer: the instance's device, or every device
// on its port, goes behind l. A port has one link for all its devices or one
// per device, not both.
func (v *env) Secure(l protocol.SecureLink, port bool) (func(), error) {
	e, d := v.e, v.dev
	e.mu.Lock()
	defer e.mu.Unlock()
	if pl := e.portSec[d.port]; pl != nil {
		return nil, fmt.Errorf("port %s is secured as a whole by %s", d.port, pl.owner)
	}
	if port {
		for _, o := range e.byPort[d.port] {
			if o.sec != nil {
				return nil, fmt.Errorf("device %s on port %s has its own MACsec: a port is secured as a whole or per device", o.name, d.port)
			}
		}
		e.portSec[d.port] = &portLink{l: l, owner: v.in.spec.Name}
	} else {
		if d.sec != nil {
			return nil, fmt.Errorf("device %s already has MACsec", d.name)
		}
		d.sec = l
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			if pl := e.portSec[d.port]; port && pl != nil && pl.l == l {
				delete(e.portSec, d.port)
			}
			if !port && d.sec == l {
				d.sec = nil
			}
		})
	}, nil
}
