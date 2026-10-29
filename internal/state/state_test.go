package state

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/events"
)

// ports is a port layer with nothing behind it.
type ports struct{}

func (ports) Send(string, []byte) error   { return nil }
func (ports) PortUp(string) bool          { return true }
func (ports) SetAdmin(string, bool) error { return nil }
func (ports) Desired() map[string]bool    { return nil }

func engine() *core.Engine {
	return core.New(ports{}, events.NewBus(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLocate(t *testing.T) {
	mount := t.TempDir()
	clab := map[string]string{"CLABGEN_LAB_MOUNT": mount, "CLAB_LABEL_CLAB_TOPO_FILE": "/home/u/lab/b2b.clab.yml",
		"CLAB_LABEL_CLAB_NODE_LAB_DIR": "/home/u/lab/clab-b2b/gen1"}
	p, host, why := Locate(env(clab))
	if p != filepath.Join(mount, "clab-b2b", "gen1", "clabgen.yml") || host != "/home/u/lab/clab-b2b/gen1/clabgen.yml" || why != "" {
		t.Errorf("containerlab: %q %q %q", p, host, why)
	}
	if p, _, _ := Locate(env(map[string]string{"CLABGEN_STATE": "/data/x.yml"})); p != "/data/x.yml" {
		t.Errorf("explicit: %q", p)
	}
	for name, m := range map[string]map[string]string{
		"off":          {"CLABGEN_STATE": "off"},
		"no clab":      {},
		"not mounted":  {"CLAB_LABEL_CLAB_TOPO_FILE": "/a/t.yml", "CLAB_LABEL_CLAB_NODE_LAB_DIR": "/a/clab-t/n", "CLABGEN_LAB_MOUNT": "/nonexistent"},
		"outside topo": {"CLABGEN_LAB_MOUNT": mount, "CLAB_LABEL_CLAB_TOPO_FILE": "/a/t.yml", "CLAB_LABEL_CLAB_NODE_LAB_DIR": "/elsewhere/clab-t/n"},
	} {
		if p, _, why := Locate(env(m)); p != "" || why == "" {
			t.Errorf("%s: %q %q", name, p, why)
		}
	}
}

func TestScenario(t *testing.T) {
	mount := t.TempDir()
	clab := env(map[string]string{"CLABGEN_LAB_MOUNT": mount, "CLAB_LABEL_CLAB_TOPO_FILE": "/home/u/lab/b2b.clab.yml"})
	if p, host := Scenario("gen1.yml", clab); p != filepath.Join(mount, "gen1.yml") || host != "/home/u/lab/gen1.yml" {
		t.Errorf("relative, in containerlab: %q %q", p, host)
	}
	if p, host := Scenario("/etc/clabgen/scenario.yml", clab); p != "/etc/clabgen/scenario.yml" || host != p {
		t.Errorf("absolute: %q %q", p, host)
	}
	if p, host := Scenario("gen1.yml", env(map[string]string{"CLABGEN_LAB_MOUNT": "/nonexistent"})); p != "gen1.yml" || host != p {
		t.Errorf("relative, nothing mounted: %q %q", p, host)
	}
	if p, _ := Scenario("", clab); p != "" {
		t.Errorf("none: %q", p)
	}
}

func TestLoadSaveReset(t *testing.T) {
	dir := t.TempDir()
	scen := filepath.Join(dir, "gen1.yml")
	os.WriteFile(scen, []byte("devices:\n  - {name: d1, port: eth1}\n"), 0o644)
	file := filepath.Join(dir, "clab-x", "gen1", "clabgen.yml")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A fresh lab: the scenario, saved at once.
	e := engine()
	s := New(e, log, file, file, "", scen, scen)
	s.Load()
	if s.Info().Source != FromScenario {
		t.Fatalf("source %q", s.Info().Source)
	}
	b, err := os.ReadFile(file)
	if err != nil || !strings.HasPrefix(string(b), "# clabgen saves") || !strings.Contains(string(b), "name: d1") {
		t.Fatalf("saved %q %v", b, err)
	}
	// Changes are saved; nothing changed, nothing written.
	if _, err := e.CreateDevice(core.DeviceSpec{Name: "d2", Port: "eth1"}); err != nil {
		t.Fatal(err)
	}
	s.Save()
	fi1, _ := os.Stat(file)
	s.Save()
	if fi2, _ := os.Stat(file); fi2.ModTime() != fi1.ModTime() {
		t.Error("rewrote an unchanged config")
	}

	// The lab restarts: the saved config wins over the scenario.
	e2 := engine()
	s2 := New(e2, log, file, file, "", scen, scen)
	s2.Load()
	if s2.Info().Source != FromSaved || len(e2.Devices()) != 2 {
		t.Fatalf("restored %q with %d devices", s2.Info().Source, len(e2.Devices()))
	}

	// Reset: back to the scenario, which is now what's saved.
	if err := s2.Reset(); err != nil {
		t.Fatal(err)
	}
	if s2.Info().Source != FromScenario || len(e2.Devices()) != 1 {
		t.Fatalf("after reset %q, %d devices", s2.Info().Source, len(e2.Devices()))
	}
	if b, _ := os.ReadFile(file); strings.Contains(string(b), "d2") {
		t.Error("reset kept d2 in the saved file")
	}

	// A saved file that doesn't parse is kept aside, not overwritten.
	os.WriteFile(file, []byte("devices: [nope"), 0o644)
	e3 := engine()
	s3 := New(e3, log, file, file, "", scen, scen)
	s3.Load()
	if s3.Info().Source != FromScenario || len(e3.Devices()) != 1 {
		t.Errorf("unreadable: %q", s3.Info().Source)
	}
	if b, err := os.ReadFile(file + ".unreadable"); err != nil || string(b) != "devices: [nope" {
		t.Errorf("not kept aside: %q %v", b, err)
	}

	// Not saving: Load still applies the scenario.
	e4 := engine()
	s4 := New(e4, log, "", "", "not a containerlab node", scen, scen)
	s4.Load()
	if s4.Info().Source != FromScenario || len(e4.Devices()) != 1 || s4.Save() != nil {
		t.Errorf("no file: %+v", s4.Info())
	}
}
