package twamp

import (
	"encoding/hex"
	"testing"
	"time"
)

// UDP payloads sent by SR-SIM 26.10.R1 (lab addresses only).
var (
	// OAM-PM session-sender-type twamp-light, IPv4.
	srTWAMPLight = "00000004eea6f5cffde2bcef0001"
	// OAM-PM session-sender-type stamp, pad-tlv-size 20, IPv6: the TLV's
	// length (20) counts its header, so 16 bytes of value follow.
	srSTAMP = "00000004eea6f5cffb034b0400010001" + zeros(28) + "80010014" + zeros(16)
	// Interface link measurement (STAMP, TTL 1, DSCP 48), pad-tlv-size 8.
	srLinkMeas = "00000005eea6f60426a3593500010003" + zeros(28) + "80010008" + zeros(4)
)

func zeros(n int) string { return hex.EncodeToString(make([]byte, n)) }

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseRouterPackets(t *testing.T) {
	tl, ok := parseRequest(mustHex(t, srTWAMPLight), true)
	if !ok || tl.seq != 4 || tl.stamp || tl.err != 1 || tl.ptp {
		t.Fatalf("twamp light: %+v", tl)
	}
	st, ok := parseRequest(mustHex(t, srSTAMP), true)
	if !ok || !st.stamp || st.seq != 4 || st.ssid != 1 || len(st.tlvs) != 1 || st.tlvs[0].typ != tlvExtraPadding || st.tlvs[0].flags != flagU || len(st.tlvs[0].value) != 16 || !st.tlvs[0].lengthWithHeader {
		t.Fatalf("stamp: %+v", st)
	}
	lm, ok := parseRequest(mustHex(t, srLinkMeas), true)
	if !ok || !lm.stamp || lm.ssid != 3 || len(lm.tlvs) != 1 || len(lm.tlvs[0].value) != 4 {
		t.Fatalf("link measurement: %+v", lm)
	}
	if lm2, _ := parseRequest(mustHex(t, srLinkMeas), false); lm2.stamp || len(lm2.tlvs) != 0 || lm2.ssid != 3 {
		t.Fatalf("twamp-light mode looked at TLVs: %+v", lm2)
	}
	// TWAMP Light padded past the STAMP base with zeros: no TLVs.
	if p, _ := parseRequest(append(mustHex(t, srTWAMPLight), make([]byte, 50)...), true); len(p.tlvs) != 0 {
		t.Fatalf("zero padding as TLVs: %+v", p)
	}
	// Lengths by RFC 8972 (value only), two TLVs.
	two := mustHex(t, "00000004eea6f5cffb034b0400010001"+zeros(28)+"80010004"+zeros(4)+"80030004"+zeros(4))
	if p, _ := parseRequest(two, true); len(p.tlvs) != 2 || p.tlvs[0].lengthWithHeader || p.tlvs[1].typ != tlvTimestamp {
		t.Fatalf("two TLVs: %+v", p)
	}
	// A TLV running past the end is not a TLV.
	bad := mustHex(t, "00000004eea6f5cffb034b0400010001"+zeros(28)+"80010020"+zeros(4))
	if p, _ := parseRequest(bad, true); len(p.tlvs) != 0 {
		t.Fatalf("overlong TLV: %+v", p)
	}
}

func TestStampFormats(t *testing.T) {
	now := time.Date(2026, 11, 17, 15, 48, 31, 980957000, time.UTC)
	b := make([]byte, 8)
	for _, ptp := range []bool{false, true} {
		stamp(b, now, ptp)
		if got := readStamp(b, ptp); got.Sub(now).Abs() > time.Microsecond {
			t.Fatalf("ptp %v: %v != %v", ptp, got, now)
		}
	}
	// The router's T1 above (NTP 0xeea6f5cf...) is 2026-11-16, in era 0.
	if got := readStamp(mustHex(t, "eea6f5cffb034b04"), false); got.Year() != 2026 {
		t.Fatalf("router NTP time %v", got)
	}
	stamp(b, time.Time{}, false)
	if !readStamp(b, false).IsZero() {
		t.Fatal("zero time")
	}
}
