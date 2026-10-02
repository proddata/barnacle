package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
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
	go func() { done <- readWS(bufio.NewReader(bytes.NewReader(frames)), &output, writer) }()
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
	if err := readWS(bufio.NewReader(bytes.NewReader(frame)), io.Discard, &wsWriter{}); err == nil {
		t.Fatal("unmasked frame accepted")
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
