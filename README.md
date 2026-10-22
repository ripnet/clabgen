# clabgen

A protocol emulator and packet generator for [containerlab](https://containerlab.dev)
labs, aimed at control-plane testing. A small agent runs as a clab node,
takes over the node's data interfaces, and emulates endpoints on them. You
drive it over an API, with `clabgenctl`, or with scenario files.

Devices can carry IPv4/IPv6 addresses: they answer ARP, ND and ping, and
resolve neighbors on demand. Currently implemented protocols: **ETH-CFM /
Y.1731 down MEPs** and **IS-IS** (point-to-point or broadcast with DIS election and
pseudonode; LSDB sync, route injection at scale, overload/metric/flap
actions). Traffic flows: **ICMP** (continuous, counted or timed
ping, or any crafted ICMP/ICMPv6 packet) and **UDP** (one way, or echoed
back by a `udp-echo` responder or any echo service), with loss, outage
time, RTT and jitter, and ICMP errors matched to the flow. Every clabgen
device measures signed flows it receives one way (loss, reordering,
duplicates, latency, outages) with no setup. That covers CCM with
defect detection; loopback, linktrace, delay (DMM, 1DM), synthetic loss
(SLM), frame loss (LMM) and test signal (TST) as both initiator and
responder; AIS, LCK and CSF; proactive measurement sessions; and fault
injection. MEPs can be untagged,
dot1q or QinQ. Interop with Nokia SR-SIM is verified.

## Quick start

```sh
make image                                    # builds ghcr.io/ripnet/clabgen:dev
cd examples/cfm-b2b && containerlab deploy -t cfm-b2b.clab.yml

docker exec clab-cfm-b2b-gen1 clabgenctl protocols
NAME          TYPE     DEVICE    STATE    SUMMARY
mep-qinq      cfm-mep  qinq      running  lvl=2 mep=201 integer(200) rmeps=[202:ok] defect=none
mep-untagged  cfm-mep  untagged  running  lvl=3 mep=1 icc(CLABGEN00001) rmeps=[2:ok] defect=none
mep-v100      cfm-mep  v100      running  lvl=5 mep=101 string(lab)/string(v100) rmeps=[102:ok] defect=none
```

The CLI also runs from the host (or a Mac) against an agent's management address:

```sh
make build          # needs Go 1.23+ and Node 20+ (for the UI)
bin/clabgenctl -s clab-cfm-b2b-gen1 events &                        # live events
bin/clabgenctl -s clab-cfm-b2b-gen2 port down eth1                  # gen1 raises LOC
bin/clabgenctl -s clab-cfm-b2b-gen1 proto run mep-v100 loopback target_mep_id=102 count=5
bin/clabgenctl -s clab-cfm-b2b-gen1 proto run mep-v100 fault ccm_enabled=false
bin/clabgenctl -s clab-cfm-b2b-gen1 proto disable mep-v100      # stop it, keep its config (enable starts it)
bin/clabgenctl -s clab-cfm-b2b-gen1 types cfm-mep                   # config + action schemas
bin/clabgenctl -s clab-cfm-b2b-gen1 device set v100 --ip 10.0.100.1/24 --gw 10.0.100.254
bin/clabgenctl -s clab-cfm-b2b-gen1 device resolve untagged 192.0.2.2  # ARP / ND
bin/clabgenctl -s clab-cfm-b2b-gen1 device show untagged               # neighbors + counters
bin/clabgenctl -s clab-cfm-b2b-gen1 ping untagged 192.0.2.2 -c 5 -i 100ms
bin/clabgenctl -s clab-cfm-b2b-gen1 stop ping-gen2                     # flows: start / stop
bin/clabgenctl -s clab-cfm-b2b-gen2 received                           # flows arriving at gen2
bin/clabgenctl -s clab-cfm-b2b-gen1 scenario get > saved.yml
```

## Web UI

Every agent serves a UI on its API port: `http://<agent-mgmt-ip>:8080/`.

For a view of every lab on the machine, run one `clabgen-ui` container per
lab host, much like Edgeshark. It finds every running containerlab node with
a clabgen image through the Docker socket and proxies the browser's calls
to them:

```sh
make ui-up          # docker compose -f deploy/compose.yml up -d; http://127.0.0.1:8090
make ui-down
CLABGEN_UI_LISTEN=0.0.0.0:8090 make ui-up       # listen on all interfaces
```

It uses host networking so it can reach every lab's management bridge.
Re-run `make ui-up` after `make image` to pick up a new build. The same
binary also runs directly on a host:

```sh
bin/clabgen-ui                                  # Docker socket discovery
bin/clabgen-ui -lab examples/cfm-b2b/cfm-b2b.clab.yml
bin/clabgen-ui -agent gen1=172.20.20.5 -agent gen2=172.20.20.6
```

From a laptop, tunnel the one port: `ssh -L 8090:127.0.0.1:8090 labhost`,
then open http://localhost:8090.

The UI has:
- a lab overview (every protocol on every agent, worst first);
- per-agent views of ports, devices (with IP neighbors and a Ping…
  shortcut), protocols, traffic flows, live events and the scenario editor;
- a flow page with live loss/outage/RTT tiles, a per-second timeline and a
  per-packet log;
- a protocol page with live status, config editing, and forms for its
  actions (loopback, linktrace, fault injection, …).

Forms are generated from each protocol's schema, so new protocol types
show up without UI changes. For UI development: `cd web && npm run dev`,
with a `clabgen-ui` running on :8090.

## Scenario files

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

An IS-IS router on a device (point-to-point to the SR in
`examples/cfm-srsim`):

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

VPN families (peer `families`: `ipv4-vpn`, `ipv6-vpn`, `evpn`, `l2vpn`):
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

### Device groups

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

Mount the file in the topology and set `CLABGEN_SCENARIO`. See
`examples/*/*.clab.yml`.

## API

`AgentService` in `api/proto/clabgen/v1/agent.proto` is served on `:8080`
over Connect, gRPC and gRPC-Web, so plain curl works too:

```sh
curl -s -H 'Content-Type: application/json' -d '{"withStatus":true}' \
  http://clab-cfm-b2b-gen1:8080/clabgen.v1.AgentService/ListProtocols
```

## Adding protocols

See [docs/writing-a-protocol.md](docs/writing-a-protocol.md).
