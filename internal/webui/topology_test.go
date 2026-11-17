package webui

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const labData = `{"name":"lab1","nodes":{
  "core1":{"shortname":"core1","kind":"nokia_srsim","image":"nokia_srsim:26.10.R1","labels":{"graph-posX":"100","graph-posY":"40.5"}},
  "client":{"shortname":"client","kind":"linux","image":"ghcr.io/ripnet/clabgen","group":"gen","mgmt-ipv4-address":"172.20.20.3"}},
 "links":[
  {"endpoints":{"a":{"node":"core1","interface":"e1-1-c1-1","mac":"aa:c1:ab:00:00:01","peer":"z"},"z":{"node":"client","interface":"eth1","peer":"a"}}},
  {"a":{"node":"core1","interface":"e1-1-c2-1"},"z":{"node":"client","interface":"eth2"}},
  {"endpoints":{"a":{"node":"client","interface":"eth3"},"z":{"node":"host","interface":"gen-eth3"}}}]}`

func TestParseTopologyData(t *testing.T) {
	tp, err := parseTopologyData([]byte(labData))
	if err != nil {
		t.Fatal(err)
	}
	if tp.Lab != "lab1" || len(tp.Nodes) != 2 || tp.Nodes[0].Name != "client" || tp.Nodes[1].Name != "core1" {
		t.Fatalf("%+v", tp)
	}
	if c := tp.Nodes[1]; c.X == nil || *c.X != 100 || *c.Y != 40.5 || c.Kind != "nokia_srsim" {
		t.Errorf("core1: %+v", c)
	}
	if tp.Nodes[0].X != nil || tp.Nodes[0].Group != "gen" || tp.Nodes[0].Mgmt != "172.20.20.3" {
		t.Errorf("client: %+v", tp.Nodes[0])
	}
	want := []TopoLink{{"core1", "e1-1-c1-1", "client", "eth1"}, {"core1", "e1-1-c2-1", "client", "eth2"}, {"client", "eth3", "host", "gen-eth3"}}
	if len(tp.Links) != 3 {
		t.Fatalf("links %+v", tp.Links)
	}
	for i, l := range want {
		if tp.Links[i] != l {
			t.Errorf("link %d: %+v, want %+v", i, tp.Links[i], l)
		}
	}
}

func TestTopologyPathIn(t *testing.T) {
	ct := func(nodeDir, src, dst string) dockerInspect {
		var c dockerInspect
		c.Config.Labels = map[string]string{"clab-node-lab-dir": nodeDir}
		c.Mounts = append(c.Mounts, struct {
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		}{src, dst})
		return c
	}
	for _, c := range []struct {
		ct   dockerInspect
		want string
	}{
		{ct("/opt/labs/mylab/clab-lab1/client", "/opt/labs/mylab", "/clab"), "/clab/clab-lab1/topology-data.json"},
		{ct("/opt/labs/mylab/clab-lab1/client", "/opt/labs/mylab/", "/clab/"), "/clab/clab-lab1/topology-data.json"},
		// The lab folder elsewhere (CLAB_LABDIR_BASE) but still below the mount.
		{ct("/opt/labs/mylab/run/clab-lab1/client", "/opt/labs/mylab", "/clab"), "/clab/run/clab-lab1/topology-data.json"},
		// Not below the mount, not at /clab, no label: no path.
		{ct("/var/clab/clab-lab1/client", "/opt/labs/mylab", "/clab"), ""},
		{ct("/opt/labs/mylab/clab-lab1/client", "/opt/labs/mylab", "/data"), ""},
		{ct("", "/opt/labs/mylab", "/clab"), ""},
	} {
		got, _ := topologyPathIn(c.ct)
		if got != c.want {
			t.Errorf("%+v: %q, want %q", c.ct, got, c.want)
		}
	}
}

