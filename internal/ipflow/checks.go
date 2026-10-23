package ipflow

import (
	"errors"
	"fmt"

	"github.com/ripnet/clabgen/internal/flowstats"
)

// Checks make a flow pass or fail: limits on what it measured since it
// started (or was cleared). Unset limits aren't checked; a flow with none
// has no verdict.
type Checks struct {
	MaxLossPct  float64 `json:"max_loss_pct,omitempty" jsonschema:"minimum=0,maximum=100,description=Highest loss in percent (replies missing)"`
	MaxOutageMs float64 `json:"max_outage_ms,omitempty" jsonschema:"minimum=0,description=Longest outage (consecutive losses) in ms"`
	MaxRTTAvgMs float64 `json:"max_rtt_avg_ms,omitempty" jsonschema:"minimum=0,description=Highest average round-trip time in ms"`
	MaxRTTMs    float64 `json:"max_rtt_ms,omitempty" jsonschema:"minimum=0,description=Highest single round-trip time in ms"`
	MaxJitterMs float64 `json:"max_jitter_ms,omitempty" jsonschema:"minimum=0,description=Highest jitter (mean RTT change between packets) in ms"`
}

func (c Checks) Validate() error {
	for _, v := range []float64{c.MaxLossPct, c.MaxOutageMs, c.MaxRTTAvgMs, c.MaxRTTMs, c.MaxJitterMs} {
		if v < 0 {
			return errors.New("checks: limits must not be negative")
		}
	}
	if c.MaxLossPct > 100 {
		return errors.New("checks: max_loss_pct must be 0-100")
	}
	return nil
}

func (c Checks) any() bool { return c != Checks{} }

// Verdicts.
const (
	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictPending = "pending" // nothing measured yet
)

// CheckResult is one limit and what the flow measured.
type CheckResult struct {
	Check string  `json:"check"`
	Limit float64 `json:"limit"`
	Value float64 `json:"value"`
	Pass  bool    `json:"pass"`
	// Unavailable: the flow can't measure it (e.g. RTT on a one-way flow).
	Unavailable bool `json:"unavailable,omitempty"`
}

// Verdict is a flow's checks against its statistics.
type Verdict struct {
	Result  string        `json:"result"` // pass, fail or pending
	Results []CheckResult `json:"checks"`
	Failed  string        `json:"failed,omitempty"` // the failed checks, for summaries
}

// Evaluate checks st. tracksReplies says whether loss and RTT are known
// (one-way flows are measured at their receivers instead).
func (c Checks) Evaluate(st flowstats.Stats, tracksReplies bool) *Verdict {
	if !c.any() {
		return nil
	}
	v := &Verdict{Result: VerdictPass, Results: []CheckResult{}}
	add := func(name string, limit, value float64, known bool) {
		if limit == 0 {
			return
		}
		r := CheckResult{Check: name, Limit: limit, Value: value, Pass: true, Unavailable: !known}
		if known && value > limit {
			r.Pass = false
		}
		v.Results = append(v.Results, r)
	}
	var avg, maxRTT, jitter float64
	if st.RTT != nil {
		avg, maxRTT, jitter = st.RTT.Avgus/1000, st.RTT.Maxus/1000, st.RTT.Jitterus/1000
	}
	add("loss_pct", c.MaxLossPct, st.LossPct, tracksReplies)
	add("outage_ms", c.MaxOutageMs, max(st.OutageMaxMs, st.OutageCurrentMs), tracksReplies)
	add("rtt_avg_ms", c.MaxRTTAvgMs, avg, tracksReplies && st.RTT != nil)
	add("rtt_ms", c.MaxRTTMs, maxRTT, tracksReplies && st.RTT != nil)
	add("jitter_ms", c.MaxJitterMs, jitter, tracksReplies && st.RTT != nil)
	known := 0
	for _, r := range v.Results {
		if r.Unavailable {
			continue
		}
		known++
		if !r.Pass {
			v.Result = VerdictFail
			if v.Failed != "" {
				v.Failed += ", "
			}
			v.Failed += fmt.Sprintf("%s %.4g > %.4g", r.Check, r.Value, r.Limit)
		}
	}
	// Before anything was answered (or lost) there is nothing to judge.
	if v.Result == VerdictPass && (known == 0 || st.Rx+st.Lost == 0) {
		v.Result = VerdictPending
	}
	return v
}
