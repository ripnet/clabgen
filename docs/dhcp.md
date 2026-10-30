# DHCP

clabgen has DHCP clients and servers, for IPv4 and IPv6 (with prefix
delegation). The servers answer clients on their own link and behind
relays, so a lab can test the relays and BNGs in between.

| Type | |
|---|---|
| `dhcp`, `dhcpv6` | [clients](#clients) that lease a device's addresses |
| `dhcp-server`, `dhcpv6-server` | [servers](#servers) with a pool per client subnet |

## Clients

A device without addresses can lease them: `dhcp` is a DHCPv4 client
and `dhcpv6` a DHCPv6 client, with prefix delegation. While a lease
lasts, its address and gateway are the device's (the Devices tab marks
them `dhcp`), so it answers ARP, ND and ping on them, and flows and pings
run from them. They go when the lease ends, is released, or the protocol
is deleted.

```yaml
devices:
  - {name: cpe, port: eth1, vlans: [{vid: 100}]}     # no addresses
protocols:
  - name: cpe-v4
    type: dhcp
    device: cpe
    config: {hostname: cpe}
  - name: cpe-v6
    type: dhcpv6
    device: cpe
    config: {ia_na: true, ia_pd: true, pd_length: 56, pd_addresses: [{subnet: 1, host: 1}]}
```

### DHCPv4

The client runs the usual exchange (Discover, Offer, Request, Ack),
renews with the server at T1 and rebinds with any server at T2. A lease
that runs out takes its address off the device and the client starts
again.

- The lease's router (option 3) becomes the device's IPv4 gateway, unless
  the device has a `gateway4` of its own or `gateway: false`.
- `client_id`, `hostname`, `vendor_class`, `request_address`,
  `lease_time` and `options` set what it sends. `options` takes any code,
  with a value as text or as hex (`0x…`). The default client identifier
  is hardware type 1 and the device's MAC.
- `broadcast` asks for broadcast replies, for servers and relays that
  can't unicast to a client without an address.
- `rapid_commit` asks for the two-message exchange (RFC 4039).

### DHCPv6

`ia_na` asks for an address and `ia_pd` for a delegated prefix (with
`pd_length` as a hint). The client sends its messages from its
link-local address, takes the best server's Advertise (or a Reply, with
`rapid_commit`), then renews at T1 and rebinds at T2.

DHCPv6 gives no default route. With `ra` on (the default), the client
solicits router advertisements and follows one:

- the router becomes the device's IPv6 gateway, for as long as its router
  lifetime lasts;
- the leased address takes the length of the on-link prefix the
  advertisement holds (a /64, say), and stays a /128 without one.

The status shows the advertisement's managed and other flags.

### Testing the delegated prefix

`pd_addresses` puts hosts in the delegated prefix, as a CPE's LAN would
have: `{subnet: 1, host: 1}` is `::1` in the prefix's second /64 (in
`2001:db8:100::/56`, that's `2001:db8:100:1::1`). The device answers ping
and flows on them, so a ping to one from elsewhere tests that the router
installed a route to the prefix and forwards to the client.

### What to look at

| Status | |
|---|---|
| `state` | `selecting` or `soliciting`, `requesting`, `bound`, `renewing`, `rebinding`, `released` |
| `address`, `addresses`, `prefixes` | the leases, with the time left |
| `gateway`, `dns`, `domain`, `domains` | what the server (or, for the IPv6 gateway, the router) gave |
| `ia_na_status`, `ia_pd_status` | why an address or prefix wasn't given, from the server's status code (`NoPrefixAvail …`) |
| `bind_time_s` | from the first Discover or Solicit to the lease |
| `counters` | messages sent and received, by type |

Protocol lists show a client without a lease as a warning. A DHCPv6
client that got an address but not its prefix (or the other way round)
is a warning too, naming the server's status.

The actions are `renew`, `rebind`, `release` (and stay without a lease),
`restart` (release, then start over) and `clear`. A client releases its
lease when it stops, unless `release_on_stop: false`.

### Many clients

