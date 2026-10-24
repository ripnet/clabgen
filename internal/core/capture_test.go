package core_test

import (
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pkt"
)

// collect reads a capture for d (or until n frames).
func collect(c *core.Capture, n int, d time.Duration) []core.CapturedFrame {
	var out []core.CapturedFrame
	t := time.After(d)
	for len(out) < n {
		select {
		case f := <-c.C:
			out = append(out, f)
		case <-t:
			return out
		}
	}
	return out
}

func TestCapture(t *testing.T) {
	a, b, _, _ := ipPair(t)
	all, err := a.StartCapture(core.CaptureFilter{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ip4, _ := a.StartCapture(core.CaptureFilter{EtherTypes: []uint16{pkt.EtherTypeIPv4}, Direction: "rx", Snaplen: 40}, 1000)
	other, _ := b.StartCapture(core.CaptureFilter{Device: "d"}, 1000)
	mkFlow(t, a, "p", `{"destination":"10.0.0.2","mode":"count","count":5,"rate":100,"auto_start":true}`)

	eventually(t, "the ping done", func() bool { return flowStatus(t, a, "p")["state"] == "done" })
	fs := collect(all, 1000, 300*time.Millisecond) // ARP, ND, 5 echoes and 5 replies
	ip := map[string]int{}
	ets := map[uint16]int{}
	for _, f := range fs {
		d, err := pkt.Decode(f.Data)
		if err != nil || f.Port != "eth1" || f.Length != len(f.Data) || f.Time.IsZero() {
			t.Fatalf("frame %+v: %v", f, err)
		}
		if len(d.Tags) != 1 || d.Tags[0].VID != 10 {
			t.Errorf("device tag missing: %+v", d.Tags)
		}
		ets[d.EtherType]++
		if d.EtherType == pkt.EtherTypeIPv4 {
			ip[f.Dir]++
		}
	}
	if ip["tx"] != 5 || ip["rx"] != 5 || ets[pkt.EtherTypeARP] < 2 {
		t.Fatalf("captured %d frames: IPv4 %v, ethertypes %v", len(fs), ip, ets)
	}

	// Filters: received IPv4 only, cut to 40 bytes.
	for _, f := range collect(ip4, 5, 300*time.Millisecond) {
		if f.Dir != "rx" || len(f.Data) != 40 || f.Length <= 40 {
			t.Errorf("filtered frame %s %d/%d", f.Dir, len(f.Data), f.Length)
		}
	}
	if got := collect(other, 10, 300*time.Millisecond); len(got) < 10 {
		t.Errorf("b's device capture got %d frames", len(got))
	}

	// A reader that falls behind loses frames (counted), never the data path.
	slow, _ := a.StartCapture(core.CaptureFilter{}, 1)
	mkFlow(t, a, "q", `{"destination":"10.0.0.2","mode":"count","count":200,"rate":2000,"auto_start":true}`)
	eventually(t, "drops counted", func() bool { return slow.Dropped() > 0 })
	eventually(t, "the flow done", func() bool { return flowStatus(t, a, "q")["state"] == "done" })
	if s := flowStatus(t, a, "q"); num(s, "lost") != 0 {
		t.Errorf("a slow capture cost the flow %v packets", s["lost"])
	}
	for _, c := range []*core.Capture{all, ip4, other, slow} {
		c.Close()
		c.Close() // twice is fine
	}
	if _, ok := <-all.C; ok {
		// drain what was buffered
		for range all.C {
		}
	}

	if _, err := a.StartCapture(core.CaptureFilter{Device: "nope"}, 10); err == nil {
		t.Error("unknown device accepted")
	}
	if _, err := a.StartCapture(core.CaptureFilter{Direction: "up"}, 10); err == nil {
		t.Error("bad direction accepted")
	}
}
