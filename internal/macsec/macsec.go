// Package macsec is the IEEE 802.1AE data plane: a SecY that protects the
// frames a device sends with its transmit SA and validates the frames it
// receives with a receive SC per peer. Keys come from outside: static
// config, or MKA.
package macsec

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

const (
	EtherType      uint16 = 0x88e5
	EtherTypeEAPOL uint16 = 0x888e
	ICVLen                = 16
	// Secure data shorter than this is announced in the SecTAG's short
	// length, so a receiver can tell it from the padding behind the ICV.
	shortLen = 48
	tagLen   = 8 // SecTAG without the SCI, ethertype included
	sciLen   = 8
)

// TCI bits (the AN is the low two bits).
const (
	tciV   = 0x80
	tciES  = 0x40
	tciSC  = 0x20
	tciSCB = 0x10
	tciE   = 0x08
	tciC   = 0x04
)

// Cipher is a cipher suite.
type Cipher uint8

const (
	GCMAES128 Cipher = iota
	GCMAES256
	GCMAESXPN128
	GCMAESXPN256
)

var cipherNames = [...]string{"gcm-aes-128", "gcm-aes-256", "gcm-aes-xpn-128", "gcm-aes-xpn-256"}

// CipherNames lists the suites by name, for config schemas.
func CipherNames() []string { return cipherNames[:] }

func ParseCipher(s string) (Cipher, error) {
	for i, n := range cipherNames {
		if strings.EqualFold(s, n) {
			return Cipher(i), nil
		}
	}
	return 0, fmt.Errorf("unknown cipher suite %q (%s)", s, strings.Join(cipherNames[:], ", "))
}

func (c Cipher) String() string {
	if int(c) < len(cipherNames) {
		return cipherNames[c]
	}
	return fmt.Sprintf("cipher(%d)", c)
}

// KeyLen is the SAK length in bytes.
func (c Cipher) KeyLen() int {
	if c == GCMAES256 || c == GCMAESXPN256 {
		return 32
	}
	return 16
}

// XPN reports whether the suite uses 64-bit packet numbers.
func (c Cipher) XPN() bool { return c == GCMAESXPN128 || c == GCMAESXPN256 }

// SuiteID is the 64-bit cipher suite identifier MKA announces.
func (c Cipher) SuiteID() uint64 { return 0x0080C20001000001 + uint64(c) }

// MaxPN is the last packet number an SA may use.
func (c Cipher) MaxPN() uint64 {
	if c.XPN() {
		return ^uint64(0)
	}
	return 0xffffffff
}

// SCI is a secure channel identifier: a MAC address and a port number.
type SCI uint64

func MakeSCI(mac pkt.MAC, port uint16) SCI {
	var b [8]byte
	copy(b[:6], mac[:])
	binary.BigEndian.PutUint16(b[6:], port)
	return SCI(binary.BigEndian.Uint64(b[:]))
}

func (s SCI) MAC() pkt.MAC {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(s))
	return pkt.MAC(b[:6])
}

func (s SCI) Port() uint16 { return uint16(s) }

// String is the SCI as 16 hex digits, as Linux and most routers show it.
func (s SCI) String() string { return fmt.Sprintf("%016x", uint64(s)) }

func (s SCI) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Validation is the validateFrames setting.
type Validation uint8

const (
	ValidateStrict Validation = iota
	ValidateCheck
	ValidateDisabled
)

var validationNames = [...]string{"strict", "check", "disabled"}

func ParseValidation(s string) (Validation, error) {
	for i, n := range validationNames {
		if s == n {
			return Validation(i), nil
		}
	}
	return 0, fmt.Errorf("validate: strict, check or disabled, not %q", s)
}

func (v Validation) String() string { return validationNames[v] }

// Config is a SecY's settings.
type Config struct {
	SCI        SCI
	Cipher     Cipher
	IncludeSCI bool // send the SCI in the SecTAG
	EndStation bool // ES: the SCI is the source MAC and port 1
	SCB        bool // single copy broadcast
	// Encrypt is confidentiality; false protects integrity only.
	Encrypt bool
	// Offset is the confidentiality offset: 0, 30 or 50 bytes left in clear.
	Offset        int
	Validate      Validation
	ReplayProtect bool
	ReplayWindow  uint64
}

// Key is one SA's key material.
type Key struct {
	AN  uint8
	SAK []byte
	// XPN only: the salt, and the SSCI of the SA's transmitter.
	Salt [12]byte
	SSCI uint32
}

