package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Release versions are vX.Y.Z (or X.Y.Z); anything else (a commit hash,
// v0.1.0-3-gabc1234) is a development build.
var releaseRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// ParseRelease returns a release version's numbers.
func ParseRelease(v string) ([3]int, bool) {
	m := releaseRE.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}

// Newer reports whether release a is newer than release b (false unless
// both are releases).
func Newer(a, b string) bool {
	x, ok1 := ParseRelease(a)
	y, ok2 := ParseRelease(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// UpdateChecker asks GitHub for the project's latest release now and then,
// so the UI can offer it. It only runs for release builds.
type UpdateChecker struct {
	Repo    string // owner/name
	Current string
	Every   time.Duration
	API     string // default https://api.github.com
	Client  *http.Client
	Log     *slog.Logger

	mu          sync.Mutex
	latest, url string
}

// Latest is the latest release seen, if any.
func (u *UpdateChecker) Latest() (version, url string) {
	if u == nil {
		return "", ""
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.latest, u.url
}

// Run checks until ctx ends. Failures (no network, a private repository,
// rate limits) only leave the last answer in place.
func (u *UpdateChecker) Run(ctx context.Context) {
	if _, ok := ParseRelease(u.Current); !ok {
		return
	}
	for {
		if err := u.check(ctx); err != nil && u.Log != nil {
			u.Log.Debug("update check", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(u.Every):
		}
	}
}

func (u *UpdateChecker) check(ctx context.Context) error {
	api := u.API
	if api == "" {
		api = "https://api.github.com"
	}
	c := u.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/releases/latest", api, u.Repo), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "clabgen-ui/"+u.Current)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	var r struct {
		Tag string `json:"tag_name"`
		URL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return err
	}
	if _, ok := ParseRelease(r.Tag); !ok {
		return fmt.Errorf("latest release %q is not a version", r.Tag)
	}
	u.mu.Lock()
	u.latest, u.url = r.Tag, r.URL
	u.mu.Unlock()
	return nil
}

// Info is what /api/info returns: the version serving the UI and, from
// the update checker, a newer release when there is one.
type Info struct {
	Version   string `json:"version"`
	Latest    string `json:"latest,omitempty"`
	LatestURL string `json:"latest_url,omitempty"`
	Update    bool   `json:"update_available"`
}
