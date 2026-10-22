package isis

import (
	"encoding/binary"
)

// Hello TLVs for graceful restart and BFD.

// Restart TLV flags (RFC 8706 3.1).
const (
	restartRR byte = 0x01 // restart request
	restartRA byte = 0x02 // restart acknowledgement
	restartSA byte = 0x04 // suppress adjacency advertisement
	restartPR byte = 0x08 // restart is planned
	restartPA byte = 0x10 // planned restart acknowledgement
)

// Restart is a decoded restart TLV.
type Restart struct {
	Flags     byte
	Remaining uint16 // with RA: the adjacency's remaining hold time
	Neighbor  SysID  // with RA: the restarting neighbor (when present)
	HasNbr    bool
}

func (r Restart) TLV() TLV {
	v := []byte{r.Flags}
	if r.Flags&restartRA != 0 {
		v = binary.BigEndian.AppendUint16(v, r.Remaining)
		if r.HasNbr {
			v = append(v, r.Neighbor[:]...)
		}
	}
	return TLV{Type: TLVRestart, Value: v}
}

// parseRestart returns a hello's restart TLV, if any.
func parseRestart(ts TLVs) (Restart, bool) {
	v, ok := ts.First(TLVRestart)
	if !ok || len(v) < 1 {
		return Restart{}, false
	}
	r := Restart{Flags: v[0]}
	if len(v) >= 3 {
		r.Remaining = binary.BigEndian.Uint16(v[1:])
	}
	if len(v) >= 9 {
		r.Neighbor, r.HasNbr = SysID(v[3:9]), true
	}
	return r, true
}

// BFDFamily is one BFD-enabled TLV entry: a topology and an address family.
type BFDFamily struct {
	MT    uint16
	NLPID byte
}

func bfdEnabledTLV(fs []BFDFamily) TLV {
	var v []byte
	for _, f := range fs {
		v = binary.BigEndian.AppendUint16(v, f.MT&0x0fff)
		v = append(v, f.NLPID)
	}
	return TLV{Type: TLVBFDEnabled, Value: v}
}

// parseBFDEnabled returns a hello's BFD-enabled entries (nil: no TLV 148).
func parseBFDEnabled(ts TLVs) []BFDFamily {
	var out []BFDFamily
	for _, v := range ts.All(TLVBFDEnabled) {
		for ; len(v) >= 3; v = v[3:] {
			out = append(out, BFDFamily{MT: binary.BigEndian.Uint16(v) & 0x0fff, NLPID: v[2]})
		}
	}
	return out
}
