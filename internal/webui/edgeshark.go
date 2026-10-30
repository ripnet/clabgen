package webui

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Edgeshark (github.com/siemens/edgeshark) on the lab host: its packetflix
// service captures any container's interfaces, and its Wireshark plugin
// opens packetflix: links, with a link handler on Linux, macOS and Windows.
// When the UI knows where packetflix is, the Capture tab offers such a link
// for an agent's ports: one click opens Wireshark on them.
type Edgeshark struct {
	// URL is packetflix as the browsers' machines reach it, when given
	// (-edgeshark http://labhost:5001); else it is looked for through the
	// Docker socket, and the browser uses the UI's own host name.
	URL string

	hc   *http.Client
	mu   sync.Mutex
	port int
	at   time.Time
}

// NewEdgeshark finds packetflix through the Docker socket (url empty) or
// takes its URL.
func NewEdgeshark(url, socket string) *Edgeshark {
	e := &Edgeshark{URL: strings.TrimRight(url, "/")}
	if e.URL == "" {
		e.hc = dockerClient(socket)
	}
	return e
}

// EdgesharkInfo tells the browser where packetflix is: a URL, or a port on
// the host the browser reached the UI by.
type EdgesharkInfo struct {
	URL  string `json:"url,omitempty"`
	Port int    `json:"port,omitempty"`
}

// Info is nil when there is no packetflix to use. Docker is asked at most
// every 30 seconds.
func (e *Edgeshark) Info(ctx context.Context) *EdgesharkInfo {
	if e == nil {
		return nil
	}
	if e.URL != "" {
		return &EdgesharkInfo{URL: e.URL}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.at) > 30*time.Second {
		e.port, e.at = packetflixPort(ctx, e.hc), time.Now()
	}
	if e.port == 0 {
		return nil
	}
	return &EdgesharkInfo{Port: e.port}
}

// packetflixPort is the host port a running packetflix container publishes
// its service (5001) on, or 0.
func packetflixPort(ctx context.Context, hc *http.Client) int {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var list []struct {
		Image string `json:"Image"`
		Ports []struct {
			PrivatePort int    `json:"PrivatePort"`
			PublicPort  int    `json:"PublicPort"`
			Type        string `json:"Type"`
		} `json:"Ports"`
	}
	if err := dockerGet(c, hc, "/containers/json", &list); err != nil {
		return 0
	}
	for _, ct := range list {
		if !strings.Contains(ct.Image, "packetflix") {
			continue
		}
		for _, p := range ct.Ports {
			if p.PrivatePort == 5001 && p.PublicPort != 0 && p.Type == "tcp" {
				return p.PublicPort
			}
		}
	}
	return 0
}
