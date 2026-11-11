package monitor

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/ripnet/clabgen/internal/flowrx"
	"github.com/ripnet/clabgen/internal/ipflow"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/protocols/cfm"
)

// Control word settings.
const (
	cwAuto = "auto"
	cwOn   = "on"
	cwOff  = "off"
)

// Reserved labels (RFC 3032, RFC 6790, RFC 5586).
const (
	labelRouterAlert = 1
	labelELI         = 7
	labelGAL         = 13
)

const etherTypeMPLSMulticast = 0x8848

// decoded is one mirrored frame taken apart.
type decoded struct {
	outer *pkt.Frame
	mpls  bool
	// With MPLS: the service (bottom) label and the labels above it, with
	// flow and entropy labels taken out.
	service   uint32
	transport []uint32
	flowLabel bool
	entropy   bool
	cw        bool
	ip        bool // IP right after the labels
	ach       bool // G-ACh (GAL or a control word starting 0001): the routers' own OAM
	// The frame inside (for plain Ethernet, the frame itself).
	inner *pkt.Frame
	what  what
}

// what a frame is.
type what struct {
	class  string // "data", "oam", "other"
	kind   string
	flowID uint32
	reply  bool
	level  *uint8
	mepID  *uint16
	ma     string
	ipSrc  netip.Addr
	ipDst  netip.Addr
}

type label struct {
	v   uint32
	ttl uint8
}

// decode takes a mirrored frame apart; nil when it isn't Ethernet.
func decode(b []byte, cw string) *decoded {
	f, err := pkt.Decode(b)
	if err != nil {
		return nil
	}
	d := &decoded{outer: f}
	if f.EtherType != pkt.EtherTypeMPLS && f.EtherType != etherTypeMPLSMulticast {
		d.inner = f
		d.what = classify(f.EtherType, f.Payload)
		return d
	}
	d.mpls = true
	var labels []label
	rest := f.Payload
	for {
		if len(rest) < 4 {
			return nil
		}
		e := binary.BigEndian.Uint32(rest)
		rest = rest[4:]
		labels = append(labels, label{e >> 12, uint8(e)})
		if e&0x100 != 0 {
			break
		}
		if len(labels) > 16 {
			return nil
		}
	}
	var kept []label
	for i := 0; i < len(labels); i++ {
		switch l := labels[i]; {
		case l.v == labelELI && i+1 < len(labels):
			d.entropy = true
			i++ // the entropy label after it
		case l.v == labelGAL:
			d.ach = true
		default:
			kept = append(kept, l)
		}
	}
	// A flow label (FAT) is the bottom label, below the pseudowire's, with
	// TTL 0.
	if n := len(kept); n >= 2 && kept[n-1].ttl == 0 && labels[len(labels)-1] == kept[n-1] {
		d.flowLabel = true
		kept = kept[:n-1]
	}
	if len(kept) == 0 {
		return nil
	}
	d.service = kept[len(kept)-1].v
	for _, l := range kept[:len(kept)-1] {
		d.transport = append(d.transport, l.v)
	}
	if d.ach {
		d.what = what{class: "other", kind: "G-ACh"}
		return d
	}
	d.payload(rest, cw)
	return d
}

// payload finds what's after the labels: IP (L3VPN) or an Ethernet frame
// (a pseudowire), with or without a control word. A control word starts
// with a 0 nibble, as many MACs do, so in auto mode both readings are tried
// and the one that makes more sense wins.
func (d *decoded) payload(b []byte, cw string) {
	if len(b) == 0 {
		d.what = what{class: "other", kind: "empty"}
		return
	}
	if v := b[0] >> 4; (v == 4 || v == 6) && cw != cwOn {
		if et, ok := ipPacket(b); ok {
			d.ip = true
			d.inner = &pkt.Frame{EtherType: et, Payload: b}
			d.what = classify(et, b)
			return
		}
	}
	type try struct {
		f     *pkt.Frame
		w     what
		cw    bool
		score int
	}
	var best *try
	for _, withCW := range []bool{true, false} {
		if withCW && (cw == cwOff || b[0]>>4 != 0) || !withCW && cw == cwOn {
			continue
		}
		off := 0
		if withCW {
			off = 4
		}
		if len(b) < off+14 {
			continue
		}
		f, err := pkt.Decode(b[off:])
		if err != nil {
			continue
		}
		w := classify(f.EtherType, f.Payload)
		t := &try{f: f, w: w, cw: withCW, score: plausible(f, w)}
		// On a tie, a control word if its 4 bytes are zero (flags and
		// sequence unused, as usual): read as a frame they'd make a DA of
		// 00:00:00:00:…, which no device has. Else no control word: the
		// shifted reading can also land on a valid packet, a tag short.
		if best == nil || t.score > best.score || t.score == best.score && t.cw == zeroCW(b) {
			best = t
		}
	}
	// A control word starting 0001 is an associated channel (VCCV, BFD):
	// unless the bytes read as a sensible Ethernet frame (a MAC can start
	// 0x1_ too).
	if b[0]>>4 == 1 && cw != cwOff && (best == nil || best.score == 0) {
		d.ach = true
		d.what = what{class: "other", kind: "G-ACh"}
		return
	}
	if best == nil {
		d.what = what{class: "other", kind: "undecoded"}
		return
	}
	d.inner, d.what, d.cw = best.f, best.w, best.cw
}

