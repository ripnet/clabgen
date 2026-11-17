package webui

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Topology is a lab's nodes and links, from containerlab's
// topology-data.json, for the UI's lab view.
type Topology struct {
	Lab   string     `json:"lab"`
	Nodes []TopoNode `json:"nodes"`
	Links []TopoLink `json:"links"`
	// Why the links are missing, when they are.
	Note string `json:"note,omitempty"`
	// Where the data came from (a file, or a container's copy of it).
	From string `json:"from,omitempty"`
}

type TopoNode struct {
	Name  string `json:"name"`
	Kind  string `json:"kind,omitempty"`
	Image string `json:"image,omitempty"`
	Group string `json:"group,omitempty"`
	Mgmt  string `json:"mgmt_ipv4,omitempty"`
	// The UI's agent ID when this node is a clabgen agent.
	Agent string `json:"agent,omitempty"`
	// containerlab's graph-posX/graph-posY labels, when set.
	X *float64 `json:"x,omitempty"`
	Y *float64 `json:"y,omitempty"`
}

type TopoLink struct {
	A   string `json:"a"`
	AIf string `json:"a_if"`
	Z   string `json:"z"`
	ZIf string `json:"z_if"`
}

// TopologyFunc returns a lab's topology.
type TopologyFunc func(ctx context.Context, lab string) (*Topology, error)

// labFiles maps lab names to the topology-data.json files that -lab
// discovery read, so their topology is read from the same file.
var labFiles sync.Map // lab name → path

// Topologies returns the topology of a lab: from the file -lab discovery
// read, else (with a Docker socket) copied out of one of the lab's
// clabgen containers: they mount the topology's folder at /clab, and
// containerlab keeps topology-data.json in the lab folder below it.
func Topologies(socket string, imageRE *regexp.Regexp) TopologyFunc {
	var hc *http.Client
	if socket != "" {
		hc = dockerClient(socket)
	}
	return func(ctx context.Context, lab string) (*Topology, error) {
		if p, ok := labFiles.Load(lab); ok {
			b, err := os.ReadFile(p.(string))
			if err != nil {
				return nil, err
			}
			t, err := parseTopologyData(b)
			if err != nil {
				return nil, err
			}
			t.From = p.(string)
			return t, nil
		}
		if hc == nil {
			return nil, fmt.Errorf("lab %s: not found", lab)
		}
		return dockerTopology(ctx, hc, imageRE, lab)
	}
}

// topoData is the part of containerlab's topology-data.json used here.
// Links are {endpoints: {a, z}} (containerlab 0.4x and later) or {a, z}.
type topoFull struct {
	Name  string `json:"name"`
	Nodes map[string]struct {
		ShortName string            `json:"shortname"`
		Kind      string            `json:"kind"`
		Image     string            `json:"image"`
		Group     string            `json:"group"`
		MgmtIPv4  string            `json:"mgmt-ipv4-address"`
		Labels    map[string]string `json:"labels"`
	} `json:"nodes"`
	Links []struct {
		Endpoints *struct {
			A topoEnd `json:"a"`
			Z topoEnd `json:"z"`
		} `json:"endpoints"`
		A *topoEnd `json:"a"`
		Z *topoEnd `json:"z"`
	} `json:"links"`
}

type topoEnd struct {
	Node      string `json:"node"`
	Interface string `json:"interface"`
}

func parseTopologyData(b []byte) (*Topology, error) {
	var d topoFull
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("topology-data.json: %w", err)
	}
	t := &Topology{Lab: d.Name, Nodes: []TopoNode{}, Links: []TopoLink{}}
	for key, n := range d.Nodes {
		name := n.ShortName
		if name == "" {
			name = key
		}
		tn := TopoNode{Name: name, Kind: n.Kind, Image: n.Image, Group: n.Group, Mgmt: n.MgmtIPv4}
		if x, err := strconv.ParseFloat(n.Labels["graph-posX"], 64); err == nil {
			if y, err := strconv.ParseFloat(n.Labels["graph-posY"], 64); err == nil {
				tn.X, tn.Y = &x, &y
			}
		}
		t.Nodes = append(t.Nodes, tn)
	}
	sort.Slice(t.Nodes, func(i, j int) bool { return t.Nodes[i].Name < t.Nodes[j].Name })
	for _, l := range d.Links {
		var a, z topoEnd
		switch {
		case l.Endpoints != nil:
			a, z = l.Endpoints.A, l.Endpoints.Z
		case l.A != nil && l.Z != nil:
			a, z = *l.A, *l.Z
		default:
			continue
		}
		t.Links = append(t.Links, TopoLink{A: a.Node, AIf: a.Interface, Z: z.Node, ZIf: z.Interface})
	}
	return t, nil
}

