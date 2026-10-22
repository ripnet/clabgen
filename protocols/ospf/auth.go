package ospf

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"net/netip"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Packet authentication. OSPFv2 (RFC 2328 appendix D, RFC 5709): null,
// simple password, keyed MD5 and HMAC-SHA; the crypto types carry a key ID
// and a sequence number that must not decrease in the header, and the
// digest follows the packet. OSPFv3 (RFC 7166): an authentication trailer
// after the packet with a security association ID, a 64-bit sequence number
// and an HMAC-SHA digest; the AT option bit marks hellos and DBDs.

const (
	OptAT = 0x400 // OSPFv3: authentication trailer

	at3Header = 16 // trailer bytes before the digest
)

var shaAlgos = map[string]struct {
	h func() hash.Hash
	l int
}{
	"hmac-sha1": {sha1.New, sha1.Size}, "hmac-sha256": {sha256.New, sha256.Size},
	"hmac-sha384": {sha512.New384, sha512.Size384}, "hmac-sha512": {sha512.New, sha512.Size},
}

func authType(a Auth) uint16 {
	switch a.Type {
	case "simple":
		return AuthSimple
	case "md5":
		return AuthCrypto
	}
	if _, ok := shaAlgos[a.Type]; ok {
		return AuthCrypto
	}
	return AuthNull
}

// digestLen is how many bytes of digest follow a v2 packet (or a v3
// trailer header).
func digestLen(a Auth) int {
	if a.Type == "md5" {
		return md5.Size
	}
	return shaAlgos[a.Type].l
}

// hmacKey is Ko (RFC 5709 3.3, RFC 7166 4.5): the key padded with zeros or
// hashed to the digest length.
func hmacKey(k []byte, h func() hash.Hash, l int) []byte {
	if len(k) > l {
		x := h()
		x.Write(k)
		return x.Sum(nil)
	}
	return append(append([]byte(nil), k...), make([]byte, l-len(k))...)
}

// apad is 0x878FE1F3 repeated; OSPFv3 puts the source address first.
func apad(l int, src netip.Addr) []byte {
	b := make([]byte, l)
	for i := 0; i < l; i += 4 {
		binary.BigEndian.PutUint32(b[i:], 0x878fe1f3)
	}
	if src.IsValid() {
		a := src.As16()
		copy(b, a[:])
	}
	return b
}

// v2Digest is the digest of a v2 packet (header + body, auth fields set).
func v2Digest(p []byte, a Auth) []byte {
	if a.Type == "md5" {
		var k [16]byte
		copy(k[:], a.Key)
		h := md5.New()
		h.Write(p)
		h.Write(k[:])
		return h.Sum(nil)
	}
	al := shaAlgos[a.Type]
	m := hmac.New(al.h, hmacKey([]byte(a.Key), al.h, al.l))
	m.Write(p)
	m.Write(apad(al.l, netip.Addr{}))
	return m.Sum(nil)
}

// sign completes an encoded OSPFv2 packet (header + body) for sending.
func sign(p []byte, a Auth, seq uint32) []byte {
	t := authType(a)
	binary.BigEndian.PutUint16(p[14:], t)
	for i := 16; i < 24; i++ {
		p[i] = 0
	}
	binary.BigEndian.PutUint16(p[12:], 0)
	if t == AuthCrypto {
		p[18], p[19] = a.KeyID, byte(digestLen(a))
		binary.BigEndian.PutUint32(p[20:], seq)
		return append(p, v2Digest(p, a)...)
	}
	binary.BigEndian.PutUint16(p[12:], pkt.Checksum(p, 0))
	if t == AuthSimple {
		copy(p[16:24], a.Key)
	}
	return p
}

// verify checks a received OSPFv2 packet against the interface's auth. For
// the crypto types it returns the packet's sequence number (the caller
// checks it against the neighbor's last one).
func verify(h *Header, a Auth) (seq uint32, crypto bool, err error) {
	want := authType(a)
	if h.AuType != want {
		return 0, false, fmt.Errorf("auth type %d, want %d", h.AuType, want)
	}
	switch want {
	case AuthNull, AuthSimple:
		if !checksumOK(h.Packet) {
			return 0, false, errors.New("bad checksum")
		}
		if want == AuthSimple {
			var k [8]byte
			copy(k[:], a.Key)
			if subtle.ConstantTimeCompare(k[:], h.Auth[:]) != 1 {
				return 0, false, errors.New("wrong password")
			}
		}
		return 0, false, nil
	}
	n := digestLen(a)
	if h.Auth[2] != a.KeyID {
		return 0, false, fmt.Errorf("key ID %d, want %d", h.Auth[2], a.KeyID)
	}
	if int(h.Auth[3]) != n || len(h.Trailer) < n {
		return 0, false, fmt.Errorf("digest of %d bytes, want %d (%s)", h.Auth[3], n, a.Type)
	}
	if subtle.ConstantTimeCompare(v2Digest(h.Packet, a), h.Trailer[:n]) != 1 {
		return 0, false, errors.New("wrong digest")
	}
	return binary.BigEndian.Uint32(h.Auth[4:]), true, nil
}

// ---- OSPFv3 authentication trailer (RFC 7166) ----

func v3Digest(packet, trailerHdr []byte, src netip.Addr, a Auth) []byte {
	al := shaAlgos[a.Type]
	ks := append([]byte(a.Key), 0, 1) // the OSPFv3 cryptographic protocol ID
	m := hmac.New(al.h, hmacKey(ks, al.h, al.l))
	m.Write(packet)
	m.Write(trailerHdr)
	m.Write(apad(al.l, src))
	return m.Sum(nil)
}

// trailer3 is the authentication trailer for a v3 packet (checksum zero).
func trailer3(packet []byte, src netip.Addr, a Auth, seq uint64) []byte {
	t := make([]byte, at3Header)
	binary.BigEndian.PutUint16(t, 1) // HMAC cryptographic authentication
	binary.BigEndian.PutUint16(t[2:], uint16(at3Header+digestLen(a)))
	binary.BigEndian.PutUint16(t[6:], a.SAID)
	binary.BigEndian.PutUint64(t[8:], seq)
	return append(t, v3Digest(packet, t, src, a)...)
}

// verify3 checks a v3 packet's trailer and returns its sequence number.
func verify3(h *Header, src netip.Addr, a Auth) (uint64, error) {
	t := h.Trailer
	if len(t) < at3Header {
		return 0, errors.New("no authentication trailer")
	}
	n := digestLen(a)
	if binary.BigEndian.Uint16(t) != 1 || int(binary.BigEndian.Uint16(t[2:])) != at3Header+n || len(t) < at3Header+n {
		return 0, fmt.Errorf("trailer of %d bytes, want %d (%s)", binary.BigEndian.Uint16(t[2:]), at3Header+n, a.Type)
	}
	if id := binary.BigEndian.Uint16(t[6:]); id != a.SAID {
		return 0, fmt.Errorf("security association %d, want %d", id, a.SAID)
	}
	if subtle.ConstantTimeCompare(v3Digest(h.Packet, t[:at3Header], src, a), t[at3Header:at3Header+n]) != 1 {
		return 0, errors.New("wrong digest")
	}
	return binary.BigEndian.Uint64(t[8:]), nil
}
