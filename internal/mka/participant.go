package mka

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ripnet/clabgen/internal/macsec"
	"github.com/ripnet/clabgen/internal/pkt"
)

// Defaults (802.1X-2010 table 9-3; the retire delay is wpa_supplicant's).
const (
	HelloTime  = 2 * time.Second
	LifeTime   = 6 * time.Second
	RetireTime = 3 * time.Second
	// NotKeyServer is the priority of a participant that won't be key server.
	NotKeyServer = 0xff
)

// GroupAddress is the PAE group address MKPDUs go to by default (nearest
// non-TPMR bridge).
var GroupAddress = pkt.MAC{0x01, 0x80, 0xc2, 0x00, 0x00, 0x03}

// Config is a participant's settings.
type Config struct {
	CAK, CKN []byte
	SCI      macsec.SCI
	Priority uint8
	// Dst is where MKPDUs go (default GroupAddress).
	Dst pkt.MAC
	// What the participant distributes as key server.
	Cipher  macsec.Cipher
	Encrypt bool
	Offset  int
	// PRx: frames without MACsec are accepted (validation isn't strict).
	PRx bool
	// PNThreshold: the key server distributes a new SAK when a transmit PN
	// passes it.
	PNThreshold uint64
	// Hello is the interval between MKPDUs; Life how long a peer lasts
	// without one; Retire how long an old key is kept after a new one is
	// used to send.
	Hello, Life, Retire time.Duration
}

// Hooks connect a participant to its port.
type Hooks struct {
	// Send transmits an MKPDU (the EAPOL frame body).
	Send func(eapol []byte) error
	// Event reports a change worth logging (peer up, key installed).
	Event func(kind, msg string)
}

type peer struct {
	id         PeerID
	sci        macsec.SCI
	priority   uint8
	keyServer  bool
	desired    bool
	capability uint8
	live       bool
	expire     time.Time
	// For the latest key: the peer receives with it (sakUsed), or (the
	// key server) transmits with it.
	sakUsed bool
	sending bool
	llpn    uint32
}

type key struct {
	ki      KI
	an      uint8
	sak     []byte
	tx, rx  bool
	created time.Time
}

// Participant is one MKA participant (actor) on a port, for one CAK.
type Participant struct {
	cfg      Config
	ick, kek []byte
	secy     *macsec.SecY
	hooks    Hooks
	now      func() time.Time

	mu      sync.Mutex
	mi      MI
	mn      uint32
	sent    map[uint32]time.Time // MNs sent within a life time
	peers   map[MI]*peer
	elected bool
	isKS    bool
	ksSCI   macsec.SCI
	ksMI    MI
	latest  *key
	old     *key
	retire  time.Time
	distKN  uint32
	distAN  uint8
	toDist  bool
	newSAK  bool
	paused  bool
	suite   uint64 // the suite in use (as key server: ours)
	stats   Stats
	lastErr string
	stop    chan struct{}
	done    chan struct{}
}

// Stats are the participant's counters.
type Stats struct {
	TxMKPDUs     uint64 `json:"tx_mkpdus"`
	RxMKPDUs     uint64 `json:"rx_mkpdus"`
	RxBadICV     uint64 `json:"rx_bad_icv,omitempty"`
	RxUnknownCKN uint64 `json:"rx_unknown_ckn,omitempty"`
	RxMalformed  uint64 `json:"rx_malformed,omitempty"`
	RxStale      uint64 `json:"rx_stale,omitempty"`
	SAKsMade     uint64 `json:"saks_distributed,omitempty"`
	SAKsReceived uint64 `json:"saks_received,omitempty"`
	Rekeys       uint64 `json:"rekeys,omitempty"`
}

