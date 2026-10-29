package bgp

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// Config changes apply in place: routes (all families) are advertised and
// withdrawn as UPDATEs, peers are added and removed on their own, and a
// peer's session is reset only when its OPEN or transport changes (remote
// AS, local address, families, hold time, TTL, BFD). A new local AS,
// router ID or speaker-wide BFD setting restarts the speaker. Routes added or withdrawn by actions stay as they
// are, unless the change advertises or withdraws the same routes.

func (s *session) Reconfigure(c any) bool {
	nc := *c.(*Config)
	s.mu.Lock()
	select {
	case <-s.stop:
		s.mu.Unlock()
		return false
	default:
	}
	oc := s.cfg
	if nc.LocalAS != oc.LocalAS || nc.RouterID != oc.RouterID || !sameJSON(nc.BFD, oc.BFD) {
		s.mu.Unlock()
		return false
	}
	var notes []string

	// ---- peers ----
	type closing struct {
		c   *conn
		sub uint8
	}
	var drop []closing
	var started []*peer
	stopPeer := func(p *peer, sub uint8) {
		close(p.quit)
		for c := range p.conns {
			drop = append(drop, closing{c, sub})
		}
	}
	old := s.byAddr
	s.cfg.HoldTime = nc.HoldTime // newPeer's default
	s.peers, s.byAddr = nil, map[netip.Addr]*peer{}
	for _, pc := range nc.Peers {
		a := netip.MustParseAddr(pc.Address).Unmap()
		p := old[a]
		delete(old, a)
		switch {
		case p == nil:
			p = newPeer(s, pc)
			started = append(started, p)
			notes = append(notes, "peer "+a.String()+" added")
		case sessionKey(p.cfg, oc.HoldTime) != sessionKey(pc, nc.HoldTime) || bfdOn(oc, p.cfg) != bfdOn(nc, pc):
			stopPeer(p, gb.BGP_ERROR_SUB_OTHER_CONFIGURATION_CHANGE)
			p = newPeer(s, pc)
			started = append(started, p)
			notes = append(notes, "peer "+a.String()+" reset (session settings changed)")
		default:
			p.cfg.Passive, p.cfg.Description, p.cfg.ConnectRetry = pc.Passive, pc.Description, pc.ConnectRetry
			p.retry = retryOf(pc)
			select {
			case p.wake <- struct{}{}:
			default:
			}
		}
		s.peers = append(s.peers, p)
		s.byAddr[a] = p
	}
	for a, p := range old {
		stopPeer(p, gb.BGP_ERROR_SUB_PEER_DECONFIGURED)
		notes = append(notes, "peer "+a.String()+" removed")
	}

	// ---- routes ----
	// What the affected routes were before, to tell each peer what changed.
	before := map[netip.Prefix]*tmpl{}
	xbefore := map[string]*xroute{}
	gone := Config{}
	added := Config{}
	gone.Routes, added.Routes = entryDiff(oc.Routes, nc.Routes)
	gone.EVPN, added.EVPN = entryDiff(oc.EVPN, nc.EVPN)
	gone.L2VPN, added.L2VPN = entryDiff(oc.L2VPN, nc.L2VPN)
	gone.SRPolicies, added.SRPolicies = entryDiff(oc.SRPolicies, nc.SRPolicies)
	keepP := map[netip.Prefix]bool{}
	for _, r := range nc.Routes {
		if r.RD == "" {
			ps, _ := r.expand()
			for _, p := range ps {
				keepP[p] = true
			}
		}
	}
	keepX := map[string]bool{}
	for _, x := range s.configX(nc) {
		keepX[x.key] = true
	}
	nAdv, nWd := 0, 0
	for _, r := range gone.Routes {
		if r.RD != "" {
			continue
		}
		ps, _ := r.expand()
		for _, p := range ps {
			t := s.out[p]
			if keepP[p] || t == nil {
				continue
			}
			if _, seen := before[p]; !seen {
				before[p] = t
			}
			t.count[famIdx(p)]--
			s.dropTmplLocked(t)
			delete(s.out, p)
			nWd++
		}
	}
	for _, x := range s.configX(gone) {
		if old := s.xout[x.key]; old != nil && !keepX[x.key] {
			if _, seen := xbefore[x.key]; !seen {
				xbefore[x.key] = old
			}
			delete(s.xout, x.key)
			nWd++
		}
	}
	type batch struct {
		t   *tmpl
		pfx []netip.Prefix
	}
	var adds []batch
	for _, r := range added.Routes {
		if r.RD != "" {
			continue
		}
		ps, _ := r.expand()
		for _, p := range ps {
			if _, seen := before[p]; !seen {
				before[p] = s.out[p]
			}
		}
		if t, ps := s.addRouteLocked(r); t != nil {
			adds = append(adds, batch{t, ps})
			nAdv += len(ps)
		}
	}
	for _, x := range s.configX(added) {
		if _, seen := xbefore[x.key]; !seen {
			xbefore[x.key] = s.xout[x.key]
		}
		s.xout[x.key] = x
		nAdv++
	}
	if nAdv > 0 {
		notes = append(notes, fmt.Sprintf("%d routes advertised", nAdv))
	}
	if nWd > 0 {
		notes = append(notes, fmt.Sprintf("%d withdrawn", nWd))
	}

	// Tell the established peers that stay; new ones get everything when
	// they come up.
	for _, p := range s.peers {
		c := p.est
		if c == nil {
			continue
		}
		for _, b := range adds {
			if b.t.forPeer(p.addr) {
				c.enqueue(job{t: b.t, pfx: b.pfx})
			}
		}
		var wd []netip.Prefix
		for x, ot := range before {
			nt := s.out[x]
			if (nt == nil || !nt.forPeer(p.addr)) && ot != nil && ot.forPeer(p.addr) {
				wd = append(wd, x)
			}
		}
		if len(wd) > 0 {
			sortPrefixes(wd)
			c.enqueue(job{withdraw: true, pfx: wd})
		}
		var xadv, xwd []*xroute
		for k, ox := range xbefore {
			nx := s.xout[k]
			switch {
			case nx != nil && nx != ox && nx.t.forPeer(p.addr):
				xadv = append(xadv, nx)
			case (nx == nil || !nx.t.forPeer(p.addr)) && ox != nil && ox.t.forPeer(p.addr):
				xwd = append(xwd, ox)
			}
		}
		if len(xwd) > 0 {
			c.enqueue(job{withdraw: true, xs: xwd})
		}
		if len(xadv) > 0 {
			c.enqueue(job{xs: xadv})
		}
	}
	s.cfg.Peers, s.cfg.Routes, s.cfg.EVPN, s.cfg.L2VPN, s.cfg.SRPolicies = nc.Peers, nc.Routes, nc.EVPN, nc.L2VPN, nc.SRPolicies
	for _, p := range started {
		s.wg.Add(1) // under s.mu: before Stop can wait
		go p.manage()
	}
	s.mu.Unlock()

	for _, d := range drop {
		d.c.notify(gb.BGP_ERROR_CEASE, d.sub, nil)
	}
	if len(notes) > 0 {
		s.event("config-changed", "BGP config applied: "+strings.Join(notes, "; "))
	}
	return true
}