// dockerTopology reads a lab's topology-data.json out of one of its
// clabgen containers (in name order, the first that has it), or failing
// that lists the lab's containers without links.
func dockerTopology(ctx context.Context, hc *http.Client, imageRE *regexp.Regexp, lab string) (*Topology, error) {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	filter := url.QueryEscape(fmt.Sprintf(`{"label":["containerlab=%s"]}`, lab))
	var list []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	if err := dockerGet(c, hc, "/containers/json?filters="+filter, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("lab %s: no running containers", lab)
	}
	sort.Slice(list, func(i, j int) bool { return strings.Join(list[i].Names, ",") < strings.Join(list[j].Names, ",") })
	fallback := &Topology{Lab: lab, Nodes: []TopoNode{}, Links: []TopoLink{}}
	var errs []string
	tried := false
	for _, e := range list {
		var ct dockerInspect
		if err := dockerGet(c, hc, "/containers/"+e.ID+"/json", &ct); err != nil {
			continue
		}
		l := ct.Config.Labels
		fallback.Nodes = append(fallback.Nodes, TopoNode{Name: l["clab-node-name"], Kind: l["clab-node-kind"], Image: ct.Config.Image, Group: l["clab-node-group"]})
		if !imageRE.MatchString(ct.Config.Image) {
			continue
		}
		p, ok := topologyPathIn(ct)
		if !ok {
			continue
		}
		tried = true
		b, err := dockerReadFile(c, hc, e.ID, p)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		t, err := parseTopologyData(b)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if t.Lab != lab {
			errs = append(errs, fmt.Sprintf("%s:%s is lab %q", strings.TrimPrefix(ct.Name, "/"), p, t.Lab))
			continue
		}
		t.From = strings.TrimPrefix(ct.Name, "/") + ":" + p
		return t, nil
	}
	sort.Slice(fallback.Nodes, func(i, j int) bool { return fallback.Nodes[i].Name < fallback.Nodes[j].Name })
	switch {
	case len(errs) > 0:
		fallback.Note = "Links unknown: " + strings.Join(errs, "; ")
	case !tried:
		fallback.Note = "Links unknown: no clabgen node mounts the topology's folder at /clab (binds: [.:/clab]), where containerlab keeps topology-data.json"
	}
	return fallback, nil
}

type dockerInspect struct {
	Name   string `json:"Name"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

// topologyPathIn: where a container sees its lab's topology-data.json: the
// lab folder (the parent of the node's folder, label clab-node-lab-dir)
// below the folder mounted at /clab.
func topologyPathIn(ct dockerInspect) (string, bool) {
	nodeDir := ct.Config.Labels["clab-node-lab-dir"]
	if nodeDir == "" {
		return "", false
	}
	labDir := filepath.Dir(filepath.Clean(nodeDir))
	for _, m := range ct.Mounts {
		if path.Clean(m.Destination) != "/clab" {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(m.Source), labDir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		return path.Join("/clab", filepath.ToSlash(rel), "topology-data.json"), true
	}
	return "", false
}

// maxTopology bounds a topology-data.json read through Docker.
const maxTopology = 16 << 20

// dockerReadFile copies one file out of a container (the Engine API's
// archive endpoint: a tar stream).
func dockerReadFile(ctx context.Context, hc *http.Client, id, p string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+id+"/archive?path="+url.QueryEscape(p), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s: %s", p, resp.Status, strings.TrimSpace(string(b)))
	}
	tr := tar.NewReader(resp.Body)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: empty archive", p)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxTopology {
			return nil, fmt.Errorf("%s: %d bytes, too big", p, h.Size)
		}
		return io.ReadAll(io.LimitReader(tr, maxTopology))
	}
}
