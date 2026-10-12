package pgws

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/proddata/barnacle/internal/gateway"
)

// fakePG plays a backend: it sends script as soon as a client connects, then
// echoes what it receives. The default script is an MD5 login, as PostgreSQL
// would send, because the gateway inspects the login.
func fakePG(t testing.TB, script []byte) string {
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
				c.Write(script)
				io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().String()
}

func startGateway(t testing.TB) (*httptest.Server, Handler) {
	return startGatewayWith(t, md5Login, false)
}

func startGatewayWith(t testing.TB, script []byte, allowTrust bool) (*httptest.Server, Handler) {
	cfg := &gateway.Config{
		PGAddr: fakePG(t, script), PGSSLMode: "disable", AllowTrustLogin: allowTrust,
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

func readLogin(t testing.TB, nc net.Conn, script []byte) {
	got := make([]byte, len(script))
	if _, err := io.ReadFull(nc, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, script) {
		t.Fatalf("login bytes changed: %x", got)
	}
}

func TestE2EEchoAndShutdown(t *testing.T) {
	srv, h := startGateway(t)
	nc, c := dial(t, srv)
	defer c.CloseNow()
	readLogin(t, nc, md5Login)
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
	readLogin(t, nc, md5Login)
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
	readLogin(b, nc, md5Login)
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
	readLogin(b, nc, md5Login)
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

func TestE2ELoginMethodsPassThrough(t *testing.T) {
	for name, script := range map[string][]byte{
		"cleartext": cleartextLogin, "md5": md5Login, "scram": scramLogin, "oauthbearer": oauthLogin,
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := startGatewayWith(t, script, false)
			nc, c := dial(t, srv)
			defer c.CloseNow()
			readLogin(t, nc, script)
			// After login the relay carries arbitrary traffic unchanged.
			payload := []byte("select 1")
			nc.Write(payload)
			echo := make([]byte, len(payload))
			if _, err := io.ReadFull(nc, echo); err != nil || !bytes.Equal(echo, payload) {
				t.Fatalf("echo %q, %v", echo, err)
			}
		})
	}
}

func TestE2ETrustLoginIsClosedBeforeClientSeesAuthenticationOk(t *testing.T) {
	for name, script := range map[string][]byte{
		"trust":          trustLogin,
		"desynchronized": concat([]byte{'N'}, trustLogin),
		"after notice":   concat(pgMessage('N', 0), trustLogin),
	} {
		t.Run(name, func(t *testing.T) {
			srv, h := startGatewayWith(t, script, false)
			nc, c := dial(t, srv)
			defer c.CloseNow()
			data, err := io.ReadAll(nc)
			if bytes.Contains(data, authOK) || bytes.Contains(data, readyForQuery) {
				t.Fatalf("client received login data: %x", data)
			}
			if err != nil {
				t.Logf("read ended with %v", err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for len(h.UpstreamSlots) != 0 {
				if time.Now().After(deadline) {
					t.Fatal("upstream slot not released")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestE2ETrustLoginReportsPolicyViolation(t *testing.T) {
	srv, _ := startGatewayWith(t, trustLogin, false)
	c, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:]+"/v2", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_, _, err = c.Read(context.Background())
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close status %v (%v)", websocket.CloseStatus(err), err)
	}
}

func TestE2ETrustLoginAllowedWhenConfigured(t *testing.T) {
	srv, _ := startGatewayWith(t, trustLogin, true)
	nc, c := dial(t, srv)
	defer c.CloseNow()
	readLogin(t, nc, trustLogin)
}
