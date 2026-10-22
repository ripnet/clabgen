package cfm

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Y.1731 PDUs beyond 802.1ag: AIS / LCK (7.4, 7.6), CSF (9.21), 1DM
// (9.14), LMM / LMR (9.12), TST (9.20); and the Sender ID (802.1Q
// 21.5.3), Organization-Specific (21.5.2) and Data TLVs.

const (
	OpTST uint8 = 37
	OpCSF uint8 = 52

	tlvOrg  uint8 = 31
	tlvTest uint8 = 32
)

func init() {
	opNames[OpTST], opNames[OpCSF] = "TST", "CSF"
}

// CSF types (Y.1731 9.21, flags bits 6-4).
var csfTypes = map[string]uint8{"los": 0, "fdi": 1, "rdi": 2, "dci": 3}

func csfName(t uint8) string {
	for k, v := range csfTypes {
		if v == t {
			return k
		}
	}
	return fmt.Sprintf("type%d", t)
}

// period codes for AIS, LCK and CSF (as CCM intervals): 1 s or 1 min.
func periodCode(d time.Duration) uint8 {
	if d >= time.Minute {
		return 6
	}
	return 4
}

func periodOf(flags uint8) time.Duration {
	if flags&7 == 6 {
		return time.Minute
	}
	return time.Second
}

// encodeSignal builds an AIS, LCK or CSF PDU (no fields, End TLV).
func encodeSignal(level, op, flags uint8) []byte {
	b := make([]byte, 5)
	putHeader(b, header{Level: level, Opcode: op, Flags: flags})
	return b
}

// ---- TLVs ----

func senderIDTLV(chassis string) []byte {
	if chassis == "" {
		return nil
	}
	v := []byte{byte(len(chassis)), 7} // subtype 7: locally assigned
	v = append(v, chassis...)
	v = append(v, 0) // no management address
	return appendTLV(nil, tlvSenderID, v)
}

// parseSenderID returns a Sender ID TLV's chassis ID.
func parseSenderID(v []byte) string {
	if len(v) < 2 || v[0] == 0 || len(v) < 2+int(v[0]) {
		return ""
	}
	id := v[2 : 2+int(v[0])]
	if v[1] == 4 && len(id) == 6 { // MAC address
		return pkt.MAC(id).String()
	}
	return string(id)
}

func (o *OrgTLV) encode() ([]byte, error) {
	oui, err := hex.DecodeString(strings.NewReplacer(":", "", "-", "").Replace(o.OUI))
	if err != nil || len(oui) != 3 {
		return nil, fmt.Errorf("org_tlv: oui %q: want 3 hex bytes like 00:16:4d", o.OUI)
	}
	val, err := hex.DecodeString(strings.ReplaceAll(o.Value, ":", ""))
	if err != nil || len(val) > 1400 {
		return nil, fmt.Errorf("org_tlv: value %q: want up to 1400 hex bytes", o.Value)
	}
	return appendTLV(nil, tlvOrg, append(append(oui, o.Subtype), val...)), nil
}

func orgString(v []byte) string {
	if len(v) < 4 {
		return hex.EncodeToString(v)
	}
	return fmt.Sprintf("%02x:%02x:%02x/%d %s", v[0], v[1], v[2], v[3], hex.EncodeToString(v[4:]))
}

// withTLVs inserts encoded TLVs before a PDU's End TLV.
func withTLVs(pdu, extra []byte) []byte {
	if len(extra) == 0 || len(pdu) == 0 || pdu[len(pdu)-1] != tlvEnd {
		return pdu
	}
	out := append(append([]byte{}, pdu[:len(pdu)-1]...), extra...)
	return append(out, tlvEnd)
}

func dataTLV(n int) []byte {
	if n <= 0 {
		return nil
	}
	return appendTLV(nil, tlvData, make([]byte, n))
}

// ---- 1DM ----

const oneDMTLVOffset = 16

func encode1DM(level uint8, tx time.Time) []byte {
	b := make([]byte, 4+oneDMTLVOffset+1)
	putHeader(b, header{Level: level, Opcode: Op1DM, TLVOffset: oneDMTLVOffset})
	putTimestamp(b[4:], tx)
	return b
}

// ---- LMM / LMR ----

const lmTLVOffset = 12

type lm struct {
	Level               uint8
	Opcode              uint8
	TxFCf, RxFCf, TxFCb uint32
}

