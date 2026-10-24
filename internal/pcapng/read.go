// Package pcapng reads pcap and pcapng captures (Ethernet) and writes
// pcapng, the format Wireshark reads live from a pipe.
package pcapng

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Limits on a capture Read accepts.
const (
	MaxBytes  = 8 << 20
	MaxFrames = 100000
)

// Read returns the Ethernet frames of a pcap or pcapng capture.
func Read(b []byte) ([][]byte, error) {
	if len(b) > MaxBytes {
		return nil, fmt.Errorf("capture is %d bytes; at most %d", len(b), MaxBytes)
	}
	if len(b) < 4 {
		return nil, errors.New("not a pcap or pcapng capture")
	}
	if binary.BigEndian.Uint32(b) == 0x0a0d0d0a {
		return readPcapNG(b)
	}
	return readClassic(b)
}

func readClassic(b []byte) ([][]byte, error) {
	if len(b) < 24 {
		return nil, errors.New("pcap: truncated header")
	}
	var bo binary.ByteOrder
	switch binary.LittleEndian.Uint32(b) {
	case 0xa1b2c3d4, 0xa1b23c4d: // microseconds, nanoseconds
		bo = binary.LittleEndian
	case 0xd4c3b2a1, 0x4d3cb2a1:
		bo = binary.BigEndian
	default:
		return nil, errors.New("not a pcap or pcapng capture")
	}
	if lt := bo.Uint32(b[20:]) & 0xffff; lt != 1 {
		return nil, fmt.Errorf("pcap: link type %d; only Ethernet (1) can be replayed", lt)
	}
	var out [][]byte
	for off := 24; off+16 <= len(b); {
		n := int(bo.Uint32(b[off+8:]))
		off += 16
		if off+n > len(b) {
			return nil, fmt.Errorf("pcap: frame %d truncated", len(out)+1)
		}
		if out = append(out, b[off:off+n]); len(out) > MaxFrames {
			return nil, fmt.Errorf("pcap: more than %d frames", MaxFrames)
		}
		off += n
	}
	return out, nil
}

func readPcapNG(b []byte) ([][]byte, error) {
	var bo binary.ByteOrder = binary.LittleEndian
	var links []uint16 // link type per interface in the current section
	var out [][]byte
	for off := 0; off+12 <= len(b); {
		typ := binary.BigEndian.Uint32(b[off:])
		if typ == 0x0a0d0d0a { // section header: its byte-order magic decides
			switch binary.LittleEndian.Uint32(b[off+8:]) {
			case 0x1a2b3c4d:
				bo = binary.LittleEndian
			case 0x4d3c2b1a:
				bo = binary.BigEndian
			default:
				return nil, errors.New("pcapng: bad byte-order magic")
			}
			links = nil
		} else {
			typ = bo.Uint32(b[off:])
		}
		n := int(bo.Uint32(b[off+4:]))
		if n < 12 || n%4 != 0 || off+n > len(b) {
			return nil, fmt.Errorf("pcapng: bad block at byte %d", off)
		}
		body := b[off+8 : off+n-4]
		switch typ {
		case 1: // interface description
			if len(body) >= 2 {
				links = append(links, bo.Uint16(body))
			}
		case 6: // enhanced packet
			if len(body) < 20 {
				return nil, errors.New("pcapng: truncated packet block")
			}
			ifc, capLen := int(bo.Uint32(body)), int(bo.Uint32(body[12:]))
			if ifc >= len(links) || 20+capLen > len(body) {
				return nil, errors.New("pcapng: bad packet block")
			}
			if links[ifc] != 1 {
				return nil, fmt.Errorf("pcapng: link type %d; only Ethernet (1) can be replayed", links[ifc])
			}
			out = append(out, body[20:20+capLen])
		case 3: // simple packet (interface 0)
			if len(links) == 0 || links[0] != 1 || len(body) < 4 {
				return nil, errors.New("pcapng: bad simple packet block")
			}
			ln := min(int(bo.Uint32(body)), len(body)-4)
			out = append(out, body[4:4+ln])
		}
		if len(out) > MaxFrames {
			return nil, fmt.Errorf("pcapng: more than %d frames", MaxFrames)
		}
		off += n
	}
	return out, nil
}
