package isis

import (
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
)

// Authentication (TLV 10): cleartext passwords (ISO 10589) and HMAC-MD5
// (RFC 5304). Hellos use one key; LSPs, CSNPs and PSNPs (the area or
// domain password) another.

const (
	authPassword byte = 1
	authHMACMD5  byte = 54
	md5Len            = 16
)

// Auth configures one authentication scope.
type Auth struct {
	Type string `json:"type" jsonschema:"enum=none,enum=password,enum=hmac-md5"`
	Key  string `json:"key,omitempty" jsonschema:"description=Password or HMAC key"`
}

func (a Auth) enabled() bool { return a.Type == "password" || a.Type == "hmac-md5" }

func (a Auth) validate(scope string) error {
	switch a.Type {
	case "", "none":
		return nil
	case "password", "hmac-md5":
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
	if a.Type == "hmac-md5" {
		if lsp {
			out[10], out[11] = 0, 0
		}
		m := hmac.New(md5.New, []byte(a.Key))
		m.Write(out)
		copy(out[hl+3:], m.Sum(nil))
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
	return nil
}