// sessionKey is what of a peer's config its session depends on.
func sessionKey(p Peer, speakerHold int) string {
	hold := p.HoldTime
	if hold == 0 {
		hold = speakerHold
	}
	return fmt.Sprintf("%d|%s|%s|%d|%d", p.RemoteAS, p.LocalAddress, strings.Join(p.Families, ","), hold, p.TTL)
}

// configX is the VPN-family routes (VPN, EVPN, L2VPN, SR policy) of c's
// route lists.
func (s *session) configX(c Config) []*xroute {
	var xs []*xroute
	for _, r := range c.Routes {
		xs = append(xs, s.vpnRoutes(r)...)
	}
	for _, e := range c.EVPN {
		xs = append(xs, s.evpnRoutes(e)...)
	}
	for _, l := range c.L2VPN {
		if x, err := l.expand(); err == nil {
			xs = append(xs, x)
		}
	}
	for _, p := range c.SRPolicies {
		if x, err := p.expand(); err == nil {
			xs = append(xs, x)
		}
	}
	return xs
}

// entryDiff returns the entries of old not in new and of new not in old
// (an edited entry is in both).
func entryDiff[T any](old, new []T) (gone, added []T) {
	key := func(x T) string {
		b, _ := json.Marshal(x)
		return string(b)
	}
	diff := func(a, b []T) []T {
		n := map[string]int{}
		for _, x := range b {
			n[key(x)]++
		}
		var out []T
		for _, x := range a {
			if k := key(x); n[k] > 0 {
				n[k]--
			} else {
				out = append(out, x)
			}
		}
		return out
	}
	return diff(old, new), diff(new, old)
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
