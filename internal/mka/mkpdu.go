// Package mka is the MACsec Key Agreement protocol (IEEE 802.1X-2010 clauses
// 9 and 11) with a pre-shared CAK: MKPDUs, peers, key server election and
// SAK distribution. It drives a macsec.SecY through Hooks.
package mka

import (
	"crypto/hmac"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ripnet/clabgen/internal/macsec"
	"github.com/ripnet/clabgen/internal/pkt"
)

const (
	eapolVersion = 3
	eapolTypeMKA = 5
	icvLen       = 16
	miLen        = 12
	// Version is the MKA version sent (others are accepted).
	Version = 1
)

// Parameter set types.
const (
	psLivePeers      = 1
	psPotentialPeers = 2
	psSAKUse         = 3
	psDistSAK        = 4
	psICV            = 255
)

// AlgorithmAgility is the only MKA algorithm (AES-CMAC-128 KDF and ICV).
var AlgorithmAgility = [4]byte{0x00, 0x80, 0xc2, 0x01}

// MACsec capability (Basic Parameter Set).
const (
	CapNone                  = 0
	CapIntegrity             = 1
	CapConfidentiality       = 2
	CapConfidentialityOffset = 3
)

// Confidentiality offset codes in a Distributed SAK.
const (
	OffsetNone = 0 // integrity only
	Offset0    = 1
	Offset30   = 2
	Offset50   = 3
)

// MI is a member identifier.
type MI [miLen]byte

// PeerID is a peer list entry.
type PeerID struct {
	MI MI
	MN uint32
}

// KI is a key identifier: the key server's MI and a key number.
type KI struct {
	MI MI
	KN uint32
}

// SAKUse is the SAK Use parameter set. Empty: no keys in use (MACsec not
// being used).
type SAKUse struct {
	Empty        bool
	LAN, OAN     uint8
	LTx, LRx     bool
	OTx, ORx     bool
	PTx, PRx     bool
	DelayProtect bool
	Latest, Old  KI
	LLPN, OLPN   uint32
}

// DistSAK is the Distributed SAK parameter set. Empty: MACsec isn't to be
// used. Suite 0 is the default (GCM-AES-128), not sent.
type DistSAK struct {
	Empty   bool
	AN      uint8
	Offset  uint8
	KN      uint32
	Suite   uint64
	Wrapped []byte
}

// MKPDU is a decoded MKPDU (the parameter sets clabgen uses).
type MKPDU struct {
	Version    uint8
	Priority   uint8
	KeyServer  bool
	Desired    bool
	Capability uint8
	SCI        macsec.SCI
	MI         MI
	MN         uint32
	CKN        []byte
	Live       []PeerID
	Potential  []PeerID
	SAKUse     *SAKUse
	DistSAK    *DistSAK
}

func pad4(n int) int { return (n + 3) &^ 3 }

// header writes a parameter set header: type, octet 2, and a 12-bit body
// length under the top four bits of octet 3.
func header(b []byte, typ, o2, o3hi byte, length int) []byte {
	return append(b, typ, o2, o3hi&0xf0|byte(length>>8)&0x0f, byte(length))
}

