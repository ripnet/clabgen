// Package state keeps an agent's config across lab restarts: it saves the
// scenario (everything built in the UI, CLI or API) to a file after every
// change, and loads that file at start in preference to the topology's
// scenario (CLABGEN_SCENARIO), which then only seeds a fresh lab.
//
// In containerlab the file is clab-<lab>/<node>/clabgen.yml, reached
// through the topology's folder mounted at /clab (binds: [.:/clab]):
// containerlab checks bind sources before it creates the lab folder, so
// the lab folder itself can't be the bind. containerlab tells each node
// where its lab folder and topology file are (CLAB_LABEL_* variables).
package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/core"
)

// Where the agent's config came from at start (or at the last reset).
const (
	FromSaved    = "saved"    // the saved file
	FromScenario = "scenario" // the topology's scenario (CLABGEN_SCENARIO)
	FromEmpty    = "empty"    // nothing to load
)

const fileName = "clabgen.yml"

const header = "# clabgen saves this node's config here after every change, and loads it\n" +
	"# when the lab starts again. Delete it (or Reset in the UI's Scenario tab)\n" +
	"# to start again from the topology's scenario.\n"

// Locate finds the saved config's file: CLABGEN_STATE, else the node's
// containerlab lab folder through the topology's folder at CLABGEN_LAB_MOUNT
// (default /clab). host is the same file as the lab host sees it (for
// display); why says why there is none.
func Locate(getenv func(string) string) (path, host, why string) {
	if p := getenv("CLABGEN_STATE"); p != "" {
		if p == "off" {
			return "", "", "turned off (CLABGEN_STATE=off)"
		}
		return p, p, ""
	}
	topo, nodeDir := getenv("CLAB_LABEL_CLAB_TOPO_FILE"), getenv("CLAB_LABEL_CLAB_NODE_LAB_DIR")
	if topo == "" || nodeDir == "" {
		return "", "", "not a containerlab node (set CLABGEN_STATE to a file to save to)"
	}
	mount, ok := labMount(getenv)
	if !ok {
		return "", "", fmt.Sprintf("the topology's folder isn't mounted at %s (add binds: [.:%s] to the node)", mount, mount)
	}
	rel, err := filepath.Rel(filepath.Dir(topo), nodeDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Sprintf("the lab folder %s isn't inside the topology's folder (CLAB_LABDIR_BASE?)", nodeDir)
	}
	return filepath.Join(mount, rel, fileName), filepath.Join(nodeDir, fileName), ""
}

// labMount is where the topology's folder is mounted (CLABGEN_LAB_MOUNT,
// default /clab), and whether it is.
func labMount(getenv func(string) string) (string, bool) {
	mount := getenv("CLABGEN_LAB_MOUNT")
	if mount == "" {
		mount = "/clab"
	}
	fi, err := os.Stat(mount)
	return mount, err == nil && fi.IsDir()
}

// Scenario resolves CLABGEN_SCENARIO: a relative path is relative to the
// topology's folder, like containerlab's own bind sources, when that is
// mounted (else to the working directory). host is the same file as the lab
// host sees it, for display.
func Scenario(p string, getenv func(string) string) (path, host string) {
	if p == "" || filepath.IsAbs(p) {
		return p, p
	}
	mount, ok := labMount(getenv)
	if !ok {
		return p, p
	}
	host = p
	if topo := getenv("CLAB_LABEL_CLAB_TOPO_FILE"); topo != "" {
		host = filepath.Join(filepath.Dir(topo), p)
	}
	return filepath.Join(mount, p), host
}

// Store loads and saves one agent's config.
type Store struct {
	eng      *core.Engine
	log      *slog.Logger
	path     string // "" when not saving
	host     string
	why      string
	scenario string // CLABGEN_SCENARIO, resolved (Scenario)
	scenHost string // as the lab host sees it

	mu      sync.Mutex
	source  string
	last    []byte // the file's content as last written or read
	lastErr string
}

