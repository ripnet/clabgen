# Example labs

- [`b2b`](b2b): two clabgen agents back to back. CFM MEPs untagged, dot1q
  and QinQ (levels 3, 5 and 2), IP hosts on both sides, and flows between
  them. Needs only the clabgen image.
- [`srsim`](srsim): one clabgen agent against a Nokia SR-SIM router, the
  lab clabgen's interop is tested in: CFM and Y.1731 (with MIPs in a VPLS),
  LLDP, IS-IS (point-to-point, LAN, L1/L2, segment routing, flex-algo,
  SRv6, BFD), OSPFv2/v3 (areas, NSSA, segment routing), BGP (unicast,
  VPNv4/v6, EVPN, L2VPN, a group of eight CEs) flows routed by the SR
  and raw L2 streams across a VPLS.
  The topology file lists every VLAN and what runs on it.

```sh
cd b2b && containerlab deploy -t b2b.clab.yml
```

SR-SIM needs its image and a license: add `license: <file>` to the `sr1`
node in `srsim/srsim.clab.yml` before deploying. Each agent's scenario is
in its `gen*.yml`; `sr1.partial.txt` is the SR's configuration.

To run a local build of clabgen instead of the latest release, set
`CLABGEN_IMAGE=ghcr.io/ripnet/clabgen:dev` (see
[development](../docs/development.md)).
