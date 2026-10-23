# clabgenctl

`clabgenctl` drives an agent from a shell or a script: everything the web
UI does, with tables for lists and YAML or JSON (`-o json`) for details.

## Getting it

- Inside every agent container: `docker exec clab-<lab>-<node> clabgenctl …`
  (it talks to the agent on localhost).
- On a host or a Mac: download `clabgenctl-<os>-<arch>` from the
  [releases](https://github.com/ripnet/clabgen/releases) and point it at
  an agent's management address or containerlab name with `-s` (or
  `CLABGEN_SERVER`):

```sh
clabgenctl -s clab-b2b-gen1 protocols
```

## Commands

```
info                      agent hostname, version and start time
ports / port              data ports; port down|up <port>
devices / device          emulated devices: add, set, show, del, resolve, clear-neighbors
protocols / proto         protocol instances: add, show, set, disable, enable, del, run <action>
types [TYPE]              protocol types, or one type's config and action schemas
groups / group            device groups: add, set, show, disable, enable, del, run <action>
ping                      ping from a device (a temporary icmp flow)
start / stop              start or stop traffic flows
received                  flows arriving at the agent's devices (one-way statistics)
preview                   the packet a config would send (packet type), decoded and as hex
events                    stream the agent's events
scenario get / apply      export or apply the whole config
```

`clabgenctl <command> --help` lists each command's options. Configs and
action arguments take `key=value` pairs or a whole document with
`--config` / `--args` (JSON, YAML, or `@file`). A value is read as JSON
when it is valid JSON (`remote_meps=[2]` is a list, `rate=50` a number)
and as a string otherwise; dots reach into nested settings:

```sh
clabgenctl proto add ping --type icmp --device d1 destination=10.9.0.2 rate=50 auto_start=true \
  checks.max_loss_pct=1 checks.max_outage_ms=200
```

## Examples

With the [b2b example lab](../examples/b2b):

```sh
clabgenctl -s clab-b2b-gen1 events &                          # live events
clabgenctl -s clab-b2b-gen2 port down eth1                    # gen1 raises LOC
clabgenctl -s clab-b2b-gen1 proto run mep-v100 loopback target_mep_id=102 count=5
clabgenctl -s clab-b2b-gen1 proto run mep-v100 fault ccm_enabled=false
clabgenctl -s clab-b2b-gen1 proto disable mep-v100            # stop it, keep its config
clabgenctl -s clab-b2b-gen1 proto enable mep-v100
clabgenctl -s clab-b2b-gen1 types cfm-mep                     # config + action schemas
clabgenctl -s clab-b2b-gen1 device set v100 --ip 10.0.100.1/24 --gw 10.0.100.254
clabgenctl -s clab-b2b-gen1 device resolve untagged 192.0.2.2 # ARP / ND
clabgenctl -s clab-b2b-gen1 device show untagged              # neighbors + counters
clabgenctl -s clab-b2b-gen1 ping untagged 192.0.2.2 -c 5 -i 100ms
clabgenctl -s clab-b2b-gen1 stop ping-gen2                    # flows: start / stop
clabgenctl -s clab-b2b-gen2 received                          # flows arriving at gen2
clabgenctl -s clab-b2b-gen1 scenario get > saved.yml
```

Packet flows (see [configuration](configuration.md#packet-builder)):

```sh
clabgenctl preview --device untagged --config '{layers: [{ipv4: {dst: 192.0.2.2}}, {tcp: {dst_port: 22, flags: SA}}]}'
clabgenctl preview --device untagged --config @packet.yml --index 5   # packet 5: modifiers applied
clabgenctl proto add replay --type packet --device untagged --pcap capture.pcapng rate=100 auto_start=true
clabgenctl proto run replay preview index=2                            # a running flow's packet
```

Device groups (see [configuration](configuration.md#device-groups)):

```sh
clabgenctl groups                                   # each group protocol's states
clabgenctl group show ce                            # the members
clabgenctl group run ce flap --members 1-20 seconds=10
clabgenctl group run ce reset --random 5
clabgenctl group disable ce --members 2,5           # until group enable
```
