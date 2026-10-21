//go:build linux

// Package port discovers the container's data interfaces and owns their raw
// sockets. containerlab attaches interfaces after the container starts, so
// ports are adopted as netlink reports them.
package port

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/pktio"
)

type Counters struct {
	RxFrames, RxBytes, TxFrames, TxBytes, TxErrors, RxUnmatched atomic.Uint64
}

type Port struct {
	name     string
	index    int
	mac      pkt.MAC
	mtu      int
	adminUp  atomic.Bool
	operUp   atomic.Bool
	conn     pktio.Conn
	counters Counters
}

type Event struct {
	Port, Kind, Message string
}

type Manager struct {
	match   *regexp.Regexp
	log     *slog.Logger
	onFrame func(port string, b []byte, ts time.Time) bool
	onEvent func(Event)

	mu      sync.RWMutex
	ports   map[string]*Port
	desired map[string]bool // admin state requested before/while port exists
}

// New creates a manager. onFrame returns false if no device consumed the
// frame (counted as rx_unmatched).
func New(match *regexp.Regexp, log *slog.Logger, onFrame func(string, []byte, time.Time) bool, onEvent func(Event)) *Manager {
	return &Manager{match: match, log: log, onFrame: onFrame, onEvent: onEvent,
		ports: map[string]*Port{}, desired: map[string]bool{}}
}

func (m *Manager) Run(ctx context.Context) error {
	updates := make(chan netlink.LinkUpdate, 64)
	done := make(chan struct{})
	defer close(done)
	if err := netlink.LinkSubscribe(updates, done); err != nil {
		return fmt.Errorf("netlink subscribe: %w", err)
	}
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("netlink list: %w", err)
	}
	for _, l := range links {
		m.handleLink(l, false)
	}
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for _, p := range m.ports {
				p.conn.Close()
			}
			m.mu.Unlock()
			return nil
		case u, ok := <-updates:
			if !ok {
				return errors.New("netlink subscription closed")
			}
			m.handleLink(u.Link, u.Header.Type == unix.RTM_DELLINK)
		}
	}
}

func (m *Manager) handleLink(l netlink.Link, deleted bool) {
	a := l.Attrs()
	if !m.match.MatchString(a.Name) {
		return
	}
	m.mu.Lock()
	p, known := m.ports[a.Name]
	if known && (deleted || p.index != a.Index) {
		p.conn.Close()
		delete(m.ports, a.Name)
		known = false
		m.mu.Unlock()
		m.onEvent(Event{a.Name, "port-removed", "interface removed"})
		m.mu.Lock()
	}
	if deleted {
		m.mu.Unlock()
		return
	}
	var want, hasWant bool
	if !known {
		np, err := m.adopt(l)
		if err != nil {
			m.mu.Unlock()
			m.log.Error("adopt port", "port", a.Name, "err", err)
			return
		}
		m.ports[a.Name] = np
		p = np
		want, hasWant = m.desired[a.Name]
	}
	m.mu.Unlock()
	if !known {
		m.log.Info("port adopted", "port", a.Name, "ifindex", a.Index, "mac", p.mac)
		m.onEvent(Event{a.Name, "port-added", "interface adopted"})
		if hasWant && want != (a.Flags&unix.IFF_UP != 0) {
			if err := m.SetAdmin(a.Name, want); err != nil {
				m.log.Error("apply admin state", "port", a.Name, "err", err)
			}
		}
	}
	admin := a.Flags&unix.IFF_UP != 0
	oper := a.OperState == netlink.OperUp || (a.OperState == netlink.OperUnknown && a.RawFlags&unix.IFF_RUNNING != 0)
	if old := p.adminUp.Swap(admin); old != admin && known {
		m.onEvent(Event{a.Name, "port-admin", fmt.Sprintf("admin %s", upDown(admin))})
	}
	if old := p.operUp.Swap(oper); old != oper && known {
		m.onEvent(Event{a.Name, "port-oper", fmt.Sprintf("oper %s", upDown(oper))})
	}
}

