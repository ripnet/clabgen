// Package macsec is a MACsec (IEEE 802.1AE) SecY on a device or a whole
// port: every frame the devices behind it send is protected, and they take
// only frames that validate. Keys come from MKA (internal/mka) with a
// pre-shared CAK, or are static. The data plane is internal/macsec.
package macsec

import (
	"context"
	"errors"
	"fmt"

	"github.com/ripnet/clabgen/internal/macsec"
	"github.com/ripnet/clabgen/internal/mka"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/protocol"
)

const TypeName = "macsec"

func init() { protocol.Register(macsecType{}) }

type macsecType struct{}

func (macsecType) Name() string { return TypeName }
func (macsecType) Description() string {
	return "MACsec (802.1AE) with MKA or static keys, on the device or its whole port: every frame sent protected, only valid frames taken"
}
func (macsecType) NewConfig() any       { return defaultConfig() }
func (macsecType) EtherTypes() []uint16 { return nil }
func (macsecType) Actions() []protocol.Action {
	return []protocol.Action{
		{Name: "rekey", Description: "Key server: distribute a new SAK now"},
		{Name: "pause-mka", Description: "Stop sending and answering MKPDUs: peers time out and remove the keys"},
		{Name: "resume-mka", Description: "Start MKA again (a new member identifier, as after a restart)"},
		{Name: "clear", Description: "Reset the counters"},
	}
}

func (macsecType) Start(env protocol.Env, c any) (protocol.Session, error) {
	cfg := c.(*Config)
	sec, ok := env.(protocol.Securer)
	if !ok {
		return nil, errors.New("this agent can't put devices behind MACsec")
	}
	d := env.Device()
	cs, _ := macsec.ParseCipher(cfg.Cipher)
	val, _ := macsec.ParseValidation(cfg.Validation)
	port := cfg.Scope == "port"
	clear := len(d.VLANs())
	if port {
		clear = 0
	} else if cfg.ClearTags != nil {
		if *cfg.ClearTags > clear {
			return nil, fmt.Errorf("clear_tags %d: device %s has %d VLAN tags", *cfg.ClearTags, d.Name(), clear)
		}
		clear = *cfg.ClearTags
	}
	s := &session{env: env, cfg: cfg, port: port, clear: clear, sci: macsec.MakeSCI(d.MAC(), cfg.PortID)}
	secy, err := macsec.New(macsec.Config{SCI: s.sci, Cipher: cs, IncludeSCI: cfg.IncludeSCI, EndStation: cfg.EndStation,
		SCB: cfg.SCB, Encrypt: cfg.Encrypt, Offset: cfg.Offset, Validate: val,
		ReplayProtect: cfg.ReplayProtect, ReplayWindow: uint64(cfg.ReplayWindow)})
	if err != nil {
		return nil, err
	}
	s.secy = secy
	if st := cfg.Static; st != nil {
		k, err := st.key(cs)
		if err != nil {
			return nil, err
		}
		if err := secy.InstallTx(k, max(st.PN, 1)); err != nil {
			return nil, err
		}
		rk := k
		rk.SSCI = st.PeerSSCI
		if err := secy.Learn(rk, max(st.PN, 1)); err != nil {
			return nil, err
		}
	}
	if m := cfg.MKA; m != nil {
		p, err := m.parse()
		if err != nil {
			return nil, err
		}
		tags := append([]pkt.VLANTag(nil), d.VLANs()[:clear]...)
		s.mka, err = mka.New(mka.Config{CAK: p.cak, CKN: p.ckn, SCI: s.sci, Priority: p.priority, Dst: p.dst,
			Cipher: cs, Encrypt: cfg.Encrypt, Offset: cfg.Offset, PRx: val != macsec.ValidateStrict, Hello: p.hello, Life: 3 * p.hello},
			secy, mka.Hooks{
				Send: func(b []byte) error {
					return env.Send(p.dst, pkt.EtherTypeEAPOL, b, protocol.SendOpts{SetTags: true, Tags: tags})
				},
				Event: func(kind, msg string) { env.Emit(kind, msg, nil) },
			})
		if err != nil {
			return nil, err
		}
	}
	release, err := sec.Secure(s, port)
	if err != nil {
		return nil, err
	}
	s.release = release
	if s.mka != nil {
		s.mka.Start()
	}
	return s, nil
}

