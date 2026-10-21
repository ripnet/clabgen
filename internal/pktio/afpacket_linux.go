//go:build linux

package pktio

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type afPacket struct {
	f  *os.File
	rc syscall.RawConn
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// Open binds an AF_PACKET socket to the interface, receiving every
// ethertype but not our own transmitted frames.
func Open(ifindex int) (Conn, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	const rcvBuf = 8 << 20
	fail := func(what string, err error) (Conn, error) {
		unix.Close(fd)
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: ifindex}); err != nil {
		return fail("bind", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_AUXDATA, 1); err != nil {
		return fail("PACKET_AUXDATA", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_IGNORE_OUTGOING, 1); err != nil {
		return fail("PACKET_IGNORE_OUTGOING", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1); err != nil {
		return fail("SO_TIMESTAMPNS", err)
	}
	// Bursts of replies to a fast flow overflow the default (~208 KB)
	// receive buffer. FORCE ignores rmem_max but needs CAP_NET_ADMIN.
	for _, opt := range []int{unix.SO_RCVBUFFORCE, unix.SO_RCVBUF} {
		if unix.SetsockoptInt(fd, unix.SOL_SOCKET, opt, rcvBuf) == nil {
			break
		}
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("afpacket:%d", ifindex))
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &afPacket{f: f, rc: rc}, nil
}

var oob = func() int { return unix.CmsgSpace(int(unsafe.Sizeof(unix.TpacketAuxdata{}))) + unix.CmsgSpace(16) }()

func (a *afPacket) ReadFrame(buf []byte) ([]byte, time.Time, error) {
	if len(buf) < 64 {
		return nil, time.Time{}, fmt.Errorf("buffer too small")
	}
	ob := make([]byte, oob)
	var n, oobn int
	var rerr error
	err := a.rc.Read(func(fd uintptr) bool {
		// Leave 4 bytes of headroom for a reinserted VLAN tag.
		n, oobn, _, _, rerr = unix.Recvmsg(int(fd), buf[4:], ob, 0)
		return rerr != unix.EAGAIN
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	if rerr != nil {
		return nil, time.Time{}, rerr
	}
	ts := time.Now()
	var tci, tpid uint16
	var tagged bool
	if cms, err := unix.ParseSocketControlMessage(ob[:oobn]); err == nil {
		for _, cm := range cms {
			switch {
			case cm.Header.Level == unix.SOL_PACKET && cm.Header.Type == unix.PACKET_AUXDATA:
				if len(cm.Data) >= int(unsafe.Sizeof(unix.TpacketAuxdata{})) {
					aux := (*unix.TpacketAuxdata)(unsafe.Pointer(&cm.Data[0]))
					if aux.Status&unix.TP_STATUS_VLAN_VALID != 0 || aux.Vlan_tci != 0 {
						tagged, tci, tpid = true, aux.Vlan_tci, 0x8100
						if aux.Status&unix.TP_STATUS_VLAN_TPID_VALID != 0 {
							tpid = aux.Vlan_tpid
						}
					}
				}
			case cm.Header.Level == unix.SOL_SOCKET && cm.Header.Type == unix.SO_TIMESTAMPNS:
				if len(cm.Data) >= 16 {
					sec := int64(binary.NativeEndian.Uint64(cm.Data[0:8]))
					nsec := int64(binary.NativeEndian.Uint64(cm.Data[8:16]))
					ts = time.Unix(sec, nsec)
				}
			}
		}
	}
	if !tagged || n < 12 {
		return buf[4 : 4+n], ts, nil
	}
	copy(buf[0:12], buf[4:16])
	binary.BigEndian.PutUint16(buf[12:], tpid)
	binary.BigEndian.PutUint16(buf[14:], tci)
	return buf[:n+4], ts, nil
}

func (a *afPacket) WriteFrame(b []byte) error {
	var werr error
	err := a.rc.Write(func(fd uintptr) bool {
		_, werr = unix.Write(int(fd), b)
		return werr != unix.EAGAIN
	})
	if err != nil {
		return err
	}
	return werr
}

func (a *afPacket) Close() error { return a.f.Close() }
