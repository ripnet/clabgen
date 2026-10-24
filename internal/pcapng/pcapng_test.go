package pcapng

import (
	"bytes"
	"testing"
	"time"
)

func TestWriteRead(t *testing.T) {
	var b bytes.Buffer
	w, err := NewWriter(&b, []Interface{{Name: "eth1"}, {Name: "eth2", Snaplen: 128}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	f1 := bytes.Repeat([]byte{1}, 61) // odd length: padded
	f2 := bytes.Repeat([]byte{2}, 100)
	w.WritePacket(0, time.Unix(1, 5), f1, len(f1), Inbound)
	w.WritePacket(1, time.Unix(2, 0), f2, 1500, Outbound)
	w.WritePacket(0, time.Unix(3, 0), f1, len(f1), Unknown)
	fs, err := Read(b.Bytes())
	if err != nil || len(fs) != 3 || !bytes.Equal(fs[0], f1) || !bytes.Equal(fs[1], f2) || !bytes.Equal(fs[2], f1) {
		t.Fatalf("read back %d frames: %v", len(fs), err)
	}
	if b.Len()%4 != 0 {
		t.Errorf("stream length %d not 32-bit aligned", b.Len())
	}
}
