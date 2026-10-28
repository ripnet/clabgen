package bgp

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

func u32p(v uint32) *uint32 { return &v }

func TestSRPolicyEncoding(t *testing.T) {
	p := SRPolicy{Endpoint: "10.0.0.5", Color: 100, Headend: "10.0.0.1", Distinguisher: 7, Preference: u32p(200), BindingSID: 100000,
		DropInvalid: true, SegmentLists: []SegmentList{{Labels: []uint32{40002, 40005}, Weight: 3}}, ENLP: "both", PathName: "cp1"}
	x, err := p.expand()
	if err != nil {
		t.Fatal(err)
	}
	// NLRI: length 96 bits, distinguisher, color, endpoint.
	if got := hex.EncodeToString(x.nlri); got != "60"+"00000007"+"00000064"+"0a000005" || x.f != famSRPv4 {
		t.Fatalf("nlri %s %v", got, x.f)
	}
	var enc []byte
	for _, a := range x.t.extra {
		if a.GetType() == gb.BGP_ATTR_TYPE_TUNNEL_ENCAP {
			enc = a.(*gb.PathAttributeUnknown).Value
		}
	}
	want := "000f" + "0038" + // tunnel type 15, 56 octets of sub-TLVs
		"0c06" + "0000" + "000000c8" + // preference 200
		"0d06" + "4000" + "186a0000" + // binding SID: I flag, label 100000 in the top 20 bits
		"0e03" + "000003" + // ENLP both
		"810004" + "00" + "637031" + // candidate path name "cp1" (2-octet length, reserved)
		"800019" + "00" + // segment list: 25 octets (reserved, weight, 2 segments)
		"0906" + "0000" + "00000003" + // weight 3
		"0106" + "0000" + "09c420ff" + // type A 40002, TC 0, TTL 255
		"0106" + "0000" + "09c450ff" // type A 40005
	if got := hex.EncodeToString(enc); got != want {
		t.Fatalf("tunnel encap\n got  %s\n want %s", got, want)
	}
	// Route target for the headend, no NO_ADVERTISE.
	if len(x.t.ext) != 1 || !strings.Contains(x.t.ext[0].String(), "10.0.0.1:0") || len(x.t.comms) != 0 {
		t.Errorf("rt %v comms %v", x.t.ext, x.t.comms)
	}

	// GoBGP, an independent decoder, reads both back.
	attr, err := gb.GetPathAttribute(append([]byte{0xc0, 23, byte(len(enc))}, enc...))
	if err != nil {
		t.Fatal(err)
	}
	if err := attr.DecodeFromBytes(append([]byte{0xc0, 23, byte(len(enc))}, enc...)); err != nil {
		t.Fatalf("GoBGP rejects the tunnel encap: %v", err)
	}
	tlv := attr.(*gb.PathAttributeTunnelEncap).Value[0]
	if tlv.Type != gb.TUNNEL_TYPE_SR_POLICY {
		t.Fatalf("tunnel type %v", tlv.Type)
	}
	var sawList, sawPref bool
	for _, s := range tlv.Value {
		switch v := s.(type) {
		case *gb.TunnelEncapSubTLVSRPreference:
			sawPref = v.Preference == 200
		case *gb.TunnelEncapSubTLVSRSegmentList:
			sawList = v.Weight != nil && v.Weight.Weight == 3 && len(v.Segments) == 2
		}
	}
	if !sawList || !sawPref {
		t.Errorf("GoBGP decoded %s", tlv.String())
	}
	n := &gb.SRPolicyIPv4{}
	if err := n.DecodeFromBytes(x.nlri); err != nil || n.Color != 100 || n.Distinguisher != 7 {
		t.Errorf("GoBGP NLRI %v %v", n, err)
	}
}

func TestSRPolicyDefaults(t *testing.T) {
	x, err := SRPolicy{Endpoint: "2001:db8::5", Color: 1, SegmentLists: []SegmentList{{Labels: []uint32{16005}}}}.expand()
	if err != nil {
		t.Fatal(err)
	}
	if x.f != famSRPv6 || x.nlri[0] != 192 || len(x.nlri) != 25 {
		t.Errorf("v6 nlri % x", x.nlri)
	}
	// No headend: NO_ADVERTISE (for the peer itself); distinguisher 1, preference 100.
	if len(x.t.comms) != 1 || x.t.comms[0] != noAdvertise || len(x.t.ext) != 0 || !bytes.Equal(x.nlri[1:5], []byte{0, 0, 0, 1}) {
		t.Errorf("defaults: comms %v ext %v", x.t.comms, x.t.ext)
	}
	if !strings.Contains(x.text, "preference 100") {
		t.Errorf("text %q", x.text)
	}
	for _, p := range []SRPolicy{
		{Endpoint: "nope", Color: 1, SegmentLists: []SegmentList{{Labels: []uint32{16}}}},
		{Endpoint: "10.0.0.1", Color: 0, SegmentLists: []SegmentList{{Labels: []uint32{16}}}},
		{Endpoint: "10.0.0.1", Color: 1},
		{Endpoint: "10.0.0.1", Color: 1, SegmentLists: []SegmentList{{}}},
		{Endpoint: "10.0.0.1", Color: 1, SegmentLists: []SegmentList{{Labels: []uint32{1 << 20}}}},
		{Endpoint: "10.0.0.1", Color: 1, BindingSID: 3, SegmentLists: []SegmentList{{Labels: []uint32{16}}}},
		{Endpoint: "10.0.0.1", Color: 1, Headend: "2001:db8::1", SegmentLists: []SegmentList{{Labels: []uint32{16}}}},
		{Endpoint: "10.0.0.1", Color: 1, ENLP: "maybe", SegmentLists: []SegmentList{{Labels: []uint32{16}}}},
	} {
		if _, err := p.expand(); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	// Two candidate paths of one policy need different distinguishers.
	c := defaultConfig()
	c.SRPolicies = []SRPolicy{{Endpoint: "10.0.0.1", Color: 1, SegmentLists: []SegmentList{{Labels: []uint32{16}}}},
		{Endpoint: "10.0.0.1", Color: 1, SegmentLists: []SegmentList{{Labels: []uint32{17}}}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "distinguisher") {
		t.Errorf("duplicate candidate paths: %v", err)
	}
}

func TestColorCommunity(t *testing.T) {
	tp, err := Route{Prefix: "198.51.100.0/24", ExtCommunities: []string{"color:100", "rt:65000:1"}}.attrs()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := tp.ext[0].Serialize()
	if hex.EncodeToString(b) != "030b000000000064" {
		t.Errorf("color community % x", b)
	}
	if _, err := (Route{Prefix: "198.51.100.0/24", ExtCommunities: []string{"color:x"}}).attrs(); err == nil {
		t.Error("color:x accepted")
	}
}