// New makes a participant driving secy. Start begins sending.
func New(cfg Config, secy *macsec.SecY, hooks Hooks) (*Participant, error) {
	if len(cfg.CAK) != 16 && len(cfg.CAK) != 32 {
		return nil, fmt.Errorf("CAK: 16 or 32 bytes, not %d", len(cfg.CAK))
	}
	if len(cfg.CKN) < 1 || len(cfg.CKN) > 32 {
		return nil, fmt.Errorf("CKN: 1 to 32 bytes, not %d", len(cfg.CKN))
	}
	if cfg.Cipher.XPN() {
		return nil, errors.New("MKA with XPN cipher suites isn't supported yet: use static keys")
	}
	if cfg.Hello == 0 {
		cfg.Hello = HelloTime
	}
	if cfg.Life == 0 {
		cfg.Life = LifeTime
	}
	if cfg.Retire == 0 {
		cfg.Retire = RetireTime
	}
	if cfg.Dst.IsZero() {
		cfg.Dst = GroupAddress
	}
	if cfg.PNThreshold == 0 {
		cfg.PNThreshold = 0xc0000000
	}
	ick, err := DeriveICK(cfg.CAK, cfg.CKN)
	if err != nil {
		return nil, err
	}
	kek, err := DeriveKEK(cfg.CAK, cfg.CKN)
	if err != nil {
		return nil, err
	}
	p := &Participant{cfg: cfg, ick: ick, kek: kek, secy: secy, hooks: hooks, now: time.Now,
		sent: map[uint32]time.Time{}, peers: map[MI]*peer{}, distKN: 1}
	p.newMI()
	return p, nil
}

func (p *Participant) newMI() {
	_, _ = rand.Read(p.mi[:])
	p.mn = 0
	p.sent = map[uint32]time.Time{}
}

func (p *Participant) event(kind, format string, a ...any) {
	if p.hooks.Event != nil {
		p.hooks.Event(kind, fmt.Sprintf(format, a...))
	}
}

// Start sends MKPDUs every hello time until Stop.
func (p *Participant) Start() {
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(p.done)
		t := time.NewTicker(p.cfg.Hello)
		defer t.Stop()
		p.Tick()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.Tick()
			}
		}
	}()
}

// Stop ends the participant; its keys are removed from the SecY.
func (p *Participant) Stop() {
	if p.stop != nil {
		close(p.stop)
		<-p.done
	}
	p.mu.Lock()
	p.secy.RemoveAll()
	p.mu.Unlock()
}

// Tick is one hello: peers that went quiet go, a SAK is made if needed,
// and an MKPDU is sent.
func (p *Participant) Tick() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	changed := false
	for mi, pr := range p.peers {
		if now.After(pr.expire) {
			delete(p.peers, mi)
			if pr.live {
				changed = true
				p.secy.RemoveRx(pr.sci)
				p.event("peer-down", "peer %s timed out", pr.sci)
			}
		}
	}
	for mn, at := range p.sent {
		if now.Sub(at) > p.cfg.Life {
			delete(p.sent, mn)
		}
	}
	if changed {
		p.membershipChanged()
	}
	if p.old != nil && !p.retire.IsZero() && now.After(p.retire) {
		p.retireOld()
	}
	if p.isKS && p.latest != nil && p.latest.tx && p.secy.TxNextPN(p.latest.an) > p.cfg.PNThreshold {
		p.newSAK = true
	}
	p.makeSAK()
	if !p.paused {
		p.sendLocked()
	}
}

// membershipChanged re-elects the key server after live peers came or went.
func (p *Participant) membershipChanged() {
	if p.liveCount() == 0 {
		if p.latest != nil || p.old != nil {
			p.event("unsecured", "no live peers: keys removed")
		}
		p.secy.RemoveAll()
		p.latest, p.old, p.elected, p.isKS, p.toDist = nil, nil, false, false, false
		return
	}
	p.elect()
	if p.isKS {
		p.newSAK = true
	}
}

func (p *Participant) liveCount() int {
	n := 0
	for _, pr := range p.peers {
		if pr.live {
			n++
		}
	}
	return n
}

// better reports whether priority a/sci a beats b for key server.
func better(pa uint8, sa macsec.SCI, pb uint8, sb macsec.SCI) bool {
	if pa != pb {
		return pa < pb
	}
	return sa < sb
}

