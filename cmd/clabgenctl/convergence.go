package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/ripnet/clabgen/gen/clabgen/v1"
	"github.com/ripnet/clabgen/gen/clabgen/v1/clabgenv1connect"
)

// convFlow is one flow's report from GetConvergence, with its agent.
type convFlow struct {
	Agent        string        `json:"agent"`
	Side         string        `json:"side"`
	Name         string        `json:"name,omitempty"`
	Device       string        `json:"device"`
	FlowID       string        `json:"flow_id"`
	OneWay       bool          `json:"one_way"`
	Running      bool          `json:"running"`
	Status       string        `json:"status"`
	LossMs       float64       `json:"loss_ms"`
	Lost         uint64        `json:"lost"`
	FirstLossMs  *float64      `json:"first_loss_ms,omitempty"`
	LastLossMs   *float64      `json:"last_loss_ms,omitempty"`
	CleanMs      float64       `json:"clean_ms"`
	ResolutionMs float64       `json:"resolution_ms"`
	Truncated    bool          `json:"truncated,omitempty"`
	Outages      []convSpan    `json:"outages"`
	Prefixes     *convPrefixes `json:"prefixes,omitempty"`
}

// convPrefixes is a flow to many prefixes: how many lost traffic, and which.
type convPrefixes struct {
	Total    int     `json:"total"`
	Impacted int     `json:"impacted"`
	ProbeMs  float64 `json:"probe_ms"`
	List     []struct {
		Prefix      string  `json:"prefix"`
		Lost        uint64  `json:"lost"`
		LossMs      float64 `json:"loss_ms"`
		FirstLossMs float64 `json:"first_loss_ms"`
		LastLossMs  float64 `json:"last_loss_ms"`
		Ongoing     bool    `json:"ongoing"`
	} `json:"list"`
}

type convSpan struct {
	StartMs float64 `json:"start_ms"`
	EndMs   float64 `json:"end_ms"`
	Lost    uint64  `json:"lost"`
}

type convEvent struct {
	Agent   string  `json:"agent"`
	AtMs    float64 `json:"at_ms"`
	Source  string  `json:"source"`
	Kind    string  `json:"kind"`
	Message string  `json:"message"`
}

// noReceiver: a one-way flow that no agent here receives: not measured.
const noReceiver = "no_receiver"

// mergeConvergence picks each flow's measurement: an echo flow's from its
// sender, a one-way flow's from its receivers (it can't know its own loss).
func mergeConvergence(rows []convFlow) []convFlow {
	senders := map[string]convFlow{}
	received := map[string]bool{}
	for _, r := range rows {
		if r.Side == "sender" {
			senders[r.FlowID] = r
		} else {
			received[r.FlowID] = true
		}
	}
	out := []convFlow{}
	for _, r := range rows {
		s, known := senders[r.FlowID]
		switch {
		case r.Side == "sender" && !r.OneWay:
			out = append(out, r)
		case r.Side == "sender":
			if !received[r.FlowID] {
				if r.Running {
					r.Status = noReceiver
				}
				out = append(out, r)
			}
		case !known:
			out = append(out, r)
		case s.OneWay:
			r.Name = s.Name
			if !s.Running {
				r.Status = "stopped" // quiet because it stopped, not an outage
			}
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Name+"/"+a.Agent+"/"+a.Device < b.Name+"/"+b.Agent+"/"+b.Device
	})
	return out
}

// convergenceDone says the flows have settled: each is stopped, unmeasured,
// or known clean for settle.
func convergenceDone(flows []convFlow, settle time.Duration) bool {
	for _, f := range flows {
		if f.Status != "stopped" && f.Status != noReceiver &&
			(f.Status == "not_restored" || f.CleanMs < float64(settle.Milliseconds())) {
			return false
		}
	}
	return true
}

