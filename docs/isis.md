# IS-IS

An IS-IS router on a device (point-to-point to the SR in
`examples/srsim`):

```yaml
  - name: isis
    type: isis
    device: i500
    config: {system_id: 0000.0000.0050, hostname: clabgen, level: L2, areas: ["49.0001"],
             prefixes: [{prefix: 198.18.0.0/24, count: 1000, metric: 20}]}
```

## Actions

- `flap seconds=5`, `overload set=true`, `metric`.
- `advertise prefixes='[{"prefix":"203.0.113.0/24"}]'` and `withdraw`.
- `topology-link` and `topology-node`: fail parts of an
  [emulated network](#emulated-network).
- `graceful-restart`, `bfd-fail seconds=3`, and `lsp id=…` to show one LSP.

For example, `clabgenctl proto run isis flap seconds=5`. A flow routed
over the adjacency measures the resulting outage.

## Interfaces and loopbacks

A router can have several interfaces: the instance's own device plus
`interfaces`, each a device with optional `circuit`, `metric`, `priority`,
`passive` and `hello_auth` (unset fields take the router's settings). It
floods between them and advertises every interface's neighbor and subnets
in one LSP; `metric` and `flap` take `interface=…` to act on one.

```yaml
    config: {system_id: 0000.0000.0050, level: L2,
             interfaces: [{device: i502, metric: 50}, {device: lan1, circuit: broadcast, priority: 100}]}
```

`loopbacks: [192.0.2.50/32, "2001:db8:ffff::50/128"]` gives the router
addresses of its own. They are advertised at metric 0 (the first IPv4 one
is the router ID) and answered (ping, flows) on every interface.

## Authentication

- `hello_auth` and `lsp_auth`: `{type, key, key_id}`, where `type` is
  `password`, `hmac-md5`, `hmac-sha1`, `hmac-sha224`, `hmac-sha256`,
  `hmac-sha384` or `hmac-sha512`. SHA is RFC 5310, where `key_id` is the
  SR OS keychain entry.
- `lsp_auth` also covers CSNPs and PSNPs, unless `snp_auth: false`.
- On SR OS the level key signs hellos too where an interface has no hello
  key, so match it with `hello_auth` on such interfaces.

For rollover, use a keychain: the key that started sending last is used,
and any key inside its accept window is accepted.

```yaml
lsp_auth: {type: keychain, keys: [
  {id: 1, type: hmac-sha256, key: old, send_until: "2026-11-01T00:00:00Z"},
  {id: 3, type: hmac-sha256, key: new, send_from: "2026-11-01T00:00:00Z"}]}
```

## Levels

An L1L2 router:

- carries level 1 into level 2 (`summaries: [{prefix: 10.207.0.0/22}]`
  replace what they cover);
- leaks level-2 prefixes within `leak_to_l1` ranges into level 1, with the
  up/down bit;
- re-advertises Prefix-SIDs between levels (R and P flags);
- sets the attached bit while level 2 reaches another area
  (`attached_bit: auto | always | never`).

## BFD and graceful restart

- `bfd: {enabled: true, min_tx_ms: 100, min_rx_ms: 100}` (or `bfd: true`
  on an interface) runs BFD with each neighbor: IPv4, and IPv6 with
  `families`. With the BFD-enabled TLV on both sides (RFC 6213), an
  adjacency waits for BFD. `bfd-fail seconds=3` silences it.
- Graceful restart (RFC 8706) is on as a helper by default
  (`graceful_restart: {helper: true}`). The `graceful-restart` action
  restarts the router gracefully: it drops the LSPs it learned and
  resynchronises them, while neighbors keep the adjacencies and its LSPs.
- `overload_on_startup_s: 60` sets the overload bit for the first minute.

## Multi-topology

`topologies: [ipv6-unicast]` (also `ipv4-multicast` and `ipv6-multicast`)
runs RFC 5120 MTs beside IPv4 unicast; IPv6 prefixes then go in MT 2.
Simulated routers follow, with `loopback6_base` and `prefix6_base` for
IPv6 addressing.

## Segment routing

- **SR-MPLS**: `segment_routing: {enabled: true, srgb_base: 16000,
  srgb_size: 8000, adj_sid_base: 24000, node_sids: [{prefix:
  192.0.2.50/32, index: 50}]}` advertises the Router Capability,
  Prefix-SIDs and Adj-SIDs. The topology's `node_sid_base` and
  `node_sid6_base` give the simulated routers node SIDs, so the real
  routers build SR tunnels across them.
- **SRLB and mapping server**: `segment_routing.srlb_base` / `srlb_size`,
  and `mappings` (`{prefix, range, index}`).
- **Flex-algo**: `flex_algos` (`{algorithm: 128, metric_type: delay,
  priority: 200}`, or `participate_only: true` for an algorithm another
  router defines), with per-algorithm node SIDs (`node_sids[].algorithm`,
  the topology's `flex_algo_sids`) and link attributes (`te_metric`,
  `delay_us` and `admin_groups` on interfaces; `link_delay_us` and
  per-link values in topologies).
- **SRv6**:

```yaml
srv6: {enabled: true, behavior: un, end_x: true, topology_locator_base: "fc00:0:100::/48",
       locators: [{prefix: "fc00:0:50::/48"}, {prefix: "fc00:80:50::/48", algorithm: 128}]}
```

## Emulated network

A `topology` makes the router emulate a network behind it: simulated
routers whose LSPs it originates, so the real routers run SPF over them.

```yaml
    config: {system_id: 0000.0000.0050, level: L2,
             topology: {kind: grid, rows: 10, cols: 10, attach: [r1-1],
                        loopback_base: 10.255.0.1/32, prefix_base: 10.200.0.0/24, prefixes_per_node: 5}}
```

- Kinds are `grid`, `ring`, `line`, `full` and `custom` (named nodes with
  links and prefixes).
- `topology-link a=r1-1 b=r1-2 up=false` (or `metric=…`) and
  `topology-node node=r2-2 up=false` (or `overload=true`) fail parts of it
  at runtime.
- `lsp id=sim-r2-2.00-00` shows one of its LSPs.
