# Monitoring a mirrored link

A `monitor` instance makes its device's port a monitor port: it takes a
router's port mirror of a link and sends nothing. It takes every frame apart
(MPLS labels, a pseudowire's control word, the frame inside) and counts what
it saw for each service label and direction:

- clabgen flows, found by their signature, with the VLAN tags they carried
  inside the service;
- CFM and Y.1731 frames (CCM, DMM, LBM…), with their level, MA and MEP, and
  their tags;
- anything else, by ethertype.

Flows can say which tags they expect there, for their own frames and for the
OAM in their service, and pass or fail on what the monitor saw. Use it to
check what a service really carries in the core: one tag or two, which TPID,
whether OAM and data match, each way. A worked example:
[VLAN tags inside Epipes](examples/wire.md).

```yaml
# The monitor, on an agent whose eth1 gets the mirror:
devices:
  - {name: tap, port: eth1}
protocols:
  - {name: core, type: monitor, device: tap}
```

```yaml
# A flow saying what it expects on the monitored link:
  - name: evpl-ping
    type: icmp
    device: evpl
    config:
      destination: 10.2.0.2
      rate: 5
      auto_start: true
      wire: {data: "163", oam: "163"}
```

## The mirror

On SR OS, a local mirror of a network port, both directions, to the port
that's wired to clabgen:

```
/configure port 1/1/c5/1 ethernet mode access
/configure port 1/1/c5/1 ethernet mtu 9212
/configure mirror mirror-dest "core" admin-state enable
/configure mirror mirror-dest "core" service-id 900
/configure mirror mirror-dest "core" sap 1/1/c5/1
/configure mirror mirror-source "core" port 1/1/c2/1 ingress true
/configure mirror mirror-source "core" port 1/1/c2/1 egress true
```

- **Wire each mirror port straight to a clabgen interface.** Not through a
  Linux bridge shared by two mirrors: the bridge sends multicast frames (CCMs)
  from one mirror out of the other, so they'd show up on both links, as bare
  frames on the wrong one. Unicast data isn't affected, which makes it easy to
  miss.
- Mirrored frames carry their labels on top of the customer's frame: the
  mirror port's MTU must take that, or the biggest frames are lost on the
  mirror only.
- A LAG: mirror every member.

One port can take both directions: they're told apart by the outer MAC
addresses (one pair per direction of a point-to-point link). To mirror each
direction to a port of its own, set `direction: ingress` or `egress` on each
monitor; it's shown with what was seen.

## What it reads

| On the wire | How it's read |
|---|---|
| Labels | The bottom label is the service's (a pseudowire, a VPLS, a VPN). A flow label (FAT, RFC 6391: TTL 0 below it) and an entropy label (after label 7) are set aside. The labels above are transport: they change with PHP and paths, so they don't identify the service. |
| Control word | After the labels, before the frame. `control_word: auto` (the default) tells by what follows; `on` or `off` when you know. |
| The frame inside | Its VLAN tags, outermost first, with the TPID when it isn't 0x8100: `3162,163`, `0x88a8:3162,163`, `untagged`. |
| IP after the labels | An L3VPN: the flow is found, there are no tags. |
| G-ACh (label 13, or a control word starting 0001) | The routers' own pseudowire OAM (VCCV, BFD): counted as other. |
| No MPLS | Plain Ethernet on the link (IS-IS, LDP…), counted by its tags; hidden in the UI unless asked for. |

## Flows' expectations

`wire` on an `icmp`, `udp` or `l2` flow (in the UI's form: **expected on the wire**):

- `data`: the tags the flow's frames should carry inside the service, outermost
  first: `3162,163`, `0x88a8:3162,163`, `untagged`. A YAML list works too:
  `[3162, 163]`.
- `oam`: the tags CFM/Y.1731 frames in the same service should carry.
- `direction`: `both` (the default), `a-to-z` (from the flow's device), or
  `z-to-a` (back).

Every stack seen must be the expected one, and every expected direction must
be seen: until then the flow is pending. A direction is seen through the
flow's frames that way (an echo flow's replies) or, without them, through the
OAM the other way round with the same MAs (CCMs from the far end). A one-way
flow checking `data` both ways stays pending: use `direction: a-to-z`.

The monitor can be on any agent: flow IDs are unique in a lab, and the UI and
`clabgenctl wire` look at every monitor they know of.

What's seen accumulates until the monitor is cleared, but verdicts are on
recent traffic, so a change doesn't leave a flow failing, or passing, on what
came before it:

- **A reused label.** Routers hand out labels again: after a service is
  rebuilt, its old label may go to another one. OAM in the label from before
  the flow was first seen there isn't counted as the flow's.
- **An old stack.** A stack that hasn't been seen for 10 s, while another one
  of the same kind (data or OAM) is, is from before a change: it's shown,
  struck through and marked old, and not judged.
- **Nothing seen lately.** When neither the data nor the OAM of a direction
  has been seen for 10 s (the mirror was turned off, or the flow and its OAM
  stopped), the direction shows its last tags greyed, "not seen for N s", and
  is pending until something is seen again. The same goes for one of them on
  its own: a stopped flow's data is pending while its service's OAM, still
  arriving, is judged. The monitor keeps its counts.

For a clean verdict after a change, clear the monitors: **Clear monitors** on
the Flows page or a flow's Wire section, or `clabgenctl --lab LAB wire --clear`
(every monitor of the lab).

## In the UI

A flow's page has a **Wire** section: per direction and monitor, the service
it was seen in (with its labels), the tags of its frames and of the OAM there,
each checked against what it expects.

The monitor's page lists the links it sees and every service: its labels, the
flows in it (named, wherever they are), its OAM and anything else, with a
filter.

## clabgenctl

```
$ clabgenctl --lab wire wire
FLOW            VERDICT  DIRECTION  MONITOR   LABEL   DATA            OAM
ce-a/enni-ping  pass     a-to-z     tap/core  524283  untagged ×61 ✓  untagged ×12 ✓
                         z-to-a     tap/core  524284  untagged ×61 ✓  untagged ×12 ✓
ce-a/epl-ping   pass     a-to-z     tap/core  524285  3162,163 ×62 ✓  3162,163 ×13 ✓
                         z-to-a     tap/core  524286  3162,163 ×62 ✓  3162,163 ×12 ✓
ce-a/evpl-ping  pass     a-to-z     tap/core  524284  163 ×61 ✓       163 ×12 ✓
                         z-to-a     tap/core  524285  163 ×61 ✓       163 ×13 ✓
```

`wire FLOW...` for some flows, `-o json` for scripts, `--services` for
everything each monitor saw, flows or not, `--clear` to clear every monitor.

## Not yet

Inline monitoring (clabgen between two routers, for routers that can't
mirror); VCCV as its own class; PCP and DEI in the expectations.
