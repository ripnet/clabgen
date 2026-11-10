package mka

import (
	"bytes"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/macsec"
	"github.com/ripnet/clabgen/internal/pkt"
)

func h(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// RFC 4493 section 4 and the AES-256 example of NIST SP 800-38B.
func TestCMAC(t *testing.T) {
	k := h("2b7e151628aed2a6abf7158809cf4f3c")
	m := h("6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411e5fbc1191a0a52eff69f2445df4f9b17ad2b417be66c3710")
	for _, c := range []struct {
		n    int
		want string
	}{{0, "bb1d6929e95937287fa37d129b756746"}, {16, "070a16b46b4d4144f79bdd9dd04a287c"}, {40, "dfa66747de9ae63030ca32611497c827"}, {64, "51f0bebf7e3b9d92fc49741779363cfe"}} {
		got, _ := cmac(k, m[:c.n])
		if hex.EncodeToString(got) != c.want {
			t.Errorf("CMAC-128 of %d bytes: %x, want %s", c.n, got, c.want)
		}
	}
	k256 := h("603deb1015ca71be2b73aef0857d77811f352c073b6108d72d9810a30914dff4")
	got, _ := cmac(k256, m[:16])
	if hex.EncodeToString(got) != "28a7023f452e8f82bd4bf28d8c37c35c" {
		t.Errorf("CMAC-256: %x", got)
	}
}

// RFC 3394 section 4.1 and 4.6.
func TestKeyWrap(t *testing.T) {
	for _, c := range []struct{ kek, key, want string }{
		{"000102030405060708090a0b0c0d0e0f", "00112233445566778899aabbccddeeff", "1fa68b0a8112b447aef34bd8fb5a7b829d3e862371d2cfe5"},
		{"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f", "00112233445566778899aabbccddeeff000102030405060708090a0b0c0d0e0f",
			"28c9f404c4b810f4cbccb35cfb87f8263f5786e2d80ed326cbc7f0e71a99f43bfb988b9b7a02dd21"},
	} {
		w, err := wrap(h(c.kek), h(c.key))
		if err != nil || hex.EncodeToString(w) != c.want {
			t.Fatalf("wrap: %x %v, want %s", w, err, c.want)
		}
		k, err := unwrap(h(c.kek), w)
		if err != nil || !bytes.Equal(k, h(c.key)) {
			t.Fatalf("unwrap: %x %v", k, err)
		}
		w[3] ^= 1
		if _, err := unwrap(h(c.kek), w); err == nil {
			t.Fatal("unwrap of a damaged key passed")
		}
	}
}

// IEEE 802.1X-2010 key derivations, checked against wpa_supplicant (the
// values are what its aes_kdf gives for this CAK and CKN).
func TestDerivations(t *testing.T) {
	cak := h("0123456789abcdef0123456789abcdef")
	ckn := h("6162636465666768696a6b6c6d6e6f707172737475767778797a303132333435")
	ick, _ := DeriveICK(cak, ckn)
	kek, _ := DeriveKEK(cak, ckn)
	if len(ick) != 16 || len(kek) != 16 || bytes.Equal(ick, kek) {
		t.Fatalf("ick %x kek %x", ick, kek)
	}
	// The derivation is deterministic, and the 256-bit form differs.
	ick2, _ := DeriveICK(cak, ckn)
	if !bytes.Equal(ick, ick2) {
		t.Fatal("ICK not deterministic")
	}
	ick256, _ := DeriveICK(append(cak, cak...), ckn)
	if len(ick256) != 32 {
		t.Fatalf("ICK-256 of %d bytes", len(ick256))
	}
}

func TestMKPDURoundTrip(t *testing.T) {
	ick := h("00112233445566778899aabbccddeeff")
	m := &MKPDU{Version: 1, Priority: 16, KeyServer: true, Desired: true, Capability: 3, SCI: 0x0200000000a10001,
		MI: MI{1, 2, 3}, MN: 7, CKN: []byte("abc"),
		Live:      []PeerID{{MI: MI{9}, MN: 3}},
		Potential: []PeerID{{MI: MI{8}, MN: 1}, {MI: MI{7}, MN: 2}},
		SAKUse:    &SAKUse{LAN: 2, LTx: true, LRx: true, OAN: 1, ORx: true, Latest: KI{MI: MI{1}, KN: 4}, LLPN: 99, Old: KI{MI: MI{1}, KN: 3}, OLPN: 5},
		DistSAK:   &DistSAK{AN: 2, Offset: 1, KN: 4, Suite: 0x0080c20001000002, Wrapped: bytes.Repeat([]byte{0xaa}, 40)}}
	dst, src := GroupAddress, pkt.MAC{2, 0, 0, 0, 0, 0xa1}
	b, err := m.Encode(dst, src, ick)
	if err != nil {
		t.Fatal(err)
	}
	if len(b)%4 != 0 {
		t.Fatalf("length %d not a multiple of 4", len(b))
	}
	got, err := Decode(dst, src, append(b, 0, 0, 0), ick) // with padding
	if err != nil {
		t.Fatal(err)
	}
	if got.MN != 7 || got.SCI != m.SCI || string(got.CKN) != "abc" || len(got.Live) != 1 || len(got.Potential) != 2 ||
		*got.SAKUse != *m.SAKUse || got.DistSAK.Suite != m.DistSAK.Suite || !bytes.Equal(got.DistSAK.Wrapped, m.DistSAK.Wrapped) ||
		got.DistSAK.AN != 2 || got.DistSAK.Offset != 1 || !got.KeyServer || got.Capability != 3 {
		t.Fatalf("decoded %+v", got)
	}
	b[20] ^= 1
	if _, err := Decode(dst, src, b, ick); err != ErrICV {
		t.Fatalf("damaged: %v", err)
	}
}

// link joins two participants: each one's MKPDUs reach the other, unless
// the sender's are dropped.
type link struct {
	mu   sync.Mutex
	ps   map[macsec.SCI]*Participant
	drop map[macsec.SCI]bool
}

func (l *link) hooks(from macsec.SCI) Hooks {
	return Hooks{Send: func(b []byte) error {
		l.mu.Lock()
		var to []*Participant
		for sci, p := range l.ps {
			if sci != from && !l.drop[from] {
				to = append(to, p)
			}
		}
		l.mu.Unlock()
		for _, p := range to {
			go p.Receive(GroupAddress, from.MAC(), append([]byte(nil), b...))
		}
		return nil
	}}
}

func newParticipant(t *testing.T, l *link, mac byte, prio uint8, cak []byte) (*Participant, *macsec.SecY) {
	t.Helper()
	sci := macsec.MakeSCI(pkt.MAC{2, 0, 0, 0, 0, mac}, 1)
	secy, _ := macsec.New(macsec.Config{SCI: sci, Cipher: macsec.GCMAES128, IncludeSCI: true, Encrypt: true, ReplayProtect: true})
	p, err := New(Config{CAK: cak, CKN: []byte("lab"), SCI: sci, Priority: prio, Cipher: macsec.GCMAES128, Encrypt: true,
		Hello: 50 * time.Millisecond, Life: 300 * time.Millisecond, Retire: 100 * time.Millisecond}, secy, l.hooks(sci))
	if err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.ps[sci] = p
	l.mu.Unlock()
	return p, secy
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// exchange sends a frame each way and reports whether both validated.
func exchange(a, b *macsec.SecY) bool {
	try := func(x, y *macsec.SecY) bool {
		dst, src := y.Config().SCI.MAC(), x.Config().SCI.MAC()
		hdr := append(append([]byte(nil), dst[:]...), src[:]...)
		var out []byte
		if x.Protect(hdr, []byte("ping over MACsec, long enough not to be short ......"), func(f []byte) error { out = f; return nil }) != nil {
			return false
		}
		_, err := y.Validate(out[:12], out[12:])
		return err == nil
	}
	return try(a, b) && try(b, a)
}

func TestPairSecures(t *testing.T) {
	l := &link{ps: map[macsec.SCI]*Participant{}, drop: map[macsec.SCI]bool{}}
	cak := h("0123456789abcdef0123456789abcdef")
	a, sa := newParticipant(t, l, 0xa1, 16, cak)
	b, sb := newParticipant(t, l, 0xb1, 32, cak)
	a.Start()
	b.Start()
	defer a.Stop()
	defer b.Stop()
	waitFor(t, "both secured", func() bool { return a.Secured() && b.Secured() })
	if !exchange(sa, sb) {
		t.Fatal("frames don't validate")
	}
	as, bs := a.Status(), b.Status()
	if !as.KeyServer || bs.KeyServer || *bs.KeyServerSCI != a.cfg.SCI || as.State != "secured" || bs.Latest.KN != 1 {
		t.Fatalf("a %+v\nb %+v", as, bs)
	}

	// A rekey: both move to the new key, and traffic keeps validating.
	if err := b.Rekey(); err == nil {
		t.Fatal("b rekeyed without being key server")
	}
	if err := a.Rekey(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new key in use", func() bool {
		st := b.Status()
		return st.Latest != nil && st.Latest.KN == 2 && st.Latest.Tx && a.Status().Latest.Tx
	})
	if !exchange(sa, sb) {
		t.Fatal("frames don't validate after the rekey")
	}
	waitFor(t, "old key retired", func() bool { return a.Status().Old == nil && b.Status().Old == nil })

	// a goes quiet: b times it out and removes its keys.
	a.Pause()
	waitFor(t, "b unsecured", func() bool { return !b.Secured() })
	if exchange(sa, sb) {
		t.Fatal("frames still validate with no live peer")
	}
	a.Resume()
	waitFor(t, "secured again", func() bool { return a.Secured() && b.Secured() })
	if !exchange(sa, sb) {
		t.Fatal("frames don't validate after resuming")
	}
}

func TestWrongCAK(t *testing.T) {
	l := &link{ps: map[macsec.SCI]*Participant{}, drop: map[macsec.SCI]bool{}}
	a, _ := newParticipant(t, l, 0xa1, 16, h("0123456789abcdef0123456789abcdef"))
	b, _ := newParticipant(t, l, 0xb1, 16, h("ffffffffffffffffffffffffffffffff"))
	a.Start()
	b.Start()
	defer a.Stop()
	defer b.Stop()
	waitFor(t, "ICV failures", func() bool { return a.Status().RxBadICV > 2 })
	if a.Secured() || len(a.Status().Peers) != 0 {
		t.Fatalf("a %+v", a.Status())
	}
}

// TestOneWayLoss: one end's MKPDUs are lost (a provider dropping EAPOL one
// way). Both ends lose the session, not just the one that stopped hearing,
// and they key again once the MKPDUs get through.
func TestOneWayLoss(t *testing.T) {
	l := &link{ps: map[macsec.SCI]*Participant{}, drop: map[macsec.SCI]bool{}}
	cak := h("0123456789abcdef0123456789abcdef")
	a, sa := newParticipant(t, l, 0xa1, 10, cak)
	b, sb := newParticipant(t, l, 0xb1, 16, cak)
	a.Start()
	b.Start()
	defer a.Stop()
	defer b.Stop()
	waitFor(t, "secured", func() bool { return a.Secured() && b.Secured() })
	l.mu.Lock()
	l.drop[a.cfg.SCI] = true
	l.mu.Unlock()
	waitFor(t, "both unsecured", func() bool { return !a.Secured() && !b.Secured() })
	l.mu.Lock()
	l.drop[a.cfg.SCI] = false
	l.mu.Unlock()
	waitFor(t, "secured again", func() bool { return a.Secured() && b.Secured() })
	if !exchange(sa, sb) {
		t.Fatal("frames don't validate after recovering")
	}
}