func zeroCW(b []byte) bool { return len(b) >= 4 && b[0]|b[1]|b[2]|b[3] == 0 }

// plausible scores a reading of an inner frame: a flow or CFM is certain,
// an IP header with a good checksum nearly so.
func plausible(f *pkt.Frame, w what) int {
	switch {
	case w.class == "data" || w.class == "oam":
		return 3
	case f.EtherType == pkt.EtherTypeIPv4:
		if len(f.Payload) >= 20 && pkt.Checksum(f.Payload[:int(f.Payload[0]&0x0f)*4], 0) == 0 {
			return 2
		}
		return 0
	case f.EtherType == pkt.EtherTypeIPv6:
		if len(f.Payload) >= 40 && f.Payload[0]>>4 == 6 {
			return 2
		}
		return 0
	case f.EtherType == pkt.EtherTypeARP, f.EtherType == pkt.EtherTypeMPLS, f.EtherType == 0x88cc, f.EtherType == 0x8809:
		return 1
	}
	return 0
}

// ipPacket says whether b is an IP packet, and which.
func ipPacket(b []byte) (uint16, bool) {
	switch b[0] >> 4 {
	case 4:
		ihl := int(b[0]&0x0f) * 4
		if ihl < 20 || len(b) < ihl || int(binary.BigEndian.Uint16(b[2:])) > len(b) {
			return 0, false
		}
		return pkt.EtherTypeIPv4, pkt.Checksum(b[:ihl], 0) == 0
	case 6:
		if len(b) < 40 || int(binary.BigEndian.Uint16(b[4:]))+40 > len(b) {
			return 0, false
		}
		return pkt.EtherTypeIPv6, true
	}
	return 0, false
}

// classify says what a frame with this ethertype and payload is.
func classify(et uint16, p []byte) what {
	switch et {
	case pkt.EtherTypeCFM:
		return cfmWhat(p)
	case pkt.EtherTypeIPv4, pkt.EtherTypeIPv6:
		return ipWhat(et, p)
	}
	if id, _, _, ok := flowrx.ParseSignature(p); ok {
		return what{class: "data", kind: "l2", flowID: id}
	}
	switch et {
	case pkt.EtherTypeLLC:
		return what{class: "other", kind: "LLC"} // 802.3: IS-IS, STP…
	case pkt.EtherTypeARP:
		return what{class: "other", kind: "ARP"}
	case 0x88cc:
		return what{class: "other", kind: "LLDP"}
	case 0x8809:
		return what{class: "other", kind: "slow protocols"}
	}
	return what{class: "other", kind: fmt.Sprintf("%#04x", et)}
}

func cfmWhat(p []byte) what {
	if len(p) < 4 {
		return what{class: "other", kind: "CFM (short)"}
	}
	lvl, op := p[0]>>5, p[1]
	w := what{class: "oam", kind: cfm.OpName(op), level: &lvl}
	if op == cfm.OpCCM && len(p) >= 58 {
		id := binary.BigEndian.Uint16(p[8:]) & 0x1fff
		w.mepID = &id
		var m cfm.MAID
		copy(m[:], p[10:58])
		w.ma = m.String()
	}
	return w
}

func ipWhat(et uint16, p []byte) what {
	w := what{class: "other"}
	var proto uint8
	var l4 []byte
	if et == pkt.EtherTypeIPv4 {
		w.kind = "ipv4"
		if len(p) < 20 {
			return w
		}
		ihl := int(p[0]&0x0f) * 4
		if ihl < 20 || len(p) < ihl {
			return w
		}
		proto = p[9]
		w.ipSrc, _ = netip.AddrFromSlice(p[12:16])
		w.ipDst, _ = netip.AddrFromSlice(p[16:20])
		end := int(binary.BigEndian.Uint16(p[2:]))
		if end > len(p) || end < ihl {
			end = len(p)
		}
		l4 = p[ihl:end]
	} else {
		w.kind = "ipv6"
		if len(p) < 40 {
			return w
		}
		proto = p[6]
		w.ipSrc, _ = netip.AddrFromSlice(p[8:24])
		w.ipDst, _ = netip.AddrFromSlice(p[24:40])
		end := 40 + int(binary.BigEndian.Uint16(p[4:]))
		if end > len(p) {
			end = len(p)
		}
		l4 = p[40:end]
	}
	switch proto {
	case 1, 58:
		w.kind = "icmp"
		if len(l4) >= 8 {
			t := l4[0]
			w.reply = t == 0 || t == 129
			if id, _, _, ok := flowrx.ParseSignature(l4[8:]); ok {
				w.class, w.flowID = "data", id
			}
		}
	case 17:
		w.kind = "udp"
		if len(l4) >= 8 {
			if id, _, _, ok := flowrx.ParseSignature(l4[8:]); ok {
				w.class, w.flowID = "data", id
			}
		}
	}
	return w
}

// flowIDString is how flows show their IDs.
func flowIDString(id uint32) string { return ipflow.FlowIDString(id) }
