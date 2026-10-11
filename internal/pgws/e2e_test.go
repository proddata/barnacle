package pgws

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/proddata/barnacle/internal/gateway"
)

func fakePG(t testing.TB) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				msg := []byte{'K', 0, 0, 0, 12, 0, 0, 4, 210, 1, 2, 3, 4}
				binary.BigEndian.PutUint32(msg[1:], 12)
				c.Write(msg)
				io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().String()
}

func startGateway(t testing.TB) (*httptest.Server, Handler) {
	cfg := &gateway.Config{
		PGAddr: fakePG(t), PGSSLMode: "disable",
		WSIdleTimeout: time.Minute, WSWriteTimeout: 30 * time.Second,
		UpstreamSlots: make(chan struct{}, 64), CancelSlots: make(chan struct{}, 8),
		Metrics: &gateway.Metrics{},
	}
	h := New(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2", h.Serve)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, h
}

func dial(t testing.TB, srv *httptest.Server) (net.Conn, *websocket.Conn) {
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws"+srv.URL[4:]+"/v2", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(-1)
	return websocket.NetConn(ctx, c, websocket.MessageBinary), c
}

func readKey(t testing.TB, nc net.Conn) {
	var k [13]byte
	if _, err := io.ReadFull(nc, k[:]); err != nil {
		t.Fatal(err)
	}
}

func TestE2EEchoAndShutdown(t *testing.T) {
	srv, h := startGateway(t)
	nc, c := dial(t, srv)
	defer c.CloseNow()
	readKey(t, nc)
	payload := make([]byte, 3<<20)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	go nc.Write(payload)
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(nc, got); err != nil {
		t.Fatal(err)
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("mismatch at %d", i)
		}
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- h.Shutdown(ctx)
	}()
	_, _, err := c.Read(context.Background())
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status %v (%v)", websocket.CloseStatus(err), err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestE2EClientCloseReleasesSlot(t *testing.T) {
	srv, h := startGateway(t)
	nc, c := dial(t, srv)
	readKey(t, nc)
	c.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(3 * time.Second)
	for len(h.UpstreamSlots) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("upstream slot not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func BenchmarkRelayBulk(b *testing.B) {
	srv, _ := startGateway(b)
	nc, c := dial(b, srv)
	defer c.CloseNow()
	readKey(b, nc)
	chunk := make([]byte, 64<<10)
	const total = 32 << 20
	b.SetBytes(2 * total) // up and down
	b.ReportAllocs()
	buf := make([]byte, 64<<10)
	for i := 0; i < b.N; i++ {
		go func() {
			for n := 0; n < total; n += len(chunk) {
				nc.Write(chunk)
			}
		}()
		for n := 0; n < total; {
			m, err := nc.Read(buf)
			if err != nil {
				b.Fatal(err)
			}
			n += m
		}
	}
}

func BenchmarkRelayPingPong(b *testing.B) {
	srv, _ := startGateway(b)
	nc, c := dial(b, srv)
	defer c.CloseNow()
	readKey(b, nc)
	msg := make([]byte, 256)
	buf := make([]byte, 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nc.Write(msg)
		if _, err := io.ReadFull(nc, buf); err != nil {
			b.Fatal(err)
		}
	}
}
