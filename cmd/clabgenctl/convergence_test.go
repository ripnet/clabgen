package main

import (
	"testing"
	"time"
)

func TestMergeConvergence(t *testing.T) {
	rows := []convFlow{
		{Side: "sender", Name: "echo", FlowID: "a", Status: "restored", Running: true},
		{Side: "receiver", Device: "far", FlowID: "a", OneWay: true, Status: "restored"}, // the echo's outbound leg
		{Side: "sender", Name: "oneway", FlowID: "b", OneWay: true, Status: "no_impact", Running: true},
		{Side: "receiver", Device: "far", FlowID: "b", OneWay: true, Status: "restored", Lost: 5},
		{Side: "sender", Name: "nobody", FlowID: "c", OneWay: true, Status: "no_impact", Running: true},
		{Side: "sender", Name: "halted", FlowID: "d", OneWay: true, Status: "stopped"},
		{Side: "receiver", Device: "far", FlowID: "d", OneWay: true, Status: "not_restored"},
		{Side: "receiver", Device: "far", FlowID: "e", OneWay: true, Status: "restored"}, // sender elsewhere
	}
	got := map[string]convFlow{}
	for _, f := range mergeConvergence(rows) {
		got[f.FlowID] = f
	}
	if len(got) != 5 {
		t.Fatalf("%+v", got)
	}
	if f := got["a"]; f.Side != "sender" {
		t.Errorf("echo measured by %s", f.Side)
	}
	if f := got["b"]; f.Side != "receiver" || f.Name != "oneway" || f.Lost != 5 {
		t.Errorf("one-way: %+v", f)
	}
	if got["c"].Status != noReceiver {
		t.Errorf("no receiver: %+v", got["c"])
	}
	if f := got["d"]; f.Side != "receiver" || f.Status != "stopped" {
		t.Errorf("stopped sender: %+v", f)
	}
}

func TestSummarizeAndDone(t *testing.T) {
	flows := []convFlow{
		{Status: "restored", LossMs: 100, Lost: 100, CleanMs: 6000},
		{Status: "no_impact", CleanMs: 6000},
		{Status: "restored", LossMs: 300, Lost: 300, CleanMs: 6000},
		{Status: "stopped", LossMs: 9999},
		{Status: noReceiver},
	}
	s := summarize(flows)
	if s.Flows != 3 || s.Impacted != 2 || s.NotRestored != 0 || s.WorstMs != 300 || s.MedianMs != 100 || s.P95Ms != 300 {
		t.Fatalf("%+v", s)
	}
	if !convergenceDone(flows, 5*time.Second) || convergenceDone(flows, 10*time.Second) {
		t.Fatal("settle")
	}
	if convergenceDone(append(flows, convFlow{Status: "not_restored", CleanMs: 9000}), time.Second) {
		t.Fatal("not restored isn't settled")
	}
}

func TestMarkUnsettled(t *testing.T) {
	flows := []convFlow{{Status: "restored", CleanMs: 100}, {Status: "restored", CleanMs: 9000}, {Status: "no_impact", CleanMs: 100}}
	markUnsettled(flows, 5*time.Second)
	if flows[0].Status != "not_restored" || flows[1].Status != "restored" || flows[2].Status != "no_impact" {
		t.Fatalf("%+v", flows)
	}
}

func TestCheckClean(t *testing.T) {
	if err := checkClean([]convFlow{{Status: "stopped"}}); err == nil {
		t.Fatal("nothing running")
	}
	if err := checkClean([]convFlow{{Name: "x", Status: "restored", Lost: 3}}); err == nil {
		t.Fatal("lossy")
	}
	if err := checkClean([]convFlow{{Name: "x", Status: noReceiver}}); err == nil {
		t.Fatal("unmeasured")
	}
	if err := checkClean([]convFlow{{Status: "no_impact"}, {Status: "stopped"}}); err != nil {
		t.Fatal(err)
	}
}
