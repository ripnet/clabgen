// Package wire is what a monitor port saw of traffic on a mirrored link,
// and the checks flows make on it: which VLAN tags their frames, and the OAM
// of their service, carry inside an MPLS service (a pseudowire, a VPLS), per
// direction. The monitor (protocols/monitor) fills Status; Judge compares it
// with a flow's Expect. They can be on different agents: flow IDs are unique
// in a lab.
package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Directions of a flow's traffic: from its device (a-to-z) and back.
const (
	Both = "both"
	AtoZ = "a-to-z"
	ZtoA = "z-to-a"
)

// Verdicts.
const (
	Pass    = "pass"
	Fail    = "fail"
	Pending = "pending" // not seen yet
)

// Untagged is how a stack without tags is written.
const Untagged = "untagged"

// Stack is a VLAN tag stack written as text, outermost first: "3162,163",
// "0x88a8:3162,163" for a TPID other than 0x8100, "untagged". In JSON and
// YAML it may also be a list: [3162, 163].
type Stack string

func (s *Stack) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(b, &items); err != nil {
			return err
		}
		parts := make([]string, 0, len(items))
		for _, it := range items {
			var str string
			if json.Unmarshal(it, &str) == nil {
				parts = append(parts, str)
				continue
			}
			var n json.Number
			if err := json.Unmarshal(it, &n); err != nil {
				return fmt.Errorf("VLAN tag %s: a VID or TPID:VID", it)
			}
			parts = append(parts, n.String())
		}
		if len(parts) == 0 {
			*s = Untagged
		} else {
			*s = Stack(strings.Join(parts, ","))
		}
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		var n json.Number
		if json.Unmarshal(b, &n) != nil {
			return errors.New("VLAN tags: text like 3162,163 or a list")
		}
		str = n.String()
	}
	*s = Stack(str)
	return nil
}

// Parse returns the tags (TPID 0 for 0x8100); ok is false when s is empty.
func (s Stack) Parse() (tags []pkt.VLANTag, ok bool, err error) {
	t := strings.TrimSpace(string(s))
	if t == "" {
		return nil, false, nil
	}
	if strings.EqualFold(t, Untagged) || t == "-" {
		return []pkt.VLANTag{}, true, nil
	}
	for _, p := range strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == ' ' || r == '.' }) {
		var tag pkt.VLANTag
		vid := p
		if i := strings.IndexByte(p, ':'); i >= 0 {
			tp, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(p[:i]), "0x"), 16, 16)
			if err != nil || !pkt.IsTPID(uint16(tp)) {
				return nil, true, fmt.Errorf("%q: TPID must be 0x8100, 0x88a8 or 0x9100", p)
			}
			if uint16(tp) != pkt.TPIDDot1Q {
				tag.TPID = uint16(tp)
			}
			vid = p[i+1:]
		}
		v, err := strconv.ParseUint(vid, 10, 16)
		if err != nil || v > 4095 {
			return nil, true, fmt.Errorf("%q: VID must be 0-4095", p)
		}
		tag.VID = uint16(v)
		tags = append(tags, tag)
	}
	return tags, true, nil
}

// Normal is the stack in the form the monitor reports, for comparison.
func (s Stack) Normal() (string, error) {
	tags, ok, err := s.Parse()
	if err != nil || !ok {
		return "", err
	}
	return Format(tags), nil
}

// Format writes tags as a Stack: VIDs, with the TPID when it isn't 0x8100.
// PCP and DEI aren't part of it.
func Format(tags []pkt.VLANTag) string {
	if len(tags) == 0 {
		return Untagged
	}
	parts := make([]string, len(tags))
	for i, t := range tags {
		if t.TPID == 0 || t.TPID == pkt.TPIDDot1Q {
			parts[i] = strconv.Itoa(int(t.VID))
		} else {
			parts[i] = fmt.Sprintf("%#04x:%d", t.TPID, t.VID)
		}
	}
	return strings.Join(parts, ",")
}

