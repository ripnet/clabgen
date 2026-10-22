package ospf

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Packet authentication (RFC 2328 appendix D): null, simple password and
// keyed MD5. With MD5 the header carries the key ID and a sequence number
// that must not decrease, and the digest follows the packet.

func authType(a Auth) uint16 {
	switch a.Type {
	case "simple":
		return AuthSimple
	case "md5":
		return AuthCrypto
	}
	return AuthNull
}

// sign completes an encoded packet (header + body) for sending.
func sign(p []byte, a Auth, seq uint32) []byte {
	t := authType(a)
	binary.BigEndian.PutUint16(p[14:], t)
	for i := 16; i < 24; i++ {
		p[i] = 0
	}
	switch t {
	case AuthCrypto:
		p[18], p[19] = a.KeyID, md5.Size
		binary.BigEndian.PutUint32(p[20:], seq)
		binary.BigEndian.PutUint16(p[12:], 0)
		return append(p, md5Digest(p, a.Key)...)
	default:
		binary.BigEndian.PutUint16(p[12:], 0)
		binary.BigEndian.PutUint16(p[12:], pkt.Checksum(p, 0))
		if t == AuthSimple {
			copy(p[16:24], a.Key)
		}
	}
	return p
}

func md5Digest(p []byte, key string) []byte {
	var k [16]byte
	copy(k[:], key)
	h := md5.New()
	h.Write(p)
	h.Write(k[:])
	return h.Sum(nil)
}

// verify checks a received packet against the interface's auth. For MD5 it
// returns the packet's sequence number (the caller checks it against the
// neighbor's last one).
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
	if h.Auth[2] != a.KeyID {
		return 0, false, fmt.Errorf("key ID %d, want %d", h.Auth[2], a.KeyID)
	}
	if h.Auth[3] != md5.Size || len(h.Trailer) < md5.Size {
		return 0, false, errors.New("missing digest")
	}
	if subtle.ConstantTimeCompare(md5Digest(h.Packet, a.Key), h.Trailer[:md5.Size]) != 1 {
		return 0, false, errors.New("wrong digest")
	}
	return binary.BigEndian.Uint32(h.Auth[4:]), true, nil
}
