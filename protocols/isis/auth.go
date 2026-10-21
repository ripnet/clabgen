package isis

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"time"
)

// Authentication (TLV 10): cleartext passwords (ISO 10589), HMAC-MD5
// (RFC 5304) and HMAC-SHA with key IDs (RFC 5310 generic cryptographic
// authentication). Hellos use one key; LSPs, CSNPs and PSNPs (the area or
// domain password) another.

const (
	authPassword byte = 1
	authGeneric  byte = 3 // RFC 5310
	authHMACMD5  byte = 54
	md5Len            = 16
)

// RFC 5310 algorithms.
var shaAlgs = map[string]func() hash.Hash{
	"hmac-sha1": sha1.New, "hmac-sha224": sha256.New224, "hmac-sha256": sha256.New,
	"hmac-sha384": sha512.New384, "hmac-sha512": sha512.New,
}

// apad fills the authentication data while the digest is computed (RFC 5310 3.3).
var apad = []byte{0x87, 0x8f, 0xe1, 0xf3}

// Auth configures one authentication scope.
type Auth struct {
	Type  string `json:"type" jsonschema:"enum=none,enum=password,enum=hmac-md5,enum=hmac-sha1,enum=hmac-sha224,enum=hmac-sha256,enum=hmac-sha384,enum=hmac-sha512,enum=keychain"`
	Key   string `json:"key,omitempty" jsonschema:"description=Password or HMAC key"`
	KeyID uint16 `json:"key_id,omitempty" jsonschema:"description=Key ID for HMAC-SHA (RFC 5310; on SR OS the keychain entry number)"`
	// Keychain (type keychain): several keys with send and accept windows,
	// for rollover without dropping adjacencies.
	Keys []AuthKey `json:"keys,omitempty" jsonschema:"description=For type keychain: keys with send/accept lifetimes"`
}

// AuthKey is one keychain entry. Times are RFC 3339; empty means unbounded.
type AuthKey struct {
	ID          uint16 `json:"id" jsonschema:"description=Key ID (sent with HMAC-SHA; the SR OS keychain entry)"`
	Type        string `json:"type" jsonschema:"enum=password,enum=hmac-md5,enum=hmac-sha1,enum=hmac-sha224,enum=hmac-sha256,enum=hmac-sha384,enum=hmac-sha512"`
	Key         string `json:"key"`
	SendFrom    string `json:"send_from,omitempty" jsonschema:"description=Start sending with this key (RFC 3339); the valid key that started last is used"`
	SendUntil   string `json:"send_until,omitempty"`
	AcceptFrom  string `json:"accept_from,omitempty" jsonschema:"description=Start accepting this key (RFC 3339)"`
	AcceptUntil string `json:"accept_until,omitempty"`
}

func (k AuthKey) single() Auth { return Auth{Type: k.Type, Key: k.Key, KeyID: k.ID} }

