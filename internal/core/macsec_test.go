package core_test

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pkt"
	_ "github.com/ripnet/clabgen/protocols/icmp"
	_ "github.com/ripnet/clabgen/protocols/macsec"
)

const msKey = "000102030405060708090a0b0c0d0e0f"

// wireLog records the layout of what crosses a wire: the VLAN IDs in front
// of the first non-tag ethertype, and that ethertype.
type wireLog struct {
	mu   sync.Mutex
	seen map[string]int
}

func (l *wireLog) watch(w *wire) {
	l.seen = map[string]int{}
	w.mu.Lock()
	w.drop = func(b []byte) bool {
		var parts []string
		off := 12
		for off+4 <= len(b) && pkt.IsTPID(binary.BigEndian.Uint16(b[off:])) {
			parts = append(parts, fmt.Sprint(binary.BigEndian.Uint16(b[off+2:])&0xfff))
			off += 4
		}
		parts = append(parts, fmt.Sprintf("%#04x", binary.BigEndian.Uint16(b[off:])))
		l.mu.Lock()
		l.seen[strings.Join(parts, " ")]++
		l.mu.Unlock()
		return false
	}
	w.mu.Unlock()
}

// reset forgets what was seen: frames sent before MACsec started (a new
// device's gratuitous ARP) are in clear.
func (l *wireLog) reset() {
	l.mu.Lock()
	l.seen = map[string]int{}
	l.mu.Unlock()
}

func (l *wireLog) layouts() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for k, v := range l.seen {
		out[k] = v
	}
	return out
}

func mkDev(t *testing.T, e *core.Engine, name, mac, ip string, vids ...uint16) {
	t.Helper()
	s := core.DeviceSpec{Name: name, Port: "eth1", MAC: mac}
	if ip != "" {
		s.Addresses = []string{ip}
	}
	for _, v := range vids {
		s.VLANs = append(s.VLANs, pkt.VLANTag{VID: v})
	}
	if _, err := e.CreateDevice(s); err != nil {
		t.Fatal(err)
	}
}

func ping(t *testing.T, e *core.Engine, name, dev, dst string) {
	t.Helper()
	mkProto(t, e, name, "icmp", dev, fmt.Sprintf(`{"destination":%q,"rate":100,"timeout":"200ms","auto_start":true}`, dst))
}

func macsecStatus(t *testing.T, e *core.Engine, name string) map[string]any {
	t.Helper()
	return flowStatus(t, e, name)
}

func rxOK(st map[string]any) float64 {
	var n float64
	scs, _ := st["rx_scs"].([]any)
	for _, sc := range scs {
		n += num(sc.(map[string]any), "in_pkts_ok")
	}
	return n
}

// TestMACsecDevice: a device's traffic, its VLAN tag in clear, protected
// with a static key; a peer without MACsec or with another key gets nothing
// through.
func TestMACsecDevice(t *testing.T) {
	a, b, wa, _ := newPair()
	var log wireLog
	log.watch(wa)
	mkDev(t, a, "d", "02:00:00:00:0a:01", "10.0.0.1/24", 100)
	mkDev(t, b, "d", "02:00:00:00:0b:01", "10.0.0.2/24", 100)
	cfg := `{"static":{"key":"` + msKey + `"}}`
	mkProto(t, a, "ms", "macsec", "d", cfg)
	mkProto(t, b, "ms", "macsec", "d", cfg)
	log.reset()
	ping(t, a, "p", "d", "10.0.0.2")
	eventually(t, "pings over MACsec", func() bool { return num(flowStatus(t, a, "p"), "rx") >= 20 })
	if st := flowStatus(t, a, "p"); num(st, "lost") != 0 {
		t.Fatalf("lost %v", st)
	}
	// Every frame a sent: VLAN 100, then MACsec.
	if l := log.layouts(); len(l) != 1 || l["100 0x88e5"] == 0 {
		t.Fatalf("wire layouts %v", l)
	}
	st := macsecStatus(t, b, "ms")
	if st["secured"] != true || num(st, "overhead") != 32 || num(st, "clear_tags") != 1 || rxOK(st) < 20 {
		t.Fatalf("b status %v", st)
	}
	scs := st["rx_scs"].([]any)
	if len(scs) != 1 || scs[0].(map[string]any)["sci"] != "0200000000a10001" && scs[0].(map[string]any)["mac"] != "02:00:00:00:0a:01" {
		t.Fatalf("b's receive SCs %v", scs)
	}

	// b without MACsec: a's frames aren't for it, and a drops b's.
	if err := b.DeleteProtocol("ms"); err != nil {
		t.Fatal(err)
	}
	run(t, a, "p", "clear")
	ping(t, b, "pb", "d", "10.0.0.1")
	eventually(t, "nothing gets through", func() bool {
		return num(flowStatus(t, a, "p"), "lost") >= 10 && num(flowStatus(t, a, "p"), "rx") == 0 && num(flowStatus(t, b, "pb"), "lost") >= 10
	})
	if num(macsecStatus(t, a, "ms"), "in_pkts_no_tag") == 0 {
		t.Fatal("a didn't count b's unprotected frames")
	}

	// Another key: b's frames fail the ICV check.
	mkProto(t, b, "ms", "macsec", "d", `{"static":{"key":"ffffffffffffffffffffffffffffffff"}}`)
	run(t, a, "ms", "clear")
	eventually(t, "not valid counted", func() bool {
		scs, _ := macsecStatus(t, a, "ms")["rx_scs"].([]any)
		return len(scs) == 1 && num(scs[0].(map[string]any), "in_pkts_not_valid") > 0
	})
	if num(flowStatus(t, a, "p"), "rx") != 0 {
		t.Fatal("replies through the wrong key")
	}
}

