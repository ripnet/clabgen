package isis

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func TestAuthSignVerify(t *testing.T) {
	hello := EncodeP2PHello(circuitL2, sysA, 9, 1, [][]byte{area}, nil, nil, ThreeWay{State: AdjDown, ExtCircuit: 1}, 0)
	lsp := EncodeLSP(2, MakeLSPID(sysA, 0, 0), 7, 1200, lspTypeL1L2, lspBody{Hostname: "a",
		Prefixes: []IPReach{{netip.MustParsePrefix("10.0.0.0/24"), 10, false}}})
	purge := purgeRaw(2, MakeLSPID(sysA, 0, 1), 3, lspTypeL1L2)
	csnp := EncodeCSNP(2, NodeID{}, LSPID{}, LSPID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, []LSPEntry{{1200, MakeLSPID(sysA, 0, 0), 7, 1}})
	for _, a := range []Auth{{"password", "secret"}, {"hmac-md5", "s3cr3t"}} {
		for name, pdu := range map[string][]byte{"hello": hello, "lsp": lsp, "purge": purge, "csnp": csnp} {
			signed := a.sign(pdu)
			if len(signed) != len(pdu)+a.tlvLen() {
				t.Fatalf("%s %s: length %d → %d", a.Type, name, len(pdu), len(signed))
			}
			p, err := Decode(frame(signed))
			if err != nil {
				t.Fatalf("%s %s: %v", a.Type, name, err)
			}
			if err := a.verify(p.Raw); err != nil {
				t.Fatalf("%s %s: %v", a.Type, name, err)
			}
			if v, ok := p.TLVs.First(TLVAuth); !ok || (a.Type == "hmac-md5" && v[0] != authHMACMD5) {
				t.Fatalf("%s %s: auth TLV %v", a.Type, name, v)
			}
			if err := (Auth{a.Type, "wrong"}).verify(p.Raw); err != errBadAuth {
				t.Fatalf("%s %s: wrong key: %v", a.Type, name, err)
			}
			if err := a.verify(pdu); err != errNoAuth {
				t.Fatalf("%s %s: unsigned: %v", a.Type, name, err)
			}
			if name == "lsp" {
				if !fletcherOK(signed[12:]) {
					t.Fatalf("%s: LSP checksum broken by signing", a.Type)
				}
				// Ageing changes the lifetime, which HMAC doesn't cover.
				aged := append([]byte{}, signed...)
				binary.BigEndian.PutUint16(aged[10:], 600)
				if err := a.verify(aged); err != nil {
					t.Fatalf("%s: aged LSP: %v", a.Type, err)
				}
				// Tampering with content does break it.
				aged[len(aged)-1] ^= 1
				if err := a.verify(aged); a.Type == "hmac-md5" && err == nil {
					t.Fatal("tampered LSP verified")
				}
			}
			if name == "purge" && (signed[24] != 0 || signed[25] != 0) {
				t.Fatalf("%s: purge checksum not zero", a.Type)
			}
		}
	}
	if (Auth{Type: "none"}).verify(hello) != nil {
		t.Fatal("no auth must accept anything")
	}
	// Padding plus authentication still fits the target size.
	padded := (Auth{"hmac-md5", "k"}).sign(EncodeP2PHello(circuitL2, sysA, 9, 1, [][]byte{area}, nil, nil, ThreeWay{}, 1497-(Auth{"hmac-md5", "k"}).tlvLen()))
	if len(padded) != 1497 {
		t.Fatalf("padded signed hello %d bytes", len(padded))
	}
}