func parseWhen(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// within reports whether now is in [from, until) (empty bounds are open).
func within(now time.Time, from, until string) bool {
	f, _ := parseWhen(from)
	u, _ := parseWhen(until)
	return (f.IsZero() || !now.Before(f)) && (u.IsZero() || now.Before(u))
}

// sendKey is the keychain key to sign with now: of those whose send window
// is open, the one that started last (then the highest ID).
func (a Auth) sendKey(now time.Time) (AuthKey, bool) {
	var best AuthKey
	var bestFrom time.Time
	found := false
	for _, k := range a.Keys {
		if !within(now, k.SendFrom, k.SendUntil) {
			continue
		}
		f, _ := parseWhen(k.SendFrom)
		if !found || f.After(bestFrom) || f.Equal(bestFrom) && k.ID > best.ID {
			best, bestFrom, found = k, f, true
		}
	}
	return best, found
}

// acceptKeys are the keychain keys valid for receiving now.
func (a Auth) acceptKeys(now time.Time) []AuthKey {
	var out []AuthKey
	for _, k := range a.Keys {
		if within(now, k.AcceptFrom, k.AcceptUntil) {
			out = append(out, k)
		}
	}
	return out
}

func (a Auth) sha() func() hash.Hash { return shaAlgs[a.Type] }

func (a Auth) enabled() bool {
	if a.Type == "keychain" {
		return len(a.Keys) > 0
	}
	return a.Type == "password" || a.Type == "hmac-md5" || a.sha() != nil
}

// shaLen is the digest length of an RFC 5310 algorithm.
func (a Auth) shaLen() int { return a.sha()().Size() }

func (a Auth) validate(scope string) error {
	switch a.Type {
	case "", "none":
		return nil
	case "keychain":
		if len(a.Keys) == 0 {
			return fmt.Errorf("%s: keychain needs keys", scope)
		}
		ids := map[uint16]bool{}
		for _, k := range a.Keys {
			if ids[k.ID] {
				return fmt.Errorf("%s: key ID %d twice", scope, k.ID)
			}
			ids[k.ID] = true
			if k.Type == "keychain" || k.Type == "none" || k.Type == "" {
				return fmt.Errorf("%s: key %d needs an algorithm", scope, k.ID)
			}
			if err := k.single().validate(fmt.Sprintf("%s key %d", scope, k.ID)); err != nil {
				return err
			}
			for _, t := range []string{k.SendFrom, k.SendUntil, k.AcceptFrom, k.AcceptUntil} {
				if _, err := parseWhen(t); err != nil {
					return fmt.Errorf("%s key %d: time %q is not RFC 3339 (e.g. 2026-10-22T09:00:00Z)", scope, k.ID, t)
				}
			}
		}
		return nil
	case "password", "hmac-md5", "hmac-sha1", "hmac-sha224", "hmac-sha256", "hmac-sha384", "hmac-sha512":
		if a.Key == "" {
			return fmt.Errorf("%s: %s needs a key", scope, a.Type)
		}
		if len(a.Key) > 254 {
			return fmt.Errorf("%s: key too long", scope)
		}
		return nil
	}
	return fmt.Errorf("%s: invalid type %q", scope, a.Type)
}

// tlvLen is the bytes the authentication TLV adds to a PDU.
func (a Auth) tlvLen() int {
	if a.Type == "keychain" {
		// Reserve room for the largest key, so fragments fit whichever is used.
		n := 0
		for _, k := range a.Keys {
			n = max(n, k.single().tlvLen())
		}
		return n
	}
	switch a.Type {
	case "password":
		return 3 + len(a.Key)
	case "hmac-md5":
		return 3 + md5Len
	}
	if a.sha() != nil {
		return 2 + 3 + a.shaLen() // type, key ID, digest
	}
	return 0
}

// pduLenOffset is where a PDU type keeps its 16-bit PDU length.
func pduLenOffset(typ uint8) int {
	switch typ {
	case PDUP2PHello, PDULANHelloL1, PDULANHelloL2:
		return 17
	}
	return 8
}

func isLSPType(typ uint8) bool { return typ == PDULSPL1 || typ == PDULSPL2 }

// sign adds the authentication TLV to a PDU (as the first TLV, so padding
// stays at the end) and, for HMAC, the digest. LSPs get their checksum
// recomputed; purges keep a zero checksum.
func (a Auth) sign(pdu []byte) []byte {
	if a.Type == "keychain" {
		k, ok := a.sendKey(time.Now())
		if !ok {
			return pdu // no valid send key: send unauthenticated (as SR OS does)
		}
		return k.single().sign(pdu)
	}
	if !a.enabled() || len(pdu) < 8 {
		return pdu
	}
	typ, hl := pdu[4]&0x1f, int(pdu[1])
	var v []byte
	switch a.Type {
	case "password":
		v = append([]byte{authPassword}, a.Key...)
	case "hmac-md5":
		v = append([]byte{authHMACMD5}, make([]byte, md5Len)...)
	default:
		v = []byte{authGeneric, byte(a.KeyID >> 8), byte(a.KeyID)}
		for len(v) < 3+a.shaLen() {
			v = append(v, apad...)
		}
	}
	out := make([]byte, 0, len(pdu)+2+len(v))
	out = append(out, pdu[:hl]...)
	out = append(out, TLVAuth, byte(len(v)))
	out = append(out, v...)
	out = append(out, pdu[hl:]...)
	binary.BigEndian.PutUint16(out[pduLenOffset(typ):], uint16(len(out)))
	lsp := isLSPType(typ)
	var life uint16
	purge := false
	if lsp {
		life = binary.BigEndian.Uint16(out[10:])
		purge = life == 0
		out[24], out[25] = 0, 0
	}
	if a.Type != "password" {
		if lsp {
			out[10], out[11] = 0, 0
		}
		h, at := md5.New, hl+3 // digest offset in the PDU
		if a.sha() != nil {
			h, at = a.sha(), hl+5
		}
		m := hmac.New(h, []byte(a.Key))
		m.Write(out) // MD5: digest zeroed; SHA: Apad in its place
		copy(out[at:], m.Sum(nil))
		if lsp {
			binary.BigEndian.PutUint16(out[10:], life)
		}
	}
	if lsp && !purge {
		binary.BigEndian.PutUint16(out[24:], Fletcher(out[12:], 24-12))
	}
	return out
}

var (
	errNoAuth      = errors.New("missing authentication")
	errBadAuth     = errors.New("authentication failed")
	errKeyID       = errors.New("unknown key ID")
	errNoAcceptKey = errors.New("no keychain key valid for receiving now")
)

// verify checks a received PDU (without LLC) against the scope. A scope
// without authentication accepts everything.
func (a Auth) verify(raw []byte) error {
	if !a.enabled() {
		return nil
	}
	if a.Type == "keychain" {
		keys := a.acceptKeys(time.Now())
		if len(keys) == 0 {
			return errNoAcceptKey
		}
		var err error
		for _, k := range keys {
			if err = k.single().verify(raw); err == nil {
				return nil
			}
		}
		return err
	}
	if len(raw) < 8 {
		return errNoAuth
	}
	typ, hl := raw[4]&0x1f, int(raw[1])
	// Find TLV 10.
	off := -1
	for i := hl; i+2 <= len(raw); i += 2 + int(raw[i+1]) {
		if i+2+int(raw[i+1]) > len(raw) {
			break
		}
		if raw[i] == TLVAuth {
			off = i
			break
		}
	}
	if off < 0 {
		return errNoAuth
	}
	v := raw[off+2 : off+2+int(raw[off+1])]
	switch a.Type {
	case "password":
		if len(v) < 1 || v[0] != authPassword || string(v[1:]) != a.Key {
			return errBadAuth
		}
		return nil
	case "hmac-md5":
		if len(v) != 1+md5Len || v[0] != authHMACMD5 {
			return errBadAuth
		}
		c := append([]byte{}, raw...)
		for i := 0; i < md5Len; i++ {
			c[off+3+i] = 0
		}
		if isLSPType(typ) {
			c[10], c[11], c[24], c[25] = 0, 0, 0, 0
		}
		m := hmac.New(md5.New, []byte(a.Key))
		m.Write(c)
		if !hmac.Equal(m.Sum(nil), v[1:]) {
			return errBadAuth
		}
		return nil
	}
	if a.sha() != nil {
		n := a.shaLen()
		if len(v) != 3+n || v[0] != authGeneric {
			return errBadAuth
		}
		if binary.BigEndian.Uint16(v[1:]) != a.KeyID {
			return errKeyID
		}
		c := append([]byte{}, raw...)
		for i := 0; i < n; i++ {
			c[off+5+i] = apad[i%4]
		}
		if isLSPType(typ) {
			c[10], c[11], c[24], c[25] = 0, 0, 0, 0
		}
		m := hmac.New(a.sha(), []byte(a.Key))
		m.Write(c)
		if !hmac.Equal(m.Sum(nil), v[3:]) {
			return errBadAuth
		}
	}
	return nil
}
