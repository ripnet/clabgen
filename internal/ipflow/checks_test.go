package ipflow

import (
	"testing"

	"github.com/ripnet/clabgen/internal/flowstats"
)

func TestChecks(t *testing.T) {
	rtt := &flowstats.RTT{Avgus: 2000, Maxus: 9000, Jitterus: 500}
	good := flowstats.Stats{Tx: 100, Rx: 100, LossPct: 0, RTT: rtt}
	bad := flowstats.Stats{Tx: 100, Rx: 95, Lost: 5, LossPct: 5, OutageMaxMs: 120, RTT: rtt}
	c := Checks{MaxLossPct: 1, MaxOutageMs: 50, MaxRTTAvgMs: 5, MaxRTTMs: 10, MaxJitterMs: 1}

	if v := (Checks{}).Evaluate(good, true); v != nil {
		t.Errorf("no checks: %+v", v)
	}
	if v := c.Evaluate(good, true); v.Result != VerdictPass || len(v.Results) != 5 || v.Failed != "" {
		t.Errorf("good: %+v", v)
	}
	v := c.Evaluate(bad, true)
	if v.Result != VerdictFail || v.Failed != "loss_pct 5 > 1, outage_ms 120 > 50" {
		t.Errorf("bad: %+v", v)
	}
	// A current outage counts before it ends.
	if v := (Checks{MaxOutageMs: 50}).Evaluate(flowstats.Stats{Tx: 10, Rx: 5, Lost: 5, OutageCurrentMs: 80}, true); v.Result != VerdictFail {
		t.Errorf("current outage: %+v", v)
	}
	// Nothing answered or lost yet: pending.
	if v := c.Evaluate(flowstats.Stats{Tx: 3}, true); v.Result != VerdictPending {
		t.Errorf("pending: %+v", v)
	}
	// One way: loss and RTT are the receivers', so nothing to judge here.
	if v := c.Evaluate(good, false); v.Result != VerdictPending || !v.Results[0].Unavailable {
		t.Errorf("one way: %+v", v)
	}
	if err := (Checks{MaxLossPct: 101}).Validate(); err == nil {
		t.Error("loss over 100% accepted")
	}
}
