package mka

import (
	"crypto/aes"
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

// cmac is AES-CMAC (RFC 4493) with a 128- or 256-bit key.
func cmac(key, msg []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	var l, k1, k2 [16]byte
	b.Encrypt(l[:], l[:])
	shift := func(dst, src *[16]byte) {
		var carry byte
		for i := 15; i >= 0; i-- {
			dst[i] = src[i]<<1 | carry
			carry = src[i] >> 7
		}
		if src[0]&0x80 != 0 {
			dst[15] ^= 0x87
		}
	}
	shift(&k1, &l)
	shift(&k2, &k1)
	n := (len(msg) + 15) / 16
	complete := n > 0 && len(msg)%16 == 0
	if n == 0 {
		n = 1
	}
	var last [16]byte
	if complete {
		copy(last[:], msg[16*(n-1):])
		subtle.XORBytes(last[:], last[:], k1[:])
	} else {
		rest := msg[16*(n-1):]
		copy(last[:], rest)
		last[len(rest)] = 0x80
		subtle.XORBytes(last[:], last[:], k2[:])
	}
	var x [16]byte
	for i := 0; i < n-1; i++ {
		subtle.XORBytes(x[:], x[:], msg[16*i:16*i+16])
		b.Encrypt(x[:], x[:])
	}
	subtle.XORBytes(x[:], x[:], last[:])
	b.Encrypt(x[:], x[:])
	return x[:], nil
}

// kdf is the 802.1X-2010 key derivation function (clause 6.2.1): AES-CMAC
// in counter mode (NIST SP 800-108).
func kdf(key []byte, label string, context []byte, bits int) ([]byte, error) {
	buf := make([]byte, 0, 1+len(label)+1+len(context)+2)
	buf = append(buf, 0)
	buf = append(buf, label...)
	buf = append(buf, 0)
	buf = append(buf, context...)
	buf = binary.BigEndian.AppendUint16(buf, uint16(bits))
	var out []byte
	for i := 1; len(out)*8 < bits; i++ {
		buf[0] = byte(i)
		m, err := cmac(key, buf)
		if err != nil {
			return nil, err
		}
		out = append(out, m...)
	}
	return out[:bits/8], nil
}

// cknContext is a CKN's first 16 octets, zero-padded: the context for the
// ICK and KEK.
func cknContext(ckn []byte) []byte {
	c := make([]byte, 16)
	copy(c, ckn)
	return c
}

// DeriveICK and DeriveKEK derive the ICV and key encrypting keys from a CAK
// (the same length as the CAK).
func DeriveICK(cak, ckn []byte) ([]byte, error) {
	return kdf(cak, "IEEE8021 ICK", cknContext(ckn), 8*len(cak))
}

func DeriveKEK(cak, ckn []byte) ([]byte, error) {
	return kdf(cak, "IEEE8021 KEK", cknContext(ckn), 8*len(cak))
}

// DeriveSAK derives a SAK from the CAK and a context (a random nonce, the
// members' MIs and the key number).
func DeriveSAK(cak, context []byte, size int) ([]byte, error) {
	return kdf(cak, "IEEE8021 SAK", context, 8*size)
}

var defaultIV = [8]byte{0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6}

// wrap is AES key wrap (RFC 3394).
func wrap(kek, key []byte) ([]byte, error) {
	if len(key)%8 != 0 || len(key) < 16 {
		return nil, errors.New("key wrap: key length")
	}
	b, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	n := len(key) / 8
	a := defaultIV
	r := make([]byte, len(key))
	copy(r, key)
	var buf [16]byte
	for j := 0; j < 6; j++ {
		for i := 0; i < n; i++ {
			copy(buf[:8], a[:])
			copy(buf[8:], r[8*i:8*i+8])
			b.Encrypt(buf[:], buf[:])
			t := uint64(n*j + i + 1)
			binary.BigEndian.PutUint64(a[:], binary.BigEndian.Uint64(buf[:8])^t)
			copy(r[8*i:], buf[8:])
		}
	}
	return append(a[:], r...), nil
}

// unwrap reverses wrap, checking the integrity value.
func unwrap(kek, wrapped []byte) ([]byte, error) {
	if len(wrapped)%8 != 0 || len(wrapped) < 24 {
		return nil, errors.New("key unwrap: length")
	}
	b, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	n := len(wrapped)/8 - 1
	var a [8]byte
	copy(a[:], wrapped[:8])
	r := make([]byte, 8*n)
	copy(r, wrapped[8:])
	var buf [16]byte
	for j := 5; j >= 0; j-- {
		for i := n - 1; i >= 0; i-- {
			t := uint64(n*j + i + 1)
			binary.BigEndian.PutUint64(buf[:8], binary.BigEndian.Uint64(a[:])^t)
			copy(buf[8:], r[8*i:8*i+8])
			b.Decrypt(buf[:], buf[:])
			copy(a[:], buf[:8])
			copy(r[8*i:], buf[8:])
		}
	}
	if subtle.ConstantTimeCompare(a[:], defaultIV[:]) != 1 {
		return nil, errors.New("key unwrap: integrity check failed (another KEK?)")
	}
	return r, nil
}
