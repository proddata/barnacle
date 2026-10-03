package pgws

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

type memoryConn struct{ bytes.Buffer }

func (*memoryConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (*memoryConn) Close() error                     { return nil }
func (*memoryConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*memoryConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*memoryConn) SetDeadline(time.Time) error      { return nil }
func (*memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (*memoryConn) SetWriteDeadline(time.Time) error { return nil }

func maskedBinaryFrame(payload []byte) []byte {
	frame := []byte{0x82}
	switch {
	case len(payload) < 126:
		frame = append(frame, 0x80|byte(len(payload)))
	case len(payload) <= 65535:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(len(payload)))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(len(payload)))
	}
	frame = append(frame, 1, 2, 3, 4)
	for i, b := range payload {
		frame = append(frame, b^byte(i%4+1))
	}
	return frame
}

func FuzzReadWSBinaryFrame(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("postgres wire data"))
	f.Add(bytes.Repeat([]byte{'x'}, 126))
	f.Add(bytes.Repeat([]byte{'x'}, 127))
	f.Add(bytes.Repeat([]byte{'y'}, 65536))
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 128<<10 {
			t.Skip()
		}
		frame := append(maskedBinaryFrame(payload), 0x88, 0x80, 0, 0, 0, 0)
		var backend bytes.Buffer
		client := &memoryConn{}
		err := readWS(bufio.NewReader(bytes.NewReader(frame)), &backend, &wsWriter{conn: client}, nil, 0)
		if err != nil || !bytes.Equal(backend.Bytes(), payload) || !bytes.Equal(client.Bytes(), []byte{0x88, 0}) {
			t.Fatalf("relay error=%v, backend bytes=%d, close=%v", err, backend.Len(), client.Bytes())
		}
	})
}
