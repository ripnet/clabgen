package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// DiscoverDocker periodically lists running containerlab containers whose
// image matches imageRE and registers them as agents (source "docker").
// It talks to the Docker Engine API on the socket (see DockerSocket), so it
// works inside a container with the socket mounted. Agents are reached on
// their management IPv4 address, port 8080.
func DiscoverDocker(ctx context.Context, reg *Registry, socket string, imageRE *regexp.Regexp, every time.Duration, log *slog.Logger) {
	hc := dockerClient(socket)
	run := func() {
		agents, err := dockerAgents(ctx, hc, imageRE)
		if err != nil {
			log.Warn("docker discovery", "err", err)
			return
		}
		if err := reg.Replace("docker", agents); err != nil {
			log.Warn("docker discovery", "err", err)
		}
	}
	run()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// DockerSocket returns the Docker Engine socket path: DOCKER_HOST when it
// is a unix:// URL, else /var/run/docker.sock.
func DockerSocket() string {
	if h, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok && h != "" {
		return h
	}
	return "/var/run/docker.sock"
}

func dockerClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

func dockerGet(ctx context.Context, hc *http.Client, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("docker %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type dockerContainer struct {
	Name   string `json:"Name"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func dockerAgents(ctx context.Context, hc *http.Client, imageRE *regexp.Regexp) ([]Agent, error) {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var list []struct {
		ID string `json:"Id"`
	}
	if err := dockerGet(c, hc, "/containers/json?filters="+url.QueryEscape(`{"label":["containerlab"]}`), &list); err != nil {
		return nil, err
	}
	var agents []Agent
	for _, e := range list {
		// The list's Image is an ID once the tag moves to a newer build;
		// inspect keeps the name the container was created from.
		var ct dockerContainer
		if err := dockerGet(c, hc, "/containers/"+e.ID+"/json", &ct); err != nil {
			continue // removed in between
		}
		if a, ok := containerAgent(ct, imageRE); ok {
			agents = append(agents, a)
		}
	}
	return agents, nil
}

func containerAgent(ct dockerContainer, imageRE *regexp.Regexp) (Agent, bool) {
	if !imageRE.MatchString(ct.Config.Image) {
		return Agent{}, false
	}
	l := ct.Config.Labels
	ip := ""
	if br := l["clab-mgmt-net-bridge"]; br != "" {
		// Prefer the clab management network when several are attached.
		for name, n := range ct.NetworkSettings.Networks {
			if n.IPAddress != "" && (ip == "" || strings.Contains(name, "clab")) {
				ip = n.IPAddress
			}
		}
	}
	for _, n := range ct.NetworkSettings.Networks {
		if ip == "" && n.IPAddress != "" {
			ip = n.IPAddress
		}
	}
	if ip == "" {
		return Agent{}, false
	}
	name := l["clab-node-name"]
	if name == "" {
		name = strings.TrimPrefix(ct.Name, "/")
	}
	id := l["clab-node-longname"]
	if id == "" {
		id = strings.TrimPrefix(ct.Name, "/")
	}
	return Agent{ID: id, Name: name, Lab: l["containerlab"], URL: ip + ":8080"}, true
}

// DiscoverLab reads containerlab's topology-data.json for a topology file
// (or a lab directory) and registers nodes whose image matches imageRE.
func DiscoverLab(ctx context.Context, reg *Registry, path string, imageRE *regexp.Regexp, every time.Duration, log *slog.Logger) {
	source := "lab:" + path
	run := func() {
		agents, err := labAgents(path, imageRE)
		if err != nil {
			log.Warn("lab discovery", "path", path, "err", err)
			return
		}
		reg.Replace(source, agents)
	}
	run()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

type topoData struct {
	Name  string `json:"name"`
	Nodes map[string]struct {
		ShortName string `json:"shortname"`
		LongName  string `json:"longname"`
		Image     string `json:"image"`
		MgmtIPv4  string `json:"mgmt-ipv4-address"`
	} `json:"nodes"`
}

func labAgents(path string, imageRE *regexp.Regexp) ([]Agent, error) {
	dataFile := path
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		dataFile = filepath.Join(path, "topology-data.json")
	} else if strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml") {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var t struct {
			Name string `json:"name"`
		}
		if err := yaml.Unmarshal(b, &t); err != nil || t.Name == "" {
			return nil, fmt.Errorf("cannot read lab name from %s", path)
		}
		dataFile = filepath.Join(filepath.Dir(path), "clab-"+t.Name, "topology-data.json")
	}
	b, err := os.ReadFile(dataFile)
	if err != nil {
		return nil, fmt.Errorf("lab not deployed? %w", err)
	}
	var td topoData
	if err := json.Unmarshal(b, &td); err != nil {
		return nil, err
	}
	var agents []Agent
	for key, n := range td.Nodes {
		if !imageRE.MatchString(n.Image) || n.MgmtIPv4 == "" {
			continue
		}
		name := n.ShortName
		if name == "" {
			name = key
		}
		id := n.LongName
		if id == "" {
			id = td.Name + "-" + name
		}
		agents = append(agents, Agent{ID: id, Name: name, Lab: td.Name, URL: n.MgmtIPv4 + ":8080"})
	}
	return agents, nil
}