// TestMACsecTagsInside: QinQ with the outer tag in clear and the inner one
// encrypted, and a plain VLAN beside it on the same port.
func TestMACsecTagsInside(t *testing.T) {
	a, b, wa, _ := newPair()
	var log wireLog
	log.watch(wa)
	for i, e := range []*core.Engine{a, b} {
		mkDev(t, e, "q", fmt.Sprintf("02:00:00:00:0%d:01", i+1), fmt.Sprintf("10.0.0.%d/24", i+1), 100, 200)
		mkDev(t, e, "plain", fmt.Sprintf("02:00:00:00:0%d:02", i+1), fmt.Sprintf("10.3.0.%d/24", i+1), 300)
		mkProto(t, e, "ms", "macsec", "q", `{"clear_tags":1,"cipher":"gcm-aes-xpn-256","static":{"key":"`+msKey+msKey+`","salt":"0102030405060708090a0b0c","ssci":`+fmt.Sprint(i+1)+`,"peer_ssci":`+fmt.Sprint(2-i)+`}}`)
	}
	log.reset()
	ping(t, a, "p", "q", "10.0.0.2")
	ping(t, a, "p300", "plain", "10.3.0.2")
	eventually(t, "both answer", func() bool {
		return num(flowStatus(t, a, "p"), "rx") >= 20 && num(flowStatus(t, a, "p300"), "rx") >= 20
	})
	l := log.layouts()
	if l["100 0x88e5"] == 0 || l["300 0x0800"] == 0 || l["100 200 0x0800"] != 0 || l["100 200 0x0806"] != 0 {
		t.Fatalf("wire layouts %v", l)
	}
	if st := macsecStatus(t, a, "ms"); num(st, "clear_tags") != 1 || st["cipher"] != "gcm-aes-xpn-256" {
		t.Fatalf("status %v", st)
	}
}

