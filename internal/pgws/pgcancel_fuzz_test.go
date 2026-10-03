package pgws

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func FuzzBackendKeyCaptureChunks(f *testing.F) {
	f.Add([]byte{}, uint32(42), uint32(99), uint8(1))
	f.Add([]byte("row data"), uint32(1234), uint32(5678), uint8(7))
	f.Fuzz(func(t *testing.T, preceding []byte, pid, secret uint32, chunkSize uint8) {
		if len(preceding) > 64<<10 {
			t.Skip()
		}
		wire := []byte{'D', 0, 0, 0, 0}
		binary.BigEndian.PutUint32(wire[1:], uint32(len(preceding)+4))
		wire = append(wire, preceding...)
		wire = append(wire, 'K', 0, 0, 0, 12)
		wire = binary.BigEndian.AppendUint32(wire, pid)
		wire = binary.BigEndian.AppendUint32(wire, secret)

		capture := &backendKeyCapture{}
		step := int(chunkSize%32) + 1
		for offset := 0; offset < len(wire); offset += step {
			capture.feed(wire[offset:min(offset+step, len(wire))])
		}
		gotPID, gotSecret := capture.key()
		var wantSecret [4]byte
		binary.BigEndian.PutUint32(wantSecret[:], secret)
		if gotPID != pid || !bytes.Equal(gotSecret, wantSecret[:]) {
			t.Fatalf("captured pid=%d secret=%x, want pid=%d secret=%x", gotPID, gotSecret, pid, wantSecret)
		}
	})
}
