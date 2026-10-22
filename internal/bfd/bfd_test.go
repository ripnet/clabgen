package bfd

import (
	"testing"
	"time"
)

// pump runs two sessions against each other for d, packets crossing
// at once; drop stops b from hearing a.
func pump(a, b *Session, now time.Time, d time.Duration, drop bool) (time.Time, int, int) {
	na, nb := 0, 0
	for end := now.Add(d); now.Before(end); now = now.Add(10 * time.Millisecond) {
		if p, _ := a.Tick(now); p != nil {
			na++
			if !drop {
				x, _ := Decode(p)
				b.Receive(x, now)
			}
		}
		if p, _ := b.Tick(now); p != nil {
			nb++
			x, _ := Decode(p)
			a.Receive(x, now)
		}
	}
	return now, na, nb
}

func TestSession(t *testing.T) {
	cfg := Config{MinTx: 100 * time.Millisecond, MinRx: 100 * time.Millisecond, Mult: 3}
	now := time.Unix(1000, 0)
	a, b := New(cfg, 1, now), New(cfg, 2, now)
	now, _, _ = pump(a, b, now, 5*time.Second, false)
	if a.State() != Up || b.State() != Up {
		t.Fatalf("states %v %v", a.Status(), b.Status())
	}
	// Up: both at 100 ms, detection 300 ms.
	now, na, nb := pump(a, b, now, time.Second, false)
	if na < 8 || nb < 8 || b.detectTime() != 300*time.Millisecond {
		t.Fatalf("a sent %d, b %d in a second; b %v", na, nb, b.Status())
	}
	// a goes silent: b is down within the detection time.
	a.SetSilent(true)
	now, _, _ = pump(a, b, now, 400*time.Millisecond, false)
	if b.State() != Down || b.Status().LastDown != "detection time expired" {
		t.Fatalf("b %v", b.Status())
	}
	a.SetSilent(false)
	pump(a, b, now, 5*time.Second, false)
	if a.State() != Up || b.State() != Up || a.Status().Ups != 2 {
		t.Fatalf("again: %v %v", a.Status(), b.Status())
	}
}

func TestDecode(t *testing.T) {
	p := Packet{State: Up, Mult: 3, MyDisc: 7, YourDisc: 9, MinTx: 100000, MinRx: 200000, Poll: true}
	x, err := Decode(p.Encode())
	if err != nil || *x != p {
		t.Fatalf("%v %+v", err, x)
	}
	bad := p.Encode()
	bad[2] = 0
	if _, err := Decode(bad); err == nil {
		t.Fatal("multiplier 0 accepted")
	}
}

func TestPeerAdminDown(t *testing.T) {
	cfg := Config{MinTx: 100 * time.Millisecond, MinRx: 100 * time.Millisecond, Mult: 3}
	now := time.Unix(1000, 0)
	a, b := New(cfg, 1, now), New(cfg, 2, now)
	now, _, _ = pump(a, b, now, 3*time.Second, false)
	b.Receive(&Packet{State: AdminDown, Mult: 3, MyDisc: 1, YourDisc: 2}, now)
	if b.State() != Down || !b.PeerAdminDown() {
		t.Fatalf("b %v admin %v", b.Status(), b.PeerAdminDown())
	}
}