func (l *lm) encode() []byte {
	b := make([]byte, 4+lmTLVOffset+1)
	putHeader(b, header{Level: l.Level, Opcode: l.Opcode, TLVOffset: lmTLVOffset})
	binary.BigEndian.PutUint32(b[4:], l.TxFCf)
	binary.BigEndian.PutUint32(b[8:], l.RxFCf)
	binary.BigEndian.PutUint32(b[12:], l.TxFCb)
	return b
}

func parseLM(h header, b []byte) (*lm, error) {
	if h.TLVOffset < lmTLVOffset || len(b) < 4+lmTLVOffset {
		return nil, errShort
	}
	return &lm{Level: h.Level, Opcode: h.Opcode, TxFCf: binary.BigEndian.Uint32(b[4:]), RxFCf: binary.BigEndian.Uint32(b[8:]), TxFCb: binary.BigEndian.Uint32(b[12:])}, nil
}

// ---- TST ----

const tstTLVOffset = 4

// Test TLV pattern types (Y.1731 9.3.1).
const (
	patNull    uint8 = 0
	patNullCRC uint8 = 1
	patPRBS    uint8 = 2
	patPRBSCRC uint8 = 3
)

// prbs31 fills b with the PRBS 2^31-1 sequence (x^31 + x^28 + 1) from an
// all-ones seed.
func prbs31(b []byte) {
	r := uint32(0x7fffffff)
	for i := range b {
		var x byte
		for bit := 0; bit < 8; bit++ {
			n := (r>>30 ^ r>>27) & 1
			r = (r<<1 | n) & 0x7fffffff
			x = x<<1 | byte(n)
		}
		b[i] = x
	}
}

func encodeTST(level uint8, seq uint32, size int, prbs, crc bool) []byte {
	b := make([]byte, 4+tstTLVOffset, 4+tstTLVOffset+8+size)
	putHeader(b, header{Level: level, Opcode: OpTST, TLVOffset: tstTLVOffset})
	binary.BigEndian.PutUint32(b[4:], seq)
	pt := patNull
	pat := make([]byte, size)
	if prbs {
		pt = patPRBS
		prbs31(pat)
	}
	v := append([]byte{pt}, pat...)
	if !crc {
		return append(appendTLV(b, tlvTest, v), tlvEnd)
	}
	v[0]++
	b = appendTLV(b, tlvTest, append(v, 0, 0, 0, 0))
	end := len(b) - 4
	binary.BigEndian.PutUint32(b[end:], tstCRC(b[4+tstTLVOffset:end]))
	return append(b, tlvEnd)
}

// tstCRC is the Test TLV's CRC-32 as SR OS computes it: over the TLV
// from its Type field through the pattern, without the final inversion.
func tstCRC(tlv []byte) uint32 { return ^crc32.ChecksumIEEE(tlv) }

type tst struct {
	Seq      uint32
	Bytes    int
	BitError bool // CRC (or pattern) mismatch
	Checked  bool // the PDU could be checked
}

func parseTST(h header, b []byte) (*tst, error) {
	if h.TLVOffset < tstTLVOffset || len(b) < 4+tstTLVOffset {
		return nil, errShort
	}
	t := &tst{Seq: binary.BigEndian.Uint32(b[4:])}
	off := 4 + int(h.TLVOffset)
	tlvs, _ := parseTLVs(b[off:])
	for _, x := range tlvs {
		if x.Type != tlvTest || len(x.Value) < 1 {
			continue
		}
		pt, pat := x.Value[0], x.Value[1:]
		if pt == patNullCRC || pt == patPRBSCRC {
			if len(pat) < 4 || len(b) < off+3+len(x.Value) {
				return t, errShort
			}
			want := binary.BigEndian.Uint32(pat[len(pat)-4:])
			pat = pat[:len(pat)-4]
			// Accept SR OS's coverage, or a CRC of the pattern alone.
			covered := b[off : off+3+len(x.Value)-4]
			t.Checked, t.BitError = true, tstCRC(covered) != want && crc32.ChecksumIEEE(pat) != want
		} else if pt == patNull {
			// All zeros; a PRBS without CRC can't be checked (seeds
			// and phase differ between implementations).
			t.Checked = true
			for _, x := range pat {
				if x != 0 {
					t.BitError = true
					break
				}
			}
		}
		t.Bytes = len(pat)
	}
	return t, nil
}

var errNoCounters = errors.New("the device has no frame counters")
