package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.2.0", "v0.1.9", true}, {"v1.0.0", "v0.9.9", true}, {"0.1.10", "v0.1.9", true},
		{"v0.1.0", "v0.1.0", false}, {"v0.1.0", "v0.2.0", false},
		{"v0.2.0", "a4cd446", false}, {"v0.2.0", "v0.1.0-3-gabc1234", false}, {"", "v0.1.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestUpdateCheckAndInfo(t *testing.T) {
	calls := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/repos/ripnet/clabgen/releases/latest" || r.Header.Get("User-Agent") == "" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.0", "html_url": "https://example.test/v0.3.0"})
	}))
	defer gh.Close()
	info := func(opt Options) Info {
		rec := httptest.NewRecorder()
		Handler(NewRegistry(), opt).ServeHTTP(rec, httptest.NewRequest("GET", "/api/info", nil))
		var in Info
		json.Unmarshal(rec.Body.Bytes(), &in)
		return in
	}

	u := &UpdateChecker{Repo: "ripnet/clabgen", Current: "v0.2.0", Every: time.Hour, API: gh.URL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.Run(ctx)
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if v, _ := u.Latest(); v != "" {
			break
		}
	}
	if in := info(Options{Version: "v0.2.0", Updates: u}); !in.Update || in.Latest != "v0.3.0" || in.LatestURL != "https://example.test/v0.3.0" {
		t.Fatalf("info %+v", in)
	}
	// Already the latest: nothing offered.
	if in := info(Options{Version: "v0.3.0", Updates: u}); in.Update || in.Latest != "" || in.Version != "v0.3.0" {
		t.Errorf("info at the latest %+v", in)
	}
	// Development builds don't check at all.
	dev := &UpdateChecker{Repo: "ripnet/clabgen", Current: "a4cd446", Every: time.Hour, API: gh.URL}
	before := calls
	dev.Run(ctx)
	if calls != before {
		t.Error("a development build asked for updates")
	}
	if in := info(Options{Version: "a4cd446"}); in.Update || in.Version != "a4cd446" {
		t.Errorf("info without a checker %+v", in)
	}
}