type sa struct {
	an      uint8
	aead    cipher.AEAD
	salt    [12]byte
	ssci    uint32
	next    uint64 // tx: next PN to send; rx: highest PN validated + 1
	lowest  uint64 // rx: lowest acceptable PN
	created time.Time
	stats   SAStats
}

// SAStats are an SA's counters (802.1AE clause 10.7).
type SAStats struct {
	OutPktsProtected uint64 `json:"out_pkts_protected,omitempty"`
	OutPktsEncrypted uint64 `json:"out_pkts_encrypted,omitempty"`
	InPktsOK         uint64 `json:"in_pkts_ok,omitempty"`
	InPktsInvalid    uint64 `json:"in_pkts_invalid,omitempty"`
	InPktsNotValid   uint64 `json:"in_pkts_not_valid,omitempty"`
	InPktsNotUsingSA uint64 `json:"in_pkts_not_using_sa,omitempty"`
	InPktsUnusedSA   uint64 `json:"in_pkts_unused_sa,omitempty"`
}

// RxSCStats are a receive SC's counters.
type RxSCStats struct {
	InPktsOK          uint64 `json:"in_pkts_ok"`
	InPktsUnchecked   uint64 `json:"in_pkts_unchecked,omitempty"`
	InPktsDelayed     uint64 `json:"in_pkts_delayed,omitempty"`
	InPktsLate        uint64 `json:"in_pkts_late,omitempty"`
	InPktsInvalid     uint64 `json:"in_pkts_invalid,omitempty"`
	InPktsNotValid    uint64 `json:"in_pkts_not_valid,omitempty"`
	InPktsNotUsingSA  uint64 `json:"in_pkts_not_using_sa,omitempty"`
	InPktsUnusedSA    uint64 `json:"in_pkts_unused_sa,omitempty"`
	InOctetsValidated uint64 `json:"in_octets_validated,omitempty"`
	InOctetsDecrypted uint64 `json:"in_octets_decrypted,omitempty"`
}

// Stats are the SecY's counters that aren't per SC.
type Stats struct {
	OutPktsProtected   uint64 `json:"out_pkts_protected"`
	OutPktsEncrypted   uint64 `json:"out_pkts_encrypted"`
	OutOctetsProtected uint64 `json:"out_octets_protected,omitempty"`
	OutOctetsEncrypted uint64 `json:"out_octets_encrypted,omitempty"`
	// Frames not sent: no transmit SA, or its packet numbers ran out.
	OutPktsNoSA      uint64 `json:"out_pkts_no_sa,omitempty"`
	OutPktsExhausted uint64 `json:"out_pkts_pn_exhausted,omitempty"`
	OutPktsUntagged  uint64 `json:"out_pkts_untagged,omitempty"`
	InPktsUntagged   uint64 `json:"in_pkts_untagged,omitempty"`
	InPktsNoTag      uint64 `json:"in_pkts_no_tag,omitempty"`
	InPktsBadTag     uint64 `json:"in_pkts_bad_tag,omitempty"`
	InPktsUnknownSCI uint64 `json:"in_pkts_unknown_sci,omitempty"`
	InPktsNoSCI      uint64 `json:"in_pkts_no_sci,omitempty"`
	InPktsOverrun    uint64 `json:"in_pkts_overrun,omitempty"`
}

type rxSC struct {
	sci   SCI
	sas   [4]*sa
	stats RxSCStats
	last  time.Time
}

// SecY is one MAC security entity.
type SecY struct {
	cfg Config

	txMu  sync.Mutex // held while a protected frame is sent, to keep PN order
	tx    [4]*sa
	encSA int // the encoding SA's AN, -1: none
	stats Stats

	rxMu    sync.Mutex
	rx      map[SCI]*rxSC
	learn   *Key // receive SCs for unknown SCIs are made with this key
	learnPN uint64
	rstats  Stats
}

var (
	ErrNoSA      = errors.New("macsec: no transmit SA")
	ErrExhausted = errors.New("macsec: packet numbers exhausted")
	ErrBadTag    = errors.New("macsec: bad SecTAG")
	ErrNoSC      = errors.New("macsec: no receive SC for the SCI")
	ErrNoRxSA    = errors.New("macsec: no receive SA for the AN")
	ErrLate      = errors.New("macsec: packet number below the replay window")
	ErrNotValid  = errors.New("macsec: ICV check failed")
)

