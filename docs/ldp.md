# LDP

An `ldp` instance is an LDP router (RFC 5036, IPv4) on a device. It finds
peers with link and targeted hellos, runs sessions with them, advertises
labels for its FECs and keeps those it's sent. It is the far end of LSPs: the
device takes traffic for its labels, but it doesn't switch labels between
peers. A worked example: [LDP with traffic in the LSPs](examples/ldp.md).

```yaml
  - name: ldp
    type: ldp
    device: core
    config:
      router_id: 192.0.2.2
      fecs:
        - {prefix: 192.0.2.2/32, mode: implicit-null}
        - {prefix: 198.51.0.0/24, count: 10}
```

- `router_id` is the LSR ID and, unless `transport_address` is set, the address
  sessions use. The device answers for it, and peers need a route to it, so
  pair LDP with [IS-IS](isis.md) or [OSPF](ospf.md) on the same device and
  advertise it there as a loopback. Advertise the FEC prefixes there too: a
  router uses a label only with a matching route.
- Timers: `hello_interval` and `hold_time` (5 and 15 s), `targeted_hello_interval`
  and `targeted_hold_time` (15 and 45 s), and `keepalive` (30 s; the lower of
  the two sides' is used).
- There is no TCP MD5 authentication (the netstack has none), and one device per
  instance for now.

## FECs and labels

Each of `fecs` is a `prefix` and `count` of consecutive prefixes, with a `mode`:

| `mode` | |
|---|---|
| `label` (the default) | a real label each, from `label` or `label_base` up. The peer sends labelled packets |
| `implicit-null` | the peer pops the label, and sends plain IP |
| `explicit-null` | label 0 |

A device takes MPLS packets whose top label it advertised: it pops the stack
and handles the IP packet inside, so ping, [flows](flows.md) and one-way
statistics work. Replies go back as plain IP. Other labels aren't taken.

The device answers for a FEC's prefix from the time it's first advertised. A
withdraw only takes the label away: a router with a route falls back to plain
IP, and that traffic still gets through.

## Targeted sessions

`targeted: [{address: 192.0.2.9}]` sends targeted hellos to a peer that isn't on
the link, asking it to answer. `accept_targeted: true` answers peers we don't
list. `no_link_hellos: true` leaves only targeted sessions.

## Actions

- `advertise fecs='[{"prefix":"198.18.0.0/24","count":5}]'` and `withdraw`.
- `reset peer=192.0.2.1`: a shutdown notification; the session comes back.
- `flap peer=… seconds=30 mode=silent`: send nothing and listen to nothing, so
  the peer's timers expire if the flap is longer than them; or `mode=notify`,
  which ends the session and stays away.
- `notify code=13` sends a notification with any status code; `fatal=true` sets
  the E bit and ends the session.
- `bindings peer=192.0.2.1 prefix=… limit=…` lists labels received.

Each session and adjacency change is an event, so it shows on a
[convergence test](flows.md#convergence-tests)'s timeline.

## Not yet

IPv6 (RFC 7552), pseudowires and VPLS (FEC 128/129), graceful restart and BFD,
downstream on demand beyond answering label requests, and several interfaces
per instance.
