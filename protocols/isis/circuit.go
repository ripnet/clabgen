package isis

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// A circuit is one interface of the router: the instance's own device
// (circuit 0, the top-level settings) or one of cfg.Interfaces, which the
// session attaches to (protocol.Env.Attach). Adjacencies, hellos and the
// DIS role are per circuit; the LSDB and our LSPs are the router's.
type circuit struct {
	idx       int
	device    string
	lan       bool
	passive   bool
	metric    uint32
	priority  uint8
	helloAuth Auth
	pn        byte // our pseudonode ID on this circuit (LAN)
	mac       pkt.MAC
	teMetric  uint32 // TE attributes (flex-algo)
	delayUS   uint32
	groups    uint32

	att     protocol.Attachment // further interfaces; nil while detached
	locals  []func()            // releases the loopbacks registered on its device
	attErr  string
	retryAt time.Time

	adj         adjacency         // point-to-point
	lans        map[int]*lanLevel // broadcast, per level
	lastCSNP    map[int]time.Time // point-to-point, per level
	nextHelo    time.Time         // point-to-point
	transitions int               // adjacency ups and downs
	flapTill    time.Time
	flapMode    string

	bfd           bool // BFD with this circuit's neighbors
	bfdPeers      map[bfdKey]*bfdPeer
	bfdSilentTill time.Time
}

// attachRetry is how often a missing further interface is attached again.
const attachRetry = time.Second

func (s *session) newCircuit(idx int, dev string, ic Interface) *circuit {
	c := &circuit{idx: idx, device: dev, lan: ic.Circuit == "broadcast", passive: ic.Passive, metric: ic.Metric,
		priority: s.cfg.Priority, helloAuth: s.cfg.HelloAuth, pn: byte(idx + 1), lastCSNP: map[int]time.Time{},
		teMetric: ic.TEMetric, delayUS: ic.DelayUS, groups: groupMask(ic.AdminGroups)}
	if ic.Circuit == "" {
		c.lan = s.cfg.Circuit == "broadcast"
	}
	if c.metric == 0 {
		c.metric = s.cfg.Metric
	}
	if ic.Priority != nil {
		c.priority = *ic.Priority
	}
	if ic.HelloAuth != nil {
		c.helloAuth = *ic.HelloAuth
	}
	c.bfd = s.cfg.BFD.Enabled
	if ic.BFD != nil {
		c.bfd = *ic.BFD
	}
	c.adj.state = stDown
	if c.lan {
		c.lans = map[int]*lanLevel{}
		for _, n := range s.cfg.levels() {
			c.lans[n] = &lanLevel{n: n, adjs: map[pkt.MAC]*lanAdj{}}
		}
	}
	return c
}

// attached reports whether the circuit has a device to work on.
func (c *circuit) attached() bool { return c.idx == 0 || c.att != nil }

func (c *circuit) circuitName() string {
	if c.lan {
		return "broadcast"
	}
	return "point-to-point"
}

func (s *session) circuitIP(c *circuit) protocol.IP {
	if c.idx == 0 {
		return s.env.IP()
	}
	if c.att != nil {
		return c.att.IP()
	}
	return nil
}

// circuitsNamed finds a circuit by device name; "" is every circuit.
func (s *session) circuitsNamed(name string) ([]*circuit, error) {
	if name == "" {
		return s.circs, nil
	}
	for _, c := range s.circs {
		if c.device == name {
			return []*circuit{c}, nil
		}
	}
	return nil, fmt.Errorf("no interface %q", name)
}

// attachLoop attaches further interfaces that aren't (or no longer are),
// and notices attachments that closed. Engine calls happen without mu.
func (s *session) attachLoop(now time.Time) {
	var need []*circuit
	s.mu.Lock()
	for _, c := range s.circs[1:] {
		if c.att != nil {
			select {
			case <-c.att.Closed():
				s.detachLocked(c, now, "device changed or deleted")
			default:
			}
		}
		if c.att == nil && !now.Before(c.retryAt) {
			c.retryAt = now.Add(attachRetry)
			need = append(need, c)
		}
	}
	s.mu.Unlock()
	for _, c := range need {
		c := c
		att, err := s.env.Attach(c.device, func(f *pkt.Frame) { s.rx(c, f) })
		s.mu.Lock()
		if err != nil {
			if c.attErr != err.Error() {
				c.attErr = err.Error()
				go s.event("interface-down", fmt.Sprintf("interface %s: %v", c.device, err))
			}
		} else {
			c.att, c.attErr, c.mac, c.nextHelo = att, "", att.Device().MAC(), now
			s.addLocalsLocked(c)
			for _, ll := range c.lans {
				ll.nextHello = now
				s.electLocked(c, ll.n, now)
			}
			s.dirtyLocked() // its addresses join our LSP
			go s.event("interface-up", fmt.Sprintf("interface %s attached", c.device))
		}
		s.mu.Unlock()
	}
}

