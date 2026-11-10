package macsec

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/ripnet/clabgen/internal/pcapng"
	"github.com/ripnet/clabgen/internal/pkt"
)

// The captures in testdata are Linux kernel MACsec between two ends with
// static keys (testdata/gen.sh): 02:00:00:00:00:a1 and :b1, port 1, AN 0,
// XPN salt 0102…0c with SSCI 1 for :a1 and 2 for :b1.
var (
	macA = pkt.MAC{2, 0, 0, 0, 0, 0xa1}
	macB = pkt.MAC{2, 0, 0, 0, 0, 0xb1}
	k128 = mustHex("000102030405060708090a0b0c0d0e0f")
	k256 = mustHex("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	salt = [12]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func ssciOf(m pkt.MAC) uint32 {
	if m == macA {
		return 1
	}
	return 2
}

type capture struct {
	file    string
	cipher  Cipher
	key     []byte
	encrypt bool
	sci     bool
	firstPN uint64
}

var captures = []capture{
	{"gcm128", GCMAES128, k128, true, true, 1},
	{"gcm256", GCMAES256, k256, true, true, 1},
	{"xpn128", GCMAESXPN128, k128, true, true, 1},
	{"xpn256", GCMAESXPN256, k256, true, true, 1},
	{"xpnwrap", GCMAESXPN128, k128, true, true, 4294967294},
	{"integ", GCMAES128, k128, false, true, 1},
	{"nosci", GCMAES128, k128, true, false, 1},
	{"clear", GCMAES128, k128, true, true, 1},  // MACsec under VLAN 100
	{"enctag", GCMAES128, k128, true, true, 1}, // VLAN 100 inside MACsec
}

// split finds a frame's MACsec part: hdr is the MACs and any VLAN tags in
// front of it, sec the rest. ok is false for frames that aren't MACsec.
func split(b []byte) (hdr, sec []byte, ok bool) {
	off := 12
	for off+2 <= len(b) {
		et := binary.BigEndian.Uint16(b[off:])
		switch {
		case et == EtherType:
			return b[:off], b[off:], true
		case pkt.IsTPID(et):
			off += 4
		default:
			return nil, nil, false
		}
	}
	return nil, nil, false
}

// TestLinuxFrames checks every MACsec frame Linux sent: it validates, and
// protecting what it carries again with the same PN gives the same bytes.
func TestLinuxFrames(t *testing.T) {
	for _, c := range captures {
		t.Run(c.file, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + c.file + ".pcap")
			if err != nil {
				t.Fatal(err)
			}
			frames, err := pcapng.Read(raw)
			if err != nil {
				t.Fatal(err)
			}
			rx, _ := New(Config{Cipher: c.cipher, Validate: ValidateStrict, ReplayProtect: true})
			for _, m := range []pkt.MAC{macA, macB} {
				if err := rx.InstallRx(MakeSCI(m, 1), Key{SAK: c.key, Salt: salt, SSCI: ssciOf(m)}, c.firstPN); err != nil {
					t.Fatal(err)
				}
			}
			n := 0
			for i, f := range frames {
				hdr, sec, ok := split(f)
				if !ok {
					continue
				}
				n++
				msdu, err := rx.Validate(hdr, sec)
				if err != nil {
					t.Fatalf("frame %d: %v", i+1, err)
				}
				src := pkt.MAC(hdr[6:12])
				pn := uint64(binary.BigEndian.Uint32(sec[4:]))
				if c.cipher.XPN() && pn < c.firstPN&0xffffffff {
					pn += c.firstPN &^ 0xffffffff
					pn += 1 << 32
				} else {
					pn += c.firstPN &^ 0xffffffff
				}
				tx, _ := New(Config{SCI: MakeSCI(src, 1), Cipher: c.cipher, IncludeSCI: c.sci, Encrypt: c.encrypt})
				if err := tx.InstallTx(Key{SAK: c.key, Salt: salt, SSCI: ssciOf(src)}, pn); err != nil {
					t.Fatal(err)
				}
				var out []byte
				if err := tx.Protect(hdr, msdu, func(b []byte) error { out = b; return nil }); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(out[:len(f)], f) || !allZero(out[len(f):]) {
					t.Fatalf("frame %d: protected again\n got %x\nwant %x", i+1, out, f)
				}
			}
			if n < 4 {
				t.Fatalf("only %d MACsec frames", n)
			}
			st := rx.Status()
			var ok uint64
			for _, sc := range st.RxSCs {
				ok += sc.InPktsOK
			}
			if ok != uint64(n) {
				t.Fatalf("InPktsOK %d, want %d", ok, n)
			}
		})
	}
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func pair(t *testing.T, cfg Config) (a, b *SecY) {
	t.Helper()
	ca, cb := cfg, cfg
	ca.SCI, cb.SCI = MakeSCI(macA, 1), MakeSCI(macB, 1)
	a, _ = New(ca)
	b, _ = New(cb)
	for _, s := range []*SecY{a, b} {
		if err := s.InstallTx(Key{SAK: k128, Salt: salt, SSCI: ssciOf(s.cfg.SCI.MAC())}, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.InstallRx(ca.SCI, Key{SAK: k128, Salt: salt, SSCI: 1}, 1); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func hdrOf(src, dst pkt.MAC) []byte { return append(append([]byte(nil), dst[:]...), src[:]...) }

func protect(t *testing.T, s *SecY, msdu []byte) []byte {
	t.Helper()
	var out []byte
	if err := s.Protect(hdrOf(macA, macB), msdu, func(b []byte) error { out = b; return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	for _, cfg := range []Config{
		{Cipher: GCMAES128, IncludeSCI: true, Encrypt: true, ReplayProtect: true},
		{Cipher: GCMAES128, Encrypt: true, Offset: 30},
		{Cipher: GCMAES128, Encrypt: true, Offset: 50, IncludeSCI: true},
		{Cipher: GCMAESXPN128, EndStation: true, Encrypt: true},
		{Cipher: GCMAES128},
	} {
		a, b := pair(t, cfg)
		for _, size := range []int{2, 29, 47, 48, 49, 100, 1500} {
			msdu := make([]byte, size)
			for i := range msdu {
				msdu[i] = byte(i)
			}
			f := protect(t, a, msdu)
			if len(f) < 60 {
				t.Fatalf("%d bytes: frame %d, not padded", size, len(f))
			}
			got, err := b.Validate(f[:12], f[12:])
			if err != nil {
				t.Fatalf("%+v, %d bytes: %v", cfg, size, err)
			}
			if !bytes.Equal(got, msdu) {
				t.Fatalf("%+v, %d bytes: got %x", cfg, size, got)
			}
			if cfg.Offset > 0 && size > cfg.Offset && !bytes.Equal(f[12+a.Overhead()-ICVLen:][:cfg.Offset], msdu[:cfg.Offset]) {
				t.Fatalf("offset %d: first bytes not in clear", cfg.Offset)
			}
		}
	}
}

func TestReplayAndTamper(t *testing.T) {
	a, b := pair(t, Config{Cipher: GCMAES128, IncludeSCI: true, Encrypt: true, ReplayProtect: true, ReplayWindow: 2})
	frames := make([][]byte, 8)
	for i := range frames {
		frames[i] = protect(t, a, []byte("hello, this is a frame long enough not to be short"))
	}
	val := func(f []byte) error { _, err := b.Validate(f[:12], f[12:]); return err }
	if err := val(frames[5]); err != nil { // PN 6
		t.Fatal(err)
	}
	if err := val(frames[4]); err != nil { // PN 5: inside the window of 2
		t.Fatalf("PN 5 within the window: %v", err)
	}
	if err := val(frames[2]); !errors.Is(err, ErrLate) { // PN 3 < 7-2
		t.Fatalf("PN 3: %v, want late", err)
	}
	bad := append([]byte(nil), frames[6]...)
	bad[40] ^= 1
	if err := val(bad); !errors.Is(err, ErrNotValid) {
		t.Fatalf("tampered: %v", err)
	}
	wrongAN := append([]byte(nil), frames[7]...)
	wrongAN[14] |= 1
	if err := val(wrongAN); !errors.Is(err, ErrNoRxSA) {
		t.Fatalf("wrong AN: %v", err)
	}
	st := b.Status()
	sc := st.RxSCs[0]
	if sc.InPktsOK != 2 || sc.InPktsLate != 1 || sc.InPktsNotValid != 1 || sc.InPktsNotUsingSA != 1 {
		t.Fatalf("counters %+v", sc.RxSCStats)
	}
}

func TestLearnAndExhaustion(t *testing.T) {
	a, _ := New(Config{SCI: MakeSCI(macA, 1), Cipher: GCMAES128, IncludeSCI: true, Encrypt: true})
	_ = a.InstallTx(Key{SAK: k128}, 0xffffffff)
	f := protect(t, a, []byte("x"))
	if err := a.Protect(hdrOf(macA, macB), []byte("y"), func([]byte) error { return nil }); !errors.Is(err, ErrExhausted) {
		t.Fatalf("after the last PN: %v", err)
	}
	b, _ := New(Config{SCI: MakeSCI(macB, 1), Cipher: GCMAES128, Encrypt: true})
	if _, err := b.Validate(f[:12], f[12:]); !errors.Is(err, ErrNoSC) {
		t.Fatalf("unknown SCI: %v", err)
	}
	_ = b.Learn(Key{SAK: k128}, 1)
	if _, err := b.Validate(f[:12], f[12:]); err != nil {
		t.Fatalf("learned SCI: %v", err)
	}
	if st := b.Status(); st.InPktsNoSCI != 1 || len(st.RxSCs) != 1 || st.RxSCs[0].SCI != MakeSCI(macA, 1) {
		t.Fatalf("status %+v", st)
	}
	none, _ := New(Config{Cipher: GCMAES128})
	if err := none.Protect(hdrOf(macA, macB), nil, func([]byte) error { return nil }); !errors.Is(err, ErrNoSA) {
		t.Fatalf("no SA: %v", err)
	}
}
