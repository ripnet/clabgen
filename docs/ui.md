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
-update-check=false         don't ask GitHub for newer releases
```

## Versions

The header shows the version serving the UI. A release build of
`clabgen-ui` asks GitHub for the latest release every 6 hours (nothing
else is sent) and, when there is a newer one, links to it next to the
version; `-update-check=false` turns that off, for labs without internet
or that shouldn't call out. To update, `docker pull
ghcr.io/ripnet/clabgen-ui` and recreate the container.

Agents can run a different version than the UI (a lab deployed before an
update). An agent on an older release gets an **update** marker in the
left nav, and the overview says how many there are: a newer UI may use
things such an agent doesn't have yet. Redeploy the lab with the newer
image (`docker pull ghcr.io/ripnet/clabgen` first) to update it. An agent
newer than the UI is marked **newer**. Development builds aren't
compared; the agent's page shows both versions.

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
- Flows: every agent's traffic flows in one table, with their pass/fail
  checks, counts of passing and failing flows, a "failing only" filter,
  and start, stop and clear for all of them (or the ones the filter
  shows).
- A flow page: live loss, outage and RTT tiles, its checks, a per-second
  timeline and a per-packet log.
- Capture: the frames an agent's ports receive and send, live, with a
  one-line decode each (filter by port, device, direction or ethertype).
  Click a frame for its layers and bytes, "Build a flow from it" to send
  it again from the packet builder, "Download .pcapng" for Wireshark, or
  copy the `curl … | wireshark -k -i -` line to watch the same capture
  live in Wireshark.
- The packet editor (packet flows): the layers as cards to add, reorder,
  edit and remove, or a template to start from, and a live preview from
  the device: the frame as hex, coloured by layer, with each field's
  value. Hovering a field shows its bytes; "+ modifier" changes it per
  packet; the arrows step through packets to see the modifiers work.
  "Import pcap…" lists a capture's frames: replay them all, or open one as
  layers to edit.

Forms come from each protocol's schema, so new protocol types appear
without UI changes.