// markUnsettled is for the end of a test that gave up: a flow still losing a
// packet now and then (one path of several) hasn't been restored.
func markUnsettled(flows []convFlow, settle time.Duration) {
	for i, f := range flows {
		if f.Status == "restored" && f.CleanMs < float64(settle.Milliseconds()) {
			flows[i].Status = "not_restored"
		}
	}
}

type convSummary struct {
	Flows       int     `json:"flows"`
	Impacted    int     `json:"impacted"`
	NotRestored int     `json:"not_restored"`
	WorstMs     float64 `json:"worst_ms"`
	MedianMs    float64 `json:"median_ms"`
	P95Ms       float64 `json:"p95_ms"`
}

// summarize takes loss durations over the flows measured (nearest rank).
func summarize(flows []convFlow) convSummary {
	var s convSummary
	var loss []float64
	for _, f := range flows {
		if f.Status == "stopped" || f.Status == noReceiver {
			continue
		}
		loss = append(loss, f.LossMs)
		if f.Lost > 0 {
			s.Impacted++
		}
		if f.Status == "not_restored" {
			s.NotRestored++
		}
	}
	s.Flows = len(loss)
	if s.Flows == 0 {
		return s
	}
	slices.Sort(loss)
	rank := func(p float64) float64 { return loss[max(0, int(math.Ceil(p*float64(len(loss))))-1)] }
	s.WorstMs, s.MedianMs, s.P95Ms = loss[len(loss)-1], rank(0.5), rank(0.95)
	return s
}

// convAgent is an agent in the measurement, with the moment by its clock.
type convAgent struct {
	name  string
	c     clabgenv1connect.AgentServiceClient
	since *timestamppb.Timestamp
}

// poll asks every agent at once; with mark, each one's now becomes its
// moment. since unset asks about the last few seconds.
func pollConvergence(ctx context.Context, agents []*convAgent, ids []string, mark bool) ([]convFlow, []convEvent, error) {
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		rows   []convFlow
		events []convEvent
		first  error
	)
	for _, a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := &v1.GetConvergenceRequest{FlowIds: ids}
			if !mark {
				req.Since = a.since
			}
			r, err := a.c.GetConvergence(ctx, connect.NewRequest(req))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				first = errors.Join(first, fmt.Errorf("%s: %w", a.name, err))
				return
			}
			if mark {
				a.since = r.Msg.Now
			}
			for _, st := range r.Msg.Flows {
				b, _ := json.Marshal(st.AsMap())
				f := convFlow{Agent: a.name}
				if err := json.Unmarshal(b, &f); err != nil {
					first = errors.Join(first, err)
					return
				}
				f.Agent = a.name
				rows = append(rows, f)
			}
			for _, e := range r.Msg.Events {
				events = append(events, convEvent{Agent: a.name, AtMs: float64(e.Time.AsTime().Sub(a.since.AsTime())) / float64(time.Millisecond),
					Source: e.Source, Kind: e.Kind, Message: e.Message})
			}
		}()
	}
	wg.Wait()
	sort.Slice(events, func(i, j int) bool { return events[i].AtMs < events[j].AtMs })
	return rows, events, first
}

type convPhase struct {
	Phase   string      `json:"phase"` // "trigger" or "restore"
	Command string      `json:"command,omitempty"`
	Summary convSummary `json:"summary"`
	Flows   []convFlow  `json:"flows"`
	Events  []convEvent `json:"events"`
}

