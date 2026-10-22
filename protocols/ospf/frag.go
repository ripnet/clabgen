package ospf

import (
	"encoding/binary"
	"net/netip"
	"sort"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// IP fragmentation for packets above the MTU (an LSA larger than a packet,
// such as a router LSA with many links) and reassembly of received
// fragments: IPv4 (RFC 791) and the IPv6 fragment header (RFC 8200 4.5).

const (
	ipv6FragHeader = 44
	reasmTimeout   = 30 * time.Second
	reasmMax       = 64 // partial packets held at once
)

// fragment4 splits an IPv4 packet into fragments of at most mtu bytes.
func fragment4(ip pkt.IPv4, mtu int) [][]byte {
	chunk := (mtu - 20) &^ 7
	p := ip.Payload
	var out [][]byte
	for off := 0; off < len(p); off += chunk {
		end := min(off+chunk, len(p))
		f := ip
		f.Payload, f.FragOff, f.Flags = p[off:end], uint16(off/8), ip.Flags&^pkt.IPv4MF
		if end < len(p) {
			f.Flags |= pkt.IPv4MF
		}
		out = append(out, f.Encode())
	}
	return out
}

// fragment6 splits an IPv6 packet (without extension headers) into
// fragments of at most mtu bytes, each with a fragment header.
func fragment6(ip pkt.IPv6, mtu int, ident uint32) [][]byte {
	chunk := (mtu - 40 - 8) &^ 7
	p := ip.Payload
	var out [][]byte
	for off := 0; off < len(p); off += chunk {
		end := min(off+chunk, len(p))
		h := make([]byte, 8, 8+end-off)
		h[0] = ip.NextHeader
		fo := uint16(off/8) << 3
		if end < len(p) {
			fo |= 1 // M
		}
		binary.BigEndian.PutUint16(h[2:], fo)
		binary.BigEndian.PutUint32(h[4:], ident)
		f := ip
		f.NextHeader, f.Payload = ipv6FragHeader, append(h, p[off:end]...)
		out = append(out, f.Encode())
	}
	return out
}

type reasmKey struct {
	src, dst netip.Addr
	id       uint32
}

type partial struct {
	parts    map[int][]byte // by byte offset
	total    int            // payload length, once the last fragment is in (-1 before)
	deadline time.Time
}

// reassemble adds a fragment and returns the whole payload when complete.
func (s *session) reassemble(k reasmKey, off int, more bool, data []byte, now time.Time) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reasm == nil {
		s.reasm = map[reasmKey]*partial{}
	}
	for x, p := range s.reasm {
		if now.After(p.deadline) {
			delete(s.reasm, x)
			s.count("rx_reassembly_timeouts")
		}
	}
	p := s.reasm[k]
	if p == nil {
		if len(s.reasm) >= reasmMax {
			s.count("rx_fragments_dropped")
			return nil, false
		}
		p = &partial{parts: map[int][]byte{}, total: -1, deadline: now.Add(reasmTimeout)}
		s.reasm[k] = p
	}
	if off+len(data) > 65535 {
		delete(s.reasm, k)
		return nil, false
	}
	p.parts[off] = append([]byte(nil), data...)
	if !more {
		p.total = off + len(data)
	}
	if p.total < 0 {
		return nil, false
	}
	offs := make([]int, 0, len(p.parts))
	for o := range p.parts {
		offs = append(offs, o)
	}
	sort.Ints(offs)
	out := make([]byte, 0, p.total)
	for _, o := range offs {
		if o != len(out) {
			return nil, false // a gap (or an overlap): wait for more
		}
		out = append(out, p.parts[o]...)
	}
	if len(out) != p.total {
		return nil, false
	}
	delete(s.reasm, k)
	s.count("rx_reassembled")
	return out, true
}
