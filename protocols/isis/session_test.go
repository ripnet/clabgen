package isis

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// fakeEnv captures what a session sends.
type fakeEnv struct {
	mu   sync.Mutex
	sent []*PDU
}

func (e *fakeEnv) InstanceName() string    { return "t" }
func (e *fakeEnv) Device() protocol.Device { return e }
func (e *fakeEnv) IP() protocol.IP         { return nil }
func (e *fakeEnv) Attach(string, func(*pkt.Frame)) (protocol.Attachment, error) {
	return nil, errors.New("no devices")
}
func (e *fakeEnv) Log() *slog.Logger                   { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (e *fakeEnv) Emit(string, string, map[string]any) {}
func (e *fakeEnv) Siblings() []protocol.Session        { return nil }
func (e *fakeEnv) Name() string                        { return "d" }
func (e *fakeEnv) Port() string                        { return "eth1" }
func (e *fakeEnv) MAC() pkt.MAC                        { return pkt.MAC{2, 0, 0, 0, 0, 1} }
func (e *fakeEnv) VLANs() []pkt.VLANTag                { return nil }
func (e *fakeEnv) PortUp() bool                        { return true }
func (e *fakeEnv) Send(_ pkt.MAC, _ uint16, b []byte, _ protocol.SendOpts) error {
	if p, err := Decode(b); err == nil {
		e.mu.Lock()
		e.sent = append(e.sent, p)
		e.mu.Unlock()
	}
	return nil
}

// lastLSP returns the latest copy of id we sent.
func (e *fakeEnv) lastLSP(id LSPID) *LSP {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.sent) - 1; i >= 0; i-- {
		if l := e.sent[i].LSP; l != nil && l.ID == id {
			return l
		}
	}
	return nil
}

var peerMAC = pkt.MAC{2, 0, 0, 0, 0, 2}

func rx(s *session, pdu []byte) {
	s.HandleRx(&pkt.Frame{Dst: AllISs, Src: peerMAC, EtherType: pkt.EtherTypeLLC, Payload: frame(pdu)})
}

func wait(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

// Someone purges our LSP at the sequence number we hold: we take it over
// with a higher one (for us and for simulated routers).
func TestPurgeOfOwnLSPAtSameSeq(t *testing.T) {
	for _, auth := range []Auth{{Type: "none"}, {Type: "hmac-sha256", Key: "k", KeyID: 1}} {
		env := &fakeEnv{}
		cfg := defaultConfig()
		cfg.SystemID, cfg.HelloInterval, cfg.LSPAuth, cfg.HelloAuth = "0000.0000.0001", 1, auth, auth
		cfg.Topology = Topology{Kind: "line", Nodes: 1}
		ss, err := isisType{}.Start(env, cfg)
		if err != nil {
			t.Fatal(err)
		}
		s := ss.(*session)
		defer s.Stop()
		// Bring the adjacency up: the peer reports Up toward us.
		hello := func(state uint8) []byte {
			return auth.sign(EncodeP2PHello(circuitL2, sysB, 30, 1, [][]byte{area}, nil, nil, nil,
				ThreeWay{State: state, ExtCircuit: 1, NeighborSys: s.sys, HasNeighbor: state != AdjDown, NeighborExtCircuit: 1}, 0))
		}
		rx(s, hello(AdjDown))
		rx(s, hello(AdjUp))
		wait(t, "adjacency up", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.circs[0].adj.state == stUp })
		for _, sys := range []SysID{s.sys, s.topo.nodes[0].sys} {
			id := MakeLSPID(sys, 0, 0)
			wait(t, "our LSP sent", func() bool { return env.lastLSP(id) != nil })
			seq := env.lastLSP(id).Seq
			// The peer purges it at the same sequence number.
			rx(s, auth.sign(purgeRaw(2, id, seq, lspTypeL1L2)))
			wait(t, "taken over", func() bool { l := env.lastLSP(id); return l.Seq == seq+1 && l.Lifetime > 0 })
		}
	}
}
