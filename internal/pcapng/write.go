package pcapng

import (
	"encoding/binary"
	"io"
	"time"
)

// Writer writes a pcapng stream: a section header, one interface per
// port, then packets. Wireshark reads it live (wireshark -k -i -).
type Writer struct {
	w   io.Writer
	buf []byte
}

// Interface describes one of the stream's interfaces (a port).
type Interface struct {
	Name    string
	Snaplen int
}

// Directions for WritePacket (the epb_flags option).
const (
	Unknown  = 0
	Inbound  = 1
	Outbound = 2
)

// NewWriter writes the section header and the interfaces; packets then
// refer to an interface by its index.
func NewWriter(w io.Writer, ifs []Interface, app string) (*Writer, error) {
	pw := &Writer{w: w}
	shb := binary.LittleEndian.AppendUint32(nil, 0x1a2b3c4d) // byte-order magic
	shb = binary.LittleEndian.AppendUint16(shb, 1)           // version 1.0
	shb = binary.LittleEndian.AppendUint16(shb, 0)
	shb = binary.LittleEndian.AppendUint64(shb, ^uint64(0)) // section length unknown
	shb = option(shb, 4, []byte(app))                       // shb_userappl
	shb = endOptions(shb)
	pw.block(0x0a0d0d0a, shb)
	for _, i := range ifs {
		snap := i.Snaplen
		if snap <= 0 {
			snap = 262144
		}
		idb := binary.LittleEndian.AppendUint16(nil, 1) // LINKTYPE_ETHERNET
		idb = binary.LittleEndian.AppendUint16(idb, 0)
		idb = binary.LittleEndian.AppendUint32(idb, uint32(snap))
		idb = option(idb, 2, []byte(i.Name)) // if_name
		idb = option(idb, 9, []byte{9})      // if_tsresol: nanoseconds
		idb = endOptions(idb)
		pw.block(1, idb)
	}
	return pw, pw.flush()
}

// WritePacket writes one frame (data, possibly cut short of origLen).
func (pw *Writer) WritePacket(iface int, ts time.Time, data []byte, origLen int, dir int) error {
	ns := uint64(ts.UnixNano())
	epb := binary.LittleEndian.AppendUint32(nil, uint32(iface))
	epb = binary.LittleEndian.AppendUint32(epb, uint32(ns>>32))
	epb = binary.LittleEndian.AppendUint32(epb, uint32(ns))
	epb = binary.LittleEndian.AppendUint32(epb, uint32(len(data)))
	epb = binary.LittleEndian.AppendUint32(epb, uint32(max(origLen, len(data))))
	epb = append(epb, data...)
	epb = pad(epb)
	if dir != Unknown {
		epb = option(epb, 2, binary.LittleEndian.AppendUint32(nil, uint32(dir))) // epb_flags
		epb = endOptions(epb)
	}
	pw.block(6, epb)
	return pw.flush()
}

func (pw *Writer) block(typ uint32, body []byte) {
	n := uint32(12 + len(body))
	pw.buf = binary.LittleEndian.AppendUint32(pw.buf, typ)
	pw.buf = binary.LittleEndian.AppendUint32(pw.buf, n)
	pw.buf = append(pw.buf, body...)
	pw.buf = binary.LittleEndian.AppendUint32(pw.buf, n)
}

func (pw *Writer) flush() error {
	_, err := pw.w.Write(pw.buf)
	pw.buf = pw.buf[:0]
	if f, ok := pw.w.(interface{ Flush() }); ok {
		f.Flush()
	}
	return err
}

func pad(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func option(b []byte, code uint16, v []byte) []byte {
	b = binary.LittleEndian.AppendUint16(b, code)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(v)))
	return pad(append(b, v...))
}

func endOptions(b []byte) []byte { return append(b, 0, 0, 0, 0) }
