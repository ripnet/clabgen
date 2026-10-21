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
