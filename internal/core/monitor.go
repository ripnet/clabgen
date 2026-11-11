package core

import (
	"fmt"
	"sync"
	"time"
)

// portMonitor is a monitor port's receiver, and the instance holding it.
type portMonitor struct {
	rx    func([]byte, time.Time)
	owner string
}

// Monitor implements protocol.Monitorer: the instance's port becomes a
// monitor port. It can't be one already, nor have MACsec on it.
func (v *env) Monitor(rx func(b []byte, ts time.Time)) (func(), error) {
	e, d := v.e, v.dev
	e.mu.Lock()
	defer e.mu.Unlock()
	if m := e.portMon[d.port]; m != nil {
		return nil, fmt.Errorf("port %s is a monitor port already (%s)", d.port, m.owner)
	}
	if pl := e.portSec[d.port]; pl != nil {
		return nil, fmt.Errorf("port %s has MACsec (%s)", d.port, pl.owner)
	}
	for _, o := range e.byPort[d.port] {
		if o.sec != nil {
			return nil, fmt.Errorf("device %s on port %s has MACsec", o.name, d.port)
		}
	}
	m := &portMonitor{rx: rx, owner: v.in.spec.Name}
	e.portMon[d.port] = m
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.portMon[d.port] == m {
				delete(e.portMon, d.port)
			}
		})
	}, nil
}
