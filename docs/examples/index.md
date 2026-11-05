# Examples

Worked labs, each built around a real test: what it shows, the
topology, clabgen's scenarios and the router's configuration, and what
to look at once it's running. Every lab is in the repository's
[`examples`](../../examples) folder, ready to deploy.

| Example | |
|---|---|
| [DHCP subscribers](dhcp.md) | DHCPv4 and DHCPv6 (with prefix delegation) clients behind a Nokia SR-SIM relay, served by a clabgen server; one client, then a group of ten |
| [Link failure convergence](convergence.md) | What a link failure costs traffic across a Nokia SR-SIM, with and without BFD, measured side by side |
| [Traffic to BGP routes](bgp-routes.md) | One flow to 100 routes across a Nokia SR-SIM, and a convergence test naming exactly the routes a failure hit |
| [LDP with traffic in the LSPs](ldp.md) | A Nokia SR-SIM pushes labels learned from clabgen onto real traffic, and falls back to IP when they're withdrawn |
| [RSVP-TE fast reroute](rsvp.md) | clabgen signals an LSP through Nokia SR-SIMs, sends into it, and measures what a link failure costs with and without a bypass |
| [L3VPN traffic with learned labels](l3vpn.md) | clabgen is the far PE of a VPN through Nokia SR-SIMs: flows take their labels from the routes received, and answers go back through the VPN |
