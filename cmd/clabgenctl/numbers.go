package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"connectrpc.com/connect"

	v1 "github.com/ripnet/clabgen/gen/clabgen/v1"
)

// key=value numbers in other bases: ethertype=0x88e5, ethertypes=[0x88b5,0x88e5].
// They aren't JSON, so they'd go as strings; where the schema of what's set
// says the field is an integer (or a list of them), they're read as numbers.
// Strings that look like numbers (keys, hex payloads) stay strings.

// schemaFn gives the JSON schema of what key=value arguments set, fetched
// only when a value needs it; nil (or a nil result) when there is none.
type schemaFn func() map[string]any

var radixNum = regexp.MustCompile(`^[+-]?0[xXoObB]\w+$`) // meant as one; parseNum says if it is

// radixList splits a value written as one number or a list ([a,b] or a,b),
// and tells whether any element is a number in another base.
func radixList(v string) ([]string, bool) {
	t := strings.TrimSpace(v)
	list := strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]")
	if list {
		t = t[1 : len(t)-1]
	}
	var parts []string
	found := false
	for _, p := range strings.Split(t, ",") {
		p = strings.TrimSpace(p)
		parts = append(parts, p)
		found = found || radixNum.MatchString(p)
	}
	return parts, found
}

// fieldSchema is the schema of the field at path, through nested objects.
func fieldSchema(s map[string]any, path []string) map[string]any {
	for _, k := range path {
		props, _ := s["properties"].(map[string]any)
		next, _ := props[k].(map[string]any)
		if next == nil {
			return nil
		}
		s = next
	}
	return s
}

func typeIs(s map[string]any, t string) bool {
	switch x := s["type"].(type) {
	case string:
		return x == t
	case []any:
		for _, y := range x {
			if y == t {
				return true
			}
		}
	}
	return false
}

func parseNum(p string) (float64, error) {
	n, err := strconv.ParseInt(p, 0, 64) // 0x, 0o, 0b, decimal, with _ separators
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", p)
	}
	return float64(n), nil
}

// numberValue reads v by the field's schema: a number field takes 0x88e5,
// 0o17 or 0b101; a list of numbers [0x88b5,0x88e5] or 0x88b5,0x88e5. ok is
// false when the field isn't one of those (v stays a string).
func numberValue(fs map[string]any, v string) (any, bool, error) {
	if fs == nil {
		return nil, false, nil
	}
	parts, _ := radixList(v)
	number := func(s map[string]any) bool { return typeIs(s, "integer") || typeIs(s, "number") }
	switch {
	case number(fs) && len(parts) == 1:
		n, err := parseNum(parts[0])
		return n, true, err
	case typeIs(fs, "array"):
		items, _ := fs["items"].(map[string]any)
		if items == nil || !number(items) {
			return nil, false, nil
		}
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			if p == "" {
				continue
			}
			n, err := parseNum(p)
			if err != nil {
				return nil, true, err
			}
			out = append(out, n)
		}
		return out, true, nil
	}
	return nil, false, nil
}

// once makes a schemaFn that fetches at most once.
func once(fetch func() map[string]any) schemaFn {
	var o sync.Once
	var s map[string]any
	return func() map[string]any {
		o.Do(func() { s = fetch() })
		return s
	}
}

func listTypes() []*v1.ProtocolType {
	c, cancel := ctx()
	defer cancel()
	r, err := client().ListProtocolTypes(c, connect.NewRequest(&v1.ListProtocolTypesRequest{}))
	if err != nil {
		return nil
	}
	return r.Msg.Types
}

func decodeSchema(s string) map[string]any {
	var m map[string]any
	json.Unmarshal([]byte(s), &m)
	return m
}

// typeSchema: a protocol type's config schema.
func typeSchema(typ string) schemaFn {
	return once(func() map[string]any {
		for _, t := range listTypes() {
			if t.Name == typ {
				return decodeSchema(t.ConfigSchema)
			}
		}
		return nil
	})
}

// actionSchema: the arguments of an action of a protocol instance (by its
// type), or with name empty of any type that has the action (a group's).
func actionSchema(name, action string) schemaFn {
	return once(func() map[string]any {
		typ := ""
		if name != "" {
			c, cancel := ctx()
			defer cancel()
			r, err := client().GetProtocol(c, connect.NewRequest(&v1.GetProtocolRequest{Name: name}))
			if err != nil {
				return nil
			}
			typ = r.Msg.Protocol.Type
		}
		props := map[string]any{}
		for _, t := range listTypes() {
			if typ != "" && t.Name != typ {
				continue
			}
			for _, a := range t.Actions {
				if a.Name != action {
					continue
				}
				p, _ := decodeSchema(a.ArgsSchema)["properties"].(map[string]any)
				for k, v := range p {
					if _, seen := props[k]; !seen {
						props[k] = v
					}
				}
			}
		}
		return map[string]any{"properties": props}
	})
}
