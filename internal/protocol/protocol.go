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
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/invopop/jsonschema"

	"github.com/ripnet/clabgen/internal/outage"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/prefixset"
	"github.com/ripnet/clabgen/internal/vpnrib"
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
	// Src, when set, replaces the device's MAC as the source address.
	Src pkt.MAC
	// When SetTags is true, Tags replace the device's VLAN tags (e.g. MKA
	// sends with only the tags in front of the SecTAG).
	SetTags bool
	Tags    []pkt.VLANTag
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
	// AddLocal makes the device answer for addr (ICMP echo, flows) without
	// owning it on the link (no ARP/ND, not in Addresses): a router's
	// loopback reachable over any of its interfaces. Call release to stop.
	AddLocal(addr netip.Addr) (release func())
	// AddLocalPrefixes makes the device answer for every address in the set,
	// as AddLocal does, for as long as the set says so: a router's advertised
	// routes. Call release to stop.
	AddLocalPrefixes(set *prefixset.Set) (release func())
	// AddLocalLabel makes the device take MPLS packets with this top label:
	// it pops the stack and handles the IP packet inside (for it, as usual).
	// Call release to stop.
	AddLocalLabel(label uint32) (release func())
	// SetTunnel publishes an LSP flows on the device can send into, by name
	// (nil: it's down).
	SetTunnel(name string, t *Tunnel)
	// Tunnel is an LSP's current state, or nil when it's down.
	Tunnel(name string) *Tunnel
	// TunnelTo is the preferred LSP ending at addr, or nil.
	TunnelTo(addr netip.Addr) *Tunnel
	// VPNRIB is the device's table of received VPN routes.
	VPNRIB() *vpnrib.Table
	// SetVPNLabels says which labels an owner advertised VPN routes with and
	// what's behind them (nil: none any more). Packets arriving with such a
	// label are for this device if they're to one of its prefixes, and
	// replies to them go back through the VPN.
	SetVPNLabels(owner string, labels map[uint32]VPNLabel)
	// VPNResolve finds what it takes to send to dst in a VPN: its route under
	// the targets, and how to reach the route's next hop (through the named
	// LSP if any, else an LSP to it, else directly when it's on-link).
	VPNResolve(targets []string, lsp string, dst netip.Addr) (*VPNSend, error)
	// IsLocal reports whether packets to addr are for this device: one of its
	// addresses, a local address, or one in a local prefix set.
	IsLocal(addr netip.Addr) bool
	// IsLocalRx is IsLocal for a received packet, which may be in a VPN.
	IsLocalRx(f *pkt.Frame, addr netip.Addr) bool
	// Reply sends an IP packet answering a received frame: back to the frame's
	// sender, or through the VPN it arrived on.
	Reply(f *pkt.Frame, dst netip.Addr, etherType uint16, b []byte) error
	// AddAddress gives the device an address on the link (prefix the
	// on-link subnet), a default gateway, or both (either may be the zero
	// value), as a DHCP client does with a lease. The device owns the
	// address like a configured one (ARP/ND, Addresses) and announces it;
	// a configured gateway wins over this one. Call release to remove them.
	AddAddress(prefix netip.Prefix, gateway netip.Addr) (release func())
	// DialTCP connects to remote from local (invalid: the device picks)
	// with the TTL / hop limit ttl (0: 64).
	DialTCP(ctx context.Context, remote netip.AddrPort, local netip.Addr, ttl int) (net.Conn, error)
	// ListenTCP accepts connections on local:port, or (local invalid) on
	// every address of the family (v6) the device has, sending with ttl.
	ListenTCP(local netip.Addr, port uint16, v6 bool, ttl int) (net.Listener, error)
}

// Tunnel is what it takes to send into an LSP: the labels to push (outermost
// first; none: the next hop pops it) and where to send the packet.
type Tunnel struct {
	Labels  []uint32
	NextHop netip.Addr
	MAC     pkt.MAC
	// To is where the LSP ends; Preference picks among LSPs to one place,
	// lowest first.
	To         netip.Addr
	Preference int
}

// Preferences of LSPs to the same place, as routers commonly rank them.
const (
	PreferenceRSVP = 7
	PreferenceLDP  = 9
)

// VPNLabel is what's behind a label advertised with VPN routes.
type VPNLabel struct {
	Prefixes []netip.Prefix
	Targets  []string
}

// VPNSend is how to send a packet to a destination in a VPN.
type VPNSend struct {
	Labels  []uint32 // to push, outermost first
	NextHop netip.Addr
	MAC     pkt.MAC
	Path    *vpnrib.Path
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
	// Attach makes the session also receive, on another device, the frames
	// of its type's ethertypes (rx is called like HandleRx) and send from
	// it, e.g. for a router's further interfaces. It fails if the device
	// doesn't exist; the attachment closes if the device is deleted or its
	// port, MAC or VLANs change (attach again once it is back).
	Attach(device string, rx func(*pkt.Frame)) (Attachment, error)
}