func (p *Participant) elect() {
	var best *peer
	for _, pr := range p.peers {
		if pr.live && pr.keyServer && pr.priority != NotKeyServer && (best == nil || better(pr.priority, pr.sci, best.priority, best.sci)) {
			best = pr
		}
	}
	self := p.cfg.Priority != NotKeyServer && (best == nil || better(p.cfg.Priority, p.cfg.SCI, best.priority, best.sci))
	wasKS, was := p.isKS, p.ksSCI
	switch {
	case self:
		p.elected, p.isKS, p.ksSCI, p.ksMI = true, true, p.cfg.SCI, p.mi
	case best != nil:
		p.elected, p.isKS, p.ksSCI, p.ksMI = true, false, best.sci, best.id.MI
	default:
		p.elected, p.isKS = false, false
	}
	if p.elected && (p.isKS != wasKS || p.ksSCI != was) {
		if p.isKS {
			p.event("key-server", "elected key server")
		} else {
			p.event("key-server", "key server is %s", p.ksSCI)
		}
	}
}

// makeSAK distributes a new SAK when the key server needs one.
func (p *Participant) makeSAK() {
	if !p.isKS || !p.newSAK || p.liveCount() == 0 {
		return
	}
	p.newSAK = false
	size := p.cfg.Cipher.KeyLen()
	ctx := make([]byte, size, size+miLen*(len(p.peers)+1)+4)
	_, _ = rand.Read(ctx)
	for _, pr := range p.livePeers() {
		ctx = append(ctx, pr.id.MI[:]...)
	}
	ctx = append(ctx, p.mi[:]...)
	ctx = binary.BigEndian.AppendUint32(ctx, p.distKN)
	sak, err := DeriveSAK(p.cfg.CAK, ctx, size)
	if err != nil {
		p.lastErr = err.Error()
		return
	}
	k := &key{ki: KI{MI: p.mi, KN: p.distKN}, an: p.distAN, sak: sak, created: p.now()}
	p.distKN++
	p.distAN = (p.distAN + 1) & 3
	if p.latest != nil {
		p.stats.Rekeys++
	}
	p.stats.SAKsMade++
	p.suite = p.cfg.Cipher.SuiteID()
	p.secy.SetConfidentiality(p.cfg.Encrypt, p.cfg.Offset)
	p.useKey(k)
	p.toDist = true
	p.event("sak", "distributing SAK %d (AN %d)", k.ki.KN, k.an)
}

// useKey makes k the latest key: receive SAs for every live peer, and the
// previous latest key becomes the old one.
func (p *Participant) useKey(k *key) {
	if p.old != nil && p.old.an != k.an {
		p.retireOld()
	}
	if p.latest != nil {
		p.old = p.latest
	}
	p.latest = k
	for _, pr := range p.peers {
		pr.sakUsed, pr.sending = false, false
		if pr.live {
			p.installRx(pr.sci)
		}
	}
	k.rx = true
}

func (p *Participant) sakKey(k *key) macsec.Key { return macsec.Key{AN: k.an, SAK: k.sak} }

// installRx gives a live peer's SC the latest (and old) key.
func (p *Participant) installRx(sci macsec.SCI) {
	for _, k := range []*key{p.old, p.latest} {
		if k != nil {
			if err := p.secy.InstallRx(sci, p.sakKey(k), 1); err != nil {
				p.lastErr = err.Error()
			}
		}
	}
}

// startTx sends with the latest key from PN 1; the old key retires later.
func (p *Participant) startTx() {
	k := p.latest
	if k == nil || k.tx {
		return
	}
	if err := p.secy.InstallTx(p.sakKey(k), 1); err != nil {
		p.lastErr = err.Error()
		return
	}
	k.tx = true
	if p.old != nil {
		if p.old.an != k.an {
			p.secy.RemoveTxAN(p.old.an)
		}
		p.old.tx = false
		p.retire = p.now().Add(p.cfg.Retire)
	}
	p.event("secured", "sending with SAK %d (AN %d)", k.ki.KN, k.an)
}

func (p *Participant) retireOld() {
	if p.old == nil {
		return
	}
	if p.latest == nil || p.latest.an != p.old.an {
		p.secy.RemoveRxAN(p.old.an)
		if p.latest == nil || !p.latest.tx {
			p.secy.RemoveTxAN(p.old.an)
		}
	}
	p.old, p.retire = nil, time.Time{}
}

func (p *Participant) livePeers() []*peer {
	var l []*peer
	for _, pr := range p.peers {
		if pr.live {
			l = append(l, pr)
		}
	}
	sort.Slice(l, func(i, j int) bool { return l[i].sci < l[j].sci })
	return l
}

