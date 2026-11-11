// Package monitor is a monitor port: its device's port takes a router's
// port mirror of a link and sends nothing. Every frame is taken apart (MPLS
// labels, pseudowire control word, the frame inside) and counted per service
// label and direction: clabgen flows (by their signature), CFM and Y.1731
// frames, and anything else, each with the VLAN tags it carried inside the
// service. Flows check those tags against their wire expectations
// (internal/wire), on this agent or another.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
	"github.com/ripnet/clabgen/internal/wire"
)

const TypeName = "monitor"

// Bounds on what's kept; beyond them frames are only counted.
const (
	maxServices = 512
	maxSeen     = 128 // entries per class in a service
	maxLinks    = 64
)

func init() { protocol.Register(monitorType{}) }

type monitorType struct{}

func (monitorType) Name() string { return TypeName }
func (monitorType) Description() string {
	return "Monitor port for a router's port mirror: per MPLS service and direction, the VLAN tags of flows and OAM inside it. The port sends nothing"
}
func (monitorType) NewConfig() any       { return &Config{ControlWord: cwAuto} }
func (monitorType) EtherTypes() []uint16 { return nil }
func (monitorType) Actions() []protocol.Action {
	return []protocol.Action{{Name: "clear", Description: "Forget what was seen (e.g. before the next configuration)"}}
}

// Config of a monitor.
type Config struct {
	ControlWord string `json:"control_word" jsonschema:"enum=auto,enum=on,enum=off,description=Whether pseudowires carry a control word after the labels. auto tells by what follows. Default: auto"`
	Direction   string `json:"direction,omitempty" jsonschema:"enum=ingress,enum=egress" jsonschema_description:"The mirror carries one direction only (one mirror destination per direction): shown with what was seen. Default: both, told apart by MAC addresses"`
}

func (c *Config) Validate() error {
	switch c.ControlWord {
	case "", cwAuto, cwOn, cwOff:
	default:
		return errors.New("control_word must be auto, on or off")
	}
	switch c.Direction {
	case "", "ingress", "egress":
	default:
		return errors.New("direction must be ingress or egress")
	}
	return nil
}

func (monitorType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := c.(*Config)
	if cfg.ControlWord == "" {
		cfg.ControlWord = cwAuto
	}
	mon, ok := env.(protocol.Monitorer)
	if !ok {
		return nil, errors.New("this agent can't make a port a monitor")
	}
	s := &session{env: env, cfg: cfg}
	s.reset()
	release, err := mon.Monitor(s.rx)
	if err != nil {
		return nil, err
	}
	s.release = release
	return s, nil
}

type session struct {
	env     protocol.Env
	cfg     *Config
	release func()

	mu        sync.Mutex
	frames    uint64
	mpls      uint64
	undecoded uint64
	truncated bool
	links     map[linkKey]*link
	services  map[svcKey]*service
}

type linkKey struct{ src, dst pkt.MAC }

type link struct {
	frames uint64
	last   time.Time
}

// svcKey: an MPLS service label in one direction, or plain Ethernet by its
// tags.
type svcKey struct {
	mpls     bool
	label    uint32
	src, dst pkt.MAC
	tags     string
}

type service struct {
	transport                     []uint32
	flowLabel, entropy, cw, ip    bool
	frames                        uint64
	last                          time.Time
	data, oam, other              map[seenKey]*seen
	dataOrder, oamOrder, othOrder []seenKey
}

type seenKey struct {
	flowID       uint32
	kind         string
	reply        bool
	level        int16
	mepID        int32
	ma           string
	tags         string
	src, dst     pkt.MAC
	ipSrc, ipDst netip.Addr
}

type seen struct {
	count       uint64
	first, last time.Time
}

func (s *session) reset() {
	s.frames, s.mpls, s.undecoded, s.truncated = 0, 0, 0, false
	s.links = map[linkKey]*link{}
	s.services = map[svcKey]*service{}
}

