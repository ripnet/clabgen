# Upgrading

Each release (`vX.Y.Z`) publishes the agent image `ghcr.io/ripnet/clabgen`
and the UI image `ghcr.io/ripnet/clabgen-ui` as `:X.Y.Z`, `:X.Y` and
`:latest`, along with `clabgenctl` and the Python package. The UI's header
links to a newer release when there is one, and marks agents running an
older release than the UI with **update** (see [Versions](ui.md#versions)).

Upgrade the UI and the agents together: a newer UI works with older
agents, but what they don't have yet (a newer API call, say) isn't
available on them until they are upgraded.

## The UI

The UI holds no state, so upgrading it means pulling the new image and
recreating the container with the same options as before (labs keep
running):

```sh
docker pull ghcr.io/ripnet/clabgen-ui:latest
docker rm -f clabgen-ui
docker run -d --name clabgen-ui --network host --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock ghcr.io/ripnet/clabgen-ui
```

Add back any options the old container had after the image name, e.g.
`-listen 0.0.0.0:8090` (`docker inspect clabgen-ui` shows them before you
remove it).

## Agents

Agents are containerlab nodes, so they get a new image when their lab is
deployed again. containerlab only pulls an image it doesn't have yet, so
pull the new one first:

```sh
docker pull ghcr.io/ripnet/clabgen:latest
containerlab redeploy -t mylab.clab.yml
```

- `redeploy` recreates every node of the lab, routers included, which
  boot again. containerlab (0.82) can't recreate only some nodes of a
  running lab.
- What you built is kept: with the topology folder bound at `/clab`, the
  agents load their [saved config](configuration.md#saved-config) again.
  Don't add `--cleanup`, which deletes the lab folder and the saved config
  with it.
- A topology that pins a version (`image: ghcr.io/ripnet/clabgen:0.8.0`)
  upgrades by changing the tag and redeploying; containerlab pulls a tag
  it doesn't have.

To check, `clabgenctl --lab mylab nodes` lists each agent's version, and
the UI's **update** markers go away.

## clabgenctl and Python

- `clabgenctl`: download it again, as when [getting it](cli.md#getting-it)
  (or `go install …@latest`). The Wireshark plugin is a link to
  `clabgenctl`, so it updates with it.
- Python: `pip install -U clabgen`.
