package main

import (
	"path/filepath"
	"testing"
)

func TestExtcapArgs(t *testing.T) {
	a := extcapArgs([]string{"--capture", "--extcap-interface", "clabgen.b2b-gen1.eth1", "--fifo=/tmp/f", "--device", "v100", "--direction", "", "--ethertype", "0x0800,0x86dd"})
	if a["extcap-interface"] != "clabgen.b2b-gen1.eth1" || a["fifo"] != "/tmp/f" || a["device"] != "v100" || a["ethertype"] != "0x0800,0x86dd" || !has(a, "capture") {
		t.Errorf("parsed %v", a)
	}
	if !isExtcap([]string{"--extcap-interfaces", "--extcap-version=4.4"}) || isExtcap([]string{"capture", "eth1"}) {
		t.Error("isExtcap")
	}
	if got := safeName.ReplaceAllString("b2b/gen 1", "-"); got != "b2b-gen-1" {
		t.Errorf("safe name %q", got)
	}
}

// Without a config, Wireshark still gets a valid (empty) interface list.
func TestExtcapUnconfigured(t *testing.T) {
	t.Setenv("CLABGEN_WIRESHARK_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	if code := runExtcap([]string{"--extcap-interfaces"}); code != 0 {
		t.Errorf("interfaces exit %d", code)
	}
	if code := runExtcap([]string{"--capture", "--extcap-interface", "x", "--fifo", "/nonexistent"}); code == 0 {
		t.Error("capture without config succeeded")
	}
}
