package port

import "github.com/ripnet/clabgen/internal/pkt"

// Info is a snapshot of one port.
type Info struct {
	Name    string
	Index   int
	MAC     pkt.MAC
	AdminUp bool
	OperUp  bool
	MTU     int

	RxFrames, RxBytes, TxFrames, TxBytes, TxErrors, RxUnmatched uint64
}
