# Example labs

- [`b2b`](b2b): two clabgen agents back to back. CFM MEPs untagged, dot1q
  and QinQ (levels 3, 5 and 2), IP hosts on both sides, and flows between
  them. Needs only the clabgen image. [`test_b2b.py`](b2b/test_b2b.py) is a
  pytest suite for it ([Python and pytest](../docs/python.md)).
- [`srsim`](srsim): one clabgen agent against a Nokia SR-SIM router, the
  lab clabgen's interop is tested in: CFM and Y.1731 (with MIPs in a VPLS),
  LLDP, IS-IS (point-to-point, LAN, L1/L2, segment routing, flex-algo,
  SRv6, BFD), OSPFv2/v3 (areas, NSSA, segment routing), BGP (unicast,
  VPNv4/v6, EVPN, L2VPN, a group of eight CEs) flows routed by the SR
  and raw L2 streams across a VPLS.
- [`dhcp`](dhcp): DHCPv4 and DHCPv6 subscribers (one client and a group
  of ten, with prefix delegation) behind a Nokia SR-SIM relay, served by
  a clabgen DHCP server. Walked through in the docs'
  [DHCP subscribers](../docs/examples/dhcp.md) example.
- [`convergence`](convergence): what a link failure costs traffic across a
  Nokia SR-SIM, with and without BFD. Walked through in the docs'
  [Link failure convergence](../docs/examples/convergence.md) example.
- [`bgp-routes`](bgp-routes): one flow to 100 BGP routes across a Nokia
  SR-SIM, and which of them a failure hits. Walked through in the docs'
  [Traffic to BGP routes](../docs/examples/bgp-routes.md) example.
- [`ldp`](ldp): LDP with a Nokia SR-SIM, which pushes the labels it learns onto
  real traffic. Walked through in the docs'
  [LDP with traffic in the LSPs](../docs/examples/ldp.md) example.
- [`rsvp`](rsvp): RSVP-TE through Nokia SR-SIMs with clabgen at both ends, and
  what a link failure costs with fast reroute. Walked through in the docs'
  [RSVP-TE fast reroute](../docs/examples/rsvp.md) example.

Each binds its topology's folder into the clabgen nodes (`.:/clab`), so
what you change in a running lab is saved in `clab-<lab>/<node>/clabgen.yml`
and comes back after a restart; the scenario files (`gen*.yml`,
`client.yml`, `server.yml`) only seed a fresh lab
([saved config](../docs/configuration.md#saved-config)).
  The topology file lists every VLAN and what runs on it.

```sh
cd b2b && containerlab deploy -t b2b.clab.yml
```

SR-SIM needs its image and a license: add `license: <file>` to the `sr1`
node in `srsim/srsim.clab.yml` (or `dhcp/dhcp.clab.yml`, `convergence/convergence.clab.yml`, `bgp-routes/bgp-routes.clab.yml`, `ldp/ldp.clab.yml`, `rsvp/rsvp.clab.yml`) before deploying.
Each agent's scenario is in its own YAML file; `sr1.partial.txt` is the
SR's configuration.

To run a local build of clabgen instead of the latest release, set
`CLABGEN_IMAGE=ghcr.io/ripnet/clabgen:dev` (see
[development](../docs/development.md)).