A [device group](device-groups.md) makes a dozen CPEs, each with its own
MAC (so its own client identifier and DUID):

```yaml
device_groups:
  - name: cpe
    count: 12
    device: {port: eth1, vlans: [{vid: 100}]}
    protocols:
      - type: dhcp
        config: {hostname: "cpe-{i}"}
      - type: dhcpv6
        config: {ia_pd: true, pd_length: 56, pd_addresses: [{subnet: 0, host: 1}]}
```

The group's view counts its members by state (`bound`, `waiting`,
`released`, and for DHCPv6 `no address` or `no prefix`), so it shows at a
glance whether every client got its address and prefix. Its member table
lists each member's leased addresses (tagged `dhcp`) and delegated prefix
(tagged `pd`), and its filter finds a member by either. `clabgenctl
groups` and `clabgenctl group show` print the same.

## Servers

A server runs on a device with a static address, usually deep in the
network behind the routers under test, with a gateway back toward them:

```yaml
devices:
  - {name: srv, port: eth1, addresses: [10.9.0.10/24, 2001:db8:9::10/64], gateway4: 10.9.0.1, gateway6: 2001:db8:9::1}
protocols:
  - name: srv4
    type: dhcp-server
    device: srv
    config:
      lease_time: 3600
      pools:
        - {subnet: 10.1.0.0/24, start: 10.1.0.100, end: 10.1.0.199, router: 10.1.0.1, dns: [10.9.0.53], domain: lab.example}
        - {subnet: 10.2.0.0/24, router: 10.2.0.1}
  - name: srv6
    type: dhcpv6-server
    device: srv
    config:
      preferred: 3600
      valid: 7200
      pools:
        - {link: 2001:db8:1::/64, start: 2001:db8:1::1000, end: 2001:db8:1::1fff,
           pd_prefix: 2001:db8:100::/40, pd_length: 56, dns: [2001:db8:9::53], domains: [lab.example]}
```

### Pools

There is a pool per client subnet (DHCPv4) or link (DHCPv6), chosen by
where the request came from:

- **Relayed**, DHCPv4: the subnet holding the relay's `giaddr`, or the
  address in a link-selection sub-option (option 82 sub-option 5, or
  option 118) when the relay sends one.
- **Relayed**, DHCPv6: the link holding the link-address of the relay
  nearest the client. A lightweight relay (LDRA, RFC 6221) sends `::`,
  so the next relay out names the link.
- **On the server's own link**: the pool holding the server device's
  address.

A request with no matching pool isn't answered (the `no_pool` counter,
and an event).

A DHCPv4 pool leases `start` to `end` (default: the whole subnet), never
its `router` or the server's own addresses. A client gets back the
address it had, else the one it asks for, else the first free one. A
DHCPv6 pool leases addresses from `start` to `end` and delegates prefixes
of `pd_length` from `pd_prefix`. Without one or the other, a request for
it gets `NoAddrsAvail` or `NoPrefixAvail`.

### Relays

Clients may be one or more relays away:

- DHCPv4 replies go to the first relay (`giaddr`), with the relay agent
  information (option 82) echoed as received. A client renews by unicast
  straight to the server; the answer goes back to its address.
- DHCPv6 replies go back through every relay: one Relay-reply per
  Relay-forward, each with its relay's Interface-ID echoed, sent to the
  relay the request came from.

The lease table shows how each lease's requests came: for DHCPv4 the
`giaddr`, hop count, circuit ID, remote ID, link selection and subscriber
ID; for DHCPv6 every relay hop with its link and peer addresses,
interface ID, remote ID, subscriber ID and client link-layer address. In
a test that's often the thing to check: did the relay insert the right
circuit ID?

### Actions

| Action | |
|---|---|
| `pause` | stop answering, for `seconds` or until `resume`: clients retry, then their leases run out |
| `resume` | answer again |
| `forget` | drop one lease (by address or prefix): the client's next renewal is refused |
| `forget-all` | drop every lease |
| `clear` | reset the counters |

Editing the pools applies in place: leases still inside a pool are kept.
