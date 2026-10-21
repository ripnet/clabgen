// Package pktio sends and receives raw Ethernet frames on an interface.
package pktio

import "time"

// Conn is a raw frame socket bound to one interface.
type Conn interface {
	// ReadFrame blocks until a frame arrives. buf must be at least
	// MTU+18 bytes; the returned slice aliases buf. VLAN tags stripped by
	// the kernel are reinserted so callers always see the wire format.
	ReadFrame(buf []byte) (frame []byte, rxTime time.Time, err error)
	WriteFrame(b []byte) error
	Close() error
}
