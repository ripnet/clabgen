// Package lldp is an LLDP agent (IEEE 802.1AB): it advertises a device
// with the basic and 802.1/802.3 TLVs (fast start, shutdown LLDPDU on
// stop) and keeps a table of the neighbors it hears, aged by their TTL.
// Actions send at once, pause transmission, or shut down and resume.
package lldp

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
	"github.com/ripnet/clabgen/internal/version"
)

const TypeName = "lldp"

func init() { protocol.Register(lldpType{}) }

type lldpType struct{}

func (lldpType) Name() string { return TypeName }
func (lldpType) Description() string {
	return "LLDP agent (802.1AB): advertise the device (basic, 802.1 and 802.3 TLVs) and list the neighbors heard"
}
func (lldpType) NewConfig() any       { return defaultConfig() }
func (lldpType) EtherTypes() []uint16 { return []uint16{EtherType} }
func (lldpType) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "send", Description: "Send an LLDPDU now"},
		{Name: "pause", Description: "Stop transmitting for a while (neighbors age us out), then resume", NewArgs: func() any { return &PauseArgs{Seconds: 150} }},
		{Name: "shutdown", Description: "Send a shutdown LLDPDU (TTL 0) and stop transmitting until resume"},
		{Name: "resume", Description: "Resume transmitting (with fast start) after pause or shutdown"},
		{Name: "clear", Description: "Forget neighbors and reset counters"},
	}
}

// maxNeighbors bounds the table (each MSAP is one entry).
const maxNeighbors = 64

const fastInterval = time.Second

type neighbor struct {
	info    *Info
	raw     []byte
	mac     pkt.MAC
	rxAt    time.Time
	since   time.Time
	changes int
}

type session struct {
	env protocol.Env
	cfg Config
	dst pkt.MAC

	mu        sync.Mutex
	neighbors map[string]*neighbor
	nextTx    time.Time
	fast      int // fast-start LLDPDUs left
	pauseTill time.Time
	shut      bool
	counters  map[string]uint64

	stop chan struct{}
	done chan struct{}
}

func (lldpType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := *c.(*Config)
	s := &session{env: env, cfg: cfg, neighbors: map[string]*neighbor{}, counters: map[string]uint64{},
		fast: cfg.FastCount, stop: make(chan struct{}), done: make(chan struct{})}
	switch cfg.Dest {
	case "nearest-nontpmr":
		s.dst = NearestNonTPMR
	case "nearest-customer":
		s.dst = NearestCustomer
	default:
		s.dst = NearestBridge
	}
	if _, err := s.build(s.ttl()); err != nil {
		return nil, err
	}
	go s.run()
	return s, nil
}

func (s *session) tx() bool { return s.cfg.Mode == "tx-rx" || s.cfg.Mode == "tx-only" }
func (s *session) rx() bool { return s.cfg.Mode == "tx-rx" || s.cfg.Mode == "rx-only" }

// ttl is txTTL (802.1AB 9.2.5.22).
func (s *session) ttl() uint16 {
	return uint16(min(65535, s.cfg.TxInterval*s.cfg.TxHold+1))
}