// Expect is what a flow's frames should look like on a monitored link.
type Expect struct {
	Data      Stack  `json:"data,omitempty" jsonschema_description:"VLAN tags the flow's frames carry inside the service on the monitored link, outermost first: 3162,163 (0x88a8:3162 for another TPID; untagged for none). Not checked when empty"`
	OAM       Stack  `json:"oam,omitempty" jsonschema_description:"VLAN tags the CFM/Y.1731 frames (CCM, DMM…) in the same service carry, as for data. Not checked when empty"`
	Direction string `json:"direction,omitempty" jsonschema:"enum=both,enum=a-to-z,enum=z-to-a" jsonschema_description:"Which directions are checked: a-to-z is from this flow's device; z-to-a needs replies (echo) to be seen. Default: both"`
}

func (e *Expect) Validate() error {
	if e == nil {
		return nil
	}
	if _, err := e.Data.Normal(); err != nil {
		return fmt.Errorf("wire.data: %w", err)
	}
	if _, err := e.OAM.Normal(); err != nil {
		return fmt.Errorf("wire.oam: %w", err)
	}
	switch e.Direction {
	case "", Both, AtoZ, ZtoA:
	default:
		return errors.New("wire.direction must be both, a-to-z or z-to-a")
	}
	return nil
}

// Dirs are the directions checked.
func (e *Expect) Dirs() []string {
	switch e.Direction {
	case AtoZ, ZtoA:
		return []string{e.Direction}
	}
	return []string{AtoZ, ZtoA}
}

// ---- what a monitor reports ----

// Status is a monitor's view of its link.
type Status struct {
	Direction string `json:"direction,omitempty"` // configured: ingress, egress
	Frames    uint64 `json:"frames"`
	MPLS      uint64 `json:"mpls"`
	Undecoded uint64 `json:"undecoded"`
	// Links are the outer unicast MAC pairs seen: one per direction of a
	// point-to-point link.
	Links    []Link    `json:"links"`
	Services []Service `json:"services"`
	// Truncated: more services or entries than kept; the rest are counted
	// only in Frames.
	Truncated bool `json:"truncated,omitempty"`
}

// Link is one outer source and destination MAC.
type Link struct {
	Src    string  `json:"src"`
	Dst    string  `json:"dst"`
	Frames uint64  `json:"frames"`
	LastS  float64 `json:"last_s"`
}

// Service is one MPLS service label in one direction (outer MAC pair), or
// plain Ethernet (no label) between two MACs.
type Service struct {
	Label *uint32 `json:"label,omitempty"`
	// Transport: the labels above it, in the last frame (they change with
	// PHP and paths, so they don't identify it).
	Transport   []uint32 `json:"transport,omitempty"`
	FlowLabel   bool     `json:"flow_label,omitempty"`   // a FAT label below it (RFC 6391)
	Entropy     bool     `json:"entropy,omitempty"`      // an entropy label (RFC 6790)
	ControlWord bool     `json:"control_word,omitempty"` // a pseudowire control word
	IP          bool     `json:"ip,omitempty"`           // IP right after the labels (L3VPN), no inner Ethernet
	Src         string   `json:"src"`                    // outer MACs
	Dst         string   `json:"dst"`
	OuterTags   string   `json:"outer_tags,omitempty"` // on the link itself
	Frames      uint64   `json:"frames"`
	LastS       float64  `json:"last_s"`
	Data        []Seen   `json:"data,omitempty"`  // clabgen flows (signed)
	OAM         []Seen   `json:"oam,omitempty"`   // CFM / Y.1731
	Other       []Seen   `json:"other,omitempty"` // anything else, by ethertype
}

// Key names the service: its label and direction.
func (s *Service) Key() string {
	l := "-"
	if s.Label != nil {
		l = strconv.FormatUint(uint64(*s.Label), 10)
	}
	return l + " " + s.Src + ">" + s.Dst
}