// detachLocked drops a further interface whose attachment ended.
func (s *session) detachLocked(c *circuit, now time.Time, why string) {
	s.adjDownLocked(c, now, "interface "+why)
	s.releaseLocalsLocked(c)
	c.att, c.attErr = nil, why
	c.retryAt = time.Time{}
	s.dirtyLocked()
	go s.event("interface-down", fmt.Sprintf("interface %s: %s", c.device, why))
}

// addLocalsLocked makes the circuit's device answer for our loopbacks.
func (s *session) addLocalsLocked(c *circuit) {
	ip := s.circuitIP(c)
	if ip == nil {
		return
	}
	for _, p := range s.loopbacks {
		c.locals = append(c.locals, ip.AddLocal(p.Addr()))
	}
}

func (s *session) releaseLocalsLocked(c *circuit) {
	for _, r := range c.locals {
		r()
	}
	c.locals = nil
}

func (s *session) dirtyLocked() {
	for _, l := range s.lv {
		l.dirty = true
	}
}

// flag marks an LSDB entry to be sent on circuit idx.
func (e *dbEntry) flag(idx int) {
	if e.srm == nil {
		e.srm = map[int]time.Time{}
	}
	e.srm[idx] = time.Time{}
}

// allSRM flags an entry for every circuit (sent where adjacencies are up).
func (s *session) allSRM() map[int]time.Time {
	m := make(map[int]time.Time, len(s.circs))
	for _, c := range s.circs {
		m[c.idx] = time.Time{}
	}
	return m
}

// floodLocked flags an entry for every circuit but the one it came from.
func (s *session) floodLocked(e *dbEntry, from int) {
	for _, c := range s.circs {
		if c.idx != from {
			e.flag(c.idx)
		}
	}
}

// pnCircuit is the LAN circuit whose pseudonode ID is pn, while we are its
// DIS at level n.
func (s *session) pnCircuit(pn byte, n int) *circuit {
	for _, c := range s.circs {
		if c.lan && c.pn == pn {
			if ll := c.lans[n]; ll != nil && ll.isDIS {
				return c
			}
		}
	}
	return nil
}

// IfStatus describes one interface (circuit).
type IfStatus struct {
	Name        string   `json:"name"`
	Circuit     string   `json:"circuit"`
	Metric      uint32   `json:"metric"`
	Priority    uint8    `json:"priority,omitempty"`
	Passive     bool     `json:"passive,omitempty"`
	Attached    bool     `json:"attached"`
	Error       string   `json:"error,omitempty"`
	MAC         string   `json:"mac,omitempty"`
	Addresses   []string `json:"addresses,omitempty"`
	HelloAuth   string   `json:"hello_auth"`
	Adjacencies int      `json:"adjacencies_up"`
	Flapping    bool     `json:"flapping,omitempty"`
}

func (s *session) ifStatusLocked(c *circuit, now time.Time) IfStatus {
	st := IfStatus{Name: c.device, Circuit: c.circuitName(), Metric: c.metric, Passive: c.passive, Attached: c.attached(),
		Error: c.attErr, HelloAuth: authType(c.helloAuth), Flapping: now.Before(c.flapTill)}
	if c.lan {
		st.Priority = c.priority
	}
	if c.attached() {
		st.MAC = c.mac.String()
	}
	if ip := s.circuitIP(c); ip != nil {
		for _, p := range ip.Addresses() {
			if !p.Addr().IsLinkLocalUnicast() {
				st.Addresses = append(st.Addresses, p.String())
			}
		}
	}
	if c.lan {
		up := map[pkt.MAC]bool{}
		for _, ll := range c.lans {
			for m, a := range ll.adjs {
				if a.state == stUp {
					up[m] = true
				}
			}
		}
		st.Adjacencies = len(up)
	} else if c.adj.state == stUp {
		st.Adjacencies = 1
	}
	return st
}

// interfaceAddrs are the router's addresses over all attached circuits,
// with the circuit each came from (for connected-subnet metrics).
func (s *session) interfaceAddrs() (v4, v6g []netip.Prefix, metric map[netip.Prefix]uint32) {
	metric = map[netip.Prefix]uint32{}
	// Loopbacks first (router ID), at metric 0.
	for _, p := range s.loopbacks {
		if _, ok := metric[p.Masked()]; !ok {
			metric[p.Masked()] = 0
		}
		if p.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6g = append(v6g, p)
		}
	}
	for _, c := range s.circs {
		a4, _, a6 := s.addrs(c)
		for _, p := range append(a4, a6...) {
			if _, ok := metric[p.Masked()]; !ok {
				metric[p.Masked()] = c.metric
			}
		}
		v4, v6g = append(v4, a4...), append(v6g, a6...)
	}
	return v4, v6g, metric
}
