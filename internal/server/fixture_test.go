package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/ripnet/clabgen/gen/clabgen/v1"
	_ "github.com/ripnet/clabgen/protocols/bgp"
	_ "github.com/ripnet/clabgen/protocols/cfm"
	_ "github.com/ripnet/clabgen/protocols/dhcp"
	_ "github.com/ripnet/clabgen/protocols/icmp"
	_ "github.com/ripnet/clabgen/protocols/isis"
	_ "github.com/ripnet/clabgen/protocols/l2stream"
	_ "github.com/ripnet/clabgen/protocols/lldp"
	_ "github.com/ripnet/clabgen/protocols/ospf"
	_ "github.com/ripnet/clabgen/protocols/packet"
	_ "github.com/ripnet/clabgen/protocols/udp"
	_ "github.com/ripnet/clabgen/protocols/udpecho"
)

// The web UI's browser tests (web/e2e) mock the agent with the protocol
// types it really publishes; this keeps their fixture current. Regenerate
// with CLABGEN_WRITE_FIXTURES=1 go test ./internal/server.
const typesFixture = "../../web/e2e/fixtures/protocol-types.json"

func TestUITypesFixture(t *testing.T) {
	r, err := (&Server{}).ListProtocolTypes(context.Background(), connect.NewRequest(&v1.ListProtocolTypesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := protojson.Marshal(r.Msg)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	json.Indent(&out, b, "", " ")
	out.WriteByte('\n')
	if os.Getenv("CLABGEN_WRITE_FIXTURES") != "" {
		if err := os.WriteFile(typesFixture, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	cur, err := os.ReadFile(typesFixture)
	if err != nil || !bytes.Equal(cur, out.Bytes()) {
		t.Fatalf("%s is stale: run CLABGEN_WRITE_FIXTURES=1 go test ./internal/server", typesFixture)
	}
}