func New(cfg Config) (*SecY, error) {
	switch cfg.Offset {
	case 0, 30, 50:
	default:
		return nil, fmt.Errorf("confidentiality offset: 0, 30 or 50, not %d", cfg.Offset)
	}
	if cfg.EndStation && cfg.IncludeSCI {
		return nil, errors.New("end station frames don't carry the SCI")
	}
	return &SecY{cfg: cfg, encSA: -1, rx: map[SCI]*rxSC{}}, nil
}

func (s *SecY) Config() Config {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	return s.cfg
}

// Overhead is the bytes protection adds to a frame.
func (s *SecY) Overhead() int {
	n := tagLen + ICVLen
	if s.cfg.IncludeSCI {
		n += sciLen
	}
	return n
}

func (s *SecY) newSA(k Key, pn uint64) (*sa, error) {
	if k.AN > 3 {
		return nil, fmt.Errorf("AN %d: 0 to 3", k.AN)
	}
	if len(k.SAK) != s.cfg.Cipher.KeyLen() {
		return nil, fmt.Errorf("%s takes a %d-byte key, not %d", s.cfg.Cipher, s.cfg.Cipher.KeyLen(), len(k.SAK))
	}
	if pn == 0 {
		pn = 1
	}
	b, err := aes.NewCipher(k.SAK)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &sa{an: k.AN, aead: g, salt: k.Salt, ssci: k.SSCI, next: pn, lowest: pn, created: time.Now()}, nil
}

// InstallTx installs a transmit SA starting at packet number pn and makes it
// the encoding SA.
func (s *SecY) InstallTx(k Key, pn uint64) error {
	a, err := s.newSA(k, pn)
	if err != nil {
		return err
	}
	s.txMu.Lock()
	s.tx[k.AN] = a
	s.encSA = int(k.AN)
	s.txMu.Unlock()
	return nil
}

// RemoveTx deletes all transmit SAs: protected frames can't be sent.
func (s *SecY) RemoveTx() {
	s.txMu.Lock()
	s.tx = [4]*sa{}
	s.encSA = -1
	s.txMu.Unlock()
}

// RemoveTxAN deletes the transmit SA with an AN; if it was the encoding SA,
// protected frames can't be sent until another is installed.
func (s *SecY) RemoveTxAN(an uint8) {
	s.txMu.Lock()
	s.tx[an&3] = nil
	if s.encSA == int(an&3) {
		s.encSA = -1
	}
	s.txMu.Unlock()
}

// TxNextPN is the next packet number of the transmit SA with an AN (0: none).
func (s *SecY) TxNextPN(an uint8) uint64 {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	if a := s.tx[an&3]; a != nil {
		return a.next
	}
	return 0
}

// SetConfidentiality changes encryption and the confidentiality offset, as a
// key server's distributed SAK says.
func (s *SecY) SetConfidentiality(encrypt bool, offset int) {
	s.txMu.Lock()
	s.rxMu.Lock()
	s.cfg.Encrypt, s.cfg.Offset = encrypt, offset
	s.rxMu.Unlock()
	s.txMu.Unlock()
}

// RemoveRx deletes a receive SC.
func (s *SecY) RemoveRx(sci SCI) {
	s.rxMu.Lock()
	delete(s.rx, sci)
	s.rxMu.Unlock()
}

// RemoveRxAN deletes the receive SAs with an AN from every SC.
func (s *SecY) RemoveRxAN(an uint8) {
	s.rxMu.Lock()
	for _, sc := range s.rx {
		sc.sas[an&3] = nil
	}
	s.rxMu.Unlock()
}

// RemoveAll deletes every SA and SC: nothing can be sent or received
// protected.
func (s *SecY) RemoveAll() {
	s.RemoveTx()
	s.rxMu.Lock()
	s.rx = map[SCI]*rxSC{}
	s.rxMu.Unlock()
}

// InstallRx installs a receive SA for sci, accepting packet numbers from
// lowest on.
func (s *SecY) InstallRx(sci SCI, k Key, lowest uint64) error {
	a, err := s.newSA(k, lowest)
	if err != nil {
		return err
	}
	s.rxMu.Lock()
	sc := s.rx[sci]
	if sc == nil {
		sc = &rxSC{sci: sci}
		s.rx[sci] = sc
	}
	sc.sas[k.AN] = a
	s.rxMu.Unlock()
	return nil
}

