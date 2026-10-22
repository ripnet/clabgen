package core_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pkt"
)

func mkMEP(t *testing.T, e *core.Engine, name, cfg string) {
	t.Helper()
	if _, err := e.CreateProtocol(core.ProtocolSpec{Name: name, Type: "cfm-mep", Device: "d", Config: json.RawMessage(cfg)}); err != nil {
		t.Fatal(err)
	}
}

func mepRun(t *testing.T, e *core.Engine, name, action, args string) map[string]any {
	t.Helper()
	r, err := e.RunAction(context.Background(), name, action, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	b, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func firstOf(st map[string]any, k string) map[string]any {
	l, _ := st[k].([]any)
	if len(l) == 0 {
		return nil
	}
	return l[0].(map[string]any)
}

// dropEvery drops every nth data (non-CFM) frame.
func dropEvery(n int) func([]byte) bool {
	var c atomic.Int64
	return func(b []byte) bool {
		f, err := pkt.Decode(b)
		if err != nil || f.EtherType == pkt.EtherTypeCFM {
			return false
		}
		return c.Add(1)%int64(n) == 0
	}
}

func TestCFMY1731(t *testing.T) {
	a, b, wa, _ := ipPair(t)
	mkMEP(t, a, "m", `{"level":3,"mep_id":1,"remote_meps":[2],"ccm_interval":"100ms","ais":{"client_level":4}}`)
	mkMEP(t, b, "m", `{"level":3,"mep_id":2,"remote_meps":[1],"ccm_interval":"100ms","ais":{"enabled":true,"client_level":5},
		"sender_id":"gen-b","org_tlv":{"oui":"00:16:4d","subtype":1,"value":"cafe"},"ccm_padding":20}`)
	// Client-level MEPs: AIS from b reaches a's level 5, LCK from a b's level 4.
	mkMEP(t, a, "c5", `{"level":5,"mep_id":11,"remote_meps":[12],"ccm_enabled":false}`)
	mkMEP(t, b, "c4", `{"level":4,"mep_id":21,"remote_meps":[22],"ccm_enabled":false}`)
	eventually(t, "up", func() bool { return rmepOK(t, a, "m") && rmepOK(t, b, "m") })

	// TLVs in b's CCMs.
	r := firstOf(mepStatus(t, a, "m"), "remote_meps")
	if r["sender_id"] != "gen-b" || r["org_tlv"] != "00:16:4d/1 cafe" || num(r, "ccm_padding") != 20 {
		t.Errorf("remote MEP TLVs: %v", r)
	}

	// 1DM and TST, counted at b.
	mepRun(t, a, "m", "one-way-delay", `{"target_mep_id":2,"count":5,"interval_ms":5}`)
	mepRun(t, a, "m", "test-signal", `{"target_mep_id":2,"count":50,"interval_ms":1,"size":100,"pattern":"prbs","crc":true}`)
	eventually(t, "1DM and TST received", func() bool {
		st := mepStatus(t, b, "m")
		o, s := firstOf(st, "one_way_delay"), firstOf(st, "test_signal")
		return o != nil && num(o, "count") == 5 && s != nil && num(s, "received") == 50
	})
	st := mepStatus(t, b, "m")
	if s := firstOf(st, "test_signal"); num(s, "bit_errors") != 0 || num(s, "lost") != 0 || num(s, "pattern_bytes") != 5000 {
		t.Errorf("test signal %v", s)
	}
	if o := firstOf(st, "one_way_delay"); num(o, "max_us") <= 0 || num(o, "max_us") > 1e6 {
		t.Errorf("one-way delay %v", o)
	}

	// LMM over data frames: a pings b at 200 pps; every 10th frame from a
	// is lost on the wire.
	mkFlow(t, a, "ping", `{"destination":"10.0.0.2","rate":200,"auto_start":true}`)
	time.Sleep(300 * time.Millisecond)
	wa.mu.Lock()
	wa.drop = dropEvery(10)
	wa.mu.Unlock()
	lr := mepRun(t, a, "m", "loss", `{"target_mep_id":2,"count":11,"interval_ms":100,"timeout_ms":500}`)
	wa.mu.Lock()
	wa.drop = nil
	wa.mu.Unlock()
	if p := num(lr, "far_end_loss_pct"); p < 7 || p > 13 || num(lr, "near_end_loss") != 0 || num(lr, "far_end_tx") < 150 {
		t.Errorf("loss: %v", lr)
	}

	// AIS: a stops CCMs, b loses it and sends AIS to level 5; it stops
	// once CCMs are back.
	mepRun(t, a, "m", "fault", `{"ccm_enabled":false}`)
	eventuallyLong(t, "AIS at a's level 5", func() bool {
		return mepStatus(t, b, "m")["ais_sending"] == true && mepStatus(t, a, "c5")["ais_received"] == true
	})
	mepRun(t, a, "m", "fault", `{"ccm_enabled":true}`)
	eventuallyLong(t, "AIS stops", func() bool { return mepStatus(t, b, "m")["ais_sending"] == false })

	// LCK and CSF from a.
	mepRun(t, a, "m", "fault", `{"lock":true,"csf":"los"}`)
	eventuallyLong(t, "LCK at b's level 4, CSF at b", func() bool {
		c, _ := mepStatus(t, b, "m")["csf_received"].(map[string]any)
		return mepStatus(t, b, "c4")["lck_received"] == true && c != nil && c["type"] == "los"
	})
	mepRun(t, a, "m", "fault", `{"lock":false,"csf":"none"}`)
}

func TestCFMSessions(t *testing.T) {
	a, b, wa, _ := ipPair(t)
	mkMEP(t, b, "m", `{"level":3,"mep_id":2,"remote_meps":[1],"ccm_interval":"100ms"}`)
	mkMEP(t, a, "m", `{"level":3,"mep_id":1,"remote_meps":[2],"ccm_interval":"100ms","sessions":[
		{"name":"dm","type":"delay","target_mep_id":2,"interval_ms":50,"bin_s":1,"bins":2},
		{"name":"sl","type":"synthetic-loss","target_mep_id":2,"interval_ms":20,"bin_s":1,"bins":2},
		{"name":"lm","type":"loss","target_mep_id":2,"interval_ms":50,"bin_s":1,"bins":2}]}`)
	mkFlow(t, a, "ping", `{"destination":"10.0.0.2","rate":200,"auto_start":true}`)
	wa.mu.Lock()
	wa.drop = dropEvery(5) // 20% of a's data frames (SLMs are CFM: not dropped)
	wa.mu.Unlock()
	sessions := func() map[string]map[string]any {
		out := map[string]map[string]any{}
		ss, _ := mepStatus(t, a, "m")["sessions"].([]any)
		for _, x := range ss {
			m := x.(map[string]any)
			out[m["name"].(string)] = m
		}
		return out
	}
	eventuallyLong(t, "two bins each", func() bool {
		for _, s := range sessions() {
			if h, _ := s["history"].([]any); len(h) < 2 {
				return false
			}
		}
		return len(sessions()) == 3
	})
	ss := sessions()
	last := func(n string) map[string]any { return ss[n]["history"].([]any)[0].(map[string]any) }
	if d, _ := last("dm")["delay"].(map[string]any); d == nil || num(last("dm"), "received") < 15 {
		t.Errorf("delay bin %v", last("dm"))
	}
	if b := last("sl"); num(b, "received") < 40 || num(b, "far_end_loss") != 0 {
		t.Errorf("synthetic loss bin %v", b)
	}
	if b := last("lm"); num(b, "far_end_loss_pct") < 12 || num(b, "far_end_loss_pct") > 28 || num(b, "near_end_loss") != 0 {
		t.Errorf("loss bin %v", b)
	}
	if h, _ := ss["dm"]["history"].([]any); len(h) > 2 {
		t.Errorf("%d bins kept", len(h))
	}
	_ = b
}