// rx takes one mirrored frame.
func (s *session) rx(b []byte, ts time.Time) {
	d := decode(b, s.cfg.ControlWord)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames++
	if d == nil {
		s.undecoded++
		return
	}
	// A direction of the link is a unicast MAC pair; the routers'
	// multicast (IS-IS, LDP hellos) isn't one.
	if lk := (linkKey{d.outer.Src, d.outer.Dst}); !lk.dst.IsMulticast() {
		if l := s.links[lk]; l != nil {
			l.frames++
			l.last = ts
		} else if len(s.links) < maxLinks {
			s.links[lk] = &link{frames: 1, last: ts}
		}
	}
	var k svcKey
	if d.mpls {
		s.mpls++
		k = svcKey{mpls: true, label: d.service, src: d.outer.Src, dst: d.outer.Dst, tags: wire.Format(d.outer.Tags)}
	} else {
		k = svcKey{tags: wire.Format(d.outer.Tags)}
	}
	svc := s.services[k]
	if svc == nil {
		if len(s.services) >= maxServices {
			s.truncated = true
			return
		}
		svc = &service{data: map[seenKey]*seen{}, oam: map[seenKey]*seen{}, other: map[seenKey]*seen{}}
		s.services[k] = svc
	}
	svc.frames++
	svc.last = ts
	if d.mpls {
		svc.transport = d.transport
		svc.flowLabel = svc.flowLabel || d.flowLabel
		svc.entropy = svc.entropy || d.entropy
		svc.cw = svc.cw || d.cw
		svc.ip = svc.ip || d.ip
	}
	if d.inner == nil && d.what.class == "" {
		return
	}
	w := d.what
	sk := seenKey{kind: w.kind, level: -1, mepID: -1}
	if d.inner != nil {
		sk.tags = wire.Format(d.inner.Tags)
		if d.ip {
			sk.tags = ""
		}
	}
	switch w.class {
	case "data":
		sk.flowID, sk.reply, sk.ipSrc, sk.ipDst = w.flowID, w.reply, w.ipSrc, w.ipDst
		if d.inner != nil && !d.ip {
			sk.src, sk.dst = d.inner.Src, d.inner.Dst
		}
		s.count(svc.data, &svc.dataOrder, sk, ts)
	case "oam":
		sk.level, sk.ma = int16(*w.level), w.ma
		if w.mepID != nil {
			sk.mepID = int32(*w.mepID)
		}
		if d.inner != nil {
			sk.src = d.inner.Src
		}
		s.count(svc.oam, &svc.oamOrder, sk, ts)
	default:
		s.count(svc.other, &svc.othOrder, sk, ts)
	}
}

func (s *session) count(m map[seenKey]*seen, order *[]seenKey, k seenKey, ts time.Time) {
	if e := m[k]; e != nil {
		e.count++
		e.last = ts
		return
	}
	if len(m) >= maxSeen {
		s.truncated = true
		return
	}
	m[k] = &seen{count: 1, first: ts, last: ts}
	*order = append(*order, k)
}

func (s *session) HandleRx(*pkt.Frame) {}

func (s *session) Status() any { return s.status(time.Now()) }

func (s *session) status(now time.Time) wire.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := wire.Status{Direction: s.cfg.Direction, Frames: s.frames, MPLS: s.mpls, Undecoded: s.undecoded, Truncated: s.truncated,
		Links: []wire.Link{}, Services: []wire.Service{}}
	age := func(t time.Time) float64 { return max(0, now.Sub(t).Round(100*time.Millisecond).Seconds()) }
	for k, l := range s.links {
		st.Links = append(st.Links, wire.Link{Src: k.src.String(), Dst: k.dst.String(), Frames: l.frames, LastS: age(l.last)})
	}
	sort.Slice(st.Links, func(i, j int) bool { return st.Links[i].Frames > st.Links[j].Frames })
	keys := make([]svcKey, 0, len(s.services))
	for k := range s.services {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.mpls != b.mpls {
			return a.mpls
		}
		if a.label != b.label {
			return a.label < b.label
		}
		if a.src != b.src {
			return a.src.String() < b.src.String()
		}
		return a.tags < b.tags
	})
	for _, k := range keys {
		v := s.services[k]
		out := wire.Service{Transport: append([]uint32(nil), v.transport...), FlowLabel: v.flowLabel, Entropy: v.entropy,
			ControlWord: v.cw, IP: v.ip, OuterTags: k.tags, Frames: v.frames, LastS: age(v.last)}
		if k.mpls {
			l := k.label
			out.Label = &l
			out.Src, out.Dst = k.src.String(), k.dst.String()
		}
		if out.OuterTags == wire.Untagged {
			out.OuterTags = ""
		}
		out.Data = seenList(v.data, v.dataOrder, age)
		out.OAM = seenList(v.oam, v.oamOrder, age)
		out.Other = seenList(v.other, v.othOrder, age)
		st.Services = append(st.Services, out)
	}
	return st
}

func seenList(m map[seenKey]*seen, order []seenKey, age func(time.Time) float64) []wire.Seen {
	var out []wire.Seen
	for _, k := range order {
		e := m[k]
		w := wire.Seen{Kind: k.kind, Reply: k.reply, MA: k.ma, Tags: k.tags, Count: e.count, FirstS: age(e.first), LastS: age(e.last)}
		if k.flowID != 0 {
			w.FlowID = flowIDString(k.flowID)
		}
		if k.level >= 0 {
			l := uint8(k.level)
			w.Level = &l
		}
		if k.mepID >= 0 {
			id := uint16(k.mepID)
			w.MEPID = &id
		}
		if !k.src.IsZero() {
			w.Src = k.src.String()
		}
		if !k.dst.IsZero() {
			w.Dst = k.dst.String()
		}
		if k.ipSrc.IsValid() {
			w.IPSrc = k.ipSrc.String()
		}
		if k.ipDst.IsValid() {
			w.IPDst = k.ipDst.String()
		}
		out = append(out, w)
	}
	return out
}

// Health: whether frames are arriving.
func (s *session) Health() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frames == 0 {
		return "idle"
	}
	return "receiving"
}

func (s *session) Action(_ context.Context, name string, _ any) (any, error) {
	switch name {
	case "clear":
		s.mu.Lock()
		s.reset()
		s.mu.Unlock()
		return nil, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

func (s *session) Stop() {
	if s.release != nil {
		s.release()
	}
}