// Seen is one kind of frame in a service, with the tags it carried.
type Seen struct {
	FlowID string  `json:"flow_id,omitempty"`
	Kind   string  `json:"kind"` // icmp, udp, l2; CCM, DMM…; an ethertype
	Reply  bool    `json:"reply,omitempty"`
	Level  *uint8  `json:"level,omitempty"`
	MEPID  *uint16 `json:"mep_id,omitempty"`
	MA     string  `json:"ma,omitempty"`
	Tags   string  `json:"tags"`
	Src    string  `json:"src,omitempty"` // inner MACs
	Dst    string  `json:"dst,omitempty"`
	IPSrc  string  `json:"ip_src,omitempty"`
	IPDst  string  `json:"ip_dst,omitempty"`
	Count  uint64  `json:"count"`
	LastS  float64 `json:"last_s"`
}

// ---- judging a flow ----

// Flow is a flow to judge: its ID, the MAC and IP address it sends from,
// and what it expects.
type Flow struct {
	FlowID string
	MAC    string
	IP     string
	Expect Expect
}

// Monitor is one monitor's status, named for display.
type Monitor struct {
	Name   string // e.g. agent/instance
	Status Status
}

// Verdict is a flow's check against what the monitors saw.
type Verdict struct {
	Verdict string `json:"verdict"`
	// Rows: per monitor, service and direction where the flow was seen,
	// and the directions expected but not seen (pending).
	Rows []Row `json:"rows"`
}

// Row is one direction of the flow in one service, on one monitor.
type Row struct {
	Direction string  `json:"direction"`
	Monitor   string  `json:"monitor,omitempty"`
	Label     *uint32 `json:"label,omitempty"`
	Src       string  `json:"src,omitempty"`
	Dst       string  `json:"dst,omitempty"`
	Data      Class   `json:"data"`
	OAM       Class   `json:"oam"`
	Verdict   string  `json:"verdict"`
}

// Class is data or OAM in a row: the stacks seen, with counts.
type Class struct {
	Expect  string       `json:"expect,omitempty"`
	Seen    []StackCount `json:"seen,omitempty"`
	Verdict string       `json:"verdict,omitempty"` // empty: not checked
}

type StackCount struct {
	Tags  string `json:"tags"`
	Count uint64 `json:"count"`
}

// direction tells which way a flow's frame went, from its inner addresses.
func (f Flow) direction(s Seen) string {
	switch {
	case f.MAC != "" && s.Src == f.MAC, f.IP != "" && s.IPSrc == f.IP:
		return AtoZ
	case f.MAC != "" && s.Dst == f.MAC, f.IP != "" && s.IPDst == f.IP:
		return ZtoA
	case s.Reply:
		return ZtoA
	}
	return AtoZ
}

