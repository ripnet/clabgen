package bgp

import (
	"net/netip"
	"testing"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

func prefixes(t *testing.T, base string, n int) []netip.Prefix {
	t.Helper()
	ps, err := Route{Prefix: base, Count: n}.expand()
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// Packed UPDATEs stay within 4096 bytes and carry every prefix, with the
// attributes GoBGP decodes back.
func TestBuildUpdates(t *testing.T) {
	med, lp := uint32(7), uint32(200)
	tm, err := Route{Prefix: "10.0.0.0/24", MED: &med, LocalPref: &lp, Communities: []string{"65000:1", "no-export"},
		LargeCommunities: []string{"1:2:3"}, ExtCommunities: []string{"rt:65000:9"}, ASPath: []uint32{64512}}.attrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		f    family
		base string
		n    int
		nh   string
	}{{famIPv4, "100.64.0.0/24", 5000, "192.0.2.1"}, {famIPv6, "2001:db8:a::/48", 3000, "2001:db8::1"}, {famIPv4, "10.0.0.0/32", 1200, "192.0.2.1"}} {
		pfx := prefixes(t, c.base, c.n)
		msgs, err := buildUpdates(c.f, tm.pathAttrs(65000, false), netip.MustParseAddr(c.nh), netip.Addr{}, pfx)
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, m := range msgs {
			if len(m) > maxMsg {
				t.Fatalf("%s: message of %d bytes", c.base, len(m))
			}
			p, err := gb.ParseBGPMessage(m)
			if err != nil {
				t.Fatalf("%s: %v", c.base, err)
			}
			u := p.Body.(*gb.BGPUpdate)
			info := summarize(u.PathAttributes)
			if info.ASPath != "64512" || *info.MED != 7 || *info.LocalPref != 200 || len(info.Communities) != 2 || len(info.LargeCommunities) != 1 || len(info.ExtCommunities) != 1 {
				t.Fatalf("%s: attributes %+v", c.base, info)
			}
			var last gb.BGPAttrType
			for _, a := range u.PathAttributes {
				if a.GetType() < last {
					t.Fatalf("%s: attribute %v after %v", c.base, a.GetType(), last)
				}
				last = a.GetType()
				if mp, ok := a.(*gb.PathAttributeMpReachNLRI); ok {
					got += len(mp.Value)
					if mp.Nexthop.String() != c.nh {
						t.Fatalf("next hop %s", mp.Nexthop)
					}
				}
			}
			got += len(u.NLRI)
		}
		if got != c.n {
			t.Fatalf("%s: %d of %d prefixes in %d messages", c.base, got, c.n, len(msgs))
		}
		if len(msgs) > c.n/200+2 && c.n > 1000 {
			t.Fatalf("%s: badly packed: %d messages", c.base, len(msgs))
		}
		// Withdrawals.
		got = 0
		for _, m := range buildWithdraws(c.f, pfx) {
			p, err := gb.ParseBGPMessage(m)
			if err != nil || len(m) > maxMsg {
				t.Fatalf("withdraw: %v (%d bytes)", err, len(m))
			}
			u := p.Body.(*gb.BGPUpdate)
			got += len(u.WithdrawnRoutes)
			for _, a := range u.PathAttributes {
				if mp, ok := a.(*gb.PathAttributeMpUnreachNLRI); ok {
					got += len(mp.Value)
				}
			}
		}
		if got != c.n {
			t.Fatalf("%s: %d of %d withdrawn", c.base, got, c.n)
		}
	}
	if eor, rf := mustParse(t, endOfRIB(famIPv6)).IsEndOfRib(); !eor || rf != gb.RF_IPv6_UC {
		t.Fatal("IPv6 End-of-RIB")
	}
}

func mustParse(t *testing.T, b []byte) *gb.BGPUpdate {
	t.Helper()
	p, err := gb.ParseBGPMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	return p.Body.(*gb.BGPUpdate)
}

func TestConfigValidate(t *testing.T) {
	good := Config{LocalAS: 1, HoldTime: 90, Peers: []Peer{{Address: "10.0.0.1", RemoteAS: 2}},
		Routes: []Route{{Prefix: "10.1.0.0/24", Count: 3, Communities: []string{"1:2"}, Peers: []string{"10.0.0.1"}}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Config{
		{LocalAS: 0},
		{LocalAS: 1, HoldTime: 2},
		{LocalAS: 1, Peers: []Peer{{Address: "x", RemoteAS: 1}}},
		{LocalAS: 1, Peers: []Peer{{Address: "10.0.0.1"}}},
		{LocalAS: 1, Peers: []Peer{{Address: "10.0.0.1", RemoteAS: 1, Families: []string{"l2vpn-evpn"}}}},
		{LocalAS: 1, Routes: []Route{{Prefix: "10.0.0.0/24", Communities: []string{"70000:1"}}}},
		{LocalAS: 1, Routes: []Route{{Prefix: "10.0.0.0/24", NextHop: "2001:db8::1"}}},
		{LocalAS: 1, Routes: []Route{{Prefix: "255.255.255.0/24", Count: 2}}},
		{LocalAS: 1, Routes: []Route{{Prefix: "10.0.0.0/24", Peers: []string{"10.9.9.9"}}}},
		{LocalAS: 1, Routes: []Route{{Prefix: "10.0.0.0/24", ExtCommunities: []string{"color:1"}}}},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