// Learn makes a receive SC with k for every SCI that isn't known yet, as
// static keys shared by a group need.
func (s *SecY) Learn(k Key, lowest uint64) error {
	if _, err := s.newSA(k, lowest); err != nil {
		return err
	}
	kk := k
	kk.SAK = append([]byte(nil), k.SAK...)
	s.rxMu.Lock()
	s.learn, s.learnPN = &kk, lowest
	s.rxMu.Unlock()
	return nil
}

// Protect sends hdr (destination and source MAC, and the VLAN tags that stay
// in clear) and msdu (the rest of the frame, unpadded) as one protected
// frame, through send. It holds the SecY's transmit lock while sending, so
// frames leave in packet number order.
func (s *SecY) Protect(hdr, msdu []byte, send func([]byte) error) error {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	if s.encSA < 0 {
		s.stats.OutPktsNoSA++
		return ErrNoSA
	}
	a := s.tx[s.encSA]
	if a.next > s.cfg.Cipher.MaxPN() || a.next == 0 {
		s.stats.OutPktsExhausted++
		return ErrExhausted
	}
	pn := a.next
	a.next++

	tl := tagLen
	if s.cfg.IncludeSCI {
		tl += sciLen
	}
	n := len(hdr) + tl + len(msdu) + ICVLen
	size := n
	if size < 60 {
		size = 60
	}
	out := make([]byte, size)
	copy(out, hdr)
	tag := out[len(hdr) : len(hdr)+tl]
	binary.BigEndian.PutUint16(tag, EtherType)
	tci := a.an
	switch {
	case s.cfg.IncludeSCI:
		tci |= tciSC
	case s.cfg.EndStation:
		tci |= tciES
	}
	if s.cfg.SCB {
		tci |= tciSCB
	}
	if s.cfg.Encrypt {
		tci |= tciE | tciC
	}
	tag[2] = tci
	if len(msdu) < shortLen {
		tag[3] = byte(len(msdu))
	}
	binary.BigEndian.PutUint32(tag[4:], uint32(pn))
	if s.cfg.IncludeSCI {
		binary.BigEndian.PutUint64(tag[8:], uint64(s.cfg.SCI))
	}
	data := out[len(hdr)+tl : len(hdr)+tl+len(msdu)]
	copy(data, msdu)
	iv := s.iv(a, s.cfg.SCI, pn)
	clear := 0
	if s.cfg.Encrypt {
		clear = min(s.cfg.Offset, len(msdu))
	} else {
		clear = len(msdu)
	}
	aad := make([]byte, 0, 12+tl+clear)
	aad = append(aad, hdr[:12]...)
	aad = append(aad, tag...)
	aad = append(aad, data[:clear]...)
	a.aead.Seal(data[clear:clear], iv[:], data[clear:], aad)
	if s.cfg.Encrypt {
		a.stats.OutPktsEncrypted++
		s.stats.OutPktsEncrypted++
		s.stats.OutOctetsEncrypted += uint64(len(msdu))
	} else {
		a.stats.OutPktsProtected++
		s.stats.OutPktsProtected++
		s.stats.OutOctetsProtected += uint64(len(msdu))
	}
	return send(out)
}

func (s *SecY) iv(a *sa, sci SCI, pn uint64) [12]byte {
	var iv [12]byte
	if s.cfg.Cipher.XPN() {
		binary.BigEndian.PutUint32(iv[0:], a.ssci)
		binary.BigEndian.PutUint64(iv[4:], pn)
		for i := range iv {
			iv[i] ^= a.salt[i]
		}
		return iv
	}
	binary.BigEndian.PutUint64(iv[0:], uint64(sci))
	binary.BigEndian.PutUint32(iv[8:], uint32(pn))
	return iv
}

// Untagged counts a frame without a SecTAG and reports whether it may be
// delivered (only when frames aren't validated strictly).
func (s *SecY) Untagged() bool {
	s.rxMu.Lock()
	defer s.rxMu.Unlock()
	if s.cfg.Validate == ValidateStrict {
		s.rstats.InPktsNoTag++
		return false
	}
	s.rstats.InPktsUntagged++
	return true
}