// Receive handles an MKPDU (the frame after the EAPOL ethertype).
func (p *Participant) Receive(dst, src pkt.MAC, eapol []byte) {
	ckn, err := PeekCKN(eapol)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if !errors.Is(err, ErrNotMKA) {
			p.stats.RxMalformed++
		}
		return
	}
	if !bytes.Equal(ckn, p.cfg.CKN) {
		p.stats.RxUnknownCKN++
		return
	}
	m, err := Decode(dst, src, eapol, p.ick)
	if err != nil {
		if errors.Is(err, ErrICV) {
			p.stats.RxBadICV++
		} else {
			p.stats.RxMalformed++
		}
		return
	}
	p.stats.RxMKPDUs++
	if p.paused {
		return
	}
	if m.MI == p.mi {
		if m.SCI != p.cfg.SCI {
			// Another participant has our MI: take a new one.
			p.newMI()
		}
		return
	}
	now := p.now()
	pr := p.peers[m.MI]
	if pr == nil {
		for _, o := range p.peers {
			if o.sci == m.SCI {
				// The peer restarted with a new MI: let the old one go
				// first (as wpa_supplicant does).
				if lim := now.Add(p.cfg.Hello * 3 / 2); o.expire.After(lim) {
					o.expire = lim
				}
				return
			}
		}
		pr = &peer{id: PeerID{MI: m.MI}}
		p.peers[m.MI] = pr
	} else if m.MN <= pr.id.MN {
		p.stats.RxStale++
		return
	}
	pr.id.MN, pr.sci, pr.priority, pr.keyServer, pr.desired, pr.capability = m.MN, m.SCI, m.Priority, m.KeyServer, m.Desired, m.Capability
	// Liveness is mutual: a live peer stays live only while its MKPDUs list
	// us. One that lost us (our MKPDUs stopped reaching it) times out here
	// too, so both ends start again together when it hears us.
	lists := p.listsUs(m)
	if !pr.live || lists {
		pr.expire = now.Add(p.cfg.Life)
	}
	send := false
	if !pr.live && lists {
		pr.live = true
		p.event("peer-up", "peer %s is live", pr.sci)
		p.installRx(pr.sci)
		p.membershipChanged()
		send = true
	}
	if pr.live {
		if m.DistSAK != nil && p.takeSAK(pr, m.DistSAK) {
			send = true
		}
		if m.SAKUse != nil && p.sakUse(pr, m.SAKUse) {
			send = true
		}
	}
	p.makeSAK()
	if send || p.toDist {
		p.sendLocked()
	}
}

// listsUs reports whether an MKPDU lists our MI with a recent MN.
func (p *Participant) listsUs(m *MKPDU) bool {
	for _, l := range [][]PeerID{m.Live, m.Potential} {
		for _, id := range l {
			if id.MI == p.mi {
				_, recent := p.sent[id.MN]
				return recent
			}
		}
	}
	return false
}

// takeSAK installs a SAK distributed by the key server.
func (p *Participant) takeSAK(pr *peer, d *DistSAK) bool {
	if p.isKS || !p.elected || pr.sci != p.ksSCI || d.Empty {
		return false
	}
	ki := KI{MI: pr.id.MI, KN: d.KN}
	if p.latest != nil && p.latest.ki == ki {
		return false
	}
	suite := d.Suite
	if suite == 0 || suite == 0x0080020001000001 { // the default; wpa_supplicant's misspelling of it
		suite = macsec.GCMAES128.SuiteID()
	}
	if suite != p.cfg.Cipher.SuiteID() {
		p.lastErr = fmt.Sprintf("key server distributes cipher suite %016x, we use %s", d.Suite, p.cfg.Cipher)
		return false
	}
	sak, err := unwrap(p.kek, d.Wrapped)
	if err != nil {
		p.lastErr = "distributed SAK: " + err.Error()
		return false
	}
	if len(sak) != p.cfg.Cipher.KeyLen() {
		p.lastErr = fmt.Sprintf("distributed SAK of %d bytes for %s", len(sak), p.cfg.Cipher)
		return false
	}
	switch d.Offset {
	case OffsetNone:
		p.secy.SetConfidentiality(false, 0)
	case Offset0:
		p.secy.SetConfidentiality(true, 0)
	case Offset30:
		p.secy.SetConfidentiality(true, 30)
	case Offset50:
		p.secy.SetConfidentiality(true, 50)
	}
	p.stats.SAKsReceived++
	if p.latest != nil {
		p.stats.Rekeys++
	}
	p.suite = suite
	p.useKey(&key{ki: ki, an: d.AN, sak: sak, created: p.now()})
	p.event("sak", "received SAK %d (AN %d) from %s", d.KN, d.AN, pr.sci)
	return true
}

