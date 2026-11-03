# RSVP-TE

An `rsvp` instance is an RSVP-TE router (RFC 3209, IPv4) on a device. It is the
head of the LSPs it lists and the tail of those signalled to it, and
[flows](flows.md#flows-into-lsps) on the device can send into its LSPs. It
doesn't switch labels between neighbors, so routers go in the middle. A worked
example: [RSVP-TE fast reroute](examples/rsvp.md).

```yaml
  - name: rsvp
    type: rsvp
    device: core
    config:
      router_id: 192.0.2.1
      lsps:
        - name: to-tail
          to: 192.0.2.2
          protection: link
          hops:
            - {address: 10.0.1.1}
            - {address: 10.0.12.2}
            - {address: 10.0.2.2}
```

- `router_id` is the address LSPs end at. The device answers for it, and routers
  need a route to it, so pair RSVP with [IS-IS](isis.md) or [OSPF](ospf.md) on
  the same device and advertise it there as a loopback.
- Timers: `refresh_interval` (30 s; state times out after about 5 refreshes),
  `hello_interval` (5 s; 0 for none; a neighbor that has answered is down after
  3.5 intervals) and `retry_interval` (5 s between tries while an LSP isn't up).
- One device per instance for now. LSP names need to be unique on a device.

## LSPs from here

Each of `lsps` has a `name` and a tail, `to`, with these options:

| Option | |
|---|---|
| `hops` | The explicit route: each an `address`, optionally `loose`. It must start at the next router. Without it, the routers choose |
| `protection` | `link` or `node` asks the routers on the way for fast reroute (facility backup) |
| `bandwidth_mbps` | Bandwidth to ask for |
| `setup_priority`, `hold_priority` | 7 and 0 by default |
| `tunnel_id` | By default, the LSP's place in the list |
| `shutdown` | Don't signal it until the `signal` action |

Its page shows the state, the label, and the path the routers recorded, with
each hop's label and whether it has local protection available or in use. When a
router repairs the LSP locally, the page says so. Traffic then stays on the
bypass, even after the failure is over, until the head moves it: run `resignal`.

An LSP that fails is signalled again after `retry_interval`, with a new LSP ID.
That timer is part of the outage of an unprotected LSP, as a router's retry timer
is.

## LSPs ending here

LSPs signalled to `router_id` end here, listed on the instance's page with their
head, label and recorded path. `tail_mode` says what they get:

| `tail_mode` | |
|---|---|
| `label` (the default) | a real label each, from `label_base` up. The device takes MPLS packets with it, as [LDP](ldp.md) does |
| `implicit-null` | the previous hop pops it, and sends plain IP |
| `explicit-null` | label 0 |

`refuse_lsps: true` answers them with an admission control error. An LSP to any
other address gets a routing problem error: there is no label switching here.

## Actions

- `tear lsp=… seconds=10`: PathTear for an LSP from here, ResvTear for one ending
  here, staying away that long. Without `seconds`, an LSP from here stays down
  until `signal lsp=…`, and one ending here is answered at its next refresh.
- `resignal lsp=… hops=…`: make before break. A new LSP ID on the same or a new
  route; traffic moves once it's up, and the old one goes.
- `error lsp=… code=24 value=5`: PathErr for LSPs ending here (`remove_state=true`
  tells routers to remove their path state), ResvErr for those from here.
- `relabel lsp=…`: new labels for LSPs ending here. The old ones keep working for
  a moment, while the neighbor switches.
- `flap seconds=30 mode=silent`: send nothing and listen to nothing, so the
  neighbors' timers expire if it's long enough; or `mode=tear`.

Each LSP and neighbor change is an event, so it shows on a
[convergence test](flows.md#convergence-tests)'s timeline.

## Not yet

Transit (label switching), bandwidth accounting, message authentication, refresh
reduction (RFC 2961; neighbors refresh in full), IPv6, P2MP, graceful restart,
and several interfaces per instance.
