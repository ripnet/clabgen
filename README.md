# clabgen

A web UI for emulating protocols and generating traffic in
[containerlab](https://containerlab.dev) labs, aimed at control-plane
testing: build emulated routers, MEPs and traffic endpoints, watch their
state live, and break them on purpose. A small agent runs as a clab node,
takes over the node's data interfaces and emulates endpoints on them.

![Protocols on one agent](docs/images/protocols.png)

## Quick start

Add clabgen nodes to a topology:

```yaml
name: demo
topology:
  nodes:
    gen1: {kind: linux, image: ghcr.io/ripnet/clabgen}
    gen2: {kind: linux, image: ghcr.io/ripnet/clabgen}
  links:
    - endpoints: [gen1:eth1, gen2:eth1]
```

```sh
containerlab deploy -t demo.clab.yml
```

Start the UI once per lab host; it finds every clabgen node in every
running lab:

```sh
docker run -d --name clabgen-ui --network host --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock ghcr.io/ripnet/clabgen-ui
```

Open http://127.0.0.1:8090 (from a laptop: `ssh -L 8090:127.0.0.1:8090
labhost`). The agents start empty: add devices and protocols in the UI,
or [load a scenario file](docs/configuration.md) at startup. Two
complete labs are in [`examples/`](examples): two agents back to back
(`b2b`), and one against a Nokia SR-SIM router (`srsim`, which needs an
SR-SIM image and license).

## Highlights

- **Everything from the browser**: a lab-wide view of every agent, live
  protocol status, and a config editor generated from each protocol's
  schema, with the changes reviewed before they are applied.
- **Break things on purpose**: flap adjacencies, withdraw routes, stop
  CCMs, inject CFM faults, silence BFD, restart gracefully, disable a
  protocol (or 20 random members of a group) and watch the device under
  test react.
- **Scale**: device groups of thousands of peers with stepped VLANs and
  addresses, emulated IS-IS and OSPF networks behind a router, route
  injection by the thousand.
- **Measure and judge**: traffic flows with loss, outages and RTT, and
  pass/fail limits on them shown across the whole lab ("every flow stayed
  under 50 ms of outage"); one-way stats on every device; Y.1731 delay and
  loss sessions in bins.
- **Scriptable**: the same things through [`clabgenctl`](docs/cli.md), an
  [API](docs/api.md), and YAML scenarios.

## Features

- **Devices**: many emulated endpoints per port (MAC, VLAN or QinQ,
  IPv4/IPv6 answering ARP, ND and ping), and device groups.
- **ETH-CFM / Y.1731**: down MEPs with CCM and defects, loopback,
  linktrace, delay (DMM, 1DM), loss (SLM, LMM), test signal, AIS/LCK/CSF,
  proactive measurement sessions and fault injection.
- **IS-IS**: point-to-point and LAN, L1/L2, multi-topology, SR-MPLS,
  SRv6 and flex-algo, keychain authentication, BFD, graceful restart.
- **OSPFv2 / OSPFv3**: areas (stub, NSSA, virtual links), segment routing,
  BFD, graceful restart.
- **BGP**: IPv4/IPv6 unicast, VPNv4/v6, EVPN and L2VPN.
- **Emulated topologies** behind IS-IS and OSPF routers (grids, rings,
  custom graphs) with link and node failures.
- **LLDP**.
- **Traffic flows**: ICMP (ping or crafted packets) and UDP, one way or
  echoed.

Interop is verified against Nokia SR-SIM.

![A BGP speaker's peers and routes](docs/images/bgp.png)

## Intended use

clabgen is for lab and test networks. An agent takes over its node's
interfaces and sends real protocol traffic (BGP updates, IS-IS and OSPF
floods, CFM, traffic flows), and its API and UI have no authentication:
keep it on lab networks, away from production ones.

## Documentation

- [Web UI](docs/ui.md): the lab-wide UI and its options
- [Configuration](docs/configuration.md): scenario files, each protocol,
  device groups
- [CLI](docs/cli.md): `clabgenctl`
- [API](docs/api.md)
- [Development](docs/development.md): building, tests, images and releases
- [Writing a protocol](docs/writing-a-protocol.md)

## License

BSD 3-Clause; see [LICENSE](LICENSE). Copyright (c) 2026, Tom Young and
the clabgen contributors.
