package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"sync"

	"github.com/ripnet/clabgen/internal/protocol"
)

// A device group is many devices from one template, each with the same set
// of protocol instances (see expand.go for the template language). Members
// are ordinary devices and instances, named <device>-<protocol>; they are
// changed and deleted through their group.

const MaxGroupSize = 4096

type GroupSpec struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	// Device is a DeviceSpec template; its name defaults to {group}-{i}.
	Device    json.RawMessage `json:"device"`
	Protocols []GroupProtocol `json:"protocols,omitempty"`
}

type GroupProtocol struct {
	// Name is unique in the group (default: the type); member instances
	// are named <device>-<name>.
	Name   string          `json:"name,omitempty"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

type group struct {
	spec    GroupSpec
	members []groupMember
}

type groupMember struct {
	dev    DeviceSpec // checked
	protos []ProtocolSpec
}

// expandGroup validates a group and renders its members.
func (e *Engine) expandGroup(g GroupSpec) (GroupSpec, []groupMember, error) {
	if !nameRE.MatchString(g.Name) {
		return g, nil, fmt.Errorf("invalid group name %q", g.Name)
	}
	if g.Count < 1 || g.Count > MaxGroupSize {
		return g, nil, fmt.Errorf("group %s: count must be 1-%d", g.Name, MaxGroupSize)
	}
	if len(bytes.TrimSpace(g.Device)) == 0 {
		return g, nil, fmt.Errorf("group %s: device template is required", g.Name)
	}
	types := make([]protocol.Type, len(g.Protocols))
	seen := map[string]bool{}
	for j := range g.Protocols {
		p := &g.Protocols[j]
		if p.Name == "" {
			p.Name = p.Type
		}
		t, ok := protocol.Lookup(p.Type)
		if !ok {
			return g, nil, fmt.Errorf("group %s: unknown protocol type %q", g.Name, p.Type)
		}
		if seen[p.Name] {
			return g, nil, fmt.Errorf("group %s: protocol name %q used twice (set name on one)", g.Name, p.Name)
		}
		seen[p.Name] = true
		types[j] = t
	}
	var tmpl map[string]any
	if err := json.Unmarshal(g.Device, &tmpl); err != nil {
		return g, nil, fmt.Errorf("group %s: device: %w", g.Name, err)
	}
	if _, ok := tmpl["name"]; !ok {
		tmpl["name"] = "{group}-{i}"
	}
	members := make([]groupMember, g.Count)
	names := map[string]bool{}
	for i := range members {
		base := map[string]string{"i": strconv.Itoa(i + 1), "i0": strconv.Itoa(i), "group": g.Name}
		fail := func(err error) (GroupSpec, []groupMember, error) {
			return g, nil, fmt.Errorf("group %s member %d: %w", g.Name, i+1, err)
		}
		dv, err := render(tmpl, "device", i, base)
		if err != nil {
			return fail(err)
		}
		raw, _ := json.Marshal(dv)
		var ds DeviceSpec
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ds); err != nil {
			return fail(fmt.Errorf("device: %w", err))
		}
		ds, mac, _, err := e.checkDevice(ds)
		if err != nil {
			return fail(err)
		}
		if names[ds.Name] {
			return fail(fmt.Errorf("device name %s repeats (use {i} in the name)", ds.Name))
		}
		names[ds.Name] = true
		vars := memberVars(base, ds, mac)
		m := groupMember{dev: ds}
		for j, p := range g.Protocols {
			raw, err := renderJSON(p.Config, p.Name, i, vars)
			if err != nil {
				return fail(err)
			}
			cfg := types[j].NewConfig()
			if err := protocol.Decode(cfg, raw); err != nil {
				return fail(fmt.Errorf("%s: %w", p.Name, err))
			}
			norm, _ := json.Marshal(cfg)
			s := ProtocolSpec{Name: ds.Name + "-" + p.Name, Type: p.Type, Device: ds.Name, Config: norm}
			if !nameRE.MatchString(s.Name) {
				return fail(fmt.Errorf("invalid protocol name %q", s.Name))
			}
			m.protos = append(m.protos, s)
		}
		members[i] = m
	}
	return g, members, nil
}

// checkFreeLocked fails if a member name is taken by something outside
// group g. Caller holds mu.
func (e *Engine) checkFreeLocked(g string, members []groupMember) error {
	for _, m := range members {
		if d, ok := e.devices[m.dev.Name]; ok && d.group != g {
			return fmt.Errorf("device %s: %w", m.dev.Name, ErrExists)
		}
		for _, p := range m.protos {
			if in, ok := e.insts[p.Name]; ok && in.group != g {
				return fmt.Errorf("protocol %s: %w", p.Name, ErrExists)
			}
		}
	}
	return nil
}

func (e *Engine) CreateGroup(g GroupSpec) (GroupView, error) {
	e.gmu.Lock()
	defer e.gmu.Unlock()
	g, members, err := e.expandGroup(g)
	if err != nil {
		return GroupView{}, err
	}
	e.mu.Lock()
	if _, dup := e.groups[g.Name]; dup {
		e.mu.Unlock()
		return GroupView{}, fmt.Errorf("group %s: %w", g.Name, ErrExists)
	}
	if err := e.checkFreeLocked(g.Name, members); err != nil {
		e.mu.Unlock()
		return GroupView{}, fmt.Errorf("group %s: %w", g.Name, err)
	}
	e.groups[g.Name] = &group{spec: g, members: members}
	e.mu.Unlock()
	var errs []error
	for _, m := range members {
		errs = append(errs, e.addMember(g.Name, m)...)
	}
	e.publish("group/"+g.Name, "group-created", fmt.Sprintf("group created: %d devices", len(members)), nil)
	v, _ := e.GetGroup(g.Name, false)
	return v, errors.Join(errs...)
}

func (e *Engine) addMember(g string, m groupMember) []error {
	var errs []error
	if _, err := e.createDevice(m.dev, g); err != nil {
		errs = append(errs, err)
	}
	for _, p := range m.protos {
		if _, err := e.createProtocol(p, g); err != nil {
			errs = append(errs, fmt.Errorf("protocol %s: %w", p.Name, err))
		}
	}
	return errs
}

func (e *Engine) removeMember(g string, m groupMember) []error {
	var errs []error
	for _, p := range m.protos {
		if err := e.deleteProtocol(p.Name, g); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	if err := e.deleteDevice(m.dev.Name, g); err != nil && !errors.Is(err, ErrNotFound) {
		errs = append(errs, err)
	}
	return errs
}

// UpdateGroup changes a group in place: members whose device and
// protocol specs are unchanged keep running, changed ones are updated
// (devices as UpdateDevice does, protocols restart), and the count can grow
// or shrink.
func (e *Engine) UpdateGroup(g GroupSpec) (GroupView, error) {
	e.gmu.Lock()
	defer e.gmu.Unlock()
	g, members, err := e.expandGroup(g)
	if err != nil {
		return GroupView{}, err
	}
	e.mu.Lock()
	old, ok := e.groups[g.Name]
	if !ok {
		e.mu.Unlock()
		return GroupView{}, fmt.Errorf("group %s: %w", g.Name, ErrNotFound)
	}
	if err := e.checkFreeLocked(g.Name, members); err != nil {
		e.mu.Unlock()
		return GroupView{}, fmt.Errorf("group %s: %w", g.Name, err)
	}
	prev := map[string]groupMember{}
	for _, m := range old.members {
		prev[m.dev.Name] = m
	}
	old.spec, old.members = g, members
	e.mu.Unlock()

	var errs []error
	changed := 0
	for _, m := range members {
		o, had := prev[m.dev.Name]
		delete(prev, m.dev.Name)
		if !had {
			errs = append(errs, e.addMember(g.Name, m)...)
			changed++
			continue
		}
		touched := false
		if !reflect.DeepEqual(o.dev, m.dev) {
			if _, err := e.updateDevice(m.dev, g.Name); err != nil {
				errs = append(errs, err)
			}
			touched = true
		}
		was := map[string]ProtocolSpec{}
		for _, p := range o.protos {
			was[p.Name] = p
		}
		for _, p := range m.protos {
			op, had := was[p.Name]
			delete(was, p.Name)
			switch {
			case !had:
				_, err = e.createProtocol(p, g.Name)
			case op.Type != p.Type:
				if err = e.deleteProtocol(p.Name, g.Name); err == nil {
					_, err = e.createProtocol(p, g.Name)
				}
			case !bytes.Equal(op.Config, p.Config):
				_, err = e.updateProtocol(p.Name, p.Config, g.Name)
			default:
				continue
			}
			touched = true
			if err != nil {
				errs = append(errs, fmt.Errorf("protocol %s: %w", p.Name, err))
			}
		}
		for name := range was {
			touched = true
			if err := e.deleteProtocol(name, g.Name); err != nil {
				errs = append(errs, err)
			}
		}
		if touched {
			changed++
		}
	}
	for _, m := range prev {
		errs = append(errs, e.removeMember(g.Name, m)...)
		changed++
	}
	e.publish("group/"+g.Name, "group-updated", fmt.Sprintf("group updated: %d devices, %d changed", len(members), changed),
		map[string]any{"changed": float64(changed)})
	v, _ := e.GetGroup(g.Name, false)
	return v, errors.Join(errs...)
}

func (e *Engine) DeleteGroup(name string) error {
	e.gmu.Lock()
	defer e.gmu.Unlock()
	e.mu.Lock()
	g, ok := e.groups[name]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("group %s: %w", name, ErrNotFound)
	}
	delete(e.groups, name)
	e.mu.Unlock()
	var errs []error
	for _, m := range g.members {
		errs = append(errs, e.removeMember(name, m)...)
	}
	e.publish("group/"+name, "group-deleted", "group deleted", nil)
	return errors.Join(errs...)
}

// ---- views ----

type GroupView struct {
	Spec GroupSpec `json:"-"`
	// Devices is how many member devices exist (all of them, unless some
	// failed to create).
	Devices   int                    `json:"devices"`
	Protocols []GroupProtocolSummary `json:"protocols"`
	Members   []GroupMemberView      `json:"members,omitempty"`
}

type GroupProtocolSummary struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// States counts instances by state (running, waiting, error).
	States map[string]int `json:"states"`
	// Health counts running instances by the protocol's own state word
	// (protocol.Healther), e.g. established or up.
	Health map[string]int `json:"health,omitempty"`
}

type GroupMemberView struct {
	Index     int                     `json:"index"` // 1-based
	Device    string                  `json:"device"`
	MAC       string                  `json:"mac"`
	VLANs     string                  `json:"vlans"`
	Addresses []string                `json:"addresses,omitempty"`
	Protocols []GroupMemberProtoState `json:"protocols,omitempty"`
}

type GroupMemberProtoState struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Health string `json:"health,omitempty"`
	Error  string `json:"error,omitempty"`
}

func health(sess protocol.Session) string {
	if h, ok := sess.(protocol.Healther); ok {
		return h.Health()
	}
	return ""
}

// GetGroup summarises a group; with members, it lists each member too.
func (e *Engine) GetGroup(name string, members bool) (GroupView, error) {
	e.mu.RLock()
	g, ok := e.groups[name]
	if !ok {
		e.mu.RUnlock()
		return GroupView{}, fmt.Errorf("group %s: %w", name, ErrNotFound)
	}
	v := GroupView{Spec: g.spec}
	type ref struct {
		in   *instance
		sess protocol.Session
		st   string
		err  string
	}
	refs := make([][]ref, len(g.members))
	var macs []string
	for i, m := range g.members {
		mac := ""
		if d, ok := e.devices[m.dev.Name]; ok && d.group == name {
			v.Devices++
			mac = d.mac.String()
		}
		macs = append(macs, mac)
		refs[i] = make([]ref, len(m.protos))
		for j, p := range m.protos {
			r := ref{st: StateError, err: "not created"}
			if in, ok := e.insts[p.Name]; ok && in.group == name {
				r = ref{in: in, sess: in.sess, st: in.state, err: in.err}
			}
			refs[i][j] = r
		}
	}
	gm := g.members
	e.mu.RUnlock()

	for _, p := range v.Spec.Protocols {
		v.Protocols = append(v.Protocols, GroupProtocolSummary{Name: p.Name, Type: p.Type, States: map[string]int{}})
	}
	for i, m := range gm {
		var mv GroupMemberView
		if members {
			mv = GroupMemberView{Index: i + 1, Device: m.dev.Name, MAC: macs[i], VLANs: vlanString(m.dev.VLANs), Addresses: m.dev.Addresses}
		}
		for j, r := range refs[i] {
			h := ""
			if r.sess != nil {
				h = health(r.sess)
			}
			s := &v.Protocols[j]
			s.States[r.st]++
			if h != "" {
				if s.Health == nil {
					s.Health = map[string]int{}
				}
				s.Health[h]++
			}
			if members {
				mv.Protocols = append(mv.Protocols, GroupMemberProtoState{Name: m.protos[j].Name, State: r.st, Health: h, Error: r.err})
			}
		}
		if members {
			v.Members = append(v.Members, mv)
		}
	}
	return v, nil
}

func (e *Engine) Groups() []GroupView {
	e.mu.RLock()
	names := make([]string, 0, len(e.groups))
	for n := range e.groups {
		names = append(names, n)
	}
	e.mu.RUnlock()
	sort.Strings(names)
	var out []GroupView
	for _, n := range names {
		if v, err := e.GetGroup(n, false); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// ---- actions ----

type GroupActionRequest struct {
	Group string
	// Protocol is the group protocol's name (default: the only one).
	Protocol string
	Action   string
	Args     json.RawMessage
	// Members selects members: "" or "all", or 1-based indices and ranges
	// like "1-10,15".
	Members string
	// Random runs the action on this many of the selected members, picked
	// at random (0: all selected).
	Random int
}

type GroupActionResult struct {
	Index    int    `json:"index"`
	Protocol string `json:"protocol"`
	Error    string `json:"error,omitempty"`
	Result   any    `json:"result,omitempty"`
}

const groupActionParallel = 32

// RunGroupAction runs a protocol action on members of a group, in
// parallel. Per-member failures are reported in the results.
func (e *Engine) RunGroupAction(ctx context.Context, r GroupActionRequest) ([]GroupActionResult, error) {
	e.mu.RLock()
	g, ok := e.groups[r.Group]
	if !ok {
		e.mu.RUnlock()
		return nil, fmt.Errorf("group %s: %w", r.Group, ErrNotFound)
	}
	spec, members := g.spec, g.members
	e.mu.RUnlock()
	j := -1
	for k, p := range spec.Protocols {
		if p.Name == r.Protocol || (r.Protocol == "" && len(spec.Protocols) == 1) {
			j = k
		}
	}
	if j < 0 {
		var names []string
		for _, p := range spec.Protocols {
			names = append(names, p.Name)
		}
		if r.Protocol == "" {
			return nil, fmt.Errorf("group %s: say which protocol (%v)", r.Group, names)
		}
		return nil, fmt.Errorf("group %s has no protocol %q (%v)", r.Group, r.Protocol, names)
	}
	t, _ := protocol.Lookup(spec.Protocols[j].Type)
	if _, ok := protocol.FindAction(t, r.Action); !ok {
		return nil, fmt.Errorf("protocol type %s has no action %q", t.Name(), r.Action)
	}
	sel, err := parseMembers(r.Members, len(members))
	if err != nil {
		return nil, err
	}
	if r.Random > 0 && r.Random < len(sel) {
		rand.Shuffle(len(sel), func(a, b int) { sel[a], sel[b] = sel[b], sel[a] })
		sel = sel[:r.Random]
		sort.Ints(sel)
	}
	out := make([]GroupActionResult, len(sel))
	var wg sync.WaitGroup
	sem := make(chan struct{}, groupActionParallel)
	for k, i := range sel {
		name := members[i].protos[j].Name
		out[k] = GroupActionResult{Index: i + 1, Protocol: name}
		wg.Add(1)
		sem <- struct{}{}
		go func(res *GroupActionResult) {
			defer func() { <-sem; wg.Done() }()
			v, err := e.RunAction(ctx, name, r.Action, r.Args)
			if err != nil {
				res.Error = err.Error()
			} else {
				res.Result = v
			}
		}(&out[k])
	}
	wg.Wait()
	e.publish("group/"+r.Group, "group-action", fmt.Sprintf("%s %s on %d members", spec.Protocols[j].Name, r.Action, len(sel)), nil)
	return out, nil
}
