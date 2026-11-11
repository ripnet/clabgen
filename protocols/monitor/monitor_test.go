package monitor

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/ripnet/clabgen/internal/flowrx"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/wire"
)

var (
	peA  = pkt.MAC{0x1c, 0xb9, 0x05, 0, 0, 1} // routers on the mirrored link
	peB  = pkt.MAC{0x1c, 0xb9, 0x05, 0, 0, 2}
	ceA  = pkt.MAC{0x02, 0, 0, 0, 0x0a, 1} // customer devices inside the service
	ceB  = pkt.MAC{0x02, 0, 0, 0, 0x0b, 1}
	ceSR = pkt.MAC{0x1c, 0xb9, 0x05, 0, 0, 9} // a MEP on a router: its MAC starts 0x1_
	ipA  = netip.MustParseAddr("192.168.1.1")
	ipB  = netip.MustParseAddr("192.168.1.2")
)

func sig(id uint32) []byte {
	b := make([]byte, flowrx.SigLen+8)
	copy(b, flowrx.SigMagic[:])
	binary.BigEndian.PutUint32(b[4:], id)
	binary.BigEndian.PutUint64(b[8:], 1)
	binary.BigEndian.PutUint64(b[16:], uint64(time.Now().UnixNano()))
	return b
}

// ping is an IPv4 ICMP echo (typ 8) or reply (0) with a flow signature.
func ping(typ uint8, src, dst netip.Addr, id uint32) []byte {
	h := pkt.IPv4{TTL: 64, Protocol: 1, Src: src, Dst: dst, Payload: pkt.ICMPEcho(typ, 1, 1, sig(id))}
	return h.Encode()
}

func eth(dst, src pkt.MAC, tags []pkt.VLANTag, et uint16, p []byte) []byte {
	f := &pkt.Frame{Dst: dst, Src: src, Tags: tags, EtherType: et, Payload: p}
	return f.Encode()
}

func ccm(level uint8, mep uint16, ma string) []byte {
	b := make([]byte, 75)
	b[0], b[1], b[3] = level<<5, 1, 70
	binary.BigEndian.PutUint16(b[8:], mep)
	b[10] = 1 // no MD name
	b[11], b[12] = 2, byte(len(ma))
	copy(b[13:], ma)
	return b
}

func vids(v ...uint16) []pkt.VLANTag {
	t := make([]pkt.VLANTag, len(v))
	for i := range v {
		t[i].VID = v[i]
	}
	return t
}

type lbl struct {
	v   uint32
	ttl uint8
}

// mpls wraps inner in a label stack, with a control word.
func mpls(src, dst pkt.MAC, labels []lbl, cw bool, inner []byte) []byte {
	var b []byte
	for i, l := range labels {
		e := l.v<<12 | uint32(l.ttl)
		if i == len(labels)-1 {
			e |= 0x100
		}
		b = binary.BigEndian.AppendUint32(b, e)
	}
	if cw {
		b = append(b, 0, 0, 0, 0)
	}
	return eth(dst, src, nil, pkt.EtherTypeMPLS, append(b, inner...))
}

func TestDecodeLayouts(t *testing.T) {
	data := eth(ceB, ceA, vids(3162, 163), pkt.EtherTypeIPv4, ping(8, ipA, ipB, 0xaa))
	oam := eth(pkt.MAC{0x01, 0x80, 0xc2, 0, 0, 0x34}, ceSR, vids(3162), pkt.EtherTypeCFM, ccm(4, 7, "svc-1"))
	cases := []struct {
		name                string
		b                   []byte
		cw                  string
		service             uint32
		transport           []uint32
		fat, el, wantCW, ip bool
		class, tags         string
	}{
		{name: "pw, control word, FAT", b: mpls(peA, peB, []lbl{{524287, 255}, {524270, 255}, {914895, 0}}, true, data), cw: cwAuto,
			service: 524270, transport: []uint32{524287}, fat: true, wantCW: true, class: "data", tags: "3162,163"},
		{name: "pw, no control word", b: mpls(peA, peB, []lbl{{100, 255}, {200, 255}}, false, data), cw: cwAuto,
			service: 200, transport: []uint32{100}, class: "data", tags: "3162,163"},
		{name: "pw, no control word, CCM from a router MAC", b: mpls(peA, peB, []lbl{{100, 255}, {200, 255}}, false, oam), cw: cwAuto,
			service: 200, transport: []uint32{100}, class: "oam", tags: "3162"},
		{name: "pw, control word, CCM", b: mpls(peA, peB, []lbl{{200, 255}}, true, oam), cw: cwAuto,
			service: 200, wantCW: true, class: "oam", tags: "3162"},
		{name: "control word forced off", b: mpls(peA, peB, []lbl{{200, 255}}, false, data), cw: cwOff,
			service: 200, class: "data", tags: "3162,163"},
		{name: "entropy label", b: mpls(peA, peB, []lbl{{100, 255}, {7, 0}, {12345, 0}, {200, 255}}, true, data), cw: cwAuto,
			service: 200, transport: []uint32{100}, el: true, wantCW: true, class: "data", tags: "3162,163"},
		{name: "L3VPN: IP after the labels", b: mpls(peA, peB, []lbl{{100, 255}, {24001, 255}}, false, ping(8, ipA, ipB, 0xaa)), cw: cwAuto,
			service: 24001, transport: []uint32{100}, ip: true, class: "data", tags: ""},
		{name: "pseudowire ACh (VCCV)", b: mpls(peA, peB, []lbl{{200, 255}}, false, []byte{0x10, 0, 0, 0x21, 0x45, 0, 0, 20}), cw: cwAuto,
			service: 200, class: "other"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decode(c.b, c.cw)
			if d == nil || !d.mpls {
				t.Fatalf("not decoded as MPLS: %+v", d)
			}
			if d.service != c.service || len(d.transport) != len(c.transport) {
				t.Fatalf("service %d transport %v, want %d %v", d.service, d.transport, c.service, c.transport)
			}
			for i := range c.transport {
				if d.transport[i] != c.transport[i] {
					t.Fatalf("transport %v, want %v", d.transport, c.transport)
				}
			}
			if d.flowLabel != c.fat || d.entropy != c.el || d.cw != c.wantCW || d.ip != c.ip {
				t.Fatalf("fat %v el %v cw %v ip %v, want %v %v %v %v", d.flowLabel, d.entropy, d.cw, d.ip, c.fat, c.el, c.wantCW, c.ip)
			}
			if d.what.class != c.class {
				t.Fatalf("class %q (%s), want %q", d.what.class, d.what.kind, c.class)
			}
			if c.class != "other" && !c.ip {
				if got := wire.Format(d.inner.Tags); got != c.tags {
					t.Fatalf("tags %s, want %s", got, c.tags)
				}
			}
		})
	}
}

