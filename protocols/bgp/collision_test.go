package bgp

import (
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	gb "github.com/osrg/gobgp/v3/pkg/packet/bgp"
)

// halfConn is a net.Pipe end that can half close like TCP, so a connection
// told to close can still read what the peer already sent.
type halfConn struct{ net.Conn }

func (halfConn) CloseWrite() error { return nil }

type testConn struct {
	c      *conn
	remote net.Conn
	done   chan string
}

// collisionPeer is a speaker with router ID 10.0.0.1, so against the peer's
// 10.0.0.2 the connection the peer opened (inbound here) wins.
func collisionPeer() *peer {
	s := &session{cfg: Config{LocalAS: 65000, HoldTime: 9}, rid: netip.MustParseAddr("10.0.0.1"), byAddr: map[netip.Addr]*peer{}, stop: make(chan struct{})}
	return newPeer(s, Peer{Address: "10.0.0.2", RemoteAS: 65000})
}

func startConn(t *testing.T, p *peer, outbound bool) *testConn {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() { local.Close(); remote.Close() })
	go io.Copy(io.Discard, remote)
	c := &conn{p: p, c: halfConn{local}, outbound: outbound, state: stOpenSent, closed: make(chan struct{}), jobSig: make(chan struct{}, 1)}
	p.s.mu.Lock()
	p.conns[c] = true
	p.s.mu.Unlock()
	tc := &testConn{c: c, remote: remote, done: make(chan string, 1)}
	go func() { tc.done <- c.handshake() }()
	return tc
}

func (tc *testConn) sendOpen(t *testing.T) {
	t.Helper()
	tc.remote.Write(serialize(gb.NewBGPOpenMessage(65000, 9, "10.0.0.2", nil)))
}

func (tc *testConn) sendKeepalive(t *testing.T) {
	t.Helper()
	tc.remote.Write(serialize(gb.NewBGPKeepAliveMessage()))
}

func (tc *testConn) result(t *testing.T) string {
	t.Helper()
	select {
	case r := <-tc.done:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("handshake didn't end")
		return ""
	}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

func (p *peer) inOpenConfirm(c *conn) bool {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	return c.state == stOpenConfirm
}

// A connection that lost the collision while in OpenConfirm still has the
// peer's KEEPALIVE to read. It must not go Established, which would close the
// connection that won.
func TestCollisionLoserStaysDown(t *testing.T) {
	p := collisionPeer()
	loser := startConn(t, p, true)
	loser.sendOpen(t)
	waitFor(t, "loser in OpenConfirm", func() bool { return p.inOpenConfirm(loser.c) })

	winner := startConn(t, p, false)
	winner.sendOpen(t)
	waitFor(t, "loser told to close", func() bool { return loser.c.closedWhy() != "" })

	loser.sendKeepalive(t)
	if r := loser.result(t); r != "connection collision" {
		t.Fatalf("loser ended with %q", r)
	}
	if winner.c.closedWhy() != "" {
		t.Fatalf("the winner was closed: %s", winner.c.closedWhy())
	}
	winner.sendKeepalive(t)
	if r := winner.result(t); r != "" {
		t.Fatalf("winner ended with %q", r)
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if p.est != winner.c || p.ups != 1 {
		t.Fatalf("established on the wrong connection, ups %d", p.ups)
	}
}

// A connection closed because another went Established stays closed, even if
// that session has already ended when the peer's OPEN arrives.
func TestClosedConnStaysDownAfterSessionEnds(t *testing.T) {
	p := collisionPeer()
	first := startConn(t, p, false)
	first.sendOpen(t)
	waitFor(t, "OpenConfirm", func() bool { return p.inOpenConfirm(first.c) })
	second := startConn(t, p, true)
	first.sendKeepalive(t)
	if r := first.result(t); r != "" {
		t.Fatalf("first ended with %q", r)
	}
	waitFor(t, "second told to close", func() bool { return second.c.closedWhy() != "" })

	p.s.mu.Lock()
	delete(p.conns, first.c)
	p.est = nil
	p.s.mu.Unlock()

	second.sendOpen(t)
	second.sendKeepalive(t)
	if r := second.result(t); r == "" {
		t.Fatal("a connection already told to close went Established")
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if p.est != nil || p.ups != 1 {
		t.Fatalf("est %v, ups %d", p.est, p.ups)
	}
}