// Validate checks a received frame. hdr is its destination and source MAC
// (and any VLAN tags in front of the SecTAG); sec is the rest, from the
// MACsec ethertype on, padding included. It returns the frame's MSDU (what
// follows the SecTAG, decrypted) or an error when the frame is dropped.
func (s *SecY) Validate(hdr, sec []byte) ([]byte, error) {
	src := pkt.MAC(hdr[6:12])
	s.rxMu.Lock()
	defer s.rxMu.Unlock()
	if len(sec) < tagLen+ICVLen || binary.BigEndian.Uint16(sec) != EtherType {
		s.rstats.InPktsBadTag++
		return nil, ErrBadTag
	}
	tci, sl := sec[2], int(sec[3])
	tl := tagLen
	if tci&tciSC != 0 {
		tl += sciLen
	}
	e, c := tci&tciE != 0, tci&tciC != 0
	pn32 := binary.BigEndian.Uint32(sec[4:])
	// Clause 9.3: V is 0; ES and SC aren't both set; SC isn't set with
	// SCB; E without C is reserved; SL is under 48; PN isn't 0.
	if tci&tciV != 0 || (tci&tciES != 0 && tci&tciSC != 0) || (tci&tciSC != 0 && tci&tciSCB != 0) ||
		(e && !c) || sl >= shortLen || (pn32 == 0 && !s.cfg.Cipher.XPN()) || len(sec) < tl+ICVLen {
		s.rstats.InPktsBadTag++
		return nil, ErrBadTag
	}
	dl := len(sec) - tl - ICVLen
	if sl != 0 {
		if sl > dl {
			s.rstats.InPktsBadTag++
			return nil, ErrBadTag
		}
		dl = sl
	} else if dl < shortLen {
		s.rstats.InPktsBadTag++
		return nil, ErrBadTag
	}
	var sci SCI
	switch {
	case tci&tciSC != 0:
		sci = SCI(binary.BigEndian.Uint64(sec[8:]))
	case tci&tciES != 0:
		sci = MakeSCI(src, 1)
	case len(s.rx) == 1:
		for k := range s.rx {
			sci = k
		}
	default:
		sci = MakeSCI(src, 1)
	}
	if sci == s.cfg.SCI {
		// Our own frame, looped back: never a peer's.
		s.rstats.InPktsNoSCI++
		return nil, ErrNoSC
	}
	sc := s.rx[sci]
	if sc == nil && s.learn != nil {
		if a, err := s.newSA(*s.learn, s.learnPN); err == nil {
			sc = &rxSC{sci: sci}
			sc.sas[a.an] = a
			s.rx[sci] = sc
		}
	}
	tag := sec[:tl]
	data := sec[tl : tl+dl]
	icv := sec[tl+dl : tl+dl+ICVLen]
	stripped := func() []byte { return append([]byte(nil), data...) }
	if sc == nil {
		if s.cfg.Validate == ValidateStrict || c {
			s.rstats.InPktsNoSCI++
			return nil, ErrNoSC
		}
		s.rstats.InPktsUnknownSCI++
		return stripped(), nil
	}
	sc.last = time.Now()
	a := sc.sas[tci&3]
	if a == nil {
		if s.cfg.Validate == ValidateStrict || c {
			sc.stats.InPktsNotUsingSA++
			return nil, ErrNoRxSA
		}
		sc.stats.InPktsUnusedSA++
		return stripped(), nil
	}
	pn := uint64(pn32)
	if s.cfg.Cipher.XPN() {
		// The SecTAG has the low 32 bits: the high ones are the lowest
		// acceptable PN's, or one more once the low bits have wrapped.
		pn |= a.lowest &^ 0xffffffff
		if pn32 < uint32(a.lowest) {
			pn += 1 << 32
		}
	}
	if s.cfg.ReplayProtect && pn < a.lowest {
		sc.stats.InPktsLate++
		return nil, ErrLate
	}
	if s.cfg.Validate == ValidateDisabled && !c {
		sc.stats.InPktsUnchecked++
		return stripped(), nil
	}
	clear := dl
	if c {
		clear = min(s.cfg.Offset, dl)
	}
	aad := make([]byte, 0, 12+tl+clear)
	aad = append(aad, hdr[:12]...)
	aad = append(aad, tag...)
	aad = append(aad, data[:clear]...)
	ct := make([]byte, 0, dl-clear+ICVLen)
	ct = append(append(ct, data[clear:]...), icv...)
	iv := s.iv(a, sci, pn)
	plain, err := a.aead.Open(nil, iv[:], ct, aad)
	if err != nil {
		if s.cfg.Validate == ValidateStrict || c {
			sc.stats.InPktsNotValid++
			a.stats.InPktsNotValid++
			return nil, ErrNotValid
		}
		sc.stats.InPktsInvalid++
		a.stats.InPktsInvalid++
		return stripped(), nil
	}
	if pn < a.lowest {
		sc.stats.InPktsDelayed++
	} else {
		sc.stats.InPktsOK++
		a.stats.InPktsOK++
	}
	if c {
		sc.stats.InOctetsDecrypted += uint64(dl)
	} else {
		sc.stats.InOctetsValidated += uint64(dl)
	}
	if pn+1 > a.next {
		a.next = pn + 1
	}
	if a.next > s.cfg.ReplayWindow && a.next-s.cfg.ReplayWindow > a.lowest {
		a.lowest = a.next - s.cfg.ReplayWindow
	}
	msdu := make([]byte, 0, dl)
	msdu = append(msdu, data[:clear]...)
	return append(msdu, plain...), nil
}

