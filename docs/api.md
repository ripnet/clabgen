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

Generated clients exist for Go (`gen/clabgen/v1`) and TypeScript
(`web/src/gen`), and [Python](python.md) has a hand-written one; `buf generate` or `protoc` with any gRPC or Connect
plugin produces others.
