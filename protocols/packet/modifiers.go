package packet

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
)

// Modifier changes a field from packet to packet.
type Modifier struct {
	Field  string   `json:"field" jsonschema:"required" jsonschema_description:"The field: layer.name (ipv4.src, udp.dst_port, ethernet.src, vlan.vid) or layer[n].name for the nth layer of a kind (vlan[2].vid)"`
	Mode   string   `json:"mode" jsonschema:"enum=increment,enum=decrement,enum=random,enum=list"`
	Start  string   `json:"start,omitempty" jsonschema:"description=First value (a number or a MAC or an address). Default: the field's value in its layer"`
	Step   string   `json:"step,omitempty" jsonschema_description:"Change per value, as a number or in the field's form (0.0.1.0, ::1:0). Default 1"`
	Count  uint64   `json:"count,omitempty" jsonschema_description:"Values before going back to start (random: values to pick from). Default: the field's whole range"`
	Values []string `json:"values,omitempty" jsonschema:"description=The values for mode list"`
	Repeat uint64   `json:"repeat,omitempty" jsonschema:"description=Packets per value. Default 1"`
}

// A compiled modifier.
type mod struct {
	f       field
	mode    string
	start   *big.Int
	step    *big.Int
	count   uint64
	values  []*big.Int
	repeat  uint64
	modulus *big.Int // 2^bits
	salt    uint64
}

func (m Modifier) check() error {
	switch m.Mode {
	case "increment", "decrement", "random":
		if len(m.Values) > 0 {
			return fmt.Errorf("modifier %s: values are for mode list", m.Field)
		}
	case "list":
		if len(m.Values) == 0 {
			return fmt.Errorf("modifier %s: mode list needs values", m.Field)
		}
	default:
		return fmt.Errorf("modifier %s: mode increment, decrement, random or list", m.Field)
	}
	if m.Field == "" {
		return errors.New("modifier: field is required")
	}
	return nil
}

func compile(ms []Modifier, t *template) ([]mod, error) {
	var out []mod
	for i, m := range ms {
		f, ok := t.fields[m.Field]
		if !ok {
			return nil, fmt.Errorf("modifier %s: no such field (the packet has %s)", m.Field, fieldList(t))
		}
		c := mod{f: f, mode: m.Mode, count: m.Count, repeat: max(m.Repeat, 1), modulus: new(big.Int).Lsh(big.NewInt(1), uint(f.Bits)), salt: uint64(i+1) * 0x9e3779b97f4a7c15}
		var err error
		if m.Start != "" {
			if c.start, err = f.parse(m.Start); err != nil {
				return nil, fmt.Errorf("modifier %s: start: %w", m.Field, err)
			}
		} else {
			c.start = f.get(t.frame)
		}
		c.step = big.NewInt(1)
		if m.Step != "" {
			if c.step, err = f.parse(m.Step); err != nil {
				return nil, fmt.Errorf("modifier %s: step: %w", m.Field, err)
			}
		}
		for _, v := range m.Values {
			x, err := f.parse(v)
			if err != nil {
				return nil, fmt.Errorf("modifier %s: %w", m.Field, err)
			}
			c.values = append(c.values, x)
		}
		out = append(out, c)
	}
	return out, nil
}

func fieldList(t *template) string {
	var names []string
	for n := range t.fields {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 12 {
		return fmt.Sprintf("%v, … (%d fields)", names[:12], len(names))
	}
	return fmt.Sprint(names)
}

// splitmix64: a reproducible pseudo-random number for packet k.
func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

// value is the field's value in packet n.
func (m mod) value(n uint64) *big.Int {
	k := n / m.repeat
	switch m.mode {
	case "list":
		return m.values[k%uint64(len(m.values))]
	case "random":
		r := mix(k ^ m.salt)
		if m.count > 0 {
			v := new(big.Int).Mul(m.step, new(big.Int).SetUint64(r%m.count))
			return v.Add(v, m.start).Mod(v, m.modulus)
		}
		v := new(big.Int)
		for v.BitLen() < m.f.Bits+64 { // enough random bits for any field
			v.Lsh(v, 64).Or(v, new(big.Int).SetUint64(r))
			r = mix(r)
		}
		return v.Mod(v, m.modulus)
	}
	if m.count > 0 {
		k %= m.count
	}
	v := new(big.Int).Mul(m.step, new(big.Int).SetUint64(k))
	if m.mode == "decrement" {
		v.Neg(v)
	}
	return v.Add(v, m.start).Mod(v, m.modulus)
}
