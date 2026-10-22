package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Group templates. Any value in a group's device or protocol template may
// be a stepped value, an object {start, step, repeat, cycle}: member i
// (0-based) gets start + k·step with k = (i / repeat) % cycle. start is an
// integer, an IP address, an address with a prefix length or a MAC; step is
// an integer or (for addresses and MACs) a value of the same form, e.g.
// 0.0.0.4 or ::1:0. Strings may contain placeholders: {i} (1-based), {i0},
// {group} and, in protocol templates, the member device's {device}, {mac},
// {ipv4}, {ipv6}, {gateway4}, {gateway6}, {vlan} and {inner_vlan}.

type stepValue struct {
	start, step any
	repeat      int
	cycle       int
}

var stepKeys = map[string]bool{"start": true, "step": true, "repeat": true, "cycle": true}

func asStep(m map[string]any) (*stepValue, bool, error) {
	if _, ok := m["start"]; !ok {
		return nil, false, nil
	}
	for k := range m {
		if !stepKeys[k] {
			return nil, false, nil
		}
	}
	s := &stepValue{start: m["start"], step: m["step"], repeat: 1}
	for _, f := range []struct {
		key string
		dst *int
	}{{"repeat", &s.repeat}, {"cycle", &s.cycle}} {
		v, ok := m[f.key]
		if !ok {
			continue
		}
		n, isNum := v.(float64)
		if !isNum || n != float64(int(n)) || n < 1 {
			return nil, true, fmt.Errorf("%s must be a positive integer", f.key)
		}
		*f.dst = int(n)
	}
	return s, true, nil
}

var placeholderRE = regexp.MustCompile(`\{([a-z0-9_]+)\}`)

// subst replaces placeholders; an unknown or empty one is an error.
func subst(s string, vars map[string]string) (string, error) {
	var err error
	out := placeholderRE.ReplaceAllStringFunc(s, func(m string) string {
		k := m[1 : len(m)-1]
		v, ok := vars[k]
		if !ok {
			err = errors.Join(err, fmt.Errorf("unknown placeholder %s", m))
		} else if v == "" {
			err = errors.Join(err, fmt.Errorf("%s: the device has none", m))
		}
		return v
	})
	return out, err
}

// render evaluates a decoded JSON template for member i.
func render(v any, path string, i int, vars map[string]string) (any, error) {
	switch x := v.(type) {
	case string:
		s, err := subst(x, vars)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return s, nil
	case []any:
		out := make([]any, len(x))
		for j, e := range x {
			r, err := render(e, fmt.Sprintf("%s[%d]", path, j), i, vars)
			if err != nil {
				return nil, err
			}
			out[j] = r
		}
		return out, nil
	case map[string]any:
		st, ok, err := asStep(x)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if ok {
			r, err := st.at(i, vars)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			return r, nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			p := k
			if path != "" {
				p = path + "." + k
			}
			r, err := render(e, p, i, vars)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	}
	return v, nil
}

func (s *stepValue) at(i int, vars map[string]string) (any, error) {
	k := int64(i / s.repeat)
	if s.cycle > 0 {
		k %= int64(s.cycle)
	}
	switch start := s.start.(type) {
	case float64:
		step := int64(1)
		if s.step != nil {
			f, ok := s.step.(float64)
			if !ok || f != float64(int64(f)) {
				return nil, errors.New("step for an integer start must be an integer")
			}
			step = int64(f)
		}
		if start != float64(int64(start)) {
			return nil, errors.New("start must be an integer")
		}
		return int64(start) + k*step, nil
	case string:
		str, err := subst(start, vars)
		if err != nil {
			return nil, err
		}
		if p, err := netip.ParsePrefix(str); err == nil {
			a, err := addAddr(p.Addr(), s.step, k)
			if err != nil {
				return nil, err
			}
			return netip.PrefixFrom(a, p.Bits()).String(), nil
		}
		if a, err := netip.ParseAddr(str); err == nil {
			a, err := addAddr(a, s.step, k)
			if err != nil {
				return nil, err
			}
			return a.String(), nil
		}
		if m, err := pkt.ParseMAC(str); err == nil {
			return addMAC(m, s.step, k)
		}
		return nil, fmt.Errorf("start %q is not an integer, IP address, prefix or MAC", str)
	}
	return nil, errors.New("start must be an integer, IP address, prefix or MAC")
}

// stepInt is a step as a big integer: a number, or an address/MAC form
// parsed by parse.
func stepInt(step any, parse func(string) (*big.Int, error)) (*big.Int, error) {
	switch x := step.(type) {
	case nil:
		return big.NewInt(1), nil
	case float64:
		if x != float64(int64(x)) {
			return nil, errors.New("step must be an integer")
		}
		return big.NewInt(int64(x)), nil
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return big.NewInt(n), nil
		}
		return parse(x)
	}
	return nil, fmt.Errorf("invalid step %v", step)
}

