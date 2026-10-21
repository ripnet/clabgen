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
	Type  string `json:"type" jsonschema:"enum=none,enum=password,enum=hmac-md5,enum=hmac-sha1,enum=hmac-sha224,enum=hmac-sha256,enum=hmac-sha384,enum=hmac-sha512"`
	Key   string `json:"key,omitempty" jsonschema:"description=Password or HMAC key"`
	KeyID uint16 `json:"key_id,omitempty" jsonschema:"description=Key ID for HMAC-SHA (RFC 5310; on SR OS the keychain entry number)"`
}

func (a Auth) sha() func() hash.Hash { return shaAlgs[a.Type] }

func (a Auth) enabled() bool { return a.Type == "password" || a.Type == "hmac-md5" || a.sha() != nil }

// shaLen is the digest length of an RFC 5310 algorithm.
func (a Auth) shaLen() int { return a.sha()().Size() }

func (a Auth) validate(scope string) error {
	switch a.Type {
	case "", "none":
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
	errNoAuth  = errors.New("missing authentication")
	errBadAuth = errors.New("authentication failed")
	errKeyID   = errors.New("unknown key ID")
)

// verify checks a received PDU (without LLC) against the scope. A scope
// without authentication accepts everything.
func (a Auth) verify(raw []byte) error {
	if !a.enabled() {
		return nil
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
