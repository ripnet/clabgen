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
defect detection; loopback, linktrace, delay (DMM) and synthetic loss (SLM)
as both initiator and responder; and fault injection. MEPs can be untagged,
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
    config: {system_id: 0000.0000.0050, hostname: clabgen, level: L2, areas: [49.0001],
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
action on a selection in parallel. Lists leave members out unless asked
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