func addAddr(a netip.Addr, step any, k int64) (netip.Addr, error) {
	st, err := stepInt(step, func(s string) (*big.Int, error) {
		b, err := netip.ParseAddr(s)
		if err != nil || b.Is4() != a.Is4() {
			return nil, fmt.Errorf("step %q must be an integer or an address of the same family", s)
		}
		return new(big.Int).SetBytes(b.AsSlice()), nil
	})
	if err != nil {
		return netip.Addr{}, err
	}
	n := new(big.Int).SetBytes(a.AsSlice())
	n.Add(n, st.Mul(st, big.NewInt(k)))
	size := len(a.AsSlice())
	if n.Sign() < 0 || n.BitLen() > size*8 {
		return netip.Addr{}, fmt.Errorf("%s stepped %d times is out of range", a, k)
	}
	b := n.FillBytes(make([]byte, size))
	out, _ := netip.AddrFromSlice(b)
	return out.WithZone(a.Zone()), nil
}

func addMAC(m pkt.MAC, step any, k int64) (string, error) {
	st, err := stepInt(step, func(s string) (*big.Int, error) {
		b, err := pkt.ParseMAC(s)
		if err != nil {
			return nil, fmt.Errorf("step %q must be an integer or a MAC", s)
		}
		return new(big.Int).SetBytes(b[:]), nil
	})
	if err != nil {
		return "", err
	}
	n := new(big.Int).SetBytes(m[:])
	n.Add(n, st.Mul(st, big.NewInt(k)))
	if n.Sign() < 0 || n.BitLen() > 48 {
		return "", fmt.Errorf("%s stepped %d times is out of range", m, k)
	}
	var out pkt.MAC
	n.FillBytes(out[:])
	return out.String(), nil
}

// renderJSON renders a raw JSON template into raw JSON.
func renderJSON(raw json.RawMessage, path string, i int, vars map[string]string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r, err := render(v, path, i, vars)
	if err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// memberVars are the placeholders a member's protocol templates can use.
func memberVars(base map[string]string, d DeviceSpec, mac pkt.MAC) map[string]string {
	v := map[string]string{"device": d.Name, "mac": mac.String(), "gateway4": d.Gateway4, "gateway6": d.Gateway6,
		"ipv4": "", "ipv6": "", "vlan": "", "inner_vlan": ""}
	for k, x := range base {
		v[k] = x
	}
	for _, a := range d.Addresses {
		p, err := netip.ParsePrefix(a)
		if err != nil {
			continue
		}
		if p.Addr().Is4() && v["ipv4"] == "" {
			v["ipv4"] = p.Addr().String()
		}
		if p.Addr().Is6() && !p.Addr().IsLinkLocalUnicast() && v["ipv6"] == "" {
			v["ipv6"] = p.Addr().String()
		}
	}
	if n := len(d.VLANs); n > 0 {
		v["vlan"] = strconv.Itoa(int(d.VLANs[0].VID))
		if n > 1 {
			v["inner_vlan"] = strconv.Itoa(int(d.VLANs[n-1].VID))
		}
	}
	return v
}

// parseMembers parses a member selection: "" or "all", or 1-based indices
// and ranges like "1-10,15".
func parseMembers(s string, count int) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "all" {
		out := make([]int, count)
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err1 != nil || err2 != nil || a < 1 || b < a || b > count {
			return nil, fmt.Errorf("members %q: want indices 1-%d like 1-10,15", part, count)
		}
		for i := a; i <= b; i++ {
			if !seen[i] {
				seen[i] = true
				out = append(out, i-1)
			}
		}
	}
	return out, nil
}
