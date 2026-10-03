package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func clientFrame(op byte, fin bool, payload []byte) []byte {
	first := op
	if fin {
		first |= 0x80
	}
	frame := []byte{first, 0x80 | byte(len(payload)), 1, 2, 3, 4}
	for i, b := range payload {
		frame = append(frame, b^byte(i%4+1))
	}
	return frame
}
func TestReadWSStreamsFragmentedBinaryAndPong(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var output bytes.Buffer
	writer := &wsWriter{conn: left}
	frames := bytes.Join([][]byte{clientFrame(2, false, []byte("abc")), clientFrame(9, true, []byte("x")), clientFrame(0, true, []byte("def")), clientFrame(8, true, nil)}, nil)
	done := make(chan error, 1)
	go func() { done <- readWS(bufio.NewReader(bytes.NewReader(frames)), &output, writer, nil, 0) }()
	pong := make([]byte, 3)
	if _, err := io.ReadFull(right, pong); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pong, []byte{0x8a, 1, 'x'}) {
		t.Fatalf("pong %v", pong)
	}
	closeFrame := make([]byte, 2)
	if _, err := io.ReadFull(right, closeFrame); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(closeFrame, []byte{0x88, 0}) {
		t.Fatalf("close %v", closeFrame)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if output.String() != "abcdef" {
		t.Fatalf("got %q", output.String())
	}
}
func TestReadWSRejectsUnmaskedFrame(t *testing.T) {
	frame := []byte{0x82, 1, 'x'}
	if err := readWS(bufio.NewReader(bytes.NewReader(frame)), io.Discard, &wsWriter{}, nil, 0); err == nil {
		t.Fatal("unmasked frame accepted")
	}
}

func TestReadWSStreamsLargeFrame(t *testing.T) {
	payload := bytes.Repeat([]byte{'q'}, 1<<20)
	frame := make([]byte, 0, len(payload)+14)
	frame = append(frame, 0x82, 0xff)
	frame = binary.BigEndian.AppendUint64(frame, uint64(len(payload)))
	frame = append(frame, 1, 2, 3, 4)
	for i, b := range payload {
		frame = append(frame, b^byte(i%4+1))
	}
	frame = append(frame, clientFrame(8, true, nil)...)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- readWS(bufio.NewReader(bytes.NewReader(frame)), &output, &wsWriter{conn: left}, nil, 0)
	}()
	var closeFrame [2]byte
	if _, err := io.ReadFull(right, closeFrame[:]); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), payload) {
		t.Fatal("large frame was not forwarded intact")
	}
}

func TestReadWSIdleTimeout(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	started := time.Now()
	err := readWS(bufio.NewReader(client), io.Discard, &wsWriter{}, client, 50*time.Millisecond)
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("idle read error = %v, want timeout", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("idle session did not time out promptly")
	}
}

func TestWebSocketAndBackendWritesTimeOut(t *testing.T) {
	for _, test := range []struct {
		name  string
		write func(net.Conn) error
	}{
		{"websocket", func(conn net.Conn) error {
			return (&wsWriter{conn: conn, writeTimeout: 50 * time.Millisecond}).frame(2, []byte("blocked"))
		}},
		{"postgres", func(conn net.Conn) error {
			return writeFull(&deadlineWriter{conn: conn, timeout: 50 * time.Millisecond}, []byte("blocked"))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, peer := net.Pipe()
			defer conn.Close()
			defer peer.Close()
			err := test.write(conn)
			if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
				t.Fatalf("stalled write error = %v, want timeout", err)
			}
		})
	}
}

func TestReadWSRejectsInvalidClose(t *testing.T) {
	for _, payload := range [][]byte{{1}, {0x03, 0xed}, {0x03, 0xe8, 0xff}} {
		frame := clientFrame(8, true, payload)
		if err := readWS(bufio.NewReader(bytes.NewReader(frame)), io.Discard, &wsWriter{}, nil, 0); err == nil {
			t.Fatalf("invalid close payload %v accepted", payload)
		}
	}
}
func TestWSHeaderLength(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	payload := bytes.Repeat([]byte{'a'}, 128)
	done := make(chan error, 1)
	go func() { done <- (&wsWriter{conn: left}).frame(2, payload) }()
	header := make([]byte, 4)
	if _, err := io.ReadFull(right, header); err != nil {
		t.Fatal(err)
	}
	if header[0] != 0x82 || header[1] != 126 || binary.BigEndian.Uint16(header[2:]) != 128 {
		t.Fatalf("header %v", header)
	}
	data := make([]byte, 128)
	if _, err := io.ReadFull(right, data); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
