//go:build !linux

package pktio

import "errors"

func Open(ifindex int) (Conn, error) {
	return nil, errors.New("raw sockets are only supported on linux")
}