// TestMACsecPort: one SecY for the port; the VLAN devices on it are behind
// it, their tags encrypted, with one SCI between the two ends.
func TestMACsecPort(t *testing.T) {
	a, b, wa, _ := newPair()
	var log wireLog
	log.watch(wa)
	for i, e := range []*core.Engine{a, b} {
		mkDev(t, e, "phy", fmt.Sprintf("02:00:00:00:0%d:00", i+1), "")
		mkDev(t, e, "v100", fmt.Sprintf("02:00:00:00:0%d:01", i+1), fmt.Sprintf("10.1.0.%d/24", i+1), 100)
		mkDev(t, e, "v200", fmt.Sprintf("02:00:00:00:0%d:02", i+1), fmt.Sprintf("10.2.0.%d/24", i+1), 200)
		mkProto(t, e, "ms", "macsec", "phy", `{"scope":"port","static":{"key":"`+msKey+`"}}`)
	}
	log.reset()
	ping(t, a, "p100", "v100", "10.1.0.2")
	ping(t, a, "p200", "v200", "10.2.0.2")
	eventually(t, "both VLANs answer", func() bool {
		return num(flowStatus(t, a, "p100"), "rx") >= 20 && num(flowStatus(t, a, "p200"), "rx") >= 20
	})
	if l := log.layouts(); len(l) != 1 || l["0x88e5"] == 0 {
		t.Fatalf("wire layouts %v (all untagged MACsec)", l)
	}
	st := macsecStatus(t, b, "ms")
	if scs, _ := st["rx_scs"].([]any); len(scs) != 1 || scs[0].(map[string]any)["mac"] != "02:00:00:00:01:00" || rxOK(st) < 40 {
		t.Fatalf("b status %v", st)
	}
	// A port is secured as a whole or per device.
	mkProto(t, a, "x", "macsec", "v100", `{"static":{"key":"`+msKey+`"}}`)
	eventually(t, "refused", func() bool {
		v, _ := a.GetProtocol("x")
		return v.State == core.StateError && strings.Contains(v.Error, "secured as a whole")
	})
	if err := a.DeleteProtocol("x"); err != nil {
		t.Fatal(err)
	}
	// A device added later is behind the port's SecY too.
	mkDev(t, a, "v300", "02:00:00:00:01:03", "10.4.0.1/24", 300)
	mkDev(t, b, "v300", "02:00:00:00:02:03", "10.4.0.2/24", 300)
	ping(t, a, "p300", "v300", "10.4.0.2")
	eventually(t, "new VLAN answers", func() bool { return num(flowStatus(t, a, "p300"), "rx") >= 10 })
	if l := log.layouts(); len(l) != 1 {
		t.Fatalf("wire layouts %v", l)
	}
	// Without the port's SecY, the devices are plain again.
	if err := a.DeleteProtocol("ms"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteProtocol("ms"); err != nil {
		t.Fatal(err)
	}
	run(t, a, "p100", "clear")
	eventually(t, "plain again", func() bool { return num(flowStatus(t, a, "p100"), "rx") >= 10 })
	if l := log.layouts(); l["100 0x0800"] == 0 {
		t.Fatalf("wire layouts %v", l)
	}
}

const mkaCfg = `"mka":{"cak":"0123456789abcdef0123456789abcdef","ckn":"6c6162","hello_time":"100ms"`

func mkaState(t *testing.T, e *core.Engine, name string) map[string]any {
	t.Helper()
	m, _ := macsecStatus(t, e, name)["mka"].(map[string]any)
	return m
}

// TestMACsecMKA: MKA keys two devices with their VLAN tag in clear; traffic
// flows once both are secured, survives a rekey, and stops when MKA does.
func TestMACsecMKA(t *testing.T) {
	a, b, wa, _ := newPair()
	var log wireLog
	log.watch(wa)
	mkDev(t, a, "d", "02:00:00:00:0a:01", "10.0.0.1/24", 100)
	mkDev(t, b, "d", "02:00:00:00:0b:01", "10.0.0.2/24", 100)
	mkProto(t, a, "ms", "macsec", "d", `{`+mkaCfg+`,"key_server_priority":1}}`)
	mkProto(t, b, "ms", "macsec", "d", `{`+mkaCfg+`}}`)
	eventually(t, "both secured", func() bool {
		return mkaState(t, a, "ms")["state"] == "secured" && mkaState(t, b, "ms")["state"] == "secured"
	})
	if ma, mb := mkaState(t, a, "ms"), mkaState(t, b, "ms"); ma["key_server"] != true || mb["key_server"] != false {
		t.Fatalf("key server: a %v, b %v", ma, mb)
	}
	log.reset()
	ping(t, a, "p", "d", "10.0.0.2")
	eventually(t, "pings", func() bool { return num(flowStatus(t, a, "p"), "rx") >= 30 })
	// MKPDUs carry the clear tag; everything else is MACsec.
	if l := log.layouts(); len(l) != 2 || l["100 0x88e5"] == 0 || l["100 0x888e"] == 0 {
		t.Fatalf("wire layouts %v", l)
	}

	// Rekey under traffic: a new key on both ends, nothing lost.
	run(t, a, "p", "clear")
	run(t, a, "ms", "rekey")
	eventually(t, "new key on both", func() bool {
		la, _ := mkaState(t, a, "ms")["latest_key"].(map[string]any)
		lb, _ := mkaState(t, b, "ms")["latest_key"].(map[string]any)
		return la != nil && lb != nil && num(la, "kn") == 2 && num(lb, "kn") == 2 && la["tx"] == true && lb["tx"] == true
	})
	eventually(t, "more pings", func() bool { return num(flowStatus(t, a, "p"), "rx") >= 50 })
	if st := flowStatus(t, a, "p"); num(st, "lost") != 0 {
		t.Fatalf("lost across the rekey: %v", st)
	}

	// b stops MKA: a times it out, removes the keys, and the pings fail.
	run(t, b, "ms", "pause-mka")
	eventually(t, "a unsecured", func() bool { return mkaState(t, a, "ms")["state"] == "no peers" })
	run(t, a, "p", "clear")
	eventually(t, "pings fail", func() bool { st := flowStatus(t, a, "p"); return num(st, "lost") >= 10 && num(st, "rx") == 0 })
	run(t, b, "ms", "resume-mka")
	run(t, a, "p", "clear")
	eventually(t, "secured and answering again", func() bool { return num(flowStatus(t, a, "p"), "rx") >= 10 })
}

// TestMACsecMKAPort: MKA for a port's SecY; MKPDUs untagged, VLANs behind it.
func TestMACsecMKAPort(t *testing.T) {
	a, b, wa, _ := newPair()
	var log wireLog
	log.watch(wa)
	for i, e := range []*core.Engine{a, b} {
		mkDev(t, e, "phy", fmt.Sprintf("02:00:00:00:0%d:00", i+1), "")
		mkDev(t, e, "v100", fmt.Sprintf("02:00:00:00:0%d:01", i+1), fmt.Sprintf("10.1.0.%d/24", i+1), 100)
		mkProto(t, e, "ms", "macsec", "phy", `{"scope":"port",`+mkaCfg+`}}`)
	}
	eventually(t, "secured", func() bool {
		return mkaState(t, a, "ms")["state"] == "secured" && mkaState(t, b, "ms")["state"] == "secured"
	})
	log.reset()
	ping(t, a, "p", "v100", "10.1.0.2")
	eventually(t, "pings", func() bool { return num(flowStatus(t, a, "p"), "rx") >= 20 })
	if l := log.layouts(); len(l) != 2 || l["0x88e5"] == 0 || l["0x888e"] == 0 {
		t.Fatalf("wire layouts %v", l)
	}
}
