package lldp

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func TestDecodeErrors(t *testing.T) {
	var w tlvWriter
	w.add(tlvPortID, []byte{5, 'p'}) // port ID first
	w.add(tlvChassisID, []byte{7, 'c'})
	w.add(tlvTTL, []byte{0, 1})
	if _, err := Decode(w.b); err != ErrMandatory {
		t.Fatalf("misordered: %v", err)
	}
	w = tlvWriter{}
	w.add(tlvChassisID, []byte{7, 'c'})
	w.add(tlvPortID, []byte{5, 'p'})
	w.add(tlvTTL, []byte{0, 120})
	w.add(tlvSysName, []byte("x"))
	if _, err := Decode(w.b[:len(w.b)-1]); err != ErrShort {
		t.Fatalf("truncated: %v", err)
	}
	if in, err := Decode(w.b); err != nil || in.TTL != 120 || in.SysName != "x" {
		t.Fatalf("no End TLV: %+v %v", in, err)
	}
}

func TestIDsAndAddresses(t *testing.T) {
	var w tlvWriter
	v, _ := idValue(false, true, "2001:db8::1")
	w.add(tlvChassisID, append([]byte{chassisSubtypes["network-address"]}, v...))
	v, _ = idValue(true, false, "02:00:00:00:00:09")
	w.add(tlvPortID, append([]byte{portSubtypes["mac"]}, v...))
	w.add(tlvTTL, binary.BigEndian.AppendUint16(nil, 121))
	w.add(tlvMgmtAddr, mgmtAddr(netip.MustParseAddr("2001:db8::7"), 3))
	w.add(tlvMgmtAddr, mgmtAddr(netip.MustParseAddr("192.0.2.7"), 3))
	w.add(42, []byte{1, 2})
	w.add(tlvEnd, nil)
	in, err := Decode(w.b)
	if err != nil {
		t.Fatal(err)
	}
	if in.ChassisID != "2001:db8::1" || in.PortID != "02:00:00:00:00:09" || in.PortSubtype != "mac" || fmt.Sprint(in.MgmtAddrs) != "[2001:db8::7 192.0.2.7]" {
		t.Fatalf("%+v", in)
	}
	if len(in.Other) != 1 || in.Other[0].Type != 42 || in.Other[0].Value != "0102" {
		t.Fatalf("unknown TLV %+v", in.Other)
	}
	// Non-printable text IDs come back as hex.
	if decodeID(5, 3, 7, []byte{0, 1}) != "0001" {
		t.Fatal("hex fallback")
	}
}

// An LLDPDU captured from SR-SIM 26.7.R1 (port 1/1/c3/1), with its
// Ethernet header.
func TestDecodeSROS(t *testing.T) {
	h, err := os.ReadFile("testdata/sr-lldpdu.hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(h)))
	if err != nil || len(b) < 14 {
		t.Fatal(err)
	}
	in, err := Decode(b[14:])
	if err != nil {
		t.Fatal(err)
	}
	if in.SysName != "sr1" || in.ChassisSubtype != "mac" || in.PortSubtype != "local" || in.TTL != 41 || fmt.Sprint(in.MgmtAddrs) != "[192.0.2.100]" ||
		fmt.Sprint(in.Capabilities) != "[bridge router]" || !strings.HasPrefix(in.PortDesc, "1/1/c3/1") || !strings.HasPrefix(in.SysDesc, "TiMOS") || len(in.Other) != 0 {
		t.Fatalf("%+v", in)
	}
}