func upDown(b bool) string {
	if b {
		return "up"
	}
	return "down"
}

// adopt silences the kernel on the port and opens the raw socket.
func (m *Manager) adopt(l netlink.Link) (*Port, error) {
	a := l.Attrs()
	// The agent owns the wire: no kernel IPv6 (RS/NS/MLD) and no addresses.
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+a.Name+"/disable_ipv6", []byte("1"), 0); err != nil {
		m.log.Warn("could not disable kernel IPv6 on port", "port", a.Name, "err", err)
	}
	if err := netlink.SetPromiscOn(l); err != nil {
		m.log.Warn("could not set promiscuous mode", "port", a.Name, "err", err)
	}
	conn, err := pktio.Open(a.Index)
	if err != nil {
		return nil, err
	}
	p := &Port{name: a.Name, index: a.Index, mac: pkt.MACFrom(a.HardwareAddr), mtu: a.MTU, conn: conn}
	go m.rxLoop(p)
	return p, nil
}

func (m *Manager) rxLoop(p *Port) {
	buf := make([]byte, 65536+4)
	for {
		b, ts, err := p.conn.ReadFrame(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			if errors.Is(err, unix.ENETDOWN) || errors.Is(err, unix.ENXIO) {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			m.log.Error("port read", "port", p.name, "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		p.counters.RxFrames.Add(1)
		p.counters.RxBytes.Add(uint64(len(b)))
		if !m.onFrame(p.name, b, ts) {
			p.counters.RxUnmatched.Add(1)
		}
	}
}

var ErrNoPort = errors.New("no such port")

func (m *Manager) Send(name string, b []byte) error {
	m.mu.RLock()
	p := m.ports[name]
	m.mu.RUnlock()
	if p == nil {
		return ErrNoPort
	}
	if err := p.conn.WriteFrame(b); err != nil {
		p.counters.TxErrors.Add(1)
		return err
	}
	p.counters.TxFrames.Add(1)
	p.counters.TxBytes.Add(uint64(len(b)))
	return nil
}

func (m *Manager) PortUp(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.ports[name]
	return p != nil && p.adminUp.Load() && p.operUp.Load()
}

// SetAdmin sets the link admin state. On a veth this also drops carrier on
// the peer, which is how link failures are presented to the DUT. The desired
// state is remembered and reapplied if the port (re)appears.
func (m *Manager) SetAdmin(name string, up bool) error {
	m.mu.Lock()
	m.desired[name] = up
	_, exists := m.ports[name]
	m.mu.Unlock()
	if !exists {
		if !m.match.MatchString(name) {
			return fmt.Errorf("%w: %s", ErrNoPort, name)
		}
		return nil // applied on adoption
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	if up {
		return netlink.LinkSetUp(l)
	}
	return netlink.LinkSetDown(l)
}

// Desired returns admin states set through the API, for scenario export.
func (m *Manager) Desired() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]bool, len(m.desired))
	for k, v := range m.desired {
		out[k] = v
	}
	return out
}

func (m *Manager) Get(name string) (Info, bool) {
	m.mu.RLock()
	p := m.ports[name]
	m.mu.RUnlock()
	if p == nil {
		return Info{}, false
	}
	return p.info(), true
}

func (m *Manager) List() []Info {
	m.mu.RLock()
	out := make([]Info, 0, len(m.ports))
	for _, p := range m.ports {
		out = append(out, p.info())
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

func (p *Port) info() Info {
	c := &p.counters
	return Info{Name: p.name, Index: p.index, MAC: p.mac, MTU: p.mtu,
		AdminUp: p.adminUp.Load(), OperUp: p.operUp.Load(),
		RxFrames: c.RxFrames.Load(), RxBytes: c.RxBytes.Load(), TxFrames: c.TxFrames.Load(),
		TxBytes: c.TxBytes.Load(), TxErrors: c.TxErrors.Load(), RxUnmatched: c.RxUnmatched.Load()}
}