// ResetStats sets every counter to zero; keys and packet numbers stay.
func (s *SecY) ResetStats() {
	s.txMu.Lock()
	s.stats = Stats{}
	for _, a := range s.tx {
		if a != nil {
			a.stats = SAStats{}
		}
	}
	s.txMu.Unlock()
	s.rxMu.Lock()
	s.rstats = Stats{}
	for _, sc := range s.rx {
		sc.stats = RxSCStats{}
		for _, a := range sc.sas {
			if a != nil {
				a.stats = SAStats{}
			}
		}
	}
	s.rxMu.Unlock()
}

// SAStatus is one SA as reported.
type SAStatus struct {
	AN       uint8     `json:"an"`
	NextPN   uint64    `json:"next_pn"`
	LowestPN uint64    `json:"lowest_pn,omitempty"`
	Created  time.Time `json:"created"`
	SAStats
}

// RxSCStatus is one receive SC as reported.
type RxSCStatus struct {
	SCI    SCI        `json:"sci"`
	MAC    string     `json:"mac"`
	Port   uint16     `json:"port"`
	LastRx time.Time  `json:"last_rx,omitempty"`
	SAs    []SAStatus `json:"sas"`
	RxSCStats
}

// Status is a snapshot of the SecY.
type Status struct {
	SCI        SCI          `json:"sci"`
	Cipher     string       `json:"cipher"`
	Overhead   int          `json:"overhead"`
	EncodingSA *int         `json:"encoding_sa"`
	TxSAs      []SAStatus   `json:"tx_sas"`
	RxSCs      []RxSCStatus `json:"rx_scs"`
	Stats
}

func (s *SecY) Status() Status {
	st := Status{SCI: s.cfg.SCI, Cipher: s.cfg.Cipher.String(), Overhead: s.Overhead(), TxSAs: []SAStatus{}, RxSCs: []RxSCStatus{}}
	s.txMu.Lock()
	if s.encSA >= 0 {
		an := s.encSA
		st.EncodingSA = &an
	}
	for _, a := range s.tx {
		if a != nil {
			st.TxSAs = append(st.TxSAs, SAStatus{AN: a.an, NextPN: a.next, Created: a.created, SAStats: a.stats})
		}
	}
	st.Stats = s.stats
	s.txMu.Unlock()
	s.rxMu.Lock()
	r := s.rstats
	for _, sc := range s.rx {
		v := RxSCStatus{SCI: sc.sci, MAC: sc.sci.MAC().String(), Port: sc.sci.Port(), LastRx: sc.last, SAs: []SAStatus{}, RxSCStats: sc.stats}
		for _, a := range sc.sas {
			if a != nil {
				v.SAs = append(v.SAs, SAStatus{AN: a.an, NextPN: a.next, LowestPN: a.lowest, Created: a.created, SAStats: a.stats})
			}
		}
		st.RxSCs = append(st.RxSCs, v)
	}
	s.rxMu.Unlock()
	sort.Slice(st.RxSCs, func(i, j int) bool { return st.RxSCs[i].SCI < st.RxSCs[j].SCI })
	st.InPktsUntagged, st.InPktsNoTag, st.InPktsBadTag = r.InPktsUntagged, r.InPktsNoTag, r.InPktsBadTag
	st.InPktsUnknownSCI, st.InPktsNoSCI, st.InPktsOverrun = r.InPktsUnknownSCI, r.InPktsNoSCI, r.InPktsOverrun
	return st
}
