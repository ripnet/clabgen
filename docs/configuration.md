# Configuration

An agent's devices and protocols are a **scenario**: built in the web UI,
changed with `clabgenctl` or the API, or loaded from a YAML file when the
agent starts, and [kept across lab restarts](#saved-config) (the `.:/clab`
bind below). Mount the file into the node and point `CLABGEN_SCENARIO` at
it:

```yaml
    gen1:
      kind: linux
      image: ghcr.io/ripnet/clabgen
      binds: [gen1.yml:/etc/clabgen/scenario.yml:ro, .:/clab]
      env: {CLABGEN_SCENARIO: /etc/clabgen/scenario.yml}
```

The Scenario tab in the UI (or `clabgenctl scenario get` / `apply` /
`reset`) exports, applies and resets the whole thing. Every protocol's full set of options,
with descriptions and defaults, is in its schema: the UI's forms show them,
and `clabgenctl types <type>` prints them. The sections below show typical
configs; [`examples/`](../examples) has two complete labs.

## Saved config

With the topology's folder mounted at `/clab` (`binds: [.:/clab]`, as in
the examples; clabgen v0.5.0 or newer), an agent saves its whole config to
`clab-<lab>/<node>/clabgen.yml` a moment after every change made in the
UI, CLI or API, and loads it when the lab starts again. The saved config
wins over `CLABGEN_SCENARIO`: the scenario file seeds a fresh lab, and
after that the lab keeps what was built on it.

| Command | Saved config |
|---|---|
| `docker restart`, `containerlab destroy` and `deploy`, `containerlab redeploy` | kept |
| `deploy --reconfigure`, `redeploy --cleanup`, `destroy --cleanup` | deleted with the lab folder: the next start is fresh |

The Scenario tab shows where the config is saved and where this start's
came from. **Reset** there (or `clabgenctl scenario reset`) deletes the
saved file and loads the topology's scenario again: also how edits to the
scenario file take effect on a lab that has a saved config.

The file is plain scenario YAML and yours to edit, but edit it while the
lab is stopped: a running agent reads it only at start, and saves over it
on its next change.

**In your own topology**, add the bind to each clabgen node (keeping its
other binds), then redeploy: mounts are fixed when a container is created.

```yaml
topology:
  nodes:
    traffic:
      kind: linux
      image: ghcr.io/ripnet/clabgen
      binds: [".:/clab"]
```

containerlab adds binds under `defaults` to every node's own, so a lab of
only clabgen nodes can say it once (`defaults: {binds: [".:/clab"]}`); in
a lab with routers, keep it per node, or they get the folder too. Nothing
else is needed: the scenario file stays optional, and without the bind
the config simply isn't kept (the Scenario tab says so).

Router kinds containerlab knows (`nokia_srsim`, …) don't need this:
containerlab creates their node folder and mounts their flash into it.
For `kind: linux`, the generic container, it does neither.

Why the bind is the topology's folder rather than the lab folder itself:
containerlab checks that bind sources exist before it creates the lab
folder, so `clab-<lab>/<node>` can't be mounted on a first deploy. The
agent finds its own folder through the variables containerlab sets
(`CLAB_LABEL_CLAB_NODE_LAB_DIR`); it writes nothing else. Other places:
`CLABGEN_STATE=/path/clabgen.yml` names the file (outside containerlab
too), `CLABGEN_STATE=off` turns saving off, and `CLABGEN_LAB_MOUNT` moves
the mount point. A saved file that doesn't parse is renamed
`clabgen.yml.unreadable` and the scenario used instead.

## Link state

Taking a port down (`ports: {eth1: {admin: down}}`, `clabgenctl port down
eth1`, or the Ports tab) sets the agent's interface down, and on a
containerlab veth link that drops carrier on the other end: the router sees
the link fail. The other way round, the port shows oper down when the other
end drops carrier.

Since containerlab 0.78, links to Nokia SR-SIM (`nokia_srsim`) nodes default
to `veth-stitch`: two veth pairs joined on the host by tc redirects
(`clab-stitch-…` interfaces). Frames pass through, link state doesn't: a port
taken down on either side stays up on the other. Where a test needs link
failures, give that link `type: veth` in the long form:

```yaml
links:
  - type: veth
    endpoints:
      - {node: gen1, interface: eth1}
      - {node: sr1, interface: 1/1/c1/1}
```