// Judge checks a flow against what the monitors saw: in every service and
// direction its frames were seen, its frames' tags against Data and the OAM
// frames' tags against OAM. Every expected direction must be seen, and every
// stack seen must be the expected one.
func Judge(f Flow, mons []Monitor) Verdict {
	wantData, _ := f.Expect.Data.Normal()
	wantOAM, _ := f.Expect.OAM.Normal()
	dirs := f.Expect.Dirs()
	var rows []Row
	seenDir := map[string]bool{}
	for _, m := range mons {
		monDir := map[string]bool{}
		mas := map[string]bool{}    // MAs of the OAM in the flow's services
		svcs := map[*Service]bool{} // services the flow was seen in
		for i := range m.Status.Services {
			svc := &m.Status.Services[i]
			by := map[string]*Row{}
			for _, s := range svc.Data {
				if s.FlowID != f.FlowID {
					continue
				}
				d := f.direction(s)
				r := by[d]
				if r == nil {
					r = &Row{Direction: d, Monitor: m.Name, Label: svc.Label, Src: svc.Src, Dst: svc.Dst,
						Data: Class{Expect: wantData}, OAM: Class{Expect: wantOAM}}
					by[d] = r
				}
				r.Data.Seen = addStack(r.Data.Seen, s.Tags, s.Count)
			}
			for _, d := range []string{AtoZ, ZtoA} {
				r := by[d]
				if r == nil {
					continue
				}
				for _, o := range svc.OAM {
					r.OAM.Seen = addStack(r.OAM.Seen, o.Tags, o.Count)
				}
				if !contains(dirs, d) {
					r.Data.Expect, r.OAM.Expect = "", ""
				}
				judgeRow(r)
				seenDir[d], monDir[d] = true, true
				svcs[svc] = true
				for _, o := range svc.OAM {
					if o.MA != "" {
						mas[o.MA] = true
					}
				}
				rows = append(rows, *r)
			}
		}
		// A direction without the flow's frames (a one-way flow, or the
		// far end not answering): its OAM is in the service, the other way
		// round, carrying the same MAs.
		for _, d := range dirs {
			if monDir[d] || len(mas) == 0 {
				continue
			}
			for i := range m.Status.Services {
				svc := &m.Status.Services[i]
				if svcs[svc] || svc.Label == nil || !reverseOf(svc, svcs) {
					continue
				}
				r := Row{Direction: d, Monitor: m.Name, Label: svc.Label, Src: svc.Src, Dst: svc.Dst,
					Data: Class{Expect: wantData}, OAM: Class{Expect: wantOAM}}
				for _, o := range svc.OAM {
					if mas[o.MA] {
						r.OAM.Seen = addStack(r.OAM.Seen, o.Tags, o.Count)
					}
				}
				if len(r.OAM.Seen) == 0 {
					continue
				}
				judgeRow(&r)
				seenDir[d] = true
				rows = append(rows, r)
			}
		}
	}
	for _, d := range dirs {
		if !seenDir[d] {
			r := Row{Direction: d, Data: Class{Expect: wantData}, OAM: Class{Expect: wantOAM}}
			r.Verdict = Pending
			if wantData != "" {
				r.Data.Verdict = Pending
			}
			if wantOAM != "" {
				r.OAM.Verdict = Pending
			}
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Direction != rows[j].Direction {
			return rows[i].Direction < rows[j].Direction
		}
		return rows[i].Monitor < rows[j].Monitor
	})
	v := Verdict{Rows: rows, Verdict: Pass}
	if wantData == "" && wantOAM == "" {
		v.Verdict = ""
		return v
	}
	for _, r := range rows {
		switch r.Verdict {
		case Fail:
			v.Verdict = Fail
		case Pending:
			if v.Verdict != Fail {
				v.Verdict = Pending
			}
		}
	}
	return v
}

// reverseOf: svc runs the other way from one of the services (its MACs swapped).
func reverseOf(svc *Service, of map[*Service]bool) bool {
	for o := range of {
		if o.Src == svc.Dst && o.Dst == svc.Src {
			return true
		}
	}
	return false
}

func judgeRow(r *Row) {
	r.Verdict = ""
	for _, c := range []*Class{&r.Data, &r.OAM} {
		if c.Expect == "" {
			continue
		}
		c.Verdict = Pass
		if len(c.Seen) == 0 {
			c.Verdict = Pending
		}
		for _, s := range c.Seen {
			if s.Tags != c.Expect {
				c.Verdict = Fail
			}
		}
		switch {
		case c.Verdict == Fail:
			r.Verdict = Fail
		case c.Verdict == Pending && r.Verdict != Fail:
			r.Verdict = Pending
		case r.Verdict == "":
			r.Verdict = Pass
		}
	}
}

func addStack(list []StackCount, tags string, n uint64) []StackCount {
	for i := range list {
		if list[i].Tags == tags {
			list[i].Count += n
			return list
		}
	}
	return append(list, StackCount{Tags: tags, Count: n})
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
