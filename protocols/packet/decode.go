package packet

import (
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"strconv"

	"github.com/ripnet/clabgen/internal/flowrx"
	"github.com/ripnet/clabgen/internal/pkt"
)

// decode turns a captured frame into layers that build the same bytes
// (checksums aside: they are recomputed). Headers it doesn't model end up
// in a raw layer, the rest in a hex payload.
func decode(b []byte) []Layer {
	if len(b) < 14 {
		return []Layer{rawOrPayload(b)}
	}
	d, s := pkt.MAC(b[0:6]), pkt.MAC(b[6:12])
	et := binary.BigEndian.Uint16(b[12:])
	eth := &Ethernet{Dst: d.String(), Src: s.String()}
	ls := []Layer{{Ethernet: eth}}
	rest, explicit := decodeEther(et, b[14:], &ls)
	if explicit {
		eth.EtherType = &et
	}
	return append(ls, rest...)
}

func u16p(v uint16) *uint16 { return &v }
func u8p(v uint8) *uint8    { return &v }

// decodeEther decodes what follows an ethertype; explicit is true when the
// ethertype isn't the one the next layer implies.
func decodeEther(et uint16, b []byte, ls *[]Layer) (rest []Layer, explicit bool) {
	switch {
	case pkt.IsTPID(et) && len(b) >= 4:
		tci, next := binary.BigEndian.Uint16(b), binary.BigEndian.Uint16(b[2:])
		v := &VLAN{VID: tci & 0xfff, PCP: uint8(tci >> 13), DEI: tci&0x1000 != 0}
		if et != pkt.TPIDDot1Q {
			v.TPID = et
		}
		*ls = append(*ls, Layer{VLAN: v})
		r, _ := decodeEther(next, b[4:], ls)
		return r, false
	case et == 0x8847 && len(b) >= 4:
		for len(b) >= 4 {
			w := binary.BigEndian.Uint32(b)
			*ls = append(*ls, Layer{MPLS: &MPLS{Label: w >> 12, TC: uint8(w>>9) & 7, TTL: u8p(uint8(w))}})
			b = b[4:]
			if w&0x100 != 0 {
				break
			}
		}
		if len(b) > 0 && b[0]>>4 == 4 {
			decodeIPv4(b, ls)
		} else if len(b) > 0 && b[0]>>4 == 6 {
			decodeIPv6(b, ls)
		} else if len(b) > 0 {
			*ls = append(*ls, rawOrPayload(b))
		}
		return nil, false
	case et == pkt.EtherTypeARP && len(b) >= 28:
		a, err := pkt.DecodeARP(b)
		if err == nil && binary.BigEndian.Uint32(b) == 0x00010800 && b[4] == 6 && b[5] == 4 {
			op := map[uint16]string{1: "request", 2: "reply"}[a.Op]
			if op == "" {
				op = strconv.Itoa(int(a.Op))
			}
			*ls = append(*ls, Layer{ARP: &ARP{Op: op, SenderMAC: a.SHA.String(), SenderIP: a.SPA.String(), TargetMAC: a.THA.String(), TargetIP: a.TPA.String()}})
			return nil, false
		}
	case et == pkt.EtherTypeIPv4 && len(b) >= 20 && b[0]>>4 == 4:
		decodeIPv4(b, ls)
		return nil, false
	case et == pkt.EtherTypeIPv6 && len(b) >= 40 && b[0]>>4 == 6:
		decodeIPv6(b, ls)
		return nil, false
	}
	if len(b) > 0 {
		*ls = append(*ls, payloadOf(b))
	}
	return nil, et != 0x88b5
}

