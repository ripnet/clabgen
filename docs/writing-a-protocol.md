# Writing a protocol plugin

A protocol is a Go package that registers a `protocol.Type` from `init()`.
The agent handles everything else: config decoding and validation, the JSON
schema the UI and CLI use, the API, scenarios, events, and frame dispatch.
`protocols/cfm` is the reference implementation.

## 1. Define the config

```go
type Config struct {
    Level    uint8  `json:"level" jsonschema:"minimum=0,maximum=7,description=MD level"`
    Interval string `json:"interval" jsonschema:"enum=1s,enum=10s"`
}

func (c *Config) Validate() error { ... } // optional; runs after decoding
```

- `NewConfig()` returns a pointer with **defaults already filled in**. The agent
  decodes user JSON on top of it, rejecting unknown fields, and the defaults
  appear in the published schema.
- `jsonschema:` tags (from [invopop/jsonschema](https://github.com/invopop/jsonschema))
  add enums, ranges and descriptions. UIs build forms from that schema, so
  adding a protocol needs no UI changes. That tag splits on commas, so a
  description with a comma goes in its own tag:
  `jsonschema_description:"Seconds between hellos, at least 1"` (a test
  catches descriptions cut short).
- Action arguments work the same way: `Action.NewArgs` returns a defaulted
  pointer, and it may implement `Validate()`.

## 2. Implement the Type

```go
func init() { protocol.Register(myType{}) }

type myType struct{}

func (myType) Name() string              { return "my-proto" }
func (myType) Description() string       { return "..." }
func (myType) NewConfig() any            { return &Config{Level: 0, Interval: "1s"} }
func (myType) EtherTypes() []uint16      { return []uint16{0x88cc} }
func (myType) Actions() []protocol.Action { return []protocol.Action{{Name: "poke", NewArgs: func() any { return &PokeArgs{} }}} }
func (myType) Start(env protocol.Env, cfg any) (protocol.Session, error) { ... }
```

`EtherTypes` is the receive classifier. The agent delivers a frame to a
session when all of these hold:

- the frame arrived on the device's port;
- its VLAN stack matches the device's (TPIDs and VIDs; PCP is ignored, and a
  lone priority tag counts as untagged);
- its destination is the device's MAC or any multicast address;
- its ethertype is in the list. IEEE 802.3 frames with an LLC header
  (IS-IS, STP, …) carry a length instead of an ethertype; they are
  delivered as `pkt.EtherTypeLLC` with the payload starting at the LLC
  header, and `Send` with `pkt.EtherTypeLLC` writes the length.

## 3. Implement the Session

```go
type Session interface {
    HandleRx(f *pkt.Frame)                                   // f.Payload starts after the VLAN tags
    Status() any                                             // JSON-marshalable snapshot
    Action(ctx context.Context, name string, args any) (any, error)
    Stop()                                                   // must stop goroutines and return
}
```

- `HandleRx`, `Status` and `Action` can be called concurrently with each
  other and with your own timers, so guard state with a mutex. `f.Payload`
  aliases the receive buffer: copy it if you keep it. `f.RxTime` is the
  kernel receive timestamp.
- Don't hold your lock while calling `env.Send` or `env.Emit`.
- Actions run synchronously in the API call. Honour `ctx` cancellation for
  long-running tests.
- Optional: `Brief() any` (`protocol.Briefer`) returns a smaller status for
  lists that poll often, and `Health() string` (`protocol.Healther`) sums
  up the state in one word (`established`, `up`, `ok`) for device group
  summaries, which count members per word. Keep the set of words small.

## 4. Use the Env

| Method | Use |
|---|---|
| `Send(dst, ethertype, payload, opts)` | Transmit from the device. The device's MAC and VLAN stack are added; `SendOpts{SetPCP: true, PCP: n}` overrides PCP on every tag. |
| `Emit(kind, msg, data)` | Publish an event (state changes, alarms) to `WatchEvents` subscribers. |
| `InstanceName()` | The instance's name (e.g. to derive stable identifiers). |
| `Device()` | MAC, VLANs, port name, port up/down. |
| `IP()` | The device's IP stack: `Addresses()`, `Source(dst)`, `ResolveNextHop(ctx, dst)` (ARP/ND through the neighbor cache; off-link destinations resolve the gateway), and `ClaimFlow(id)` so a flow's own echoes aren't listed as received (`ipflow` does this). |
| `Siblings()` | Other sessions of the same type on the same device (CFM uses this for MD-level demux). |
| `Log()` | Structured logger tagged with the instance name. |

## 5. Traffic flows

A type that implements `Category() string` returning
`protocol.CategoryTraffic` is a flow. The UI lists flows in the Traffic tab
instead of Protocols, and expects their status to embed
`flowstats.Stats`. Use `internal/flowstats` to track them:

- call `Sent(seq, t)` *before* transmitting (a reply on a fast link can
  otherwise arrive first) and `TxError(seq, t, err)` if the send fails;
- call `Received(seq, rx, sentAt, detail)` for replies,
  `ErrorReceived(seq, rx, detail)` for error responses (e.g. ICMP
  unreachable), and `Expire(now, false)` periodically;
- embed `tracker.Snapshot(now, seconds)` in the status.

The tracker computes loss, late and duplicate replies, RTT and jitter, a
per-second timeline by send time, and outages (runs of consecutive losses,
settled in sequence order). Flows should offer `start`, `stop` and `clear`
actions.

IP flows can build on `internal/ipflow`. It provides config parts to
embed (target, run, addressing, IP header, payload), the send loop with
pacing, source selection and next-hop following, IP encapsulation, the
signature, status, the shared actions and preview. A flow type then only
implements `ipflow.Transport` (build its header around the payload) and
`HandleRx` (match replies and ICMP errors). `protocols/icmp` and
`protocols/udp` are about 200 lines each. Keep the signature on: clabgen
receivers (`internal/flowrx`, fed by the device IP stack) measure any
signed flow one way. `ipflow.Run` alone (mode, rate, count, duration)
with `Run.Pace` gives any flow the same send loop.

A type can also implement `protocol.Previewer`: `Preview(ctx, env, cfg,
args)` shows what a config would send before an instance exists (the
`PreviewProtocol` API; the packet editor calls it as you type). The env
is bound to the device but isn't an instance's: don't keep it or send
with it.

## 6. Wire it in

Import the package for its side effects in `cmd/clabgen-agent/main.go`:

```go
_ "github.com/ripnet/clabgen/protocols/myproto"
```

It then appears in `clabgenctl types`, can be created with
`clabgenctl proto add`, and can be used in scenario files.

## 7. UI (optional)

The web UI needs no changes for a new protocol:

- the create and edit forms come from the config schema, in struct field
  order;
- each action gets a form generated from its args schema;
- status and action results are rendered generically (scalars as
  key/value pairs, arrays of objects as tables).

For a richer view, add `web/src/protocols/<name>.tsx` that calls
`registerView("<type>", { summary, Status })` (and optionally
`ConfigEditor`, a component replacing the generic create and edit form,
as `web/src/protocols/packet.tsx` does) and import it in
`web/src/main.tsx`. `summary` returns a health (ok/warn/bad/idle) and one
line of text for tables; `Status` renders the detail panel. See
`web/src/protocols/cfm.tsx`. `Status` also receives `agentId` and `name`
so it can run actions. Flows can reuse `web/src/components/Flow.tsx`
(summary, stat tiles, timeline chart, per-packet log), as
`web/src/protocols/icmp.tsx` does.

## 8. Test it

Drive two sessions back to back with an in-memory `Env`, as
`protocols/cfm/mep_test.go` does (no root or containers needed). Then add an
example lab under `examples/` with the device you tested against.