// sakUse handles a peer's SAK Use: the key server starts sending once every
// live peer receives with the latest key; the others once the key server
// sends with it.
func (p *Participant) sakUse(pr *peer, u *SAKUse) bool {
	if u.Empty || p.latest == nil {
		return false
	}
	pr.llpn = u.LLPN
	if u.LRx && u.Latest == p.latest.ki {
		pr.sakUsed = true
		pr.sending = u.LTx
	}
	if p.isKS {
		if u.LLPN > uint32(min(p.cfg.PNThreshold, 0xffffffff)) {
			p.newSAK = true
		}
		if p.latest.tx {
			return false
		}
		for _, o := range p.peers {
			if o.live && !o.sakUsed {
				return false
			}
		}
		p.toDist = false
		p.startTx()
		return true
	}
	if pr.sci == p.ksSCI && pr.sending && !p.latest.tx {
		p.startTx()
		return true
	}
	return false
}

func (p *Participant) sendLocked() {
	m := &MKPDU{Version: Version, Priority: p.cfg.Priority, Desired: true, Capability: CapConfidentialityOffset,
		SCI: p.cfg.SCI, MI: p.mi, CKN: p.cfg.CKN}
	if p.elected {
		m.KeyServer = p.isKS
	} else {
		m.KeyServer = p.cfg.Priority != NotKeyServer
	}
	ids := make([]*peer, 0, len(p.peers))
	for _, pr := range p.peers {
		ids = append(ids, pr)
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i].id.MI[:], ids[j].id.MI[:]) < 0 })
	for _, pr := range ids {
		if pr.live {
			m.Live = append(m.Live, pr.id)
		} else {
			m.Potential = append(m.Potential, pr.id)
		}
	}
	if k := p.latest; k != nil {
		u := &SAKUse{LAN: k.an, LTx: k.tx, LRx: k.rx, Latest: k.ki, LLPN: p.lpn(k), PRx: p.cfg.PRx}
		if o := p.old; o != nil {
			u.OAN, u.OTx, u.ORx, u.Old, u.OLPN = o.an, o.tx, o.rx, o.ki, p.lpn(o)
		}
		m.SAKUse = u
		if p.isKS && p.toDist {
			wrapped, err := wrap(p.kek, k.sak)
			if err == nil {
				d := &DistSAK{AN: k.an, Offset: offsetCode(p.cfg.Encrypt, p.cfg.Offset), KN: k.ki.KN, Wrapped: wrapped}
				if p.cfg.Cipher != macsec.GCMAES128 {
					d.Suite = p.cfg.Cipher.SuiteID()
				}
				m.DistSAK = d
			}
		}
	}
	p.mn++
	m.MN = p.mn
	p.sent[p.mn] = p.now()
	b, err := m.Encode(p.cfg.Dst, p.cfg.SCI.MAC(), p.ick)
	if err != nil {
		p.lastErr = err.Error()
		return
	}
	if err := p.hooks.Send(b); err == nil {
		p.stats.TxMKPDUs++
	}
}

// lpn is the lowest PN a receiver should accept from us for a key: the last
// one we sent.
func (p *Participant) lpn(k *key) uint32 {
	if !k.tx {
		return 1
	}
	if n := p.secy.TxNextPN(k.an); n > 1 {
		return uint32(min(n-1, 0xffffffff))
	}
	return 1
}

func offsetCode(encrypt bool, offset int) uint8 {
	switch {
	case !encrypt:
		return OffsetNone
	case offset == 30:
		return Offset30
	case offset == 50:
		return Offset50
	}
	return Offset0
}

