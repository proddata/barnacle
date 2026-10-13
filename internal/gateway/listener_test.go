package gateway

import (
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func testLimitedListener(t *testing.T, limit int) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	limited := LimitListener(listener, limit)
	t.Cleanup(func() { limited.Close() })
	return limited
}

func dialLimitedListener(t *testing.T, listener net.Listener) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Pending TCP connections may reach the OS backlog, but cannot consume a
// server connection until an accepted connection closes. Repeated Close calls
// must not return another slot.
func TestLimitListenerBoundsConnectionsAndReusesSlots(t *testing.T) {
	listener := testLimitedListener(t, 1)
	dialLimitedListener(t, listener)
	first, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	dialLimitedListener(t, listener)
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	accept := func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}
	go accept()
	select {
	case conn := <-accepted:
		conn.Close()
		t.Fatal("accepted more connections than the limit")
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(50 * time.Millisecond):
	}
	first.Close()
	var second net.Conn
	select {
	case second = <-accepted:
		defer second.Close()
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("closing a connection did not release its slot")
	}
	first.Close()
	dialLimitedListener(t, listener)
	go accept()
	select {
	case conn := <-accepted:
		conn.Close()
		t.Fatal("closing a connection twice released an extra slot")
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(50 * time.Millisecond):
	}
	listener.Close()
	select {
	case err := <-acceptErr:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("closing a saturated listener did not wake Accept")
	}
}

// A hijacked connection is no longer owned by net/http. It must retain its
// listener slot until the WebSocket/session owner closes the socket.
func TestLimitListenerCountsHijackedConnections(t *testing.T) {
	listener := testLimitedListener(t, 1)
	hijacked := make(chan net.Conn, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		hijacked <- conn
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	client := dialLimitedListener(t, listener)
	if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	var session net.Conn
	select {
	case session = <-hijacked:
		defer session.Close()
	case <-time.After(time.Second):
		t.Fatal("HTTP connection was not hijacked")
	}
	other := dialLimitedListener(t, listener)
	if _, err := other.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case conn := <-hijacked:
		conn.Close()
		t.Fatal("hijacking released the first connection's slot")
	case <-time.After(50 * time.Millisecond):
	}
	session.Close()
	select {
	case conn := <-hijacked:
		conn.Close()
	case <-time.After(time.Second):
		t.Fatal("closing the hijacked connection did not release its slot")
	}
}

type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }

func TestLimitListenerReturnsSlotOnAcceptError(t *testing.T) {
	listener := LimitListener(failingListener{}, 1)
	for range 2 {
		result := make(chan error, 1)
		go func() { _, err := listener.Accept(); result <- err }()
		select {
		case err := <-result:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("failed Accept leaked a connection slot")
		}
	}
}
