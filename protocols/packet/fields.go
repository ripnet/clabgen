package packet

import (
	"fmt"
	"math/big"
	"net/netip"
	"strconv"
	"strings"

	"github.com/ripnet/clabgen/internal/pkt"
)

// A field is a run of bits in a built frame: what modifiers change and
// the preview highlights.
type field struct {
	Name string // "src", "vid", …
	Pos  int    // bit offset from the start of the frame (outer Ethernet header, no device tags)
	Bits int
	Kind kind
}

type kind int

const (
	kindInt kind = iota
	kindMAC
	kindIPv4
	kindIPv6
)

func (f field) bytes() (off, n int) { return f.Pos / 8, (f.Pos%8 + f.Bits + 7) / 8 }

// get reads the field as a number.
func (f field) get(b []byte) *big.Int {
	v := new(big.Int)
	for i := 0; i < f.Bits; i++ {
		p := f.Pos + i
		v.Lsh(v, 1)
		if b[p/8]&(0x80>>(p%8)) != 0 {
			v.SetBit(v, 0, 1)
		}
	}
	return v
}

// set writes v (taken modulo the field's width) into b.
func (f field) set(b []byte, v *big.Int) {
	for i := 0; i < f.Bits; i++ {
		p := f.Pos + i
		mask := byte(0x80 >> (p % 8))
		if v.Bit(f.Bits-1-i) != 0 {
			b[p/8] |= mask
		} else {
			b[p/8] &^= mask
		}
	}
}

// format shows a field's value the way configs write it.
func (f field) format(b []byte) string {
	v := f.get(b)
	switch f.Kind {
	case kindMAC:
		var m pkt.MAC
		v.FillBytes(m[:])
		return m.String()
	case kindIPv4:
		var a [4]byte
		v.FillBytes(a[:])
		return netip.AddrFrom4(a).String()
	case kindIPv6:
		var a [16]byte
		v.FillBytes(a[:])
		return netip.AddrFrom16(a).String()
	}
	if f.Bits > 8 && f.Bits%4 == 0 && (f.Name == "ethertype" || f.Name == "tpid" || f.Name == "checksum") {
		return fmt.Sprintf("0x%0*x", f.Bits/4, v)
	}
	return v.String()
}

// parse reads a value of the field's kind: a number (decimal or 0x hex),
// a MAC or an address.
func (f field) parse(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	switch f.Kind {
	case kindMAC:
		if m, err := pkt.ParseMAC(s); err == nil {
			return new(big.Int).SetBytes(m[:]), nil
		}
	case kindIPv4, kindIPv6:
		if a, err := netip.ParseAddr(s); err == nil {
			if a.Is4() != (f.Kind == kindIPv4) {
				return nil, fmt.Errorf("%q is not an IPv%d address", s, map[bool]int{true: 4, false: 6}[f.Kind == kindIPv4])
			}
			return new(big.Int).SetBytes(a.AsSlice()), nil
		}
	}
	u, err := strconv.ParseUint(s, 0, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid %s value", s, f.Name)
	}
	v := new(big.Int).SetUint64(u)
	if v.BitLen() > f.Bits {
		return nil, fmt.Errorf("%s %s doesn't fit in %d bits", f.Name, s, f.Bits)
	}
	return v, nil
}
