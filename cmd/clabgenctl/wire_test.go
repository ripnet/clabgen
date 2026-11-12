package main

import (
	"testing"

	"github.com/ripnet/clabgen/internal/wire"
)

func TestClassText(t *testing.T) {
	for _, tc := range []struct {
		c    wire.Class
		want string
	}{
		{wire.Class{Expect: "163", Verdict: wire.Pass, Seen: []wire.StackCount{{Tags: "163", Count: 5}}}, "163 ×5 ✓"},
		{wire.Class{Expect: "163", Verdict: wire.Fail, Seen: []wire.StackCount{{Tags: "3162,163", Count: 5}}}, "3162,163 ×5 ✗ (want 163)"},
		{wire.Class{Expect: "163", Verdict: wire.Pending}, "none yet (want 163)"},
		{wire.Class{Seen: []wire.StackCount{{Tags: "2101", Count: 7}}}, "2101 ×7"},
		// Not seen lately: shown, not judged.
		{wire.Class{Expect: "163", Verdict: wire.Pending, IdleS: 42, Seen: []wire.StackCount{{Tags: "3162,163", Count: 5, LastS: 42}}},
			"3162,163 ×5 (not seen for 42 s, pending; want 163)"},
		{wire.Class{IdleS: 12, Seen: []wire.StackCount{{Tags: "2101", Count: 7, LastS: 12}}}, "2101 ×7 (not seen for 12 s)"},
	} {
		if got := classText(tc.c); got != tc.want {
			t.Errorf("classText(%+v) = %q, want %q", tc.c, got, tc.want)
		}
	}
}
