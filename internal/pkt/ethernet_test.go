package pkt

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeQinQ(t *testing.T) {
	src, _ := ParseMAC("02:00:00:00:00:01")
	f := &Frame{
		Dst:       MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x33},
		Src:       src,
		Tags:      []VLANTag{{TPID: TPIDDot1AD, VID: 100, PCP: 5}, {VID: 200, DEI: true}},
		EtherType: EtherTypeCFM,
		Payload:   []byte{1, 2, 3, 4},
	}
	b := f.Encode()
	if len(b) != 60 {
		t.Fatalf("expected padding to 60, got %d", len(b))
	}
	g, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if g.Dst != f.Dst || g.Src != f.Src || g.EtherType != EtherTypeCFM {
		t.Fatalf("header mismatch: %+v", g)
	}
	if len(g.Tags) != 2 || g.Tags[0] != f.Tags[0] || g.Tags[1].VID != 200 || !g.Tags[1].DEI || g.Tags[1].TPID != TPIDDot1Q {
		t.Fatalf("tags mismatch: %+v", g.Tags)
	}
	if !bytes.HasPrefix(g.Payload, f.Payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestSameVLANs(t *testing.T) {
	cases := []struct {
		a, b []VLANTag
		want bool
	}{
		{nil, nil, true},
		{nil, []VLANTag{{VID: 0, PCP: 7}}, true},
		{[]VLANTag{{VID: 10}}, []VLANTag{{TPID: TPIDDot1Q, VID: 10, PCP: 3}}, true},
		{[]VLANTag{{VID: 10}}, []VLANTag{{TPID: TPIDDot1AD, VID: 10}}, false},
		{[]VLANTag{{VID: 10}}, nil, false},
		{[]VLANTag{{VID: 10}, {VID: 20}}, []VLANTag{{VID: 10}, {VID: 21}}, false},
	}
	for i, c := range cases {
		if got := SameVLANs(c.a, c.b); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestLLCFrame(t *testing.T) {
	llc := []byte{0xfe, 0xfe, 0x03, 0x83, 0x14}
	f := &Frame{Dst: MAC{0x01, 0x80, 0xc2, 0, 0, 0x14}, Tags: []VLANTag{{VID: 5}}, EtherType: EtherTypeLLC, Payload: llc}
	b := f.Encode()
	if len(b) != 60 || b[16] != 0 || b[17] != byte(len(llc)) {
		t.Fatalf("encoded % x", b[:20])
	}
	g, err := Decode(b)
	if err != nil || g.EtherType != EtherTypeLLC || !bytes.Equal(g.Payload, llc) || len(g.Tags) != 1 {
		t.Fatalf("decoded %+v %v", g, err)
	}
	b[17] = 200 // length beyond the frame
	if _, err := Decode(b); err == nil {
		t.Fatal("accepted a bad length")
	}
}