func (s *session) systemName() string {
	if s.cfg.SystemName != "" {
		return s.cfg.SystemName
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return s.env.InstanceName()
}

// build encodes our LLDPDU with the given TTL (0: shutdown, mandatory
// TLVs only).
func (s *session) build(ttl uint16) ([]byte, error) {
	c := s.cfg
	dev := s.env.Device()
	var w tlvWriter
	cs := chassisSubtypes[c.ChassisIDSubtype]
	cid := c.ChassisID
	if cid == "" {
		cid = s.systemName()
		if c.ChassisIDSubtype == "mac" {
			cid = dev.MAC().String()
		}
	}
	v, err := idValue(cs == chassisSubtypes["mac"], cs == chassisSubtypes["network-address"], cid)
	if err != nil {
		return nil, fmt.Errorf("chassis ID: %w", err)
	}
	w.add(tlvChassisID, append([]byte{cs}, v...))
	ps := portSubtypes[c.PortIDSubtype]
	pid := c.PortID
	if pid == "" {
		switch c.PortIDSubtype {
		case "interface-name":
			pid = dev.Port()
		case "mac":
			pid = dev.MAC().String()
		default:
			pid = dev.Name()
		}
	}
	if v, err = idValue(ps == portSubtypes["mac"], ps == portSubtypes["network-address"], pid); err != nil {
		return nil, fmt.Errorf("port ID: %w", err)
	}
	w.add(tlvPortID, append([]byte{ps}, v...))
	w.add(tlvTTL, binary.BigEndian.AppendUint16(nil, ttl))
	if ttl > 0 && !c.NoOptional {
		pd := c.PortDescription
		if pd == "" {
			pd = dev.Name()
		}
		w.add(tlvPortDesc, []byte(pd))
		w.add(tlvSysName, []byte(s.systemName()))
		sd := c.SystemDescription
		if sd == "" {
			sd = "clabgen " + version.Version
		}
		w.add(tlvSysDesc, []byte(sd))
		caps := c.Capabilities
		if len(caps) == 0 {
			caps = []string{"router"}
		}
		en := c.EnabledCaps
		if len(en) == 0 {
			en = caps
		}
		cm, _ := capMask(caps)
		em, _ := capMask(en)
		w.add(tlvSysCap, binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, cm), em))
		for _, a := range s.mgmtAddrs() {
			w.add(tlvMgmtAddr, mgmtAddr(a, 1))
		}
		if c.PortVLANID != 0 {
			w.org(oui8021, 1, binary.BigEndian.AppendUint16(nil, c.PortVLANID))
		}
		for _, n := range c.VLANNames {
			w.org(oui8021, 3, append(binary.BigEndian.AppendUint16(nil, n.VID), append([]byte{byte(len(n.Name))}, n.Name...)...))
		}
		if la := c.LinkAgg; la != nil {
			var st byte
			if la.Capable {
				st |= 1
			}
			if la.Enabled {
				st |= 2
			}
			w.org(oui8021, 7, binary.BigEndian.AppendUint32([]byte{st}, la.AggPortID))
		}
		if m := c.MACPHY; m != nil {
			var st byte
			if m.AutonegSupported {
				st |= 1
			}
			if m.AutonegEnabled {
				st |= 2
			}
			w.org(oui8023, 1, binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16([]byte{st}, m.Advertised), m.MAUType))
		}
		if c.MaxFrameSize != 0 {
			w.org(oui8023, 4, binary.BigEndian.AppendUint16(nil, c.MaxFrameSize))
		}
		for _, t := range c.CustomTLVs {
			typ, v, err := t.encode()
			if err != nil {
				return nil, err
			}
			w.add(typ, v)
		}
	}
	w.add(tlvEnd, nil)
	if len(w.b) > 1500 {
		return nil, fmt.Errorf("LLDPDU is %d bytes (more than 1500)", len(w.b))
	}
	return w.b, nil
}

// mgmtAddrs are the management addresses to advertise.
func (s *session) mgmtAddrs() []netip.Addr {
	var out []netip.Addr
	if len(s.cfg.MgmtAddresses) == 0 || len(s.cfg.MgmtAddresses) == 1 && s.cfg.MgmtAddresses[0] == "auto" {
		if ip := s.env.IP(); ip != nil {
			for _, p := range ip.Addresses() {
				if !p.Addr().IsLinkLocalUnicast() {
					out = append(out, p.Addr())
				}
			}
		}
		return out
	}
	for _, a := range s.cfg.MgmtAddresses {
		if x, err := netip.ParseAddr(a); err == nil {
			out = append(out, x)
		}
	}
	return out
}

func (s *session) send(ttl uint16) {
	b, err := s.build(ttl)
	if err == nil {
		err = s.env.Send(s.dst, EtherType, b, protocol.SendOpts{})
	}
	s.mu.Lock()
	if err != nil {
		s.counters["tx_errors"]++
	} else if ttl == 0 {
		s.counters["tx_shutdowns"]++
	} else {
		s.counters["tx_frames"]++
	}
	s.mu.Unlock()
}