// Encode builds the EAPOL frame body (EAPOL header, parameter sets, ICV) for
// dst and src. The ICV covers the Ethernet header without VLAN tags.
func (m *MKPDU) Encode(dst, src pkt.MAC, ick []byte) ([]byte, error) {
	b := make([]byte, 4, 128)
	// Basic Parameter Set.
	flags := m.Capability << 4
	if m.KeyServer {
		flags |= 0x80
	}
	if m.Desired {
		flags |= 0x40
	}
	blen := 28 + len(m.CKN)
	b = append(b, m.Version, m.Priority, flags|byte(blen>>8)&0x0f, byte(blen))
	b = binary.BigEndian.AppendUint64(b, uint64(m.SCI))
	b = append(b, m.MI[:]...)
	b = binary.BigEndian.AppendUint32(b, m.MN)
	b = append(b, AlgorithmAgility[:]...)
	b = append(b, m.CKN...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	peers := func(typ byte, l []PeerID) {
		if len(l) == 0 {
			return
		}
		b = header(b, typ, 0, 0, 16*len(l))
		for _, p := range l {
			b = append(b, p.MI[:]...)
			b = binary.BigEndian.AppendUint32(b, p.MN)
		}
	}
	peers(psLivePeers, m.Live)
	peers(psPotentialPeers, m.Potential)
	if u := m.SAKUse; u != nil {
		var o2, o3 byte
		o2 = u.LAN<<6 | u.OAN<<2
		if u.LTx {
			o2 |= 0x20
		}
		if u.LRx {
			o2 |= 0x10
		}
		if u.OTx {
			o2 |= 0x02
		}
		if u.ORx {
			o2 |= 0x01
		}
		if u.PTx {
			o3 |= 0x80
		}
		if u.PRx {
			o3 |= 0x40
		}
		if u.DelayProtect {
			o3 |= 0x10
		}
		if u.Empty {
			b = header(b, psSAKUse, 0, o3, 0)
		} else {
			b = header(b, psSAKUse, o2, o3, 40)
			b = append(b, u.Latest.MI[:]...)
			b = binary.BigEndian.AppendUint32(b, u.Latest.KN)
			b = binary.BigEndian.AppendUint32(b, u.LLPN)
			b = append(b, u.Old.MI[:]...)
			b = binary.BigEndian.AppendUint32(b, u.Old.KN)
			b = binary.BigEndian.AppendUint32(b, u.OLPN)
		}
	}
	if d := m.DistSAK; d != nil {
		if d.Empty {
			b = header(b, psDistSAK, 0, 0, 0)
		} else {
			n := 4 + len(d.Wrapped)
			if d.Suite != 0 {
				n += 8
			}
			b = header(b, psDistSAK, d.AN<<6|d.Offset<<4, 0, n)
			b = binary.BigEndian.AppendUint32(b, d.KN)
			if d.Suite != 0 {
				b = binary.BigEndian.AppendUint64(b, d.Suite)
			}
			b = append(b, d.Wrapped...)
			for len(b)%4 != 0 {
				b = append(b, 0)
			}
		}
	}
	// EAPOL header: the length counts the ICV.
	b[0], b[1] = eapolVersion, eapolTypeMKA
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)-4+icvLen))
	icv, err := computeICV(ick, dst, src, b)
	if err != nil {
		return nil, err
	}
	return append(b, icv...), nil
}

func computeICV(ick []byte, dst, src pkt.MAC, eapol []byte) ([]byte, error) {
	msg := make([]byte, 0, 14+len(eapol))
	msg = append(msg, dst[:]...)
	msg = append(msg, src[:]...)
	msg = binary.BigEndian.AppendUint16(msg, pkt.EtherTypeEAPOL)
	msg = append(msg, eapol...)
	return cmac(ick, msg)
}

var (
	ErrNotMKA     = errors.New("not an EAPOL-MKA frame")
	ErrMalformed  = errors.New("malformed MKPDU")
	ErrUnknownCKN = errors.New("MKPDU for another CKN")
	ErrICV        = errors.New("MKPDU ICV check failed")
)

// PeekCKN returns an MKPDU's CKN without checking it, to pick the CAK.
func PeekCKN(eapol []byte) ([]byte, error) {
	if len(eapol) < 4 || eapol[1] != eapolTypeMKA {
		return nil, ErrNotMKA
	}
	n := int(binary.BigEndian.Uint16(eapol[2:]))
	if n < 32 || n%4 != 0 || 4+n > len(eapol) {
		return nil, ErrMalformed
	}
	body := eapol[4 : 4+n]
	blen := int(body[2]&0x0f)<<8 | int(body[3])
	if blen < 29 || blen > 28+32 || 4+blen+icvLen > n {
		return nil, ErrMalformed
	}
	return body[32 : 4+blen], nil
}

