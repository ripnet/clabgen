package twamp

import (
	"encoding/binary"
	"strconv"
	"time"
)

// Test packets (unauthenticated mode). A sender's packet: sequence number,
// timestamp T1, error estimate, then for STAMP (RFC 8762) the session ID
// (SSID) and 28 bytes MBZ, 44 in all, followed by TLVs (RFC 8972); a TWAMP
// Light sender's (RFC 5357) is 14 bytes plus any padding.
//
// The reflector's packet has the same layout for both: sequence number,
// timestamp T3, error estimate, SSID (MBZ in TWAMP), receive timestamp T2,
// the sender's sequence number, timestamp and error estimate, MBZ, the
// sender's TTL, then (STAMP) 3 bytes MBZ and the TLVs; TWAMP Light pads
// from byte 41. It is as long as the sender's packet, at least 41 bytes.
const (
	senderMin    = 14
	stampBase    = 44
	reflectorMin = 41
)

// TLV types (RFC 8972, 9503).
const (
	tlvExtraPadding = 1
	tlvLocation     = 2
	tlvTimestamp    = 3
	tlvClassOfSvc   = 4
	tlvDirectMeas   = 5
	tlvAccessReport = 6
	tlvFollowUp     = 7
	tlvHMAC         = 8
)

// TLV flags.
const (
	flagU = 0x80 // unrecognized: a sender sets it, a reflector that knows the type clears it
	flagM = 0x40 // malformed
	flagI = 0x20 // integrity failed
)

// Error estimate bits.
const (
	errS = 0x8000 // clock synchronized to an external source
	errZ = 0x4000 // PTP timestamp format (RFC 8186), else NTP
)

const ntpEpoch = 2208988800 // seconds from 1900 to 1970

// stamp writes t at b in NTP (64-bit, seconds since 1900) or truncated PTP
// (seconds since 1970, nanoseconds) format.
func stamp(b []byte, t time.Time, ptp bool) {
	if t.IsZero() {
		binary.BigEndian.PutUint64(b, 0)
		return
	}
	sec, ns := t.Unix(), uint64(t.Nanosecond())
	if ptp {
		binary.BigEndian.PutUint32(b, uint32(sec))
		binary.BigEndian.PutUint32(b[4:], uint32(ns))
		return
	}
	binary.BigEndian.PutUint32(b, uint32(sec+ntpEpoch))
	binary.BigEndian.PutUint32(b[4:], uint32(ns<<32/1e9))
}

// readStamp is the inverse of stamp (seconds past 2036 for NTP are not
// handled: era 0 only).
func readStamp(b []byte, ptp bool) time.Time {
	sec, frac := binary.BigEndian.Uint32(b), binary.BigEndian.Uint32(b[4:])
	if sec == 0 && frac == 0 {
		return time.Time{}
	}
	if ptp {
		return time.Unix(int64(sec), int64(frac))
	}
	return time.Unix(int64(sec)-ntpEpoch, int64(uint64(frac)*1e9>>32))
}

// tlv is one TLV of a packet, at off (its header) in the packet.
type tlv struct {
	off   int
	flags uint8
	typ   uint8
	value []byte // aliases the packet
	// lengthWithHeader: the length field counts the 4-byte header too
	// (SR OS 26.10 does this; RFC 8972 counts the value only).
	lengthWithHeader bool
}

// parseTLVs reads the TLVs after the STAMP base of b. It stops at the
// first that doesn't fit (ok false: the rest is padding to us), or at a
// zero header (TWAMP Light zero padding). A last TLV whose length counts
// its header, ending exactly at the end of the packet, is taken as such.
func parseTLVs(b []byte) (out []tlv, ok bool) {
	for off := stampBase; off < len(b); {
		if len(b)-off < 4 {
			return out, false
		}
		flags, typ, n := b[off], b[off+1], int(binary.BigEndian.Uint16(b[off+2:]))
		if flags == 0 && typ == 0 && n == 0 {
			return out, false
		}
		if off+4+n > len(b) {
			if n >= 4 && off+n == len(b) {
				out = append(out, tlv{off: off, flags: flags, typ: typ, value: b[off+4:], lengthWithHeader: true})
				return out, true
			}
			return out, false
		}
		out = append(out, tlv{off: off, flags: flags, typ: typ, value: b[off+4 : off+4+n]})
		off += 4 + n
	}
	return out, true
}

// request is what a reflector needs from a sender's packet.
type request struct {
	seq  uint32
	t1   []byte // as sent (8 bytes)
	err  uint16
	ssid uint16
	ptp  bool
	tlvs []tlv
	// stamp: the packet is long enough for STAMP and its TLVs (if any)
	// parsed; TWAMP Light packets are reflected without TLV processing.
	stamp bool
}

func parseRequest(b []byte, stampMode bool) (request, bool) {
	if len(b) < senderMin {
		return request{}, false
	}
	r := request{seq: binary.BigEndian.Uint32(b), t1: b[4:12], err: binary.BigEndian.Uint16(b[12:])}
	r.ptp = r.err&errZ != 0
	if len(b) >= 16 {
		r.ssid = binary.BigEndian.Uint16(b[14:])
	}
	if stampMode && len(b) >= stampBase {
		var ok bool
		r.tlvs, ok = parseTLVs(b)
		r.stamp = ok || len(r.tlvs) > 0 || len(b) == stampBase
	}
	return r, true
}

// TLVName names a TLV type for status.
func TLVName(t uint8) string {
	switch t {
	case tlvExtraPadding:
		return "extra-padding"
	case tlvLocation:
		return "location"
	case tlvTimestamp:
		return "timestamp-info"
	case tlvClassOfSvc:
		return "class-of-service"
	case tlvDirectMeas:
		return "direct-measurement"
	case tlvAccessReport:
		return "access-report"
	case tlvFollowUp:
		return "follow-up-telemetry"
	case tlvHMAC:
		return "hmac"
	}
	return "type-" + strconv.Itoa(int(t))
}
