// Package protocol defines the plugin contract for stateful protocols.
//
// A protocol package registers a Type from init(). The agent decodes user
// config into the struct returned by NewConfig (so defaults set there apply),
// publishes a JSON schema derived from that struct so UIs can render forms,
// and starts one Session per configured instance. See docs/writing-a-protocol.md.
package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"sync"

	"github.com/invopop/jsonschema"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Device is the emulated endpoint a session is bound to.
type Device interface {
	Name() string
	Port() string
	MAC() pkt.MAC
	VLANs() []pkt.VLANTag
	PortUp() bool
}

type SendOpts struct {
	// When SetPCP is true, PCP overrides the priority on every VLAN tag.
	SetPCP bool
	PCP    uint8
}

// IP is the device's IP stack, for protocols that send IP packets.
type IP interface {
	// Addresses returns the device's addresses, including the automatic
	// link-local one. Empty when the device has no IP configuration.
	Addresses() []netip.Prefix
	// Source picks the device's address to send from toward dst.
	Source(dst netip.Addr) (netip.Addr, bool)
	// ResolveNextHop resolves the MAC to send to for dst: dst itself when
	// on-link, else the device's gateway. It uses the neighbor cache and
	// sends ARP/ND requests when needed.
	ResolveNextHop(ctx context.Context, dst netip.Addr) (nextHop netip.Addr, mac pkt.MAC, err error)
	// ClaimFlow marks a flow ID as sent from this device, so its returning
	// (echoed) packets aren't listed as received here: the flow itself
	// accounts for them. Call release when the flow goes away.
	ClaimFlow(id uint32) (release func())
}

// Env is what the agent provides to a running session.
type Env interface {
	// InstanceName is the protocol instance's name.
	InstanceName() string
	Device() Device
	IP() IP
	// Send transmits payload from the device, adding its MAC and VLAN stack.
	Send(dst pkt.MAC, etherType uint16, payload []byte, opt SendOpts) error
	// Emit publishes an event to API watchers (state changes, alarms).
	Emit(kind, msg string, data map[string]any)
	Log() *slog.Logger
	// Siblings returns the other sessions of the same type on the same device.
	Siblings() []Session
}

// Session is one running protocol instance. HandleRx, Status and Action may
// be called concurrently with the session's own goroutines.
type Session interface {
	// HandleRx is called for every frame on the device whose ethertype is in
	// Type.EtherTypes. The frame is only valid for the duration of the call.
	HandleRx(f *pkt.Frame)
	// Status returns a JSON-marshalable snapshot.
	Status() any
	// Action runs a named action; args is the value from Action.NewArgs.
	Action(ctx context.Context, name string, args any) (any, error)
	Stop()
}

type Action struct {
	Name        string
	Description string
	// NewArgs returns a pointer to a defaulted args struct.
	NewArgs func() any
}

type Type interface {
	Name() string
	Description() string
	// NewConfig returns a pointer to a config struct populated with defaults.
	NewConfig() any
	Actions() []Action
	EtherTypes() []uint16
	Start(env Env, cfg any) (Session, error)
}

// Categorized may be implemented by a Type to group it in UIs. Types
// without it are in CategoryProtocol.
type Categorized interface{ Category() string }

const (
	CategoryProtocol = "protocol"
	// CategoryTraffic types are flows: started and stopped by actions and
	// reported with tx/rx/loss statistics.
	CategoryTraffic = "traffic"
)

// CategoryOf returns t's category.
func CategoryOf(t Type) string {
	if c, ok := t.(Categorized); ok {
		return c.Category()
	}
	return CategoryProtocol
}

// Validator may be implemented by config and args structs.
type Validator interface{ Validate() error }

var (
	regMu    sync.RWMutex
	registry = map[string]Type{}
)

func Register(t Type) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[t.Name()]; dup {
		panic("protocol: duplicate registration of " + t.Name())
	}
	registry[t.Name()] = t
}

func Lookup(name string) (Type, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	t, ok := registry[name]
	return t, ok
}

func Types() []Type {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Type, 0, len(registry))
	for _, t := range registry {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Decode fills dst (a defaulted pointer) from raw JSON, rejecting unknown
// fields, then runs Validate if implemented.
func Decode(dst any, raw []byte) error {
	if len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(dst); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}
	}
	if v, ok := dst.(Validator); ok {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}
	}
	return nil
}

// FindAction returns the named action of t.
func FindAction(t Type, name string) (Action, bool) {
	for _, a := range t.Actions() {
		if a.Name == name {
			return a, true
		}
	}
	return Action{}, false
}

// Schema returns the JSON schema of v's type, with defaults taken from v.
func Schema(v any) string {
	r := &jsonschema.Reflector{DoNotReference: true, ExpandedStruct: true, AllowAdditionalProperties: false}
	s := r.Reflect(v)
	s.Version = ""
	b, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	var sm map[string]any
	var vm any
	vb, _ := json.Marshal(v)
	if json.Unmarshal(b, &sm) != nil || json.Unmarshal(vb, &vm) != nil {
		return string(b)
	}
	addDefaults(sm, vm)
	// JSON objects are unordered; publish the struct field order for UIs.
	if s.Properties != nil {
		var order []string
		for p := s.Properties.Oldest(); p != nil; p = p.Next() {
			order = append(order, p.Key)
		}
		sm["x-order"] = order
	}
	b, _ = json.Marshal(sm)
	return string(b)
}

// addDefaults copies values from a marshalled default config into the
// "default" keyword of each matching schema property.
func addDefaults(schema map[string]any, val any) {
	props, _ := schema["properties"].(map[string]any)
	obj, _ := val.(map[string]any)
	for k, p := range props {
		ps, ok := p.(map[string]any)
		if !ok {
			continue
		}
		dv, present := obj[k]
		if !present || dv == nil {
			continue
		}
		if _, isObj := dv.(map[string]any); isObj {
			addDefaults(ps, dv)
			continue
		}
		if _, has := ps["default"]; !has {
			ps["default"] = dv
		}
	}
}
