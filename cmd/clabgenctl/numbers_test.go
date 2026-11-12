package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSetsRadix(t *testing.T) {
	calls := 0
	schema := func() map[string]any {
		calls++
		return decodeSchema(`{"properties": {
			"ethertype": {"type": "integer"},
			"rate": {"type": "number"},
			"ethertypes": {"type": "array", "items": {"type": "integer"}},
			"payload_data": {"type": "string"},
			"cak": {"type": "string"},
			"tags": {"type": "array", "items": {"type": "string"}},
			"wire": {"type": "object", "properties": {"label": {"type": "integer"}}}}}`)
	}
	m, err := parseSets([]string{
		"ethertype=0x88e5", "rate=0b101", "ethertypes=[0x88b5,0x88e5]", "wire.label=0o17",
		"payload_data=0xdeadbeef", "cak=0x0123456789abcdef", "tags=0x8100", "unknown=0x10", "name=x", "count=7",
	}, schema)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"ethertype": float64(0x88e5), "rate": float64(5), "ethertypes": []any{float64(0x88b5), float64(0x88e5)},
		"wire": map[string]any{"label": float64(15)},
		// Strings by the schema, or unknown: left as written.
		"payload_data": "0xdeadbeef", "cak": "0x0123456789abcdef", "tags": "0x8100", "unknown": "0x10",
		"name": "x", "count": float64(7),
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %#v\nwant %#v", m, want)
	}
	if calls == 0 {
		t.Fatal("schema never asked for")
	}

	// A list without brackets.
	if m, _ := parseSets([]string{"ethertypes=0x88b5,0x88e5"}, schema); !reflect.DeepEqual(m["ethertypes"], []any{float64(0x88b5), float64(0x88e5)}) {
		t.Fatalf("bare list: %#v", m["ethertypes"])
	}
	// A bad number in a number field is an error naming the key.
	if _, err := parseSets([]string{"ethertypes=[0x88b5,0xZZ]"}, schema); err == nil || !strings.Contains(err.Error(), "ethertypes") {
		t.Fatalf("bad list: %v", err)
	}

	if _, err := parseSets([]string{"ethertype=0xZZ"}, schema); err == nil || !strings.Contains(err.Error(), `ethertype: "0xZZ" is not a number`) {
		t.Fatalf("bad number: %v", err)
	}
	// Nothing in another base: the schema isn't fetched.
	calls = 0
	if _, err := parseSets([]string{"rate=10", "name=x", "dst_mac=02:00:00:00:00:01"}, schema); err != nil || calls != 0 {
		t.Fatalf("plain values: err %v, schema fetched %d times", err, calls)
	}
	// Without a schema, as before: a string.
	if m, _ := parseSets([]string{"ethertype=0x88e5"}, nil); m["ethertype"] != "0x88e5" {
		t.Fatalf("no schema: %#v", m["ethertype"])
	}
}

// YAML (--config) reads 0x88e5 as a number already.
func TestBuildStructYAMLHex(t *testing.T) {
	s, err := buildStruct("{ethertype: 0x88e5, ethertypes: [0x88b5]}", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := s.AsMap()
	if m["ethertype"] != float64(0x88e5) || !reflect.DeepEqual(m["ethertypes"], []any{float64(0x88b5)}) {
		t.Fatalf("got %#v", m)
	}
}
