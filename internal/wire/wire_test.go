package wire

import (
	"encoding/json"
	"testing"
)

func TestStack(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`"3162,163"`, "3162,163"},
		{`"3162, 163"`, "3162,163"},
		{`[3162, 163]`, "3162,163"},
		{`["0x88a8:3162", 163]`, "0x88a8:3162,163"},
		{`"88A8:3162,163"`, "0x88a8:3162,163"},
		{`"0x8100:5"`, "5"},
		{`3162`, "3162"},
		{`[]`, Untagged},
		{`"untagged"`, Untagged},
		{`""`, ""},
	} {
		var s Stack
		if err := json.Unmarshal([]byte(c.in), &s); err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		got, err := s.Normal()
		if err != nil || got != c.want {
			t.Fatalf("%s: %q %v, want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"4096", "0x1234:5", "x", "1,,y"} {
		if _, err := Stack(bad).Normal(); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if err := (&Expect{Direction: "up"}).Validate(); err == nil {
		t.Fatal("bad direction accepted")
	}
}

func u32(v uint32) *uint32 { return &v }

// A link with flow f1 both ways in service 500 (CCMs with one tag each way),
// and f1's frames with a different stack in service 600 on another monitor.
func monitors() []Monitor {
	a, b := "1c:00:00:00:00:01", "1c:00:00:00:00:02"
	fA, fB := "02:00:00:00:00:0a", "02:00:00:00:00:0b"
	m1 := Status{Services: []Service{
		{Label: u32(500), Src: a, Dst: b,
			Data: []Seen{{FlowID: "f1", Kind: "icmp", Tags: "3162,163", Src: fA, Dst: fB, Count: 10}},
			OAM:  []Seen{{Kind: "CCM", Tags: "3162", Count: 5}}},
		{Label: u32(500), Src: b, Dst: a,
			Data: []Seen{{FlowID: "f1", Kind: "icmp", Reply: true, Tags: "3162,163", Src: fB, Dst: fA, Count: 10}},
			OAM:  []Seen{{Kind: "CCM", Tags: "3162", Count: 5}}},
	}}
	m2 := Status{Services: []Service{
		{Label: u32(600), Src: a, Dst: b,
			Data: []Seen{{FlowID: "f2", Kind: "udp", Tags: "163", IPSrc: "10.0.0.1", Count: 4}}},
	}}
	return []Monitor{{Name: "a/m1", Status: m1}, {Name: "b/m2", Status: m2}}
}

func TestJudge(t *testing.T) {
	mons := monitors()
	f1 := Flow{FlowID: "f1", MAC: "02:00:00:00:00:0a", Expect: Expect{Data: "3162,163", OAM: "3162"}}
	v := Judge(f1, mons)
	if v.Verdict != Pass || len(v.Rows) != 2 || v.Rows[0].Direction != AtoZ || v.Rows[1].Direction != ZtoA {
		t.Fatalf("f1: %+v", v)
	}
	// Two tags expected on OAM: fails both ways.
	f1.Expect.OAM = "3162,163"
	if v := Judge(f1, mons); v.Verdict != Fail || v.Rows[0].OAM.Verdict != Fail || v.Rows[0].Data.Verdict != Pass {
		t.Fatalf("f1 oam: %+v", v)
	}
	// A one-way flow, matched by IP: z-to-a is pending unless only a-to-z
	// is asked for.
	f2 := Flow{FlowID: "f2", IP: "10.0.0.1", Expect: Expect{Data: "163"}}
	if v := Judge(f2, mons); v.Verdict != Pending || len(v.Rows) != 2 || v.Rows[1].Verdict != Pending {
		t.Fatalf("f2: %+v", v)
	}
	f2.Expect.Direction = AtoZ
	if v := Judge(f2, mons); v.Verdict != Pass || len(v.Rows) != 1 || v.Rows[0].Monitor != "b/m2" || v.Rows[0].OAM.Verdict != "" {
		t.Fatalf("f2 a-to-z: %+v", v)
	}
	// Frames one way only (no replies): the other way's OAM is found by its
	// MA, in the service running the other way.
	m3 := Status{Services: []Service{
		{Label: u32(700), Src: "1c:00:00:00:00:01", Dst: "1c:00:00:00:00:02",
			Data: []Seen{{FlowID: "f3", Kind: "icmp", Tags: "3162,163", Src: "02:00:00:00:00:0a", Count: 9}},
			OAM:  []Seen{{Kind: "CCM", MA: "evpl", Tags: "3162,163", Count: 3}}},
		{Label: u32(701), Src: "1c:00:00:00:00:02", Dst: "1c:00:00:00:00:01",
			OAM: []Seen{{Kind: "CCM", MA: "evpl", Tags: "163", Count: 3}, {Kind: "CCM", MA: "other", Tags: "9", Count: 3}}},
	}}
	f3 := Flow{FlowID: "f3", MAC: "02:00:00:00:00:0a", Expect: Expect{Data: "163", OAM: "163"}}
	v3 := Judge(f3, []Monitor{{Name: "m3", Status: m3}})
	if v3.Verdict != Fail || len(v3.Rows) != 2 || v3.Rows[0].Data.Verdict != Fail {
		t.Fatalf("f3: %+v", v3)
	}
	if back := v3.Rows[1]; back.Direction != ZtoA || *back.Label != 701 || back.OAM.Verdict != Pass || back.Data.Verdict != Pending || len(back.OAM.Seen) != 1 {
		t.Fatalf("f3 back: %+v", back)
	}
	// Not seen anywhere.
	if v := Judge(Flow{FlowID: "zz", Expect: Expect{Data: "1"}}, mons); v.Verdict != Pending {
		t.Fatalf("unseen: %+v", v)
	}
	// Nothing expected: rows, no verdict.
	if v := Judge(Flow{FlowID: "f1", MAC: "02:00:00:00:00:0a"}, mons); v.Verdict != "" || len(v.Rows) != 2 {
		t.Fatalf("no expectation: %+v", v)
	}
}

// A label the router gave another service before: its OAM from then isn't
// the flow's, and neither are its MAs.
func TestJudgeReusedLabel(t *testing.T) {
	m := Status{Services: []Service{
		{Label: u32(524286), Src: "1c:00:00:00:00:01", Dst: "1c:00:00:00:00:02",
			Data: []Seen{{FlowID: "new", Kind: "icmp", Tags: "2121", Src: "02:00:00:00:00:0a", Count: 60, FirstS: 60, LastS: 0.5}},
			OAM: []Seen{
				{Kind: "CCM", MA: "E2101", Tags: "2101", Count: 900, FirstS: 1000, LastS: 120},
				{Kind: "CCM", MA: "E2121", Tags: "2121", Count: 60, FirstS: 61, LastS: 0.4},
			}},
		{Label: u32(524200), Src: "1c:00:00:00:00:02", Dst: "1c:00:00:00:00:01",
			OAM: []Seen{{Kind: "CCM", MA: "E2101", Tags: "2101", Count: 900, LastS: 0.2}, {Kind: "CCM", MA: "E2121", Tags: "2121", Count: 60, LastS: 0.3}}},
	}}
	f := Flow{FlowID: "new", MAC: "02:00:00:00:00:0a", Expect: Expect{Data: "2121", OAM: "2121"}}
	v := Judge(f, []Monitor{{Name: "m", Status: m}})
	// No replies, so z-to-a's data is pending; its OAM is found by MA.
	if v.Verdict != Pending || len(v.Rows) != 2 || v.Rows[0].Verdict != Pass {
		t.Fatalf("%+v", v)
	}
	if back := v.Rows[1]; back.OAM.Verdict != Pass || len(back.OAM.Seen) != 1 || back.OAM.Seen[0].Tags != "2121" {
		t.Fatalf("back: only E2121's OAM is the flow's: %+v", back)
	}
	// Without first sightings (an older agent), everything counts.
	m.Services[0].Data[0].FirstS = 0
	if v := Judge(f, []Monitor{{Name: "m", Status: m}}); v.Verdict != Fail {
		t.Fatalf("no first_s: %+v", v)
	}
}

// A stack not seen for a while, while another is, is from before a change:
// shown, not judged. Alone, it still is (the flow stopped).
func TestJudgeStale(t *testing.T) {
	svc := func(seen ...Seen) Status {
		return Status{Services: []Service{{Label: u32(9), Src: "1c:00:00:00:00:01", Dst: "1c:00:00:00:00:02", Data: seen}}}
	}
	f := Flow{FlowID: "f", MAC: "02:00:00:00:00:0a", Expect: Expect{Data: "163", Direction: AtoZ}}
	old := Seen{FlowID: "f", Kind: "icmp", Tags: "3162,163", Src: f.MAC, Count: 50, FirstS: 100, LastS: 40}
	now := Seen{FlowID: "f", Kind: "icmp", Tags: "163", Src: f.MAC, Count: 30, FirstS: 30, LastS: 0.5}
	v := Judge(f, []Monitor{{Name: "m", Status: svc(old, now)}})
	if v.Verdict != Pass || len(v.Rows[0].Data.Seen) != 2 || !v.Rows[0].Data.Seen[0].Stale || v.Rows[0].Data.Seen[1].Stale {
		t.Fatalf("stale: %+v", v)
	}
	if v := Judge(f, []Monitor{{Name: "m", Status: svc(old)}}); v.Verdict != Fail || v.Rows[0].Data.Seen[0].Stale {
		t.Fatalf("alone: %+v", v)
	}
}