func decodeIPv4(b []byte, ls *[]Layer) {
	hl := int(b[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(b[2:]))
	if hl < 20 || hl > len(b) {
		*ls = append(*ls, rawOrPayload(b))
		return
	}
	h := &IPv4{
		Src: netip.AddrFrom4([4]byte(b[12:16])).String(), Dst: netip.AddrFrom4([4]byte(b[16:20])).String(),
		DSCP: b[1] >> 2, ECN: b[1] & 3, ID: u16p(binary.BigEndian.Uint16(b[4:])), TTL: u8p(b[8]),
	}
	fo := binary.BigEndian.Uint16(b[6:])
	h.DF, h.MF, h.FragOffset = fo&0x4000 != 0, fo&0x2000 != 0, fo&0x1fff
	if hl > 20 {
		h.Options = hex.EncodeToString(b[20:hl])
	}
	if total >= hl && total <= len(b) {
		b = b[:total] // drop Ethernet padding
	} else {
		h.Length = u16p(uint16(total))
	}
	proto := b[9]
	*ls = append(*ls, Layer{IPv4: h})
	body := b[hl:]
	if h.MF || h.FragOffset != 0 {
		h.Protocol = u8p(proto)
		if len(body) > 0 {
			*ls = append(*ls, payloadOf(body))
		}
		return
	}
	if !decodeIPPayload(proto, body, ls, true) {
		h.Protocol = u8p(proto)
	}
}

func decodeIPv6(b []byte, ls *[]Layer) {
	plen := int(binary.BigEndian.Uint16(b[4:]))
	w := binary.BigEndian.Uint32(b)
	h := &IPv6{
		Src: netip.AddrFrom16([16]byte(b[8:24])).String(), Dst: netip.AddrFrom16([16]byte(b[24:40])).String(),
		TrafficClass: uint8(w >> 20), FlowLabel: w & 0xfffff, HopLimit: u8p(b[7]),
	}
	if 40+plen <= len(b) {
		b = b[:40+plen]
	} else {
		h.Length = u16p(uint16(plen))
	}
	nh := b[6]
	*ls = append(*ls, Layer{IPv6: h})
	body := b[40:]
	if len(body) == 0 && nh == 59 {
		return
	}
	if !decodeIPPayload(nh, body, ls, false) {
		h.NextHeader = u8p(nh)
	}
}

// decodeIPPayload decodes an IP payload; false when the protocol isn't
// one a following layer implies (it then needs to be explicit).
func decodeIPPayload(proto uint8, b []byte, ls *[]Layer, v4 bool) bool {
	switch {
	case proto == 17 && len(b) >= 8:
		sp, dp := binary.BigEndian.Uint16(b), binary.BigEndian.Uint16(b[2:])
		u := &UDP{SrcPort: u16p(sp), DstPort: u16p(dp)}
		if l := int(binary.BigEndian.Uint16(b[4:])); l != len(b) {
			u.Length = u16p(uint16(l))
		}
		if binary.BigEndian.Uint16(b[6:]) == 0 && v4 {
			u.Checksum = "zero"
		}
		*ls = append(*ls, Layer{UDP: u})
		body := b[8:]
		if dp == 4789 && len(body) >= 8 {
			*ls = append(*ls, Layer{VXLAN: &VXLAN{VNI: binary.BigEndian.Uint32(body[4:]) >> 8, Flags: flagsIfNot(body[0], 0x08)}})
			inner := decode(body[8:])
			return appendAll(ls, inner)
		}
		if len(body) > 0 {
			*ls = append(*ls, payloadOf(body))
		}
		return true
	case proto == 6 && len(b) >= 20:
		off := int(b[12]>>4) * 4
		if off < 20 || off > len(b) {
			break
		}
		t := &TCP{SrcPort: u16p(binary.BigEndian.Uint16(b)), DstPort: binary.BigEndian.Uint16(b[2:]),
			Seq: binary.BigEndian.Uint32(b[4:]), Ack: binary.BigEndian.Uint32(b[8:]), Flags: strconv.Itoa(int(b[13])),
			Window: u16p(binary.BigEndian.Uint16(b[14:])), Urgent: binary.BigEndian.Uint16(b[18:])}
		if off > 20 {
			t.Options = hex.EncodeToString(b[20:off])
		}
		*ls = append(*ls, Layer{TCP: t})
		if len(b) > off {
			*ls = append(*ls, payloadOf(b[off:]))
		}
		return true
	case (proto == 1 && v4 || proto == 58 && !v4) && len(b) >= 8:
		c := &ICMP{Type: u8p(b[0]), Code: b[1], ID: binary.BigEndian.Uint16(b[4:]), Seq: binary.BigEndian.Uint16(b[6:])}
		if v4 {
			*ls = append(*ls, Layer{ICMP: c})
		} else {
			*ls = append(*ls, Layer{ICMPv6: c})
		}
		if len(b) > 8 {
			*ls = append(*ls, payloadOf(b[8:]))
		}
		return true
	case proto == 4 && len(b) >= 20 && b[0]>>4 == 4:
		decodeIPv4(b, ls)
		return true
	case proto == 41 && len(b) >= 40 && b[0]>>4 == 6:
		decodeIPv6(b, ls)
		return true
	}
	if len(b) > 0 {
		*ls = append(*ls, payloadOf(b))
	}
	return false
}

func flagsIfNot(v, def uint8) *uint8 {
	if v == def {
		return nil
	}
	return &v
}

func appendAll(ls *[]Layer, inner []Layer) bool {
	*ls = append(*ls, inner...)
	return true
}

func rawOrPayload(b []byte) Layer { return Layer{Raw: &Raw{Hex: hex.EncodeToString(b)}} }

// payloadOf keeps the bytes as a hex payload (with the signature on when
// they start with one: a clabgen stream).
func payloadOf(b []byte) Layer {
	_, _, _, sig := flowrx.ParseSignature(b)
	return Layer{Payload: &Payload{Size: len(b), Pattern: "hex", Data: hex.EncodeToString(b), Signature: &sig}}
}
