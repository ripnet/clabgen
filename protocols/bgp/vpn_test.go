package bgp

import (
	"net/netip"
	"strings"
	"testing"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// Each VPN family's UPDATEs parse back with GoBGP: NLRI, labels, next hop
// and the route-type communities.
func TestVPNRoundTrip(t *testing.T) {
	rid := netip.MustParseAddr("192.0.2.50")
	seq := uint32(3)
	var all []*xroute
	r4 := Route{Prefix: "10.50.0.0/24", Count: 300, RD: "65100:500", Label: 5000, RouteTargets: []string{"65100:500"}}
	t4, _ := r4.attrs()
	x4, err := r4.vpnRoutes(t4)
	if err != nil {
		t.Fatal(err)
	}
	r6 := Route{Prefix: "2001:db8:500::/48", Count: 10, RD: "192.0.2.50:500", RouteTargets: []string{"65100:500"}}
	t6, _ := r6.attrs()
	x6, _ := r6.vpnRoutes(t6)
	all = append(append(all, x4...), x6...)
	for _, e := range []EVPNRoute{
		{Type: "mac-ip", RD: "65100:600", RouteTargets: []string{"65100:600"}, MAC: "02:00:00:00:06:01", IP: "10.60.0.1", Count: 100, Label: 600, MACMobility: &seq},
		{Type: "inclusive-multicast", RD: "65100:600", RouteTargets: []string{"65100:600"}, Label: 600},
		{Type: "ethernet-ad", RD: "65100:600", ESI: "00:11:22:33:44:55:66:77:88:99", Label: 600, SingleActive: true},
		{Type: "ethernet-segment", RD: "192.0.2.50:0", ESI: "00:11:22:33:44:55:66:77:88:99"},
		{Type: "ip-prefix", RD: "65100:601", RouteTargets: []string{"65100:601"}, Prefix: "10.61.0.0/24", Count: 5, Label: 601},
		{Type: "mac-ip", RD: "65100:602", MAC: "02:00:00:00:06:02", Label: 10602, Encap: "vxlan"},
	} {
		xs, _, err := e.expand(rid)
		if err != nil {
			t.Fatalf("%s: %v", e.Type, err)
		}
		all = append(all, xs...)
	}
	for _, l := range []L2VPNRoute{{RD: "65100:700", RouteTargets: []string{"65100:700"}, VEID: 2, LabelBase: 700000, MTU: 1500},
		{Kind: "vpws", RD: "65100:800", VEID: 1, LabelBase: 800000, BlockSize: 2}} {
		x, err := l.expand()
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, x)
	}
	groups := map[*tmpl][]*xroute{}
	for _, x := range all {
		groups[x.t] = append(groups[x.t], x)
	}
	got := map[gb.RouteFamily]int{}
	var texts []string
	for tm, xs := range groups {
		var nl [][]byte
		for _, x := range xs {
			nl = append(nl, x.nlri)
		}
		msgs, err := buildMPUpdates(xs[0].f, tm.pathAttrs(65100, false), xNextHop(xs[0].f, rid), nl)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if len(m) > maxMsg {
				t.Fatalf("%d bytes", len(m))
			}
			msg, err := gb.ParseBGPMessage(m)
			if err != nil {
				t.Fatalf("%s: %v", xs[0].f.name, err)
			}
			u := msg.Body.(*gb.BGPUpdate)
			info := summarize(u.PathAttributes)
			for _, a := range u.PathAttributes {
				if mp, ok := a.(*gb.PathAttributeMpReachNLRI); ok {
					rf := gb.AfiSafiToRouteFamily(mp.AFI, mp.SAFI)
					got[rf] += len(mp.Value)
					if nh := mp.Nexthop.String(); nh != "192.0.2.50" && nh != "::ffff:192.0.2.50" {
						t.Fatalf("%v next hop %s", rf, nh)
					}
					vx := strings.Contains(strings.Join(info.ExtCommunities, " "), "VXLAN")
					for _, n := range mp.Value[:1] {
						texts = append(texts, rxText(n, vx)+" | "+strings.Join(info.ExtCommunities, ","))
					}
				}
			}
		}
	}
	if got[gb.RF_IPv4_VPN] != 300 || got[gb.RF_IPv6_VPN] != 10 || got[gb.RF_EVPN] != 109 || got[gb.RF_VPLS] != 2 {
		t.Fatalf("decoded %v", got)
	}
	want := []string{"65100:500:10.50.0.0/24 label [5000] | 65100:500", "label [600]", "esi ESI_ARBITRARY", "label 601", "label [10602]",
		"(block 8, labels from 700000)", "l2-info encap 19 flags 0x0 mtu 1500", "l2-info encap 5", "65100:600", "esi-label: 0, single-active", "mac-mobility: 3", "VXLAN"}
	joined := strings.Join(texts, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("missing %q in:\n%s", w, joined)
		}
	}
	// Withdrawals parse too.
	for _, m := range buildMPWithdraws(famEVPN, [][]byte{all[len(all)-3].nlri}) {
		if _, err := gb.ParseBGPMessage(m); err != nil {
			t.Fatal(err)
		}
	}
}

// L2VPN attributes are taken out of an UPDATE and decoded; the rest still
// parses with GoBGP.
func TestTakeL2VPN(t *testing.T) {
	x, err := L2VPNRoute{RD: "65100:700", RouteTargets: []string{"65100:700"}, VEID: 1, LabelBase: 524288, BlockSize: 8}.expand()
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := buildMPUpdates(famL2VPN, x.t.pathAttrs(65100, false), xNextHop(famL2VPN, netip.MustParseAddr("192.0.2.100")), [][]byte{x.nlri})
	b, u := takeL2VPN(msgs[0])
	if u == nil || len(u.reach) != 1 || u.nextHop != "192.0.2.100" || u.reach[0].text != "65100:700:1:1 (block 8, labels from 524288)" {
		t.Fatalf("%+v", u)
	}
	m, err := gb.ParseBGPMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	info := summarize(m.Body.(*gb.BGPUpdate).PathAttributes)
	if !strings.Contains(strings.Join(info.ExtCommunities, " "), "l2-info encap 19") || info.LocalPref == nil {
		t.Fatalf("attributes %+v", info)
	}
	eor := endOfRIB(famL2VPN)
	if b2, u2 := takeL2VPN(eor); u2 == nil || len(u2.reach)+len(u2.unreach) != 0 || len(b2) != 23 {
		t.Fatalf("EoR: %d bytes, %+v", len(b2), u2)
	}
	if b3, u3 := takeL2VPN(msgs[0][:30]); u3 != nil || len(b3) != 30 {
		t.Fatal("truncated message changed")
	}
}
