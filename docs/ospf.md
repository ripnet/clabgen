# OSPFv2 and OSPFv3

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
exchanges and floods LSAs, and originates its router LSA and a network LSA
where it is DR. It installs no routes. The `prefixes` become:

- AS-external LSAs: `external-2` by default, or `external-1`, with tag and
  forwarding address;
- with `type: internal`, stub links in the router LSA;
- with `type: inter-area`, summary LSAs.

Packets larger than the `mtu` (a router LSA with hundreds of links) go out
in IP fragments, and received fragments are reassembled. The MTU in
database descriptions is `mtu` (default 1500). A neighbor with a larger
one is refused (as RFC 2328 says), so match it on the peer (SR OS:
`ip-mtu 1500`) or set `mtu_ignore`.

## Actions

`flap` (reset or silent, per interface), `cost`, `max-metric` (RFC 6987),
`advertise` / `withdraw`, `graceful-restart`, `bfd-fail`, `topology-link`
and `topology-node` (see Emulated network below), and `lsa` (one LSA
decoded).

## Interfaces and areas

- Network types are `broadcast`, `point-to-point`, `non-broadcast` (NBMA:
  hellos to the `neighbors` listed, DR election) and
  `point-to-multipoint` (every neighbor adjacent; with `neighbors`,
  non-broadcast).
- Each interface has an `area` (default: the router's). In more than one
  area the router is an area border router. It runs SPF over each area
  only to summarise it into the others.
- `areas` sets an area's `type` (`stub`, `nssa`), `no_summary`,
  `default_cost`, `ranges` (aggregation, `no_advertise`) and NSSA
  translation (`nssa_translate`). Externals become type-7 LSAs in an NSSA,
  and a border router translates others' into AS-externals.
- `topology.area` puts an emulated network in its own area behind us.
- `virtual_links` reach the backbone across a transit area.
  `hello_interval` / `dead_interval` are set per interface or virtual link
  (SR OS virtual links default to 10/60).

```yaml
             areas: [{id: 2, ranges: [{prefix: 100.72.0.0/20}]}],
             topology: {kind: grid, rows: 2, cols: 3, metric: 20, area: 2,
                        loopback_base: 10.255.60.1/32, prefix_base: 100.72.0.0/24, prefixes_per_node: 2}
```

## Segment routing and TE

- **SR** (`segment_routing: {enabled, srgb_base, srgb_size, node_sids,
  adj_sid_base, srlb_base}`, both versions) is advertised in opaque LSAs
  for OSPFv2 (RFC 8665: Router Information, Extended Prefix and Extended
  Link LSAs) and in extended LSAs for OSPFv3 (RFC 8666 with RFC 8362
  sparse mode). That covers node SIDs for our prefixes and the simulated
  routers' loopbacks (`topology.node_sid_base`), Adj-SIDs and LAN
  Adj-SIDs, and Prefix-SIDs carried into other areas by a border router.
- **Flex-algo** (OSPFv2): `flex_algos` definitions, per-algorithm node
  SIDs, link `delay_us` / `te_metric` / `admin_groups` as
  application-specific link attributes, and flex-algo prefix metrics
  between areas.
- **TE LSAs** (OSPFv2): `traffic_engineering` (RFC 3630).

## Graceful restart and BFD

- `graceful_restart: {helper, grace_period}` (RFC 3623 / 5187): as a
  helper we keep a restarting neighbor adjacent. The `graceful-restart`
  action restarts us gracefully: grace LSAs, then neighbors and LSDB
  dropped and relearned without re-originating, with BFD kept running.
- `bfd: {enabled, min_tx_ms, min_rx_ms, multiplier}` runs BFD (RFC
  5880/5881) with every neighbor and drops a neighbor whose session fails.
  `bfd-fail` silences ours for a while.

## Authentication

- OSPFv2: `{type: simple | md5 | hmac-sha1 | hmac-sha256 | hmac-sha384 |
  hmac-sha512, key, key_id}` (RFC 5709 for the SHA types), per router or
  per interface.
- OSPFv3: the authentication trailer (RFC 7166), `{type: hmac-sha*, key,
  sa_id}`. SR OS authenticates OSPFv3 with IPsec instead, so the trailer
  was only tested between clabgen routers.
- Both take `{type: keychain, keys: [{id, type, key, send_from,
  send_until, accept_from, accept_until}]}` for hitless rollover: we send
  with the key whose send window started last, and accept any key in its
  accept window.

## Emulated network

Both versions emulate a network behind the router with `topology`, like
IS-IS (`grid`, `ring`, `line`, `full`, `custom`). Each simulated router
gets a router ID from `router_id_base`, a loopback from `loopback_base`
and `prefixes_per_node` prefixes from `prefix_base`, joined by unnumbered
point-to-point links. `topology-link` and `topology-node` fail links and
routers (or set max-metric on one) at run time.

```yaml
             topology: {kind: grid, rows: 2, cols: 3, metric: 20, router_id_base: 10.254.60.1,
                        loopback_base: 10.255.60.1/32, prefix_base: 100.72.0.0/24, prefixes_per_node: 2}
```

## OSPFv3

OSPFv3 (`type: ospfv3`) is the same router over IPv6 (RFC 5340):

- It needs an IPv6 address on the device (it speaks from the link-local
  one), and a `router_id` unless the device also has an IPv4 address.
- It originates a router LSA, a link LSA per interface, a network LSA and
  its intra-area-prefix LSA where it is DR, intra-area-prefix LSAs for its
  own prefixes (interfaces that aren't transit, `loopbacks`, `internal`
  prefixes) and AS-external LSAs.
- `instance_id` selects the instance.

```yaml
  - name: ospf6
    type: ospfv3
    device: o600
    config: {router_id: 192.0.2.60, network: point-to-point, loopbacks: ["2001:db8:ffff::60/128"],
             interfaces: [{device: o601a, network: broadcast, priority: 100}],
             prefixes: [{prefix: "2001:db8:6600::/48", count: 500, tag: 600}]}
```
