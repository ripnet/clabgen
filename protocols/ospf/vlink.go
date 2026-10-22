package ospf

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/ripnet/clabgen/internal/pkt"
)

// Virtual links (RFC 2328 15, RFC 5340 4.4.3): a backbone interface to
// another area border router across a transit area, for a border router
// without a backbone link of its own. Its cost and the far end's address
// (OSPFv3: a global address it advertises with the LA bit) come from the
// transit area's shortest paths; packets go unicast through the first hop
// toward the far end.

type VirtualLink struct {
	TransitArea AreaID `json:"transit_area" jsonschema:"description=Area the virtual link crosses (a normal area we have an interface in)"`
	RouterID    string `json:"router_id" jsonschema:"description=The other end's router ID"`
	Auth        *Auth  `json:"auth,omitempty" jsonschema:"description=Default: the router's auth"`
	// SR OS virtual links default to 10/60 s.
	HelloInterval int `json:"hello_interval,omitempty" jsonschema:"description=Seconds. Default: the router's"`
	DeadInterval  int `json:"dead_interval,omitempty" jsonschema:"description=Seconds. Default: the router's (SR OS virtual links use 60)"`
}

type vlink struct {
	transit *area
	end     netip.Addr // the far end's router ID
	via     *iface     // our interface toward the first hop
	nhMAC   pkt.MAC
	src     netip.Addr // our address on via
	dst     netip.Addr // the far end's interface address
	cost    uint32
}

func validateVirtualLinks(c *Config) error {
	for _, v := range c.VirtualLinks {
		a, err := ParseArea(string(v.TransitArea))
		if err != nil {
			return fmt.Errorf("virtual_links: %w", err)
		}
		if a == zero4 {
			return fmt.Errorf("virtual_links: the transit area can't be the backbone")
		}
		if r, err := netip.ParseAddr(v.RouterID); err != nil || !r.Is4() {
			return fmt.Errorf("virtual_links: router_id %q: want a dotted ID", v.RouterID)
		}
		if err := validTimers(v.HelloInterval, v.DeadInterval, c); err != nil {
			return fmt.Errorf("virtual link to %s: %w", v.RouterID, err)
		}
		if v.Auth != nil {
			if err := v.Auth.Validate(); err != nil {
				return fmt.Errorf("virtual link to %s: %w", v.RouterID, err)
			}
		}
	}
	return nil
}

// addVirtualLinks creates the virtual interfaces (after the real ones).
func (s *session) addVirtualLinks(byID func(AreaID) *area) error {
	if len(s.cfg.VirtualLinks) == 0 {
		return nil
	}
	bb := byID("0")
	for _, v := range s.cfg.VirtualLinks {
		t := byID(v.TransitArea)
		if t.kind != areaNormal {
			return fmt.Errorf("virtual link to %s: transit area %s is a %s area", v.RouterID, t.id, t.kind)
		}
		end := netip.MustParseAddr(v.RouterID)
		idx := len(s.ifs)
		f := &iface{idx: idx, ifID: uint32(idx + 1), device: "vlink-" + v.RouterID, kind: "virtual", p2p: true, unicast: true,
			cost: s.cfg.Cost, prio: 0, auth: s.cfg.Auth, state: ifDown, nbrs: map[netip.Addr]*nbr{}, dr: zero4, bdr: zero4,
			cryptoSeq: uint32(time.Now().Unix()), area: bb, sendKey: -2, err: "far end unreachable in the transit area",
			vl: &vlink{transit: t, end: end}, helloIv: s.cfg.HelloInterval, deadIv: s.cfg.DeadInterval}
		if v.HelloInterval > 0 {
			f.helloIv = v.HelloInterval
		}
		if v.DeadInterval > 0 {
			f.deadIv = v.DeadInterval
		}
		if v.Auth != nil {
			f.auth = *v.Auth
		}
		s.ifs = append(s.ifs, f)
	}
	return nil
}

// updateVirtualLinksLocked follows the transit areas' shortest paths:
// a virtual link is up while its far end is reachable.
func (s *session) updateVirtualLinksLocked(now time.Time) {
	for _, f := range s.ifs {
		v := f.vl
		if v == nil {
			continue
		}
		r := v.transit.spf
		var d uint32
		ok := r != nil
		if ok {
			d, ok = r.dist[v.end]
		}
		var via *iface
		var mac pkt.MAC
		if ok {
			hop := r.firstHop[v.end]
			for _, x := range s.ifs {
				if n := x.nbrs[hop]; x.area == v.transit && n != nil && n.state == nFull {
					via, mac = x, n.mac
				}
			}
			ok = via != nil && r.addr[v.end].IsValid() && r.addr[v.end] != zero4
		}
		if !ok {
			if f.state != ifDown {
				s.ifDownLocked(f, now, "far end unreachable in the transit area")
				go s.event("interface-down", fmt.Sprintf("virtual link to %s down", v.end))
			}
			f.err = "far end unreachable in the transit area"
			continue
		}
		var src netip.Addr
		if s.cd.v3 {
			if ps := s.ifPrefixes6(via); len(ps) > 0 {
				src = ps[0].Addr()
			}
		} else if p, ok := s.ifAddrLocked(via); ok {
			src = p.Addr()
		}
		if !src.IsValid() {
			f.err = "no address on the interface toward the far end"
			continue
		}
		changed := v.via != via || v.src != src || v.dst != r.addr[v.end]
		v.via, v.nhMAC, v.src, v.dst = via, mac, src, r.addr[v.end]
		f.statics = []netip.Addr{v.dst}
		if v.cost != d {
			v.cost = d
			s.dirty = true
		}
		if f.state == ifDown {
			s.ifUpLocked(f, now)
			go s.event("interface-up", fmt.Sprintf("virtual link to %s up (cost %d via %s)", v.end, d, via.device))
		} else if changed {
			for _, n := range f.nbrs {
				n.addr = v.dst
			}
		}
	}
}

// virtualFor is the virtual link a backbone packet received on transit
// interface f belongs to.
func (s *session) virtualFor(f *iface, areaID, rid netip.Addr) *iface {
	if areaID != zero4 {
		return nil
	}
	for _, x := range s.ifs {
		if x.vl != nil && x.vl.transit == f.area && x.vl.end == rid && x.state != ifDown {
			return x
		}
	}
	return nil
}

// virtualThrough: a full virtual adjacency crosses area a (its routers get
// the V bit).
func (s *session) virtualThrough(a *area) bool {
	for _, f := range s.ifs {
		if f.vl != nil && f.vl.transit == a {
			for _, n := range f.nbrs {
				if n.state == nFull {
					return true
				}
			}
		}
	}
	return false
}
