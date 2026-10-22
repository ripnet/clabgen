package core

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	tmpl := `{
	  "name": "c-{i}",
	  "n": {"start": 65001},
	  "neg": {"start": 100, "step": -10},
	  "outer": {"start": 100, "repeat": 3},
	  "inner": {"start": 1, "cycle": 3},
	  "addr": {"start": "10.0.0.2/30", "step": "0.0.0.4"},
	  "v6": {"start": "2001:db8::/64", "step": "0:0:0:1::"},
	  "ip": {"start": "192.0.2.{i}"},
	  "mac": {"start": "02:00:00:00:00:fe", "step": 1},
	  "list": [{"start": 1, "step": 2}, "x{i0}"],
	  "obj": {"start": 1, "other": true}
	}`
	var v any
	json.Unmarshal([]byte(tmpl), &v)
	got := func(i int) map[string]any {
		r, err := render(v, "", i, map[string]string{"i": itoa(i + 1), "i0": itoa(i)})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(r)
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}
	m := got(4)
	want := map[string]any{
		"name": "c-5", "n": 65005.0, "neg": 60.0, "outer": 101.0, "inner": 2.0,
		"addr": "10.0.0.18/30", "v6": "2001:db8:0:4::/64", "ip": "192.0.2.9", "mac": "02:00:00:00:01:02",
		"list": []any{9.0, "x4"}, "obj": map[string]any{"start": 1.0, "other": true},
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("member 5:\n got %v\nwant %v", m, want)
	}

	for _, c := range []struct{ tmpl, err string }{
		{`{"a": "{nope}"}`, "unknown placeholder {nope}"},
		{`{"a": {"start": "x"}}`, "not an integer, IP address"},
		{`{"a": {"start": "10.0.0.1", "step": "::1"}}`, "same family"},
		{`{"a": {"start": "255.255.255.255"}}`, "out of range"},
		{`{"a": {"start": 1, "repeat": 0}}`, "positive integer"},
		{`{"a": {"start": 1.5}}`, "must be an integer"},
	} {
		var v any
		json.Unmarshal([]byte(c.tmpl), &v)
		_, err := render(v, "", 1, map[string]string{"i": "2"})
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: err %v, want %q", c.tmpl, err, c.err)
		}
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestParseMembers(t *testing.T) {
	for _, c := range []struct {
		s    string
		want []int
		err  bool
	}{
		{"", []int{0, 1, 2, 3, 4}, false},
		{"all", []int{0, 1, 2, 3, 4}, false},
		{"2-3, 5,2", []int{1, 2, 4}, false},
		{"0", nil, true},
		{"4-6", nil, true},
		{"3-2", nil, true},
		{"x", nil, true},
	} {
		got, err := parseMembers(c.s, 5)
		if (err != nil) != c.err || (!c.err && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("%q: got %v, %v", c.s, got, err)
		}
	}
}