func TestDecodeCCMAndTPID(t *testing.T) {
	inner := eth(pkt.MAC{0x01, 0x80, 0xc2, 0, 0, 0x34}, ceA, []pkt.VLANTag{{TPID: 0x88a8, VID: 2113}, {VID: 113}}, pkt.EtherTypeCFM, ccm(4, 2113, "E2113"))
	d := decode(mpls(peA, peB, []lbl{{1, 255}, {2, 255}}, true, inner), cwAuto)
	if d == nil || d.what.class != "oam" || d.what.kind != "CCM" || *d.what.level != 4 || *d.what.mepID != 2113 {
		t.Fatalf("got %+v", d)
	}
	if d.what.ma == "" || wire.Format(d.inner.Tags) != "0x88a8:2113,113" {
		t.Fatalf("ma %q tags %s", d.what.ma, wire.Format(d.inner.Tags))
	}
}

// A session counts per service label and direction, flows by ID and
// direction, OAM by MEP; a plain frame on the link is its own service.
func TestSessionGroups(t *testing.T) {
	s := &session{cfg: &Config{ControlWord: cwAuto}}
	s.reset()
	now := time.Now()
	req := eth(ceB, ceA, vids(3162, 163), pkt.EtherTypeIPv4, ping(8, ipA, ipB, 0xaa))
	rep := eth(ceA, ceB, vids(3162, 163), pkt.EtherTypeIPv4, ping(0, ipB, ipA, 0xaa))
	cc := eth(pkt.MAC{0x01, 0x80, 0xc2, 0, 0, 0x34}, ceSR, vids(3162), pkt.EtherTypeCFM, ccm(4, 1, "svc"))
	for i := 0; i < 3; i++ {
		s.rx(mpls(peA, peB, []lbl{{9, 255}, {500, 255}}, true, req), now)
		s.rx(mpls(peB, peA, []lbl{{9, 255}, {500, 255}}, true, rep), now) // same label number back: other direction
		s.rx(mpls(peA, peB, []lbl{{9, 255}, {500, 255}}, true, cc), now)
		if i == 0 {
			s.rx(mpls(peB, peA, []lbl{{9, 255}, {500, 255}}, true, cc), now)
		}
	}
	s.rx(eth(pkt.MAC{0x01, 0x80, 0xc2, 0, 0, 0x14}, peA, nil, 0x8870, []byte{0xfe, 0xfe, 3, 0x83}), now) // IS-IS-ish
	s.rx([]byte{1, 2, 3}, now)
	st := s.status(now)
	if st.Frames != 12 || st.MPLS != 10 || st.Undecoded != 1 {
		t.Fatalf("counts %+v", st)
	}
	if len(st.Services) != 3 {
		t.Fatalf("services %d: %+v", len(st.Services), st.Services)
	}
	ab, ba := st.Services[0], st.Services[1]
	if ab.Src != peA.String() {
		ab, ba = ba, ab
	}
	if *ab.Label != 500 || len(ab.Data) != 1 || ab.Data[0].Count != 3 || ab.Data[0].Reply || ab.Data[0].Src != ceA.String() || len(ab.OAM) != 1 {
		t.Fatalf("a>b %+v", ab)
	}
	if len(ba.Data) != 1 || !ba.Data[0].Reply || len(ba.OAM) != 1 {
		t.Fatalf("b>a %+v", ba)
	}
	if st.Services[2].Label != nil || len(st.Services[2].Other) != 1 {
		t.Fatalf("plain %+v", st.Services[2])
	}

	f := wire.Flow{FlowID: "000000aa", MAC: ceA.String(), IP: ipA.String(), Expect: wire.Expect{Data: "3162,163", OAM: "3162"}}
	mons := []wire.Monitor{{Name: "m", Status: st}}
	if v := wire.Judge(f, mons); v.Verdict != wire.Pass {
		t.Fatalf("judge %+v", v)
	}
	f.Expect.OAM = "3162,163"
	if v := wire.Judge(f, mons); v.Verdict != wire.Fail {
		t.Fatalf("judge with the wrong OAM stack %+v", v)
	}
	if _, err := s.Action(nil, "clear", nil); err != nil || s.status(now).Frames != 0 {
		t.Fatal("clear")
	}
}
