# API

Each agent serves `AgentService`
([`api/proto/clabgen/v1/agent.proto`](../api/proto/clabgen/v1/agent.proto))
on port 8080 over Connect, gRPC and gRPC-Web. The UI and `clabgenctl` use
nothing else, so anything they do can be scripted. With Connect's JSON
encoding, plain curl works:

```sh
curl -s -H 'Content-Type: application/json' -d '{"withStatus":true}' \
  http://clab-b2b-gen1:8080/clabgen.v1.AgentService/ListProtocols
```

Packet captures also come as a live pcapng stream over plain HTTP, from
an agent or through clabgen-ui (`/agents/<id>/capture.pcapng`):

```sh
curl -sSfN 'http://clab-b2b-gen1:8080/capture.pcapng?port=eth1' | wireshark -k -i -
curl -sSfN 'http://host:8090/agents/clab-b2b-gen1/capture.pcapng?device=v100&seconds=10' -o v100.pcapng
```

Parameters (all optional): `port` (repeatable; default every port),
`device`, `direction` (`rx`/`tx`), `ethertype` (repeatable, e.g.
`0x0800`), `snaplen`, `count`, `seconds`. Without `count` or `seconds` it
runs until the client goes away. (If `wireshark -k -i -` isn't allowed to
capture on your machine, `tshark -r -` reads the stream too.)

Generated clients exist for Go (`gen/clabgen/v1`) and TypeScript
(`web/src/gen`), and [Python](python.md) has a hand-written one; `buf generate` or `protoc` with any gRPC or Connect
plugin produces others.
