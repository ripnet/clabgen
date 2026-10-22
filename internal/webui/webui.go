// Package webui serves the browser UI and proxies API calls to agents.
//
// The browser only ever talks to the origin that served it. Calls for agent
// ID are sent to /agents/ID/<rpc path> and forwarded to that agent, so one
// reachable port (e.g. an SSH tunnel to clabgen-ui on the lab host) reaches
// every agent in every lab.
package webui

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

//go:embed all:dist
var dist embed.FS

// Agent is one agent the UI can reach.
type Agent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Lab    string `json:"lab,omitempty"`
	URL    string `json:"url,omitempty"`
	Source string `json:"source"` // self, static, docker, lab, manual

	handler http.Handler // in-process agent, when Source is "self"
	proxy   http.Handler
}

// Registry holds the known agents. Each discovery source replaces only its
// own entries.
type Registry struct {
	mu     sync.RWMutex
	agents map[string]*Agent
}

func NewRegistry() *Registry { return &Registry{agents: map[string]*Agent{}} }

var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// AddLocal registers the in-process agent, served without a network hop.
func (r *Registry) AddLocal(id, name string, h http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agents[id] = &Agent{ID: id, Name: name, Source: "self", handler: h}
}

// NormalizeURL accepts host, host:port or a full URL; the default port is 8080.
func NormalizeURL(addr string) (string, error) {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return "", errors.New("invalid agent address")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("agent address must be http or https")
	}
	if u.Port() == "" {
		u.Host += ":8080"
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return u.String(), nil
}

func newAgent(a Agent) (*Agent, error) {
	if !idRE.MatchString(a.ID) {
		return nil, errors.New("invalid agent id")
	}
	u, err := NormalizeURL(a.URL)
	if err != nil {
		return nil, err
	}
	a.URL = u
	target, _ := url.Parse(u)
	p := httputil.NewSingleHostReverseProxy(target)
	p.FlushInterval = -1 // stream WatchEvents immediately
	a.proxy = p
	if a.Name == "" {
		a.Name = a.ID
	}
	return &a, nil
}

// Replace sets all agents of one source, keeping other sources' agents.
func (r *Registry) Replace(source string, list []Agent) error {
	built := map[string]*Agent{}
	var errs []error
	for _, a := range list {
		a.Source = source
		na, err := newAgent(a)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		built[na.ID] = na
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, a := range r.agents {
		if a.Source == source {
			if nb, ok := built[id]; ok && nb.URL == a.URL && nb.Name == a.Name && nb.Lab == a.Lab {
				built[id] = a // keep the existing proxy
			}
			delete(r.agents, id)
		}
	}
	for id, a := range built {
		if old, taken := r.agents[id]; taken && old.Source != source {
			continue // don't shadow an agent from another source
		}
		r.agents[id] = a
	}
	return errors.Join(errs...)
}

func (r *Registry) Add(a Agent) (*Agent, error) {
	na, err := newAgent(a)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.agents[na.ID]; dup {
		return nil, errors.New("agent id already exists")
	}
	r.agents[na.ID] = na
	return na, nil
}

func (r *Registry) Remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[id]
	if !ok || a.Source == "self" {
		return false
	}
	delete(r.agents, id)
	return true
}

func (r *Registry) Get(id string) *Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.agents[id]
}

func (r *Registry) List() []Agent {
	r.mu.RLock()
	out := make([]Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, *a)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lab != out[j].Lab {
			return out[i].Lab < out[j].Lab
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Options controls what the UI is allowed to do.
type Options struct {
	// AllowManual lets the browser add and remove agents.
	AllowManual bool
	// Version is the version serving the UI (shown in its header).
	Version string
	// Updates, when set, tells the UI about newer releases.
	Updates *UpdateChecker
}

// Handler serves /api/info, /api/agents, /agents/{id}/... and the UI
// itself.
func Handler(reg *Registry, opt Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) {
		in := Info{Version: opt.Version}
		if v, u := opt.Updates.Latest(); Newer(v, opt.Version) {
			in.Latest, in.LatestURL, in.Update = v, u, true
		}
		writeJSON(w, http.StatusOK, in)
	})
	mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"agents": reg.List(), "allow_manual": opt.AllowManual})
	})
	mux.HandleFunc("POST /api/agents", func(w http.ResponseWriter, r *http.Request) {
		if !opt.AllowManual {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "adding agents is disabled"})
			return
		}
		var a Agent
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&a); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.Source, a.Lab = "manual", "manual"
		na, err := reg.Add(a)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, na)
	})
	mux.HandleFunc("DELETE /api/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		a := reg.Get(r.PathValue("id"))
		if !opt.AllowManual || a == nil || a.Source != "manual" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only manually added agents can be removed"})
			return
		}
		reg.Remove(a.ID)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/agents/{id}/", func(w http.ResponseWriter, r *http.Request) {
		a := reg.Get(r.PathValue("id"))
		if a == nil {
			http.Error(w, "unknown agent", http.StatusNotFound)
			return
		}
		h := a.proxy
		if a.handler != nil {
			h = a.handler
		}
		http.StripPrefix("/agents/"+a.ID, h).ServeHTTP(w, r)
	})
	mux.Handle("/", spa())
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

const notBuilt = `<!doctype html><meta charset="utf-8"><title>clabgen</title>
<body style="font-family:system-ui;padding:2rem"><h1>clabgen</h1>
<p>The web UI was not built into this binary. Run <code>make web</code> (or <code>make build</code>) and rebuild.</p>
<p>The API is available at <code>/clabgen.v1.AgentService/</code>.</p></body>`

// spa serves the built assets, falling back to index.html for client routes.
func spa() http.Handler {
	sub, _ := fs.Sub(dist, "dist")
	files := http.FileServer(http.FS(sub))
	_, err := fs.Stat(sub, "index.html")
	built := err == nil
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !built {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(notBuilt))
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(sub, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		files.ServeHTTP(w, r2)
	})
}