// Attachment is another device a session sends and receives on.
type Attachment interface {
	Device() Device
	IP() IP
	Send(dst pkt.MAC, etherType uint16, payload []byte, opt SendOpts) error
	// Closed is closed when the attachment ends.
	Closed() <-chan struct{}
	Close()
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

// Reconfigurer may be implemented by a Session that can take a new config
// (validated, from Type.NewConfig) while running, e.g. to change its routes
// without dropping its sessions. It returns false when it can't; the engine
// then restarts the session with the new config.
type Reconfigurer interface{ Reconfigure(cfg any) bool }

// Briefer may be implemented by a Session whose Status is large (e.g. an
// LSDB). Brief returns a compact status for lists that poll many
// protocols; detail views still use Status.
type Briefer interface{ Brief() any }

// FrameCounter may be implemented by a Device: the data (non-CFM) frames
// it sent and received, for Y.1731 frame loss measurement.
type FrameCounter interface{ DataFrames() (tx, rx uint64) }

// Healther may be implemented by a Session to summarise its state in one
// short word for group views, e.g. "established" or "up". Counts of each
// word are shown per group, so it should have few distinct values.
type Healther interface{ Health() string }

// FlowMeasurer may be implemented by a traffic flow's Session: what the
// flow lost since a moment, for convergence measurements.
type FlowMeasurer interface {
	Convergence(since, now time.Time) outage.Flow
}

// Delegator may be implemented by a Session holding prefixes delegated to
// it (DHCPv6 prefix delegation), listed beside its device's addresses in
// group views.
type Delegator interface{ Delegated() []netip.Prefix }

// ConfigEtherTypes may be implemented by a Type whose ethertypes depend
// on the instance's config (e.g. raw L2 streams); the result replaces
// EtherTypes for that instance.
type ConfigEtherTypes interface{ EtherTypesFor(cfg any) []uint16 }

// MACClaimer may be implemented by an Env: the device also accepts frames
// addressed to the claimed MACs (e.g. a stream's extra source MACs, for
// replies to them), until release is called.
type MACClaimer interface {
	ClaimMACs(macs []pkt.MAC) (release func())
}

// SecureLink protects the frames of the devices behind it (MACsec). The
// engine sends their frames, except EAPOL, through Protect, and hands it the
// MACsec frames they receive.
type SecureLink interface {
	// Protect sends f protected, through send.
	Protect(f *pkt.Frame, send func([]byte) error) error
	// Validate checks a received MACsec frame (f decoded from b) and returns
	// the frame it carries, or nil when it's dropped.
	Validate(f *pkt.Frame, b []byte) []byte
	// Unprotected is told about a frame without a SecTAG for a device behind
	// the link; true delivers it anyway.
	Unprotected(f *pkt.Frame) bool
	// EAPOL is an EAPOL frame (MKA) with the link's clear VLAN tags.
	EAPOL(f *pkt.Frame)
	// ClearTags is how many VLAN tags are in front of the SecTAG.
	ClearTags() int
}

// Securer may be implemented by an Env: Secure puts the session's device
// (port false) or every device on its port behind l, until release is
// called. It fails if they're behind another link already.
type Securer interface {
	Secure(l SecureLink, port bool) (release func(), err error)
}

// Categorized may be implemented by a Type to group it in UIs. Types
// without it are in CategoryProtocol.
type Categorized interface{ Category() string }

// Previewer is a type that can show what a config would send before an
// instance of it exists (for editors). env is bound to the device, but it
// isn't an instance's: Preview mustn't keep it or send with it.
type Previewer interface {
	Preview(ctx context.Context, env Env, cfg any, args map[string]any) (any, error)
}

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
	// Only fields tagged jsonschema:"required" are required: a field without
	// omitempty may still have a default.
	r := &jsonschema.Reflector{DoNotReference: true, ExpandedStruct: true, AllowAdditionalProperties: false, RequiredFromJSONSchemaTags: true}
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
	addOrder(s, sm)
	b, _ = json.Marshal(sm)
	return string(b)
}

// addOrder publishes struct field order ("x-order") for UIs, since JSON
// objects are unordered, on the schema and every nested object or list
// item schema.
func addOrder(s *jsonschema.Schema, m map[string]any) {
	if s == nil || m == nil {
		return
	}
	if s.Properties != nil {
		var order []string
		props, _ := m["properties"].(map[string]any)
		for p := s.Properties.Oldest(); p != nil; p = p.Next() {
			order = append(order, p.Key)
			sub, _ := props[p.Key].(map[string]any)
			addOrder(p.Value, sub)
		}
		m["x-order"] = order
	}
	if s.Items != nil {
		sub, _ := m["items"].(map[string]any)
		addOrder(s.Items, sub)
	}
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
