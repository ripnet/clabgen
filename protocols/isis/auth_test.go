package isis

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

func TestAuthSignVerify(t *testing.T) {
	hello := EncodeP2PHello(circuitL2, sysA, 9, 1, [][]byte{area}, nil, nil, nil, ThreeWay{State: AdjDown, ExtCircuit: 1}, 0)
	lsp := EncodeLSP(2, MakeLSPID(sysA, 0, 0), 7, 1200, lspTypeL1L2, lspBody{Hostname: "a",
		Prefixes: []IPReach{{Prefix: netip.MustParsePrefix("10.0.0.0/24"), Metric: 10, Down: false}}})
	purge := purgeRaw(2, MakeLSPID(sysA, 0, 1), 3, lspTypeL1L2)
	csnp := EncodeCSNP(2, NodeID{}, LSPID{}, LSPID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, []LSPEntry{{1200, MakeLSPID(sysA, 0, 0), 7, 1}})
	for _, a := range []Auth{{Type: "password", Key: "secret", KeyID: 0}, {Type: "hmac-md5", Key: "s3cr3t", KeyID: 0}, {Type: "hmac-sha1", Key: "k", KeyID: 3}, {Type: "hmac-sha224", Key: "k", KeyID: 4},
		{Type: "hmac-sha256", Key: "k", KeyID: 5}, {Type: "hmac-sha384", Key: "k", KeyID: 6}, {Type: "hmac-sha512", Key: "k", KeyID: 7}} {
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
			if v, ok := p.TLVs.First(TLVAuth); !ok || (a.Type == "hmac-md5" && v[0] != authHMACMD5) || (a.sha() != nil && (v[0] != authGeneric || int(v[2]) != int(a.KeyID))) {
				t.Fatalf("%s %s: auth TLV %v", a.Type, name, v)
			}
			if err := (Auth{Type: a.Type, Key: "wrong", KeyID: a.KeyID}).verify(p.Raw); err != errBadAuth {
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
				if err := a.verify(aged); a.Type != "password" && err == nil {
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
	for _, a := range []Auth{{Type: "hmac-md5", Key: "k", KeyID: 0}, {Type: "hmac-sha512", Key: "k", KeyID: 1}} {
		padded := a.sign(EncodeP2PHello(circuitL2, sysA, 9, 1, [][]byte{area}, nil, nil, nil, ThreeWay{}, 1497-a.tlvLen()))
		if len(padded) != 1497 {
			t.Fatalf("%s: padded signed hello %d bytes", a.Type, len(padded))
		}
	}
	padded := (Auth{Type: "hmac-md5", Key: "k", KeyID: 0}).sign(EncodeP2PHello(circuitL2, sysA, 9, 1, [][]byte{area}, nil, nil, nil, ThreeWay{}, 1497-(Auth{Type: "hmac-md5", Key: "k", KeyID: 0}).tlvLen()))
	if len(padded) != 1497 {
		t.Fatalf("padded signed hello %d bytes", len(padded))
	}
}

func TestAuthKeyID(t *testing.T) {
	a := Auth{Type: "hmac-sha256", Key: "k", KeyID: 9}
	p, _ := Decode(frame(a.sign(EncodeP2PHello(circuitL2, sysA, 9, 1, [][]byte{area}, nil, nil, nil, ThreeWay{}, 0))))
	if err := (Auth{Type: "hmac-sha256", Key: "k", KeyID: 10}).verify(p.Raw); err != errKeyID {
		t.Fatalf("other key ID: %v", err)
	}
	if err := (Auth{Type: "hmac-sha1", Key: "k", KeyID: 9}).verify(p.Raw); err != errBadAuth {
		t.Fatalf("other algorithm: %v", err)
	}
	if v, _ := p.TLVs.First(TLVAuth); len(v) != 3+32 {
		t.Fatalf("SHA-256 auth TLV length %d", len(v))
	}
}

func at(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }

func TestKeychain(t *testing.T) {
	hello := func(a Auth) []byte {
		p, _ := Decode(frame(a.sign(EncodeP2PHello(circuitL2, sysA, 9, 1, [][]byte{area}, nil, nil, nil, ThreeWay{}, 0))))
		return p.Raw
	}
	k1 := AuthKey{ID: 1, Type: "hmac-md5", Key: "old"}
	k2 := AuthKey{ID: 2, Type: "hmac-sha256", Key: "new"}
	// Key 2 has started sending (latest start wins); both are accepted.
	k1.SendFrom, k2.SendFrom = at(-time.Hour), at(-time.Minute)
	chain := Auth{Type: "keychain", Keys: []AuthKey{k1, k2}}
	if err := chain.validate("t"); err != nil {
		t.Fatal(err)
	}
	if k, ok := chain.sendKey(time.Now()); !ok || k.ID != 2 {
		t.Fatalf("send key %+v %v", k, ok)
	}
	raw := hello(chain)
	if err := (Auth{Type: "hmac-sha256", Key: "new", KeyID: 2}).verify(raw); err != nil {
		t.Fatalf("signed with key 2? %v", err)
	}
	// The chain accepts both the old (MD5) and new (SHA-256) key.
	for _, k := range []AuthKey{k1, k2} {
		if err := chain.verify(hello(k.single())); err != nil {
			t.Fatalf("key %d not accepted: %v", k.ID, err)
		}
	}
	if err := chain.verify(hello(Auth{Type: "hmac-sha256", Key: "new", KeyID: 9})); err == nil {
		t.Fatal("unknown key ID accepted")
	}
	// Key 2 not started yet: key 1 sends. Key 1's accept window closed: rejected.
	k2.SendFrom, k1.AcceptUntil = at(time.Hour), at(-time.Second)
	chain = Auth{Type: "keychain", Keys: []AuthKey{k1, k2}}
	if k, _ := chain.sendKey(time.Now()); k.ID != 1 {
		t.Fatalf("send key %d, want 1", k.ID)
	}
	if err := chain.verify(hello(k1.single())); err == nil {
		t.Fatal("key past its accept window accepted")
	}
	// No valid send key: unauthenticated (and the TLV room is the largest key's).
	none := Auth{Type: "keychain", Keys: []AuthKey{{ID: 1, Type: "hmac-sha512", Key: "k", SendFrom: at(time.Hour)}, {ID: 2, Type: "password", Key: "p"}}}
	none.Keys[1].SendUntil = at(-time.Hour)
	if _, ok := none.sendKey(time.Now()); ok {
		t.Fatal("a send key without any open window")
	}
	if p, _ := Decode(frame(none.sign(EncodeP2PHello(circuitL2, sysA, 9, 1, nil, nil, nil, nil, ThreeWay{}, 0)))); len(p.TLVs.All(TLVAuth)) != 0 {
		t.Fatal("signed without a send key")
	}
	if none.tlvLen() != (Auth{Type: "hmac-sha512", Key: "k"}).tlvLen() {
		t.Fatal("tlvLen must cover the largest key")
	}
	for _, bad := range []Auth{
		{Type: "keychain"},
		{Type: "keychain", Keys: []AuthKey{{ID: 1, Type: "hmac-md5", Key: "a"}, {ID: 1, Type: "hmac-md5", Key: "b"}}},
		{Type: "keychain", Keys: []AuthKey{{ID: 1, Type: "hmac-md5", Key: "a", SendFrom: "tomorrow"}}},
		{Type: "keychain", Keys: []AuthKey{{ID: 1, Type: "keychain", Key: "a"}}},
	} {
		if bad.validate("t") == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