// runPhase marks the moment, runs the command, and waits for the flows to
// settle. With no command something else causes the failure, so it also
// waits for loss to appear.
func runPhase(ctx context.Context, name, command string, agents []*convAgent, ids []string, settle, maxWait time.Duration) (*convPhase, error) {
	if _, _, err := pollConvergence(ctx, agents, ids, true); err != nil {
		return nil, err
	}
	if command != "" {
		c := exec.CommandContext(ctx, "sh", "-c", command)
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		// The command's clabgenctl finds the same lab.
		c.Env = append(os.Environ(), "CLABGEN_LAB="+labArg)
		if err := c.Run(); err != nil {
			return nil, fmt.Errorf("%s command: %w", name, err)
		}
	}
	end := time.Now().Add(maxWait)
	for {
		rows, events, err := pollConvergence(ctx, agents, ids, false)
		if err != nil {
			return nil, err
		}
		flows := mergeConvergence(rows)
		stop := time.Now().After(end) || ctx.Err() != nil
		if stop {
			markUnsettled(flows, settle)
		}
		s := summarize(flows)
		if stop || convergenceDone(flows, settle) && (command != "" || s.Impacted > 0) {
			return &convPhase{Phase: name, Command: command, Summary: s, Flows: flows, Events: events}, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func fmtMs(v float64) string {
	if v >= 10000 {
		return fmt.Sprintf("%.1f s", v/1000)
	}
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f ms", v)
	}
	return fmt.Sprintf("%.1f ms", v)
}

func printPhase(p *convPhase) {
	title := p.Phase
	if p.Command != "" {
		title += ": " + p.Command
	}
	fmt.Println(strings.ToUpper(title[:1]) + title[1:])
	w := table()
	fmt.Fprintln(w, "FLOW\tDEVICE\tSTATUS\tLOSS\tLOST\tFIRST LOSS\tLAST LOSS\tRESOLUTION")
	opt := func(v *float64) string {
		if v == nil {
			return "-"
		}
		return "+" + fmtMs(*v)
	}
	for _, f := range p.Flows {
		name := f.Name
		if name == "" {
			name = f.FlowID
		}
		status := strings.ReplaceAll(f.Status, "_", " ")
		if f.Truncated {
			status += " (at least)"
		}
		fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\t%d\t%s\t%s\t%s\n", name, f.Agent, f.Device, status, fmtMs(f.LossMs), f.Lost, opt(f.FirstLossMs), opt(f.LastLossMs), fmtMs(f.ResolutionMs))
	}
	w.Flush()
	s := p.Summary
	fmt.Printf("Loss duration: worst %s, median %s, p95 %s (%d flows, %d lost traffic, %d not restored)\n",
		fmtMs(s.WorstMs), fmtMs(s.MedianMs), fmtMs(s.P95Ms), s.Flows, s.Impacted, s.NotRestored)
	for _, f := range p.Flows {
		x := f.Prefixes
		if x == nil || x.Impacted == 0 {
			continue
		}
		fmt.Printf("%s: %d of %d prefixes lost traffic (each probed every %s):\n", f.Name, x.Impacted, x.Total, fmtMs(x.ProbeMs))
		for _, e := range x.List {
			until := "+" + fmtMs(e.LastLossMs)
			if e.Ongoing {
				until = "still losing"
			}
			fmt.Printf("  %-22s %8s  %d lost, +%s to %s\n", e.Prefix, fmtMs(e.LossMs), e.Lost, fmtMs(e.FirstLossMs), until)
		}
	}
	if len(p.Events) > 0 {
		fmt.Println("Events:")
		for _, e := range p.Events {
			fmt.Printf("  +%s  %s %s: %s\n", fmtMs(e.AtMs), e.Agent, e.Source, e.Message)
		}
	}
	fmt.Println()
}

// convergenceAgents is the whole lab with --lab, else the agent given by -s.
func convergenceAgents() ([]*convAgent, error) {
	if labArg == "" {
		return []*convAgent{{name: serverAddr, c: client()}}, nil
	}
	lab, _, err := labOf(labArg)
	if err != nil {
		return nil, err
	}
	nodes, err := labNodes(lab)
	if err != nil {
		return nil, err
	}
	var out []*convAgent
	for n, addr := range nodes {
		c := clientFor(addr)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := c.GetInfo(ctx, connect.NewRequest(&v1.GetInfoRequest{}))
		cancel()
		if err == nil { // other nodes of the lab aren't clabgen agents
			out = append(out, &convAgent{name: n, c: c})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no clabgen agents in lab %s", lab)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// checkClean refuses a measurement of flows that aren't running cleanly,
// whose loss it couldn't tell from what the trigger causes.
func checkClean(flows []convFlow) error {
	var bad []string
	running := 0
	for _, f := range flows {
		name := f.Name
		if name == "" {
			name = f.FlowID
		}
		switch {
		case f.Status == "stopped":
		case f.Status == noReceiver:
			running++
			bad = append(bad, name+" has no receiver in this lab, so it can't be measured")
		case f.Lost > 0:
			running++
			bad = append(bad, fmt.Sprintf("%s lost %d packets in the last 5 s", name, f.Lost))
		default:
			running++
		}
	}
	if running == 0 {
		return errors.New("no flows running: start them first")
	}
	if len(bad) > 0 {
		return errors.New("flows aren't clean (--force measures anyway):\n  " + strings.Join(bad, "\n  "))
	}
	return nil
}

func convergenceCmd() *cobra.Command {
	var trigger, restore string
	var settle, maxWait time.Duration
	var force bool
	cmd := &cobra.Command{
		Use:   "convergence [FLOW...]",
		Short: "Measure what a failure costs running flows: loss duration per flow",
		Long: `Measure what a failure (and optionally the repair) costs running flows:
loss duration per flow (lost packets × send interval), when the loss began
and ended, and the agent's events meanwhile. Each command runs in a shell;
any failure you can script will do. With --lab it covers every agent in the
lab, otherwise the agent given by -s. Statistics and checks are left alone.

Without --trigger, make the failure yourself (on the router, say): it waits
for loss, then for the flows to settle, or for --max-wait or Ctrl-C.

--settle must be longer than the failure takes to reach traffic: a silent
failure that a 90 s hold timer detects needs --settle 100s.`,
		Example: `  clabgenctl --lab b2b convergence --trigger 'clabgenctl -s gen2 port down eth1' --restore 'clabgenctl -s gen2 port up eth1'
  clabgenctl --lab b2b convergence ping1 ping2 --settle 10s`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			agents, err := convergenceAgents()
			if err != nil {
				return err
			}
			rows, _, err := pollConvergence(ctx, agents, nil, true)
			if err != nil {
				return err
			}
			var ids []string
			for _, want := range args {
				found := false
				for _, r := range rows {
					if r.Side == "sender" && (r.Name == want || r.FlowID == want || r.Agent+"/"+r.Name == want) {
						ids, found = append(ids, r.FlowID), true
					}
				}
				if !found {
					return fmt.Errorf("no flow %q", want)
				}
			}
			if len(ids) > 0 {
				rows = slices.DeleteFunc(rows, func(r convFlow) bool { return !slices.Contains(ids, r.FlowID) })
			}
			flows := mergeConvergence(rows)
			if err := checkClean(flows); err != nil && !force {
				return err
			}
			if trigger == "" {
				fmt.Fprintln(os.Stderr, "Measuring: make the failure now.")
			}
			phases := []*convPhase{}
			p, err := runPhase(ctx, "trigger", trigger, agents, ids, settle, maxWait)
			if err != nil {
				return err
			}
			phases = append(phases, p)
			if restore != "" {
				p, err := runPhase(ctx, "restore", restore, agents, ids, settle, maxWait)
				if err != nil {
					return err
				}
				phases = append(phases, p)
			}
			if outFormat == "json" {
				b, err := json.Marshal(phases)
				if err != nil {
					return err
				}
				return printJSON(b)
			}
			for _, p := range phases {
				printPhase(p)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&trigger, "trigger", "", "shell command that causes the failure (default: you do, elsewhere)")
	f.StringVar(&restore, "restore", "", "shell command that repairs it, measured again")
	f.DurationVar(&settle, "settle", 5*time.Second, "how long flows must stay clean to count as settled")
	f.DurationVar(&maxWait, "max-wait", 60*time.Second, "report anyway after this long")
	f.BoolVar(&force, "force", false, "measure even if flows aren't clean to start with")
	return cmd
}
