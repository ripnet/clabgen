package isis

import (
	"strings"
	"testing"
)

func TestBuildTopology(t *testing.T) {
	g, err := buildTopology(Topology{Kind: "grid", Rows: 3, Cols: 4, LoopbackBase: "10.255.0.1/32", PrefixBase: "10.200.0.0/24", PrefixesPerNode: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := g.status()
	// 3×4 grid: 3·3 + 2·4 = 17 links, plus the attachment to us.
	if st.Nodes != 12 || st.Links != 18 || len(st.Attached) != 1 || st.Attached[0] != "r1-1" || st.Prefixes != 36 {
		t.Fatalf("%+v", st)
	}
	n := g.byName["r2-2"]
	if len(n.links) != 4 || n.sys.String() != "0000.1000.0006" || n.loopback.String() != "10.255.0.6/32" || n.prefixes[0].Prefix.String() != "10.200.10.0/24" {
		t.Fatalf("r2-2: %+v", n)
	}
	r, err := buildTopology(Topology{Kind: "ring", Nodes: 5, Attach: []string{"r1", "r3"}}, nil)
	if err != nil || r.status().Links != 7 || len(r.byName["r1"].links) != 3 {
		t.Fatalf("ring %+v %v", r.status(), err)
	}
	f, err := buildTopology(Topology{Kind: "full", Nodes: 4}, nil)
	if err != nil || f.status().Links != 7 {
		t.Fatalf("full %v", err)
	}
	c, err := buildTopology(Topology{Kind: "custom", Custom: []SimNode{
		{Name: "pe1", Links: []SimLink{{To: "us"}, {To: "pe2", Metric: 50}}, Prefixes: []Prefix{{Prefix: "192.0.2.0/24"}}},
		{Name: "pe2", SystemID: "0000.0000.9999"},
	}}, nil)
	if err != nil || c.byName["pe2"].links["pe1"] != 50 || c.byName["pe2"].sys.String() != "0000.0000.9999" || len(c.byName["pe1"].prefixes) != 1 {
		t.Fatalf("custom %v", err)
	}
	us := SysID{0, 0, 0x10, 0, 0, 2}
	for _, bad := range []struct {
		c    Topology
		us   *SysID
		want string
	}{
		{Topology{Kind: "grid", Rows: 2}, nil, "rows and cols"},
		{Topology{Kind: "ring", Nodes: 3, Attach: []string{"r9"}}, nil, "unknown node"},
		{Topology{Kind: "custom", Custom: []SimNode{{Name: "a", Links: []SimLink{{To: "b"}}}}}, nil, "unknown node"},
		{Topology{Kind: "custom", Custom: []SimNode{{Name: "a"}, {Name: "a"}}}, nil, "twice"},
		{Topology{Kind: "line", Nodes: 3}, &us, "already used"},
		{Topology{Kind: "line", Nodes: 3, PrefixesPerNode: 2}, nil, "prefix_base"},
		{Topology{Kind: "grid", Rows: 200, Cols: 200}, nil, "at most"},
	} {
		if _, err := buildTopology(bad.c, bad.us); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%+v: %v (want %q)", bad.c, err, bad.want)
		}
	}
}
