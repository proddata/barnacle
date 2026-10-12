package pgws

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

func loginFailureCount(t *testing.T, h Handler, transport string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Metrics.Serve(rec, nil)
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, `barnacle_login_failures_total{transport="`+transport+`"}`) {
			return strings.TrimPrefix(line, `barnacle_login_failures_total{transport="`+transport+`"} `)
		}
	}
	t.Fatal("login failure metric missing")
	return ""
}

func TestE2EFailedLoginIsCounted(t *testing.T) {
	failures := map[string][]byte{
		"wrong password":       concat(authRequest(10, []byte("SCRAM-SHA-256\x00\x00")...), errorResponse("28P01", "password authentication failed")),
		"unknown database":     errorResponse("3D000", "database does not exist"),
		"successful md5 login": md5Login,
	}
	for name, script := range failures {
		t.Run(name, func(t *testing.T) {
			srv, h := startGatewayWith(t, script, false)
			nc, c := dial(t, srv)
			defer c.CloseNow()
			// The client receives the server's reply unchanged.
			got := make([]byte, len(script))
			if _, err := io.ReadFull(nc, got); err != nil || !bytes.Equal(got, script) {
				t.Fatalf("client received %x, %v", got, err)
			}
			want := "0"
			if name == "wrong password" {
				want = "1"
			}
			if count := loginFailureCount(t, h, "websocket"); count != want {
				t.Fatalf("websocket login failures = %s, want %s", count, want)
			}
		})
	}
}

func TestE2EFailedLoginCountedOncePerSession(t *testing.T) {
	script := concat(errorResponse("28P01", "first"), errorResponse("28P01", "second"))
	srv, h := startGatewayWith(t, script, false)
	nc, c := dial(t, srv)
	defer c.CloseNow()
	got := make([]byte, len(script))
	if _, err := io.ReadFull(nc, got); err != nil {
		t.Fatal(err)
	}
	if count := loginFailureCount(t, h, "websocket"); count != "1" {
		t.Fatalf("websocket login failures = %s, want 1", count)
	}
}

// startGatewayWithBackend serves /v2 in front of a fake PostgreSQL listener
// whose behavior each test supplies through handle. Unlike fakePG, which always
// echoes, this lets a test end the backend session in a chosen way.
func startGatewayWithBackend(t testing.TB, handle func(net.Conn)) *httptest.Server {
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
			go handle(c)
		}
	}()
	cfg := &gateway.Config{
		PGAddr: l.Addr().String(), PGSSLMode: "disable",
		WSIdleTimeout: time.Minute, WSWriteTimeout: 30 * time.Second,
		UpstreamSlots: make(chan struct{}, 4), CancelSlots: make(chan struct{}, 4),
		Metrics: &gateway.Metrics{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2", New(cfg).Serve)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// closeStatus reads from the client until the WebSocket ends and returns the
// close code and reason it received. A code of -1 means the connection dropped
// without a close frame, which clients treat as an abnormal closure (1006).
func closeStatus(t testing.TB, c *websocket.Conn) (websocket.StatusCode, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) {
				return closeErr.Code, closeErr.Reason
			}
			return -1, err.Error()
		}
	}
}

// What it tests: PostgreSQL ends the session cleanly (it closes the TCP
// connection after the login bytes, as it does after a client Terminate).
// Expected: Barnacle sends a WebSocket close frame with code 1000 rather than
// dropping the connection. A proxy relaying this session, such as the one in
// front of the PostgreSQL editor, can then tell a finished session from a
// failed one instead of seeing an abnormal closure.
func TestE2EPostgresCloseIsReportedAsNormalClosure(t *testing.T) {
	srv := startGatewayWithBackend(t, func(c net.Conn) {
		c.Write(md5Login)
		time.Sleep(50 * time.Millisecond)
		c.Close()
	})
	_, c := dial(t, srv)
	defer c.CloseNow()
	if code, reason := closeStatus(t, c); code != websocket.StatusNormalClosure {
		t.Fatalf("close = %d %q, want 1000", code, reason)
	}
}

// What it tests: PostgreSQL's connection dies abruptly (a TCP reset, as when the
// backend is killed). Expected: close code 1011 with the fixed reason
// "upstream connection lost", so clients see a failure, and no network error
// text from the upstream reaches them.
func TestE2EPostgresConnectionResetIsReportedAsInternalError(t *testing.T) {
	srv := startGatewayWithBackend(t, func(c net.Conn) {
		c.Write(md5Login)
		time.Sleep(50 * time.Millisecond)
		c.(*net.TCPConn).SetLinger(0) // Closing now sends a reset.
		c.Close()
	})
	_, c := dial(t, srv)
	defer c.CloseNow()
	if code, reason := closeStatus(t, c); code != websocket.StatusInternalError || reason != "upstream connection lost" {
		t.Fatalf("close = %d %q, want 1011 without detail", code, reason)
	}
}

// What it tests: a non-browser client, like the PostgreSQL editor's server-side
// relay, connects with no Origin header and sends HTTP Basic credentials for
// the ingress in front of Barnacle. Expected: the upgrade is accepted, the
// credentials are not forwarded to PostgreSQL (the login bytes and the echo
// come back unchanged), and the session works normally. Barnacle must keep
// ignoring that header, since PostgreSQL does the authenticating.
func TestE2ENonBrowserClientWithBasicAuth(t *testing.T) {
	srv, _ := startGateway(t)
	ctx := context.Background()
	header := http.Header{"Authorization": {"Basic YWRtaW46c2VjcmV0"}}
	c, _, err := websocket.Dial(ctx, "ws"+srv.URL[4:]+"/v2", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(-1)
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	defer c.CloseNow()
	readLogin(t, nc, md5Login)
	nc.Write([]byte("ping"))
	got := make([]byte, 4)
	if _, err := io.ReadFull(nc, got); err != nil || string(got) != "ping" {
		t.Fatalf("echo = %q, %v", got, err)
	}
}