// Rekey makes the key server distribute a new SAK now. It fails on a
// participant that isn't the key server.
func (p *Participant) Rekey() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.isKS {
		return errors.New("only the key server distributes SAKs")
	}
	p.newSAK = true
	p.makeSAK()
	p.sendLocked()
	return nil
}

// Pause stops sending (and listening to) MKPDUs, so peers time out; Resume
// starts again with a new MI, as a restarted participant would.
func (p *Participant) Pause() {
	p.mu.Lock()
	p.paused = true
	p.mu.Unlock()
}

func (p *Participant) Resume() {
	p.mu.Lock()
	if p.paused {
		p.paused = false
		p.newMI()
		p.peers = map[MI]*peer{}
		p.membershipChanged()
		p.sendLocked()
	}
	p.mu.Unlock()
}

// PeerStatus is a peer as reported.
type PeerStatus struct {
	SCI       macsec.SCI `json:"sci"`
	MI        string     `json:"mi"`
	MN        uint32     `json:"mn"`
	Live      bool       `json:"live"`
	Priority  uint8      `json:"priority"`
	KeyServer bool       `json:"key_server"`
	Receiving bool       `json:"receiving_latest"`
	Sending   bool       `json:"sending_latest"`
	ExpiresIn float64    `json:"expires_in_s"`
}

// KeyStatus is a key as reported.
type KeyStatus struct {
	KN      uint32    `json:"kn"`
	AN      uint8     `json:"an"`
	Server  string    `json:"key_server_mi"`
	Tx      bool      `json:"tx"`
	Rx      bool      `json:"rx"`
	Created time.Time `json:"created"`
}

// Status is a snapshot.
type Status struct {
	State        string       `json:"state"`
	MI           string       `json:"mi"`
	MN           uint32       `json:"mn"`
	Priority     uint8        `json:"priority"`
	KeyServer    bool         `json:"key_server"`
	KeyServerSCI *macsec.SCI  `json:"key_server_sci,omitempty"`
	CipherSuite  string       `json:"cipher_suite,omitempty"`
	Latest       *KeyStatus   `json:"latest_key,omitempty"`
	Old          *KeyStatus   `json:"old_key,omitempty"`
	Peers        []PeerStatus `json:"peers"`
	Paused       bool         `json:"paused,omitempty"`
	LastError    string       `json:"last_error,omitempty"`
	Stats
}

func (p *Participant) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Status{MI: fmt.Sprintf("%x", p.mi), MN: p.mn, Priority: p.cfg.Priority, KeyServer: p.isKS, Peers: []PeerStatus{},
		Paused: p.paused, LastError: p.lastErr, Stats: p.stats}
	if p.elected {
		s := p.ksSCI
		st.KeyServerSCI = &s
	}
	ks := func(k *key) *KeyStatus {
		if k == nil {
			return nil
		}
		return &KeyStatus{KN: k.ki.KN, AN: k.an, Server: fmt.Sprintf("%x", k.ki.MI), Tx: k.tx, Rx: k.rx, Created: k.created}
	}
	st.Latest, st.Old = ks(p.latest), ks(p.old)
	if p.latest != nil {
		st.CipherSuite = p.cfg.Cipher.String()
	}
	now := p.now()
	for _, pr := range p.peers {
		st.Peers = append(st.Peers, PeerStatus{SCI: pr.sci, MI: fmt.Sprintf("%x", pr.id.MI), MN: pr.id.MN, Live: pr.live, Priority: pr.priority,
			KeyServer: pr.elected(p), Receiving: pr.sakUsed, Sending: pr.sending, ExpiresIn: pr.expire.Sub(now).Seconds()})
	}
	sort.Slice(st.Peers, func(i, j int) bool { return st.Peers[i].SCI < st.Peers[j].SCI })
	switch {
	case p.paused:
		st.State = "paused"
	case p.latest != nil && p.latest.tx:
		st.State = "secured"
	case p.liveCount() > 0:
		st.State = "keying"
	case len(p.peers) > 0:
		st.State = "potential peers"
	default:
		st.State = "no peers"
	}
	return st
}

func (pr *peer) elected(p *Participant) bool { return p.elected && !p.isKS && pr.sci == p.ksSCI }

// Secured reports whether the participant sends with a key from MKA.
func (p *Participant) Secured() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latest != nil && p.latest.tx
}
