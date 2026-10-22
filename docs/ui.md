# Web UI

## The lab-wide UI

One `clabgen-ui` container per lab host shows every running containerlab
lab on the machine, much like Edgeshark. It finds containerlab nodes
running a clabgen image through the Docker socket and proxies the
browser's calls to them:

```sh
docker run -d --name clabgen-ui --network host --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock ghcr.io/ripnet/clabgen-ui
```

Then open http://127.0.0.1:8090. Host networking lets it reach every
lab's management bridge. It listens on localhost only, since the API has
no authentication; from a laptop, tunnel the one port with
`ssh -L 8090:127.0.0.1:8090 labhost` and open http://localhost:8090. To
listen on all interfaces instead, add `-listen 0.0.0.0:8090` after the
image name.

Other options, after the image name (or on the `clabgen-ui` binary that
ships in the agent image):

```
-listen addr     listen address (default 127.0.0.1:8090)
-lab file        a containerlab topology file or lab directory to find agents in (repeatable)
-agent [name=]host[:port]   an agent to add by address (repeatable)
-docker auto|on|off         discovery through the Docker socket (default auto)
-image regexp    container images that count as agents (default "clabgen")
```

## Each agent's own UI

Every agent also serves the UI for itself on its API port:
`http://<agent-mgmt-ip>:8080/`. Other agents can be added there by
address.

## What's in it

- A lab overview: every protocol on every agent, worst first.
- Per agent: ports, devices (with IP neighbors and a ping shortcut), device
  groups, protocols (with on/off switches and enable/disable all),
  traffic flows, live events, and the scenario as YAML.
- A protocol page: live status, the actions with their forms (loopback,
  flap, fault injection, graceful restart, …), recent events, and the
  config editor: sections per part of the config, field search, the
  changes reviewed before they are applied, and an agent's rejection shown
  on the fields it names.
- A flow page: live loss, outage and RTT tiles, a per-second timeline and
  a per-packet log.

Forms come from each protocol's schema, so new protocol types appear
without UI changes.
