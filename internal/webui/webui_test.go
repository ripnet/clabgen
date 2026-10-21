package webui

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestProxyAndRegistry(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "remote:"+r.URL.Path)
	}))
	defer backend.Close()

	reg := NewRegistry()
	reg.AddLocal("self", "me", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "local:"+r.URL.Path)
	}))
	if err := reg.Replace("docker", []Agent{{ID: "lab-gen1", Name: "gen1", Lab: "lab", URL: backend.URL}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(reg, Options{AllowManual: true}))
	defer srv.Close()

	get := func(p string) (int, string) {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if _, b := get("/agents/self/clabgen.v1.AgentService/GetInfo"); b != "local:/clabgen.v1.AgentService/GetInfo" {
		t.Errorf("local routing: %q", b)
	}
	if _, b := get("/agents/lab-gen1/clabgen.v1.AgentService/ListPorts"); b != "remote:/clabgen.v1.AgentService/ListPorts" {
		t.Errorf("proxy routing: %q", b)
	}
	if code, _ := get("/agents/nope/x"); code != 404 {
		t.Errorf("unknown agent: %d", code)
	}
	if _, b := get("/api/agents"); !strings.Contains(b, `"lab-gen1"`) || !strings.Contains(b, `"self"`) {
		t.Errorf("agent list: %s", b)
	}

	// Replacing a source drops its stale agents but not other sources'.
	reg.Replace("docker", nil)
	if reg.Get("lab-gen1") != nil || reg.Get("self") == nil {
		t.Error("replace semantics")
	}

	resp, _ := http.Post(srv.URL+"/api/agents", "application/json", strings.NewReader(`{"id":"x1","url":"10.0.0.1"}`))
	if resp.StatusCode != 200 || reg.Get("x1") == nil || reg.Get("x1").URL != "http://10.0.0.1:8080" {
		t.Errorf("manual add: %d %+v", resp.StatusCode, reg.Get("x1"))
	}
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/agents/self", nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 403 {
		t.Errorf("self must not be removable: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("DELETE", srv.URL+"/api/agents/x1", nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 204 || reg.Get("x1") != nil {
		t.Errorf("manual delete: %d", resp.StatusCode)
	}

	// Client-side routes fall back to the SPA (or the not-built page).
	if code, _ := get("/a/self/ports"); code != 200 {
		t.Errorf("spa fallback: %d", code)
	}
}

func TestLabDiscovery(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.clab.yml"), []byte("name: demo\ntopology: {}\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "clab-demo"), 0o755)
	os.WriteFile(filepath.Join(dir, "clab-demo", "topology-data.json"), []byte(`{"name":"demo","nodes":{
	  "gen1":{"shortname":"gen1","longname":"clab-demo-gen1","image":"ghcr.io/ripnet/clabgen:dev","mgmt-ipv4-address":"172.20.20.3"},
	  "sr1":{"shortname":"sr1","longname":"clab-demo-sr1","image":"nokia_srsim:26.7.R1","mgmt-ipv4-address":"172.20.20.4"}}}`), 0o644)
	agents, err := labAgents(filepath.Join(dir, "x.clab.yml"), regexp.MustCompile("clabgen"))
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].ID != "clab-demo-gen1" || agents[0].Lab != "demo" || agents[0].URL != "172.20.20.3:8080" {
		t.Errorf("%+v", agents)
	}
}

func TestDockerAgents(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("filters"), "containerlab") {
			t.Errorf("missing label filter: %q", r.URL.RawQuery)
		}
		w.Write([]byte(`[{"Id":"a1"},{"Id":"b2"},{"Id":"gone"}]`))
	})
	mux.HandleFunc("/containers/a1/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Name":"/clab-lab-gen1","Config":{"Image":"ghcr.io/ripnet/clabgen:dev",
			"Labels":{"containerlab":"lab","clab-node-name":"gen1","clab-node-longname":"clab-lab-gen1","clab-mgmt-net-bridge":"br-1"}},
			"NetworkSettings":{"Networks":{"other":{"IPAddress":"10.0.0.9"},"clab":{"IPAddress":"172.20.20.2"}}}}`))
	})
	mux.HandleFunc("/containers/b2/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Name":"/clab-lab-sr1","Config":{"Image":"nokia_srsim:26.7.R1","Labels":{"containerlab":"lab"}},
			"NetworkSettings":{"Networks":{"clab":{"IPAddress":"172.20.20.3"}}}}`))
	})
	mux.HandleFunc("/containers/gone/json", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	defer srv.Close()

	got, err := dockerAgents(context.Background(), dockerClient(sock), regexp.MustCompile("clabgen"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Agent{{ID: "clab-lab-gen1", Name: "gen1", Lab: "lab", URL: "172.20.20.2:8080"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