func (s *session) run() {
	defer close(s.done)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

func (s *session) tick(now time.Time) {
	s.mu.Lock()
	for k, n := range s.neighbors {
		if now.Sub(n.rxAt) > time.Duration(n.info.TTL)*time.Second {
			delete(s.neighbors, k)
			s.counters["ageouts"]++
			go s.env.Emit("neighbor-aged-out", fmt.Sprintf("neighbor %s port %s aged out", n.name(), n.info.PortID), nil)
		}
	}
	due := s.tx() && !s.shut && !now.Before(s.pauseTill) && !now.Before(s.nextTx)
	if due {
		iv := time.Duration(s.cfg.TxInterval) * time.Second
		if s.fast > 0 {
			s.fast--
			iv = fastInterval
		}
		s.nextTx = now.Add(iv)
	}
	s.mu.Unlock()
	if due {
		s.send(s.ttl())
	}
}

func (n *neighbor) name() string {
	if n.info.SysName != "" {
		return n.info.SysName
	}
	return n.info.ChassisID
}

func (s *session) HandleRx(f *pkt.Frame) {
	if f.Dst != s.dst || f.Src == s.env.Device().MAC() || !s.rx() {
		return
	}
	in, err := Decode(f.Payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters["rx_frames"]++
	if err != nil {
		s.counters["rx_discarded"]++
		return
	}
	s.counters["rx_tlvs_unrecognized"] += uint64(len(in.Other))
	key := in.ChassisSubtype + "/" + in.ChassisID + "|" + in.PortSubtype + "/" + in.PortID
	n := s.neighbors[key]
	now := time.Now()
	if in.TTL == 0 {
		if n != nil {
			delete(s.neighbors, key)
			s.counters["rx_shutdowns"]++
			go s.env.Emit("neighbor-removed", fmt.Sprintf("neighbor %s port %s shut down", n.name(), in.PortID), nil)
		}
		return
	}
	raw := append([]byte(nil), f.Payload...)
	if n == nil {
		if len(s.neighbors) >= maxNeighbors {
			s.counters["rx_too_many_neighbors"]++
			return
		}
		n = &neighbor{since: now}
		s.neighbors[key] = n
		s.counters["inserts"]++
		// A new neighbor: tell it about us quickly (802.1AB 9.2.7.7.4).
		if s.tx() && !s.shut && s.fast == 0 {
			s.fast, s.nextTx = s.cfg.FastCount, now
		}
		go s.env.Emit("neighbor-added", fmt.Sprintf("neighbor %s port %s (from %s)", orName(in), in.PortID, f.Src), nil)
	} else if !bytes.Equal(n.raw, raw) {
		n.changes++
		s.counters["updates"]++
		go s.env.Emit("neighbor-changed", fmt.Sprintf("neighbor %s port %s changed its advertisement", orName(in), in.PortID), nil)
	}
	n.info, n.raw, n.mac, n.rxAt = in, raw, f.Src, now
}

func orName(in *Info) string {
	if in.SysName != "" {
		return in.SysName
	}
	return in.ChassisID
}

func (s *session) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
	s.mu.Lock()
	shut := s.shut
	s.mu.Unlock()
	if s.tx() && !shut {
		s.send(0) // tell neighbors we're gone
	}
}

// ---- status ----

type NeighborStatus struct {
	MAC          string  `json:"mac"`
	TTLRemaining float64 `json:"ttl_remaining_s"`
	Since        string  `json:"since"`
	LastRx       string  `json:"last_rx"`
	Changes      int     `json:"changes"`
	Info
}

type Status struct {
	Mode        string            `json:"mode"`
	Dest        string            `json:"dest"`
	Transmit    string            `json:"transmit"` // on, paused, shut down, off
	NextTxS     float64           `json:"next_tx_s,omitempty"`
	PausedUntil string            `json:"paused_until,omitempty"`
	Local       *Info             `json:"local,omitempty"` // what we advertise
	Neighbors   []NeighborStatus  `json:"neighbors"`
	Counters    map[string]uint64 `json:"counters"`
}

func (s *session) Status() any {
	now := time.Now()
	st := Status{Mode: s.cfg.Mode, Dest: s.cfg.Dest, Neighbors: []NeighborStatus{}, Counters: map[string]uint64{}}
	if b, err := s.build(s.ttl()); err == nil {
		st.Local, _ = Decode(b)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.tx():
		st.Transmit = "off"
	case s.shut:
		st.Transmit = "shut down"
	case now.Before(s.pauseTill):
		st.Transmit, st.PausedUntil = "paused", s.pauseTill.Format(time.RFC3339)
	default:
		st.Transmit = "on"
		st.NextTxS = max(0, s.nextTx.Sub(now).Seconds())
	}
	for k, v := range s.counters {
		st.Counters[k] = v
	}
	keys := make([]string, 0, len(s.neighbors))
	for k := range s.neighbors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := s.neighbors[k]
		st.Neighbors = append(st.Neighbors, NeighborStatus{MAC: n.mac.String(), Info: *n.info, Changes: n.changes,
			TTLRemaining: (time.Duration(n.info.TTL)*time.Second - now.Sub(n.rxAt)).Seconds(),
			Since:        n.since.Format(time.RFC3339), LastRx: n.rxAt.Format(time.RFC3339)})
	}
	return st
}

// ---- actions ----

func (s *session) Action(_ context.Context, name string, args any) (any, error) {
	now := time.Now()
	switch name {
	case "send":
		if !s.tx() {
			return nil, fmt.Errorf("mode %s does not transmit", s.cfg.Mode)
		}
		s.send(s.ttl())
		return map[string]any{"sent": true}, nil
	case "pause":
		a := args.(*PauseArgs)
		s.mu.Lock()
		s.pauseTill = now.Add(time.Duration(a.Seconds) * time.Second)
		s.nextTx = s.pauseTill
		s.mu.Unlock()
		return map[string]any{"paused_until": s.pauseTill.Format(time.RFC3339)}, nil
	case "shutdown":
		s.mu.Lock()
		was := s.shut
		s.shut = true
		s.mu.Unlock()
		if !was && s.tx() {
			s.send(0)
		}
		return map[string]any{"transmit": "shut down"}, nil
	case "resume":
		s.mu.Lock()
		s.shut, s.pauseTill, s.nextTx, s.fast = false, time.Time{}, now, s.cfg.FastCount
		s.mu.Unlock()
		return map[string]any{"transmit": "on"}, nil
	case "clear":
		s.mu.Lock()
		s.neighbors, s.counters = map[string]*neighbor{}, map[string]uint64{}
		s.mu.Unlock()
		return map[string]any{"cleared": true}, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}