What that costs: impairments. containerlab applies `containerlab tools netem`
on a stitched link's host side, which works in both directions; the stitch
exists because SR-SIM's own interfaces can't be impaired. On a plain veth
link, netem can only go on the agent's end
(`containerlab tools netem set -n clab-<lab>-gen1 -i eth1 --delay 20ms`),
which impairs what the agent sends and not what the router sends back. Keep
the default stitched link where you need impairments both ways and use
`type: veth` where you need link state; a lab can mix the two link by link.

## Devices and CFM MEPs

```yaml
ports:
  eth1: {admin: up}
devices:
  - {name: v100, port: eth1, vlans: [{vid: 100}], addresses: [10.0.100.2/24, 2001:db8:100::2/64], gateway4: 10.0.100.1}
  - {name: qinq, port: eth2, vlans: [{tpid: 0x88a8, vid: 200}, {vid: 10}]}
protocols:
  - name: mep1
    type: cfm-mep
    device: v100
    config: {level: 5, mep_id: 101, remote_meps: [1], md_format: string, md_name: lab,
             ma_format: string, ma_name: v100, ccm_interval: 1s}
```

Y.1731 beyond CCM: `one-way-delay` (1DM; the receiving MEP shows the delay,
clocks being shared in a lab), `loss` (LMM over the devices' data-frame
counters: every non-CFM frame they send and receive, e.g. a flow's) and
`test-signal` (TST with a null or PRBS pattern and optional CRC; the
receiver counts loss, reordering and bit errors) are actions; `fault`
also forces AIS, locks (LCK) or sends CSF. `ais: {enabled: true,
client_level: 6}` sends AIS while the MEP has a defect. `sessions` measure
continuously, kept in bins:

```yaml
    config: {…, sender_id: gen1,
             sessions: [{name: dm, type: delay, target_mep_id: 1, interval_ms: 1000, bin_s: 60, bins: 10},
                        {name: sl, type: synthetic-loss, target_mep_id: 1, interval_ms: 100}]}
```

## Traffic flows

Flows are protocol instances of a traffic type:

```yaml
  - name: ping-gw
    type: icmp
    device: v100
    config: {destination: 10.0.100.1, rate: 100, timeout: 500ms, auto_start: true,
             ttl: 64, dscp: 46, payload_size: 200}
```

Every IP and ICMP field can be set, including the type and code,
identifier, sequence start or fixed sequence, TTL, DSCP/ECN, DF, IP ID,
flow label and payload pattern. Checksums can be set deliberately wrong.
The `preview` action shows the next packet as hex. An outage is a run of
consecutive lost packets times the send interval, so a 100 pps flow
measures a failover to within 10 ms.

UDP flows (`type: udp`) add `dst_port`, `src_port`, `src_port_count`
(cycle source ports to spread ECMP/LAG hashing), `udp_checksum` and
`reply: none | echo`. One-way flows are measured where they land: any
clabgen device counts signed packets addressed to it (`clabgenctl
received`, the Traffic tab's "Received here", and the Receivers table on
the sender's flow page). One-way latency is meaningful because agents on
one host share its clock. When an echo flow's destination is a clabgen
device, its page splits the round trip into outbound and return legs (loss
and latency per direction).

```yaml
  - {name: reflector, type: udp-echo, device: v401, config: {ports: [7]}}
  - name: udp-rt
    type: udp
    device: v400
    config: {destination: 10.0.41.2, reply: echo, rate: 100, src_port_count: 16, auto_start: true}
```

### Raw L2 streams

An L2 stream (`type: l2`) sends Ethernet frames with no IP header, so it
tests a circuit (an Epipe, a VPLS, a bridge domain) with no addresses on
either end. Its devices need only a port, a MAC and their VLAN tags. The
payload starts with the same signature as the IP flows, so the far device
counts the stream on its own: loss, reordering, duplicates, one-way
latency and outages, with the source MACs it saw and the ethertype.

```yaml
devices:
  - {name: a, port: eth1, mac: "02:00:00:00:01:01", vlans: [{vid: 300}]}
  - {name: b, port: eth2, mac: "02:00:00:00:02:01", vlans: [{vid: 300}]}
protocols:
  - name: a-to-b
    type: l2
    device: a
    config: {dst_mac: "02:00:00:00:02:01", rate: 1000, frame_size: 1500, auto_start: true}
```

- `dst_mac`: the far device's MAC. The default is broadcast, which a VPLS
  floods.
- `src_mac`: the first source MAC. The default is the device's MAC.
- `src_mac_count`: cycles through this many source MACs, one per frame,
  to fill MAC tables or test MAC limits.
- `ethertype`: the default is 0x88b5 (local experimental). Types that a
  switch or router would consume are refused: IP, ARP, CFM, LLDP, slow
  protocols (LACP, OAM), PTP and VLAN tags.
- `frame_size`: 60–9018 bytes without the FCS, VLAN tags included. 60 is
  a minimum 64-byte frame on the wire.
- `vlan_pcp`: the PCP on every VLAN tag.
- `payload_pattern`: the fill after the signature.
- `mode`, `rate`, `count`, `duration` and `timeout`: as for the IP flows.

`reply: echo` measures the round trip. An `l2-echo` responder on the far
device answers stream frames of its ethertypes. Each answer goes back to
the frame's source MAC and keeps the request's PCP. The stream then has
loss, outage, RTT and jitter, and `checks` work as for the IP flows below.
The stream answers for every MAC it cycles through, so echo works with
`src_mac_count` too.

```yaml
  - {name: echo, type: l2-echo, device: b, config: {ethertypes: [0x88b5]}}
  - name: a-b-rt
    type: l2
    device: a
    config: {dst_mac: "02:00:00:00:02:01", reply: echo, rate: 100, auto_start: true,
             checks: {max_loss_pct: 0.1, max_outage_ms: 50}}
```

### Packet builder

A packet flow (`type: packet`) sends frames you build layer by layer, with
every field settable, or replays a capture. Layers go outermost first; the
device adds its MAC and VLAN tags to the first Ethernet header, written or
not. Lengths, checksums, next-protocol numbers and ethertypes are filled
in unless set (set them wrong on purpose to test how a device copes).

```yaml
  - name: crafted
    type: packet
    device: v400
    config:
      layers:
        - ipv4: {dst: 10.0.41.2, dscp: 46, ttl: 5}
        - udp: {dst_port: 7001}
        - payload: {size: 64}
      modifiers:
        - {field: udp.src_port, mode: increment, start: "30000", count: 16}
        - {field: ipv4.ttl, mode: list, values: ["64", "1"]}
      rate: 100
      auto_start: true
```

- **Layers**: `ethernet`, `vlan` (a tag inside the device's own), `mpls`,
  `arp`, `ipv4`, `ipv6`, `udp`, `tcp`, `icmp`, `icmpv6`, `vxlan`, `raw`
  (hex bytes) and `payload`. An `ethernet` layer after VXLAN or MPLS is an
  inner frame. Each layer's fields are in the schema (`clabgenctl types
  packet`). Unset source addresses are the device's, and an unset first
  destination MAC is the next hop of the first IP destination (ARP/ND).
- **Modifiers** change a field from packet to packet. A field is named
  `layer.field` (`ipv4.src`, `ethernet.src`, `vlan.vid`), or
  `layer[n].field` for the nth layer of a kind (`vlan[2].vid`). The modes
  are `increment` and `decrement` (from `start`, by `step`, `count` values
  before wrapping), `random` (in `count` values from `start`, or the whole
  range) and `list` (`values`). `repeat` holds each value for that many
  packets. Checksums are redone after each change. Addresses step in their
  own form (`step: 0.0.1.0`) or as numbers.
- **Measuring**: the payload starts with the signature (on by default), so
  clabgen receivers count the stream one way, like the other flows. A
  packet flow doesn't wait for replies.
- **Replaying a capture**: `pcap: {data: <base64 of a pcap or pcapng>}`
  replays its frames in order, at the flow's rate. `frames` picks some
  (`1-10,15`). `src_mac: keep` keeps the captured source MAC (default: the
  device's). `vlans: keep` keeps the captured VLAN tags inside the
  device's (default: the device's tags replace them). `clabgenctl proto
  add NAME --type packet --pcap FILE` fills in the data; a capture is at
  most 8 MiB and is stored in the flow's config.
- **Previewing**: `clabgenctl preview` (below) and the UI's packet editor
  show the exact bytes, decoded by layer, for any packet index, before or
  after the flow exists.

### Pass/fail checks

`checks` give a flow a verdict: limits on what it measured since it
started or was last cleared. Any limit can be left out.

```yaml
    config: {destination: 10.0.4.1, rate: 100, auto_start: true,
             checks: {max_loss_pct: 1, max_outage_ms: 50, max_rtt_avg_ms: 5, max_rtt_ms: 20, max_jitter_ms: 2}}
```

The verdict is `pending` until something was answered or lost, then
`pass` or `fail`. A failure stays until the flow's statistics are cleared,
so a test is: clear (or start) the flows, do the thing under test (flap a
peer, fail a link), and read the verdicts. The Flows page shows every
agent's flows with their verdicts and starts, stops or clears them all;
`clabgenctl protocols` adds `checks=pass` or `checks=fail (…)`, and the
status has the measured value of each check. Loss, outage and RTT are
measured by echo flows; a one-way flow's checks stay `pending` (its loss
is counted at the receivers).

## IS-IS

An IS-IS router on a device (point-to-point to the SR in
`examples/srsim`):

```yaml
  - name: isis
    type: isis
    device: i500
    config: {system_id: 0000.0000.0050, hostname: clabgen, level: L2, areas: ["49.0001"],
             prefixes: [{prefix: 198.18.0.0/24, count: 1000, metric: 20}]}
```

`clabgenctl proto run isis flap seconds=5`, `… overload set=true`, `… advertise
prefixes='[{"prefix":"203.0.113.0/24"}]'` and `… withdraw …` change it at
runtime; a flow routed over the adjacency measures the resulting outage.

A `topology` makes the router emulate a network behind it: simulated
routers whose LSPs it originates, so the real routers run SPF over them.

```yaml
    config: {system_id: 0000.0000.0050, level: L2,
             topology: {kind: grid, rows: 10, cols: 10, attach: [r1-1],
                        loopback_base: 10.255.0.1/32, prefix_base: 10.200.0.0/24, prefixes_per_node: 5}}
```

Authentication: `hello_auth` and `lsp_auth` (`{type: password | hmac-md5 |
hmac-sha1 | hmac-sha224 | hmac-sha256 | hmac-sha384 | hmac-sha512, key: …,
key_id: …}`; SHA is RFC 5310, where `key_id` is the SR OS keychain entry); `lsp_auth` also covers CSNPs/PSNPs unless `snp_auth: false`. On
SR OS the level key signs hellos too where an interface has no hello key,
so match it with `hello_auth` on such interfaces.

An L1L2 router carries level 1 into level 2 (`summaries: [{prefix:
10.207.0.0/22}]` replace what they cover), leaks level-2 prefixes within
`leak_to_l1` ranges into level 1 with the up/down bit, re-advertises
Prefix-SIDs between levels (R and P flags), and sets the attached bit while
level 2 reaches another area (`attached_bit: auto | always | never`).
`bfd: {enabled: true, min_tx_ms: 100, min_rx_ms: 100}` (or `bfd: true` on
an interface) runs BFD with each neighbor (IPv4, and IPv6 with `families`);
with the BFD-enabled TLV on both sides (RFC 6213) an adjacency waits for
BFD, and `proto run isis bfd-fail seconds=3` silences it. Graceful restart
(RFC 8706): as helper by default (`graceful_restart: {helper: true}`), and
`proto run isis graceful-restart` restarts the router gracefully: it drops
the LSPs it learned and resynchronises them while neighbors keep the
adjacencies and its LSPs. `overload_on_startup_s: 60` sets the overload bit
for the first minute.

For rollover use a keychain; the key that started sending last is used,
and any key inside its accept window is accepted:

```yaml
lsp_auth: {type: keychain, keys: [
  {id: 1, type: hmac-sha256, key: old, send_until: "2026-11-01T00:00:00Z"},
  {id: 3, type: hmac-sha256, key: new, send_from: "2026-11-01T00:00:00Z"}]}
```

Multi-topology: `topologies: [ipv6-unicast]` (also `ipv4-multicast`,
`ipv6-multicast`) runs RFC 5120 MTs beside IPv4 unicast; IPv6 prefixes then
go in MT 2. Simulated routers follow, with `loopback6_base` / `prefix6_base`
for IPv6 addressing.

Segment routing (SR-MPLS): `segment_routing: {enabled: true, srgb_base:
16000, srgb_size: 8000, adj_sid_base: 24000, node_sids: [{prefix:
192.0.2.50/32, index: 50}]}` advertises the Router Capability, Prefix-SIDs and
Adj-SIDs; the topology's `node_sid_base` / `node_sid6_base` give the
simulated routers node SIDs, so the real routers build SR tunnels across them.

A router can have several interfaces: the instance's own device plus
`interfaces`, each a device with optional `circuit`, `metric`, `priority`,
`passive` and `hello_auth` (unset fields take the router's settings). It
floods between them and advertises every interface's neighbor and
subnets in one LSP; `metric` and `flap` take `interface=…` to act on one.

```yaml
    config: {system_id: 0000.0000.0050, level: L2,
             interfaces: [{device: i502, metric: 50}, {device: lan1, circuit: broadcast, priority: 100}]}
```

`loopbacks: [192.0.2.50/32, "2001:db8:ffff::50/128"]` gives the router
addresses of its own: advertised at metric 0 (the first IPv4 one is the
router ID) and answered (ping, flows) on every interface.

SR extensions: `segment_routing.srlb_base`/`srlb_size` (SRLB) and
`mappings` (mapping server: `{prefix, range, index}`); `flex_algos`
(`{algorithm: 128, metric_type: delay, priority: 200}`, or
`participate_only: true` for an algorithm another router defines) with
per-algorithm node SIDs (`node_sids[].algorithm`, topology
`flex_algo_sids`) and link attributes (`te_metric`, `delay_us`,
`admin_groups` on interfaces; `link_delay_us` and per-link values in
topologies); SRv6:

```yaml
srv6: {enabled: true, behavior: un, end_x: true, topology_locator_base: "fc00:0:100::/48",
       locators: [{prefix: "fc00:0:50::/48"}, {prefix: "fc00:80:50::/48", algorithm: 128}]}
```

Kinds are `grid`, `ring`, `line`, `full` and `custom` (named nodes with
links and prefixes). `topology-link a=r1-1 b=r1-2 up=false` (or `metric=…`)
and `topology-node node=r2-2 up=false` (or `overload=true`) fail parts of
it at runtime; `lsp id=sim-r2-2.00-00` shows one LSP.

## BGP

BGP runs on a device with addresses (its TCP is the device's, so it can
peer from loopbacks too):

```yaml
  - name: bgp
    type: bgp
    device: i500
    config: {local_as: 65050, router_id: 192.0.2.50, hold_time: 30,
             peers: [{address: 10.0.50.1, remote_as: 65100, families: [ipv4-unicast, ipv6-unicast]},
                     {address: 192.0.2.100, remote_as: 65100, local_address: 192.0.2.50, ttl: 64}],
             routes: [{prefix: 100.64.0.0/24, count: 10000, communities: ["65050:1"], large_communities: ["65050:1:1"]},
                      {prefix: "2001:db8:abc::/48", count: 1000, med: 50}]}
```

`clabgenctl proto run bgp advertise routes='[{"prefix":"101.0.0.0/24","count":200000}]'`
(and `withdraw`), `… reset peer=10.0.50.1`, `… flap peer=… seconds=30 mode=silent`
(the peer's hold timer expires) or `mode=notify`, `… notify code=6 subcode=2
message="maintenance"`, `… route-refresh` and `… rib peer=10.0.50.1 prefix=…`.
There is no TCP MD5 authentication (the netstack has none).

VPN families (peer `families`: `ipv4-vpn`, `ipv6-vpn`, `evpn`, `l2vpn`;
SR policies below):
a route with an `rd` is VPNv4/VPNv6 (`label`, `route_targets`; the VPNv6
next hop is IPv4-mapped over IPv4, as 6VPE); `evpn` entries are MAC/IP
(counting MACs and IPs up), inclusive multicast (PMSI tunnel, ingress
replication), Ethernet A-D (ESI label, single-active), Ethernet segment
and IP prefix routes, with MPLS labels or VXLAN VNIs (`encapsulation`),
MAC mobility and the router's MAC; `l2vpn` entries are BGP VPLS (RFC 4761)
or VPWS (RFC 6624) label blocks (`ve_id`, `block_offset`, `block_size`,
`label_base`, Layer2 Info MTU and control word). Received routes of these
families show in the `rib` action (filter by text, e.g. `65100:500` or
`mac:02:00`).

```yaml
             routes: [{prefix: 10.205.0.0/24, count: 100, rd: "65050:500", label: 5000, route_targets: ["65100:500"]}],
             evpn: [{type: mac-ip, rd: "65050:600", route_targets: ["65100:600"], mac: "02:00:00:06:00:01", ip: 10.206.0.1, count: 50, label: 6000},
                    {type: inclusive-multicast, rd: "65050:600", route_targets: ["65100:600"], label: 6001}],
             l2vpn: [{kind: vpws, rd: "65050:800", route_targets: ["65100:800"], ve_id: 2, label_base: 800000, block_size: 2}]
```

Over eBGP, SR OS drops VPN routes under its RFC 8212 default reject
(`vpn-apply-import` is off, so the group's import policy doesn't apply to
them): set `ebgp-default-reject-policy import false export false` on the
group, or use iBGP.

### SR policies

clabgen can be the controller that hands SR policies to headends: family
`ipv4-srpolicy` / `ipv6-srpolicy` (BGP SR Policy, RFC 9830), one route per
candidate path. A headend installs the candidate path with the highest
preference among the valid ones and steers traffic into it: traffic to
the binding SID, and BGP routes with the policy's color.

```yaml
             peers: [{address: 192.0.2.100, remote_as: 65100, families: [ipv4-unicast, ipv4-srpolicy]}],
             sr_policies:
               - {endpoint: 192.0.2.50, color: 100, headend: 192.0.2.100, preference: 200,
                  binding_sid: 32050, segment_lists: [{labels: [20050]}]}
               - {endpoint: 192.0.2.50, color: 100, headend: 192.0.2.100, distinguisher: 2, preference: 100,
                  segment_lists: [{labels: [20003, 20050], weight: 1}, {labels: [20004, 20050], weight: 2}]}
             routes: [{prefix: 198.51.100.0/24, ext_communities: ["color:100"]}]   # steered into color 100
```

- `endpoint` and `color` name the policy; `distinguisher` tells its
  candidate paths apart (default 1: give each candidate path of a policy
  its own).
- `headend`: the BGP identifier of the router that should install it (a
  route target `<headend>:0`). Without it the route carries NO_ADVERTISE,
  for when the headend is the BGP peer itself.
- Each candidate path has a `preference` (default 100), segment lists of
  SR-MPLS labels outermost first (with optional `weight`s to split
  traffic), an optional `binding_sid`, and, among the advanced options,
  the S and I binding-SID flags, `priority`, `explicit_null`, and policy
  and candidate path names.
- `color:N` in a route's `ext_communities` steers that route into the
  policies of color N.
- `advertise` and `withdraw` take `sr_policies` too, to change candidate
  paths while traffic runs (e.g. withdraw the preferred one and measure
  the failover).

SR OS takes them with `bgp sr-policy-import true`, `family sr-policy-ipv4`
on the group and `segment-routing sr-policies admin-state enable` (plus a
`reserved-label-block` for binding SIDs); `show router segment-routing
sr-policies bgp` shows what it installed. Send traffic to a binding SID
over an interface that forwards MPLS (an IS-IS or OSPF segment-routing
link): a plain IES interface drops labelled packets.

## OSPFv2 and OSPFv3

An OSPFv2 router on a device, here point-to-point with MD5 and a second
interface on a LAN where it wants to be DR:

```yaml
  - name: ospf
    type: ospf
    device: o600
    config: {router_id: 192.0.2.60, area: 0.0.0.0, network: point-to-point,
             auth: {type: md5, key: clabgen, key_id: 1}, loopbacks: [192.0.2.60/32],
             interfaces: [{device: o601a, network: broadcast, priority: 100, auth: {type: none}}],
             prefixes: [{prefix: 100.66.0.0/24, count: 1000, tag: 600},
                        {prefix: 100.65.60.0/24, type: internal, metric: 5}]}
```

It forms adjacencies (full DR/BDR election on broadcast interfaces),
exchanges and floods LSAs, and originates its router LSA, a network LSA
where it is DR, and the `prefixes`: AS-external LSAs (`external-2` by
default, or `external-1`, with tag and forwarding address) or, with `type:
internal`, stub links in the router LSA, or with `type: inter-area`,
summary LSAs. Packets larger than the `mtu` (a router LSA with hundreds of
links) go out in IP fragments, and received fragments are reassembled.
Actions: `flap` (reset or silent, per interface), `cost`, `max-metric`
(RFC 6987), `advertise` / `withdraw`, `graceful-restart`, `bfd-fail` and
`lsa` (one LSA decoded). It installs no routes. The MTU in database
descriptions is `mtu` (default 1500); a neighbor with a larger one is
refused (as RFC 2328 says), so match it on the peer (SR OS: `ip-mtu 1500`)
or set `mtu_ignore`.

Interfaces are `broadcast`, `point-to-point`, `non-broadcast` (NBMA: hellos
to the `neighbors` listed, DR election) or `point-to-multipoint` (every
neighbor adjacent; with `neighbors`, non-broadcast). Each has an `area`
(default: the router's); in more than one area the router is an area
border router. It runs SPF over each area only to summarise it into the
others: `areas` sets an area's `type` (`stub`, `nssa`), `no_summary`,
`default_cost`, `ranges` (aggregation, `no_advertise`) and NSSA
translation (`nssa_translate`; externals become type-7 LSAs in an NSSA, and
a border router translates others' into AS-externals). `topology.area` puts
an emulated network in its own area behind us, and `virtual_links` reach
the backbone across a transit area (`hello_interval` / `dead_interval` per
interface or virtual link: SR OS virtual links default to 10/60).

```yaml
             areas: [{id: 2, ranges: [{prefix: 100.72.0.0/20}]}],
             topology: {kind: grid, rows: 2, cols: 3, metric: 20, area: 2,
                        loopback_base: 10.255.60.1/32, prefix_base: 100.72.0.0/24, prefixes_per_node: 2}
```

Segment routing (`segment_routing: {enabled, srgb_base, srgb_size,
node_sids, adj_sid_base, srlb_base}`, both versions) is advertised in
opaque LSAs for OSPFv2 (RFC 8665: Router Information, Extended Prefix and
Extended Link LSAs) and in extended LSAs for OSPFv3 (RFC 8666 with RFC 8362
sparse mode): node SIDs for our prefixes and the simulated routers'
loopbacks (`topology.node_sid_base`), Adj-SIDs and LAN Adj-SIDs, and
Prefix-SIDs carried into other areas by a border router. OSPFv2 also does
flex-algo (`flex_algos`: definitions, per-algorithm node SIDs, link
`delay_us` / `te_metric` / `admin_groups` as application-specific link
attributes, flex-algo prefix metrics between areas) and TE LSAs
(`traffic_engineering`, RFC 3630).

`graceful_restart: {helper, grace_period}` (RFC 3623 / 5187): as a helper
we keep a restarting neighbor adjacent; the `graceful-restart` action
restarts us gracefully (grace LSAs, then neighbors and LSDB dropped and
relearned without re-originating, BFD kept running). `bfd: {enabled,
min_tx_ms, min_rx_ms, multiplier}` runs BFD (RFC 5880/5881) with every
neighbor and drops a neighbor whose session fails; `bfd-fail` silences ours
for a while.

OSPFv3 (`type: ospfv3`) is the same router over IPv6 (RFC 5340): it needs
an IPv6 address on the device (it speaks from the link-local one) and a
`router_id` unless the device also has an IPv4 address. It originates a
router LSA, a link LSA per interface, a network LSA and its
intra-area-prefix LSA where it is DR, intra-area-prefix LSAs for its own
prefixes (interfaces that aren't transit, `loopbacks`, `internal`
prefixes) and AS-external LSAs; `instance_id` selects the instance.

Authentication: OSPFv2 takes `{type: simple | md5 | hmac-sha1 |
hmac-sha256 | hmac-sha384 | hmac-sha512, key, key_id}` (RFC 5709 for the
SHA types), per router or per interface; OSPFv3 takes the authentication
trailer (RFC 7166) as `{type: hmac-sha*, key, sa_id}`. (SR OS authenticates
OSPFv3 with IPsec instead, so the trailer was only tested between clabgen
routers.) Both take `{type: keychain, keys: [{id, type, key, send_from,
send_until, accept_from, accept_until}]}` for hitless rollover: we send
with the key whose send window started last and accept any key in its
accept window.

Both versions emulate a network behind the router with `topology`, like
IS-IS (`grid`, `ring`, `line`, `full`, `custom`): each simulated router
gets a router ID from `router_id_base`, a loopback from `loopback_base` and
`prefixes_per_node` prefixes from `prefix_base`, joined by unnumbered
point-to-point links. `topology-link` and `topology-node` fail links and
routers (or set max-metric on one) at run time.

```yaml
             topology: {kind: grid, rows: 2, cols: 3, metric: 20, router_id_base: 10.254.60.1,
                        loopback_base: 10.255.60.1/32, prefix_base: 100.72.0.0/24, prefixes_per_node: 2}
```

```yaml
  - name: ospf6
    type: ospfv3
    device: o600
    config: {router_id: 192.0.2.60, network: point-to-point, loopbacks: ["2001:db8:ffff::60/128"],
             interfaces: [{device: o601a, network: broadcast, priority: 100}],
             prefixes: [{prefix: "2001:db8:6600::/48", count: 500, tag: 600}]}
```

## LLDP

LLDP runs on an untagged device (LLDP frames carry no VLAN tag):

```yaml
devices:
  - {name: lldp1, port: eth1}
protocols:
  - name: lldp-eth1
    type: lldp
    device: lldp1
    config: {tx_interval: 30, system_name: gen1, capabilities: [bridge, router],
             management_addresses: [192.0.2.50], port_vlan_id: 1, max_frame_size: 9212}
```

It lists the neighbors it hears (`clabgenctl proto show lldp-eth1`);
`… run lldp-eth1 shutdown` / `resume` / `pause seconds=120` make
neighbors drop or age it out, and `custom_tlvs` add arbitrary TLVs.

## Device groups

A device group is many devices from one template, each running the same
protocols: 200 BGP peers, 500 MEPs on 500 VLANs, a host per subnet. Any
value in the device or protocol template can be stepped per member with
`{start, step, repeat, cycle}`, where `start` is an integer, an IP
address, an address with a prefix length or a MAC, and `step` an integer
or a value of the same form (`0.0.0.4`, `::1:0`). Member `i` (from 0)
gets `start + k·step`, with `k = (i / repeat) % cycle`, so QinQ is an
outer VLAN with `repeat: 100` and an inner one with `cycle: 100`. Strings
take placeholders: `{i}` (from 1), `{i0}`, `{group}`, and in protocol
configs the member's `{device}`, `{ipv4}`, `{ipv6}`, `{gateway4}`,
`{gateway6}`, `{mac}`, `{vlan}` and `{inner_vlan}`.

```yaml
device_groups:
  - name: ce
    count: 200
    device:                       # named ce-1 … ce-200 unless it sets a name
      port: eth3
      vlans: [{vid: {start: 1000}}]
      addresses: [{start: 10.1.0.2/30, step: 0.0.0.4}]
      gateway4: {start: 10.1.0.1, step: 0.0.0.4}
    protocols:                    # instances ce-1-bgp … ce-200-bgp
      - type: bgp
        config:
          local_as: {start: 65001}
          peers: [{address: "{gateway4}", remote_as: 65100}]
          routes: [{prefix: {start: 172.16.0.0/24, step: 0.0.10.0}, count: 10, communities: ["65100:{i}"]}]
```

Members are ordinary devices and protocol instances (their pages, actions
and flows work as usual) but change only through the group. Changing a
group (`clabgenctl group set ce.yml`) keeps members whose rendered config
is the same running, so the count can grow or shrink without touching the
rest. `clabgenctl groups` summarises each group protocol by the protocols'
own state (e.g. `198 established, 2 active`), `group show ce` lists the
members, and `group run ce flap --members 1-20` or `--random 5` runs an
action on a selection in parallel. `group disable ce --members 2,5` (or
`--random 3`) stops a group protocol on some members, keeping their
config, until `group enable`; on every member (`group disable ce`) it is
recorded in the spec as `disabled: true` on the group protocol, so it is
exported with the scenario. Lists leave members out unless asked
(`devices --all`, `protocols --group ce`); the UI has a Groups tab.
