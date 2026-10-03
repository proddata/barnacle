package pgws

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestSessionRegistrySendsGoingAwayAndWaits(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	backend, dbPeer := net.Pipe()
	defer dbPeer.Close()
	registry := newSessionRegistry()
	session := &wsSession{client: client, backend: backend, writer: &wsWriter{conn: client}}
	if !registry.add(session) {
		t.Fatal("session was rejected before shutdown")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- registry.shutdown(context.Background()) }()
	var frame [4]byte
	if _, err := io.ReadFull(peer, frame[:]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame[:], []byte{0x88, 2, 0x03, 0xe9}) {
		t.Fatalf("shutdown close frame = %v", frame)
	}
	registry.remove(session)
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if registry.add(session) {
		t.Fatal("session was accepted after shutdown")
	}
}

func TestSessionRegistryForcesCloseAtDeadline(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	backend, dbPeer := net.Pipe()
	defer dbPeer.Close()
	registry := newSessionRegistry()
	session := &wsSession{client: client, backend: backend, writer: &wsWriter{conn: client}}
	registry.add(session)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := registry.shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded", err)
	}
	var one [1]byte
	if _, err := peer.Read(one[:]); err == nil {
		t.Fatal("client connection stayed open after shutdown deadline")
	}
	registry.remove(session)
}
