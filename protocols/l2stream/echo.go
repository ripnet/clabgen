package l2stream

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ripnet/clabgen/internal/flowrx"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

// l2-echo sends l2 stream frames that ask for it (reply echo) back to
// their source MAC, marked as replies (so two responders never bounce a
// frame between them).

const EchoTypeName = "l2-echo"

func init() { protocol.Register(echoType{}) }

type EchoConfig struct {
	EtherTypes []uint16 `json:"ethertypes" jsonschema:"description=Ethertypes to answer (the streams'). Default 0x88b5"`
}

func (c *EchoConfig) Validate() error {
	if len(c.EtherTypes) == 0 {
		return errors.New("at least one ethertype")
	}
	for _, et := range c.EtherTypes {
		if n, ok := reserved[et]; ok || et < 0x0600 {
			return fmt.Errorf("ethertype %#04x can't be echoed (%s)", et, map[bool]string{true: n, false: "a length"}[ok])
		}
	}
	return nil
}

type echoType struct{}

func (echoType) Name() string { return EchoTypeName }
func (echoType) Description() string {
	return "Raw L2 echo responder: sends l2 stream frames (reply echo) back to their sender"
}
func (echoType) NewConfig() any       { return &EchoConfig{EtherTypes: []uint16{DefaultEtherType}} }
func (echoType) EtherTypes() []uint16 { return nil }
func (echoType) EtherTypesFor(c any) []uint16 {
	return c.(*EchoConfig).EtherTypes
}
func (echoType) Actions() []protocol.Action {
	return []protocol.Action{{Name: "clear", Description: "Reset counters"}}
}

type echo struct {
	env protocol.Env

	mu           sync.Mutex
	rx, tx, txEr uint64
	lastFrom     pkt.MAC
}

func (echoType) Start(env protocol.Env, _ any) (protocol.Session, error) {
	return &echo{env: env}, nil
}

func (e *echo) HandleRx(fr *pkt.Frame) {
	// Echo requests addressed to us (or broadcast / multicast).
	if fr.Dst != e.env.Device().MAC() && !fr.Dst.IsMulticast() || len(fr.Payload) < headerLen || fr.Payload[flowrx.SigLen] != kindRequest {
		return
	}
	if _, _, _, ok := flowrx.ParseSignature(fr.Payload); !ok {
		return
	}
	out := append([]byte(nil), fr.Payload...)
	out[flowrx.SigLen] = kindReply
	opt := protocol.SendOpts{}
	if len(fr.Tags) > 0 {
		opt = protocol.SendOpts{SetPCP: true, PCP: fr.Tags[0].PCP} // keep the request's priority
	}
	err := e.env.Send(fr.Src, fr.EtherType, out, opt)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rx++
	e.lastFrom = fr.Src
	if err != nil {
		e.txEr++
		return
	}
	e.tx++
}

type EchoStatus struct {
	Received uint64 `json:"received"`
	Sent     uint64 `json:"sent"`
	TxErrors uint64 `json:"tx_errors"`
	LastFrom string `json:"last_from,omitempty"`
}

func (e *echo) Status() any {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := EchoStatus{Received: e.rx, Sent: e.tx, TxErrors: e.txEr}
	if !e.lastFrom.IsZero() {
		s.LastFrom = e.lastFrom.String()
	}
	return s
}

func (e *echo) Action(_ context.Context, name string, _ any) (any, error) {
	if name != "clear" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	e.mu.Lock()
	e.rx, e.tx, e.txEr = 0, 0, 0
	e.mu.Unlock()
	return map[string]any{"cleared": true}, nil
}

func (e *echo) Stop() {}
