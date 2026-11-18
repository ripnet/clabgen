# API

Each agent serves `AgentService`
([`api/proto/clabgen/v1/agent.proto`](../api/proto/clabgen/v1/agent.proto))
on port 8080 over Connect, gRPC and gRPC-Web (and `GET /healthz`).
`clabgenctl` uses nothing else, and the UI only adds the UI server's few
[routes](#the-ui-servers-routes), so anything they do can be scripted.

## Calling it

With Connect's JSON encoding, plain curl works:

```sh
curl -s -H 'Content-Type: application/json' -d '{"withStatus":true}' \
  http://clab-b2b-gen1:8080/clabgen.v1.AgentService/ListProtocols
```

A `clabgen-ui` passes calls through to the agents it knows, so from a
machine that can only reach the UI:

```sh
curl -s http://labhost:8090/api/agents        # the agents, with their ids
curl -s -H 'Content-Type: application/json' -d '{}' \
  http://labhost:8090/agents/clab-b2b-gen1/clabgen.v1.AgentService/GetInfo
```

`ListProtocolTypes` returns each type's config and action schemas (JSON
schema), and `scenario_schema`, the schema of a whole scenario file: a
protocol's `config` takes the schema of its `type` (marked
`x-config-of`). The UI's forms and YAML editor are built from them.

## The UI server's routes

`clabgen-ui` adds a few JSON routes of its own:

- `GET /api/info`: its version, a newer release if any, Edgeshark, and
  whether lab topologies are available.
- `GET /api/agents`: the agents it knows, with their ids. `POST
  /api/agents` adds one by address and `DELETE /api/agents/{id}` removes
  one added that way (where adding is allowed).
- `GET /api/labs/{lab}/topology`: the lab's nodes and links from
  containerlab's `topology-data.json` (`nodes` with `name`, `kind`,
  `image` and `agent`; `links` with `a`, `a_if`, `z`, `z_if`; a `note`
  when the links can't be read).
- `/agents/{id}/…`: passed through to that agent.

## Packet captures over HTTP

Captures also come as a live pcapng stream over plain HTTP, from an agent
or through `clabgen-ui` (`/agents/<id>/capture.pcapng`):

```sh
curl -sSfN 'http://clab-b2b-gen1:8080/capture.pcapng?port=eth1' | wireshark -k -i -
curl -sSfN 'http://host:8090/agents/clab-b2b-gen1/capture.pcapng?device=v100&seconds=10' -o v100.pcapng
```

The parameters are all optional:

- `port` (repeatable; default every port), `device`, `direction` (`rx` or
  `tx`) and `ethertype` (repeatable, e.g. `0x0800`) filter the frames.
- `snaplen` cuts each frame.
- `count` and `seconds` stop it. Without them, it runs until the client
  goes away.

If `wireshark -k -i -` isn't allowed to capture on your machine,
`tshark -r -` reads the stream too.

## Clients

- Go: generated, in `gen/clabgen/v1`.
- TypeScript: generated, in `web/src/gen`.
- [Python](python.md): a hand-written package, with pytest fixtures.

`buf generate` or `protoc`, with any gRPC or Connect plugin, produces
others.
