package rsvp

import (
	"net/netip"
	"reflect"
	"testing"
)

var (
	addrA = netip.MustParseAddr("192.0.2.1")
	addrB = netip.MustParseAddr("192.0.2.2")
	sess  = SessionID{To: addrB, TunnelID: 7, ExtID: addrA}
	snd   = SenderID{From: addrA, LSPID: 3}
)

func TestPathRoundTrip(t *testing.T) {
	p := PathMsg{Session: sess, Hop: Hop{addrA, 9}, RefreshMs: 30000, Sender: snd,
		TSpec:  TSpec{Rate: 1000, Bucket: 1000, Peak: 2000, MinPolice: 20, MaxPacket: 1500},
		Attr:   Attr{Setup: 7, Hold: 0, Flags: AttrSE | AttrLabelRecording, Name: "abc"},
		ERO:    []EROHop{{Addr: addrB, Bits: 32, Loose: true}},
		FRR:    &FRR{Setup: 7, HopLimit: 16, Flags: 2},
		Record: true, RRO: []RROHop{{Addr: addrA}}}
	m, err := Decode(p.Encode(64))
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != MsgPath || m.SendTTL != 64 || m.Session != sess || m.Hop != p.Hop || m.RefreshM != 30000 || m.Sender != snd || m.LabelReq != 0x0800 || !m.HasLReq {
		t.Fatalf("%+v", m)
	}
	if *m.TSpec != p.TSpec || m.Attr.Name != "abc" || m.Attr.Flags != p.Attr.Flags || m.FRR.Flags != 2 {
		t.Fatalf("%+v %+v %+v", m.TSpec, m.Attr, m.FRR)
	}
	if !reflect.DeepEqual(m.ERO, p.ERO) || !reflect.DeepEqual(m.RRO, p.RRO) {
		t.Fatalf("%+v %+v", m.ERO, m.RRO)
	}
}

func TestResvSharedExplicit(t *testing.T) {
	second := SenderID{From: addrA, LSPID: 4}
	r := ResvMsg{Session: sess, Hop: Hop{addrB, 9}, RefreshMs: 30000, Style: StyleSE, Flows: []Flow{
		{Sender: snd, Label: 100000, HasLbl: true, HasRRO: true, RRO: []RROHop{{Addr: addrB, Label: 100000, HasLabel: true}}},
		{Sender: second, Label: 100001, HasLbl: true},
	}}
	m, err := Decode(r.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if m.Style != StyleSE || len(m.Flows) != 2 || m.Flows[0].Label != 100000 || m.Flows[1].Label != 100001 || m.Flows[1].Sender != second {
		t.Fatalf("%+v", m)
	}
	if h := m.Flows[0].RRO; len(h) != 1 || h[0].Addr != addrB || !h[0].HasLabel || h[0].Label != 100000 {
		t.Fatalf("%+v", h)
	}
}

// The RRO order: a node's address, then its label.
func TestRROLabelAfterAddress(t *testing.T) {
	v := []byte{1, 8, 10, 0, 0, 1, 32, RRProtectionAvail, 3, 8, 1, 1, 0, 1, 0x86, 0xa0, 1, 8, 10, 0, 0, 2, 32, 0}
	hops, err := decodeRRO(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 || !hops[0].HasLabel || hops[0].Label != 100000 || hops[0].Flags != RRProtectionAvail || hops[1].HasLabel {
		t.Fatalf("%+v", hops)
	}
}

// Ignored classes and unknown objects don't stop parsing.
func TestUnknownObjectsSkipped(t *testing.T) {
	var w writer
	w.session(sess)
	w.obj(63, 7, make([]byte, 8)) // DETOUR
	w.hop(Hop{addrA, 1})
	m, err := Decode(finish(MsgPath, 255, w.b))
	if err != nil || !m.HasSess || !m.HasHop {
		t.Fatalf("%v %+v", err, m)
	}
}

func TestDecodeRejects(t *testing.T) {
	b := HelloMsg(false, 1, 0)
	b[2] ^= 0xff
	if _, err := Decode(b); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if _, err := Decode([]byte{1 << 4, 1}); err == nil {
		t.Fatal("short accepted")
	}
	c := HelloMsg(false, 1, 0)
	c[8], c[9] = 0, 2 // object shorter than its header
	if _, err := Decode(c); err == nil {
		t.Fatal("bad length accepted")
	}
}

func TestHelloRoundTrip(t *testing.T) {
	m, err := Decode(HelloMsg(true, 5, 6))
	if err != nil || m.Type != MsgHello || !m.HelloAck || m.SrcInst != 5 || m.DstInst != 6 {
		t.Fatalf("%v %+v", err, m)
	}
}