// fakeDocker serves the Engine API calls topology reading makes: a lab's
// containers, their inspect output, and a file copied out as a tar.
func fakeDocker(t *testing.T, files map[string]string, containers map[string]string) string {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("filters"), `containerlab=lab1`) {
			w.Write([]byte(`[]`))
			return
		}
		var list []map[string]any
		for id := range containers {
			list = append(list, map[string]any{"Id": id, "Names": []string{"/clab-lab1-" + id}})
		}
		json.NewEncoder(w).Encode(list)
	})
	for id, inspect := range containers {
		mux.HandleFunc("/containers/"+id+"/json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(inspect)) })
		mux.HandleFunc("/containers/"+id+"/archive", func(w http.ResponseWriter, r *http.Request) {
			body, ok := files[id+":"+r.URL.Query().Get("path")]
			if !ok {
				http.Error(w, `{"message":"no such file"}`, http.StatusNotFound)
				return
			}
			var b bytes.Buffer
			tw := tar.NewWriter(&b)
			tw.WriteHeader(&tar.Header{Name: "topology-data.json", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
			tw.Close()
			w.Write(b.Bytes())
		})
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func inspect(name, image, nodeDir, mountSrc string) string {
	mounts := "[]"
	if mountSrc != "" {
		mounts = `[{"Source":"` + mountSrc + `","Destination":"/clab"}]`
	}
	return `{"Name":"/clab-lab1-` + name + `","Config":{"Image":"` + image + `","Labels":{"containerlab":"lab1","clab-node-name":"` + name +
		`","clab-node-kind":"linux","clab-node-lab-dir":"` + nodeDir + `"}},"Mounts":` + mounts + `}`
}

func TestDockerTopology(t *testing.T) {
	re := regexp.MustCompile("clabgen")
	// a-gen has no mount, b-gen's file is another lab's, c-gen has it.
	sock := fakeDocker(t, map[string]string{
		"b-gen:/clab/clab-lab1/topology-data.json": `{"name":"other","nodes":{},"links":[]}`,
		"c-gen:/clab/clab-lab1/topology-data.json": labData,
	}, map[string]string{
		"a-gen": inspect("a-gen", "ghcr.io/ripnet/clabgen", "/labs/x/clab-lab1/a-gen", ""),
		"b-gen": inspect("b-gen", "ghcr.io/ripnet/clabgen", "/labs/x/clab-lab1/b-gen", "/labs/x"),
		"c-gen": inspect("c-gen", "ghcr.io/ripnet/clabgen:dev", "/labs/x/clab-lab1/c-gen", "/labs/x"),
		"r1":    inspect("r1", "nokia_srsim:26.10.R1", "/labs/x/clab-lab1/r1", "/labs/x"),
	})
	tp, err := Topologies(sock, re)(context.Background(), "lab1")
	if err != nil {
		t.Fatal(err)
	}
	if tp.From != "clab-lab1-c-gen:/clab/clab-lab1/topology-data.json" || len(tp.Links) != 3 || tp.Note != "" {
		t.Fatalf("%+v", tp)
	}
	if _, err := Topologies(sock, re)(context.Background(), "nope"); err == nil {
		t.Error("a lab with no containers: no error")
	}

	// Nobody has the file: the nodes, no links, and why.
	sock = fakeDocker(t, nil, map[string]string{
		"a-gen": inspect("a-gen", "ghcr.io/ripnet/clabgen", "/labs/x/clab-lab1/a-gen", ""),
		"r1":    inspect("r1", "nokia_srsim:26.10.R1", "/labs/x/clab-lab1/r1", ""),
	})
	tp, err = Topologies(sock, re)(context.Background(), "lab1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tp.Nodes) != 2 || tp.Nodes[0].Name != "a-gen" || tp.Nodes[1].Kind != "linux" || len(tp.Links) != 0 || !strings.Contains(tp.Note, "binds: [.:/clab]") {
		t.Fatalf("%+v", tp)
	}
}

func TestTopologyRoute(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "clab-lab1"), 0o755)
	data := strings.Replace(labData, `"group":"gen",`, `"group":"gen","longname":"clab-lab1-client",`, 1)
	os.WriteFile(filepath.Join(dir, "clab-lab1", "topology-data.json"), []byte(data), 0o644)
	reg := NewRegistry()
	agents, err := labAgents(filepath.Join(dir, "clab-lab1"), regexp.MustCompile("clabgen"))
	if err != nil {
		t.Fatal(err)
	}
	reg.Replace("lab", agents)

	srv := httptest.NewServer(Handler(reg, Options{Topology: Topologies("", regexp.MustCompile("clabgen"))}))
	defer srv.Close()
	var in Info
	r, _ := http.Get(srv.URL + "/api/info")
	json.NewDecoder(r.Body).Decode(&in)
	if !in.Topology {
		t.Error("info: topology not offered")
	}
	r, err = http.Get(srv.URL + "/api/labs/lab1/topology")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("%v %v", r.Status, err)
	}
	var tp Topology
	json.NewDecoder(r.Body).Decode(&tp)
	if tp.Nodes[0].Name != "client" || tp.Nodes[0].Agent != "clab-lab1-client" || tp.Nodes[1].Agent != "" || !strings.HasSuffix(tp.From, "topology-data.json") {
		t.Errorf("%+v", tp)
	}
	if r, _ := http.Get(srv.URL + "/api/labs/nope/topology"); r.StatusCode != http.StatusBadGateway {
		t.Errorf("unknown lab: %s", r.Status)
	}

	// Without a source the route says so.
	srv2 := httptest.NewServer(Handler(reg, Options{}))
	defer srv2.Close()
	if r, _ := http.Get(srv2.URL + "/api/labs/lab1/topology"); r.StatusCode != http.StatusNotFound {
		t.Errorf("no topology source: %s", r.Status)
	}
}