func New(eng *core.Engine, log *slog.Logger, path, host, why, scenario, scenarioHost string) *Store {
	return &Store{eng: eng, log: log, path: path, host: host, why: why, scenario: scenario, scenHost: scenarioHost, source: FromEmpty}
}

// Info is what the agent reports: the saved file (as the lab host sees it;
// empty when not saving, with why), where the config came from, the
// topology's scenario file, and the last save error.
type Info struct {
	File, Why, Source, Scenario, Error string
}

func (s *Store) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Info{File: s.host, Why: s.why, Source: s.source, Scenario: s.scenHost, Error: s.lastErr}
}

func (s *Store) apply(file string, replace bool) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	sc, err := core.ParseScenario(b)
	if err != nil {
		return err
	}
	return s.eng.Apply(sc, replace)
}

// Load applies the saved config if there is one, else the scenario, then
// saves (so the file exists from the first start on). A saved file that
// doesn't parse is kept aside (.unreadable) and the scenario used; one that
// can't be read at all turns saving off rather than being overwritten.
func (s *Store) Load() {
	if s.path != "" {
		b, err := os.ReadFile(s.path)
		switch {
		case err == nil:
			if sc, perr := core.ParseScenario(b); perr != nil {
				bad := s.path + ".unreadable"
				os.Rename(s.path, bad)
				s.log.Error("saved config unreadable: kept aside, starting from the scenario", "file", s.host, "err", perr)
			} else {
				if err := s.eng.Apply(sc, false); err != nil {
					s.log.Error("saved config restored with errors", "file", s.host, "err", err)
				} else {
					s.log.Info("saved config restored", "file", s.host)
				}
				s.mu.Lock()
				s.source = FromSaved
				s.mu.Unlock()
				s.Save()
				return
			}
		case !errors.Is(err, os.ErrNotExist):
			s.log.Error("can't read the saved config: not saving", "file", s.host, "err", err)
			s.why = "can't read " + s.host + ": " + err.Error()
			s.path, s.host = "", ""
		}
	}
	s.loadScenario(false)
	s.Save()
}

func (s *Store) loadScenario(replace bool) {
	source := FromEmpty
	switch {
	case s.scenario != "":
		if err := s.apply(s.scenario, replace); err != nil {
			s.log.Error("scenario applied with errors", "file", s.scenHost, "err", err)
		} else {
			s.log.Info("scenario applied", "file", s.scenHost)
		}
		source = FromScenario
	case replace:
		s.eng.Apply(&core.Scenario{}, true)
	}
	s.mu.Lock()
	s.source = source
	s.mu.Unlock()
}

// Reset forgets the saved config: the topology's scenario (or nothing)
// replaces the agent's config, and is saved as the new starting point.
func (s *Store) Reset() error {
	if s.path != "" {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	s.mu.Lock()
	s.last = nil
	s.mu.Unlock()
	s.loadScenario(true)
	return s.Save()
}

// Save writes the config if it changed since the last write. The file is
// replaced whole (written aside, then renamed), so a crash leaves the old
// or the new one, never half of one.
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	y, err := s.eng.Export().YAML()
	if err != nil {
		return err
	}
	b := append([]byte(header), y...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes.Equal(b, s.last) {
		return nil
	}
	err = writeAtomic(s.path, b)
	if err != nil {
		if msg := err.Error(); msg != s.lastErr { // once per distinct error
			s.log.Error("saving the config failed", "file", s.host, "err", err)
		}
		s.lastErr = err.Error()
		return err
	}
	s.last, s.lastErr = b, ""
	return nil
}

// The folder and file are group-writable: containerlab gives the lab's
// owner access to the lab folder with an ACL, which a mode's group bits
// cap (the ACL mask), and the agent writes as root.
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return err
	}
	os.Chmod(dir, 0o775)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".clabgen-*.yml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	os.Chmod(tmp.Name(), 0o664)
	return os.Rename(tmp.Name(), path)
}

// Run saves changes every interval until ctx ends, then once more.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	if s.path == "" {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Save()
			return
		case <-t.C:
			s.Save()
		}
	}
}