// Decode checks an MKPDU's ICV with ick and decodes it. eapol is the frame
// after the EAPOL ethertype (padding allowed).
func Decode(dst, src pkt.MAC, eapol, ick []byte) (*MKPDU, error) {
	ckn, err := PeekCKN(eapol)
	if err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(eapol[2:]))
	msg := eapol[:4+n]
	want, err := computeICV(ick, dst, src, msg[:len(msg)-icvLen])
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(want, msg[len(msg)-icvLen:]) {
		return nil, ErrICV
	}
	body := msg[4 : len(msg)-icvLen]
	m := &MKPDU{Version: body[0], Priority: body[1], KeyServer: body[2]&0x80 != 0, Desired: body[2]&0x40 != 0,
		Capability: body[2] >> 4 & 3, CKN: append([]byte(nil), ckn...)}
	m.SCI = macsec.SCI(binary.BigEndian.Uint64(body[4:]))
	copy(m.MI[:], body[12:24])
	m.MN = binary.BigEndian.Uint32(body[24:])
	if [4]byte(body[28:32]) != AlgorithmAgility {
		return nil, fmt.Errorf("%w: algorithm agility %x", ErrMalformed, body[28:32])
	}
	blen := int(body[2]&0x0f)<<8 | int(body[3])
	for p := body[pad4(4+blen):]; len(p) >= 4; {
		typ := p[0]
		l := int(p[2]&0x0f)<<8 | int(p[3])
		if typ == psICV {
			break
		}
		if 4+l > len(p) {
			return nil, fmt.Errorf("%w: parameter set %d of %d bytes", ErrMalformed, typ, l)
		}
		ps := p[4 : 4+l]
		switch typ {
		case psLivePeers, psPotentialPeers:
			if l%16 != 0 {
				return nil, fmt.Errorf("%w: peer list of %d bytes", ErrMalformed, l)
			}
			var l2 []PeerID
			for i := 0; i < l; i += 16 {
				var id PeerID
				copy(id.MI[:], ps[i:])
				id.MN = binary.BigEndian.Uint32(ps[i+12:])
				l2 = append(l2, id)
			}
			if typ == psLivePeers {
				m.Live = l2
			} else {
				m.Potential = l2
			}
		case psSAKUse:
			u := &SAKUse{Empty: l == 0, PTx: p[2]&0x80 != 0, PRx: p[2]&0x40 != 0, DelayProtect: p[2]&0x10 != 0}
			if l != 0 {
				if l < 40 {
					return nil, fmt.Errorf("%w: SAK use of %d bytes", ErrMalformed, l)
				}
				u.LAN, u.LTx, u.LRx = p[1]>>6, p[1]&0x20 != 0, p[1]&0x10 != 0
				u.OAN, u.OTx, u.ORx = p[1]>>2&3, p[1]&0x02 != 0, p[1]&0x01 != 0
				copy(u.Latest.MI[:], ps[0:12])
				u.Latest.KN = binary.BigEndian.Uint32(ps[12:])
				u.LLPN = binary.BigEndian.Uint32(ps[16:])
				copy(u.Old.MI[:], ps[20:32])
				u.Old.KN = binary.BigEndian.Uint32(ps[32:])
				u.OLPN = binary.BigEndian.Uint32(ps[36:])
			}
			m.SAKUse = u
		case psDistSAK:
			d := &DistSAK{Empty: l == 0}
			switch {
			case l == 0:
			case l == 28:
				d.AN, d.Offset, d.KN = p[1]>>6, p[1]>>4&3, binary.BigEndian.Uint32(ps)
				d.Wrapped = append([]byte(nil), ps[4:28]...)
			case l >= 36:
				d.AN, d.Offset, d.KN = p[1]>>6, p[1]>>4&3, binary.BigEndian.Uint32(ps)
				d.Suite = binary.BigEndian.Uint64(ps[4:])
				d.Wrapped = append([]byte(nil), ps[12:l]...)
			default:
				return nil, fmt.Errorf("%w: distributed SAK of %d bytes", ErrMalformed, l)
			}
			m.DistSAK = d
		}
		p = p[pad4(4+l):]
	}
	return m, nil
}
