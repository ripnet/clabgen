package icmp

import (
	"strings"
	"testing"

	"github.com/ripnet/clabgen/internal/protocol"
)

func TestSchemaOrder(t *testing.T) {
	s := protocol.Schema(defaultConfig())
	want := `"x-order":["destination","mode","count","duration","rate","timeout","auto_start","source","dst_mac","vlan_pcp",` +
		`"ttl","dscp","ecn","df","ip_id","flow_label","ip_checksum","icmp_type","icmp_code","identifier","seq_start","seq_mode","icmp_checksum",` +
		`"payload_size","payload_pattern","payload_data","signature"]`
	if !strings.Contains(s, want) {
		t.Fatalf("schema order:\n%s", s)
	}
	if !strings.Contains(s, `"default":64`) {
		t.Fatal("defaults missing")
	}
}
