package core_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ripnet/clabgen/internal/core"
	_ "github.com/ripnet/clabgen/protocols/packet"
)

func mkPacket(t *testing.T, e *core.Engine, name, cfg string) {
	t.Helper()
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: "packet", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

func rxOf(t *testing.T, e *core.Engine, proto string) []core.ReceivedFlow {
	t.Helper()
	rs, err := e.ReceivedFlows("d")
	if err != nil {
		t.Fatal(err)
	}
	var out []core.ReceivedFlow
	for _, r := range rs {
		if r.Proto == proto {
			out = append(out, r)
		}
	}
	return out
}

func TestPacketStreams(t *testing.T) {
	a, b, _, _ := ipPair(t)
	// UDP to b (the next hop resolved with ARP), the source port cycled.
	mkPacket(t, a, "udp", `{"layers":[{"ipv4":{"dst":"10.0.0.2","dscp":10}},{"udp":{"dst_port":5001}},{"payload":{"size":64}}],
		"modifiers":[{"field":"udp.src_port","mode":"increment","start":"40000","count":4}],"mode":"count","count":200,"rate":1000,"auto_start":true}`)
	eventually(t, "b counted 200 UDP packets", func() bool {
		rs := rxOf(t, b, "udp")
		return len(rs) == 1 && rs[0].Rx == 200
	})
	if r := rxOf(t, b, "udp")[0]; r.Lost != 0 || r.Src != "10.0.0.1" || r.Dst != "10.0.0.2" || r.DstPort != 5001 {
		t.Errorf("received %+v", r.Flow)
	}
	if s := flowStatus(t, a, "udp"); s["kind"] != "Ethernet/IPv4/UDP/Payload" || num(s, "tx") != 200 || s["tracks_replies"] != false || s["dst_mac"] != "02:00:00:00:0b:01" {
		t.Errorf("status %v", s)
	}

	// Raw L2 with a VLAN layer inside the device's tag: b's device on VLAN 10 doesn't see it,
	// so send plain L2 to b instead and check it's counted as l2.
	mkPacket(t, a, "l2", `{"layers":[{"ethernet":{"dst":"02:00:00:00:0b:01","ethertype":34997}},{"payload":{"size":50}}],"mode":"count","count":50,"rate":1000,"auto_start":true}`)
	eventually(t, "b counted the L2 frames", func() bool {
		rs := rxOf(t, b, "l2")
		return len(rs) == 1 && rs[0].Rx == 50
	})
}

func TestPacketPcapReplay(t *testing.T) {
	a, b, _, _ := ipPair(t)
	// A capture of a stream to b: preview packets 0..2 of a UDP config, as frames.
	cfg := `{"layers":[{"ethernet":{"dst":"02:00:00:00:0b:01","src":"02:99:00:00:00:01"}},{"ipv4":{"src":"10.0.0.1","dst":"10.0.0.2"}},{"udp":{"dst_port":7}},{"payload":{"size":30}}]}`
	var frames [][]byte
	for i := 0; i < 3; i++ {
		r, err := a.PreviewProtocol(context.Background(), "packet", "d", json.RawMessage(cfg), map[string]any{"index": float64(i)})
		if err != nil {
			t.Fatal(err)
		}
		j, _ := json.Marshal(r)
		var p struct {
			Hex string `json:"hex"`
		}
		json.Unmarshal(j, &p)
		f, _ := hex.DecodeString(p.Hex)
		frames = append(frames, f)
	}
	pc := binary.LittleEndian.AppendUint32(nil, 0xa1b2c3d4)
	pc = append(pc, 2, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0, 0, 1, 0, 0, 0)
	for _, f := range frames {
		pc = append(pc, make([]byte, 8)...)
		pc = binary.LittleEndian.AppendUint32(pc, uint32(len(f)))
		pc = binary.LittleEndian.AppendUint32(pc, uint32(len(f)))
		pc = append(pc, f...)
	}
	mkPacket(t, a, "replay", `{"pcap":{"data":"`+base64.StdEncoding.EncodeToString(pc)+`","frames":"1-2"},"mode":"count","count":10,"rate":1000,"auto_start":true}`)
	eventually(t, "the replay done", func() bool { return flowStatus(t, a, "replay")["state"] == "done" })
	if s := flowStatus(t, a, "replay"); num(s, "tx") != 10 || s["kind"] != "pcap · 2 frames" {
		t.Errorf("status %v", s)
	}
	// The frames carry a signature (sequences 0 and 1, replayed 5 times each): b counts them.
	eventually(t, "b counted the replay", func() bool {
		rs := rxOf(t, b, "udp")
		return len(rs) == 1 && rs[0].Rx+rs[0].Duplicate >= 10
	})
}

func TestPacketPreview(t *testing.T) {
	a, _, _, _ := ipPair(t)
	r, err := a.PreviewProtocol(context.Background(), "packet", "d",
		json.RawMessage(`{"layers":[{"ipv4":{"dst":"10.0.0.2"}},{"udp":{"dst_port":53}}],"modifiers":[{"field":"ipv4.ttl","mode":"list","values":["1","2"]}]}`),
		map[string]any{"index": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(r)
	var p struct {
		Hex     string `json:"hex"`
		Summary string `json:"summary"`
		Layers  []struct {
			Layer  int    `json:"layer"`
			Title  string `json:"title"`
			Offset int    `json:"offset"`
			Fields []struct {
				Name, Value string
			} `json:"fields"`
		} `json:"layers"`
	}
	json.Unmarshal(j, &p)
	// Ethernet (implicit), device tag, EtherType, IPv4, UDP.
	titles := []string{}
	for _, l := range p.Layers {
		titles = append(titles, l.Title)
	}
	if strings.Join(titles, ",") != "Ethernet,Device VLAN tags (1),EtherType,IPv4,UDP" || p.Layers[3].Offset != 18 {
		t.Fatalf("layers %v (%s)", titles, j)
	}
	for _, f := range p.Layers[3].Fields {
		if f.Name == "ttl" && f.Value != "2" {
			t.Errorf("packet 1 ttl %s", f.Value)
		}
	}
	if !strings.Contains(p.Summary, "IPv4 10.0.0.1 > 10.0.0.2, UDP") || !strings.HasPrefix(p.Hex, "02000000 0b01"[:8]) {
		t.Errorf("summary %q hex %s", p.Summary, p.Hex[:24])
	}
	// Unknown device, type without a preview, bad config.
	if _, err := a.PreviewProtocol(context.Background(), "packet", "nope", nil, nil); err == nil {
		t.Error("unknown device")
	}
	if _, err := a.PreviewProtocol(context.Background(), "icmp", "d", json.RawMessage(`{"destination":"10.0.0.2"}`), nil); err == nil {
		t.Error("icmp has no preview")
	}
	if _, err := a.PreviewProtocol(context.Background(), "packet", "d", json.RawMessage(`{"layers":[{"udp":{}}]}`), nil); err == nil {
		t.Error("bad layers accepted")
	}
	// An unresolvable next hop: shown toward broadcast, with a note.
	r, err = a.PreviewProtocol(context.Background(), "packet", "d", json.RawMessage(`{"layers":[{"ipv4":{"dst":"10.0.0.99"}},{"udp":{}}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := json.Marshal(r); !strings.Contains(string(j), `"note":"next hop unresolved`) || !strings.Contains(string(j), `"hex":"ffffffffffff`) {
		t.Errorf("unresolved: %s", j)
	}
}