type session struct {
	env     protocol.Env
	cfg     *Config
	port    bool
	clear   int
	sci     macsec.SCI
	secy    *macsec.SecY
	mka     *mka.Participant
	release func()
}

// ---- protocol.SecureLink ----

func (s *session) ClearTags() int { return s.clear }

func (s *session) Protect(f *pkt.Frame, send func([]byte) error) error {
	k := min(s.clear, len(f.Tags))
	b := f.EncodeUnpadded()
	h := 12 + 4*k
	err := s.secy.Protect(b[:h], b[h:], send)
	if errors.Is(err, macsec.ErrNoSA) && s.cfg.Policy == "should-secure" {
		return send(f.Encode())
	}
	return err
}

func (s *session) Validate(f *pkt.Frame, b []byte) []byte {
	h := 12 + 4*len(f.Tags)
	msdu, err := s.secy.Validate(b[:h], b[h:])
	if err != nil {
		return nil
	}
	out := make([]byte, h+len(msdu))
	copy(out, b[:h])
	copy(out[h:], msdu)
	return out
}

func (s *session) Unprotected(*pkt.Frame) bool {
	// should-secure takes frames in clear until MKA has a key, as a router's
	// controlled port does while it's unsecured.
	if s.cfg.Policy == "should-secure" && s.mka != nil && !s.mka.Secured() {
		return true
	}
	return s.secy.Untagged()
}

func (s *session) EAPOL(f *pkt.Frame) {
	if s.mka != nil {
		s.mka.Receive(f.Dst, f.Src, f.Payload)
	}
}

// ---- protocol.Session ----

func (s *session) HandleRx(*pkt.Frame) {}

// Status is what the UI and API show.
type Status struct {
	Scope string `json:"scope"`
	// ClearTags: the VLAN tags in front of the SecTAG; Overhead: bytes added
	// to every frame.
	ClearTags int `json:"clear_tags"`
	// The device's VLAN IDs in front of the SecTAG, and inside it (device
	// scope; a port's devices each have their own, all inside).
	ClearVLANs     []uint16 `json:"clear_vlans"`
	EncryptedVLANs []uint16 `json:"encrypted_vlans"`
	Overhead       int      `json:"overhead"`
	// Encrypt: confidentiality (else integrity only), from Offset bytes on.
	Encrypt bool   `json:"encrypt"`
	Offset  int    `json:"confidentiality_offset"`
	Keys    string `json:"keys"`
	Secured bool   `json:"secured"`
	Policy  string `json:"policy"`
	// MKA is the key agreement's state (keys: mka).
	MKA *mka.Status `json:"mka,omitempty"`
	macsec.Status
}

func (s *session) Status() any {
	st := s.secy.Status()
	out := Status{Scope: s.cfg.Scope, ClearTags: s.clear, Overhead: s.secy.Overhead(), Keys: "static",
		Secured: st.EncodingSA != nil, Policy: s.cfg.Policy, Status: st, ClearVLANs: []uint16{}, EncryptedVLANs: []uint16{}}
	c := s.secy.Config()
	out.Encrypt, out.Offset = c.Encrypt, c.Offset
	if !s.port {
		for i, t := range s.env.Device().VLANs() {
			if i < s.clear {
				out.ClearVLANs = append(out.ClearVLANs, t.VID)
			} else {
				out.EncryptedVLANs = append(out.EncryptedVLANs, t.VID)
			}
		}
	}
	if s.mka != nil {
		ms := s.mka.Status()
		out.Keys, out.MKA = "mka", &ms
	}
	return out
}

func (s *session) Health() string {
	if s.mka != nil {
		return s.mka.Status().State
	}
	if s.secy.Status().EncodingSA != nil {
		return "secured"
	}
	return "unsecured"
}

func (s *session) Action(_ context.Context, name string, _ any) (any, error) {
	switch name {
	case "clear":
		s.secy.ResetStats()
		return nil, nil
	case "rekey", "pause-mka", "resume-mka":
		if s.mka == nil {
			return nil, errors.New("static keys: no MKA")
		}
		switch name {
		case "rekey":
			return nil, s.mka.Rekey()
		case "pause-mka":
			s.mka.Pause()
		default:
			s.mka.Resume()
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

func (s *session) Stop() {
	if s.mka != nil {
		s.mka.Stop()
	}
	if s.release != nil {
		s.release()
	}
}
