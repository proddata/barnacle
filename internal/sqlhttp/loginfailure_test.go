package sqlhttp

import (
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/proddata/barnacle/internal/gateway"
)

// refusingBackend answers every connection with an error at the given stage:
// after the password was sent (a failed login), or straight after startup.
func refusingBackend(t *testing.T, code string, afterPassword bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				backend := pgproto3.NewBackend(conn, conn)
				if _, err := backend.ReceiveStartupMessage(); err != nil {
					return
				}
				if afterPassword {
					backend.Send(&pgproto3.AuthenticationCleartextPassword{})
					if backend.Flush() != nil {
						return
					}
					if _, err := backend.Receive(); err != nil {
						return
					}
				}
				backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: code, Message: "refused"})
				_ = backend.Flush()
			}()
		}
	}()
	return listener.Addr().String()
}

func scrapeCounter(cfg *gateway.Config, line string) bool {
	rec := httptest.NewRecorder()
	cfg.Metrics.Serve(rec, nil)
	return strings.Contains(rec.Body.String(), line)
}

func TestFailedLoginIsCountedOnlyForAuthenticationErrors(t *testing.T) {
	for _, tc := range []struct {
		name          string
		code          string
		afterPassword bool
		failures      string
		upstream      string
	}{
		{"wrong password", "28P01", true, "1", "0"},
		{"role not permitted", "28000", false, "1", "0"},
		{"too many connections", "53300", false, "0", "1"},
		{"unknown database", "3D000", false, "0", "1"},
		{"database starting up", "57P03", false, "0", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &gateway.Config{
				PGAddr: refusingBackend(t, tc.code, tc.afterPassword), PGSSLMode: "disable", QueryTimeout: 5 * time.Second,
				HTTPSlots: make(chan struct{}, 2), UpstreamSlots: make(chan struct{}, 2),
				MaxHTTPRequestBytes: 1 << 20, MaxHTTPRowBytes: 1 << 20, MaxHTTPBufferedBytes: 1 << 20, MaxHTTPResponseBytes: 1 << 20,
				Metrics: &gateway.Metrics{},
			}
			req := httptest.NewRequest("POST", "/sql", strings.NewReader(`{"query":"select 1"}`))
			req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
			response := httptest.NewRecorder()
			New(cfg).Serve(response, req)
			if response.Code < 400 {
				t.Fatalf("unexpected success: %d", response.Code)
			}
			if !scrapeCounter(cfg, `barnacle_login_failures_total{transport="http"} `+tc.failures) {
				t.Errorf("login failures not %s for %s", tc.failures, tc.name)
			}
			if !scrapeCounter(cfg, "barnacle_upstream_connection_failures_total "+tc.upstream) {
				t.Errorf("upstream failures not %s for %s", tc.upstream, tc.name)
			}
		})
	}
}

func TestMissingAuthenticationMethodIsNotAFailedLogin(t *testing.T) {
	// A server that only trusts the connection has no credential to refuse;
	// that is a configuration problem and is reported as an upstream failure.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		backend := pgproto3.NewBackend(conn, conn)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			return
		}
		backend.Send(&pgproto3.AuthenticationOk{})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		_ = backend.Flush()
	}()
	cfg := &gateway.Config{
		PGAddr: listener.Addr().String(), PGSSLMode: "disable", QueryTimeout: 5 * time.Second,
		HTTPSlots: make(chan struct{}, 2), UpstreamSlots: make(chan struct{}, 2),
		MaxHTTPRequestBytes: 1 << 20, MaxHTTPRowBytes: 1 << 20, MaxHTTPBufferedBytes: 1 << 20, MaxHTTPResponseBytes: 1 << 20,
		Metrics: &gateway.Metrics{},
	}
	req := httptest.NewRequest("POST", "/sql", strings.NewReader(`{"query":"select 1"}`))
	req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
	New(cfg).Serve(httptest.NewRecorder(), req)
	if !scrapeCounter(cfg, `barnacle_login_failures_total{transport="http"} 0`) {
		t.Error("a missing authentication method was counted as a failed login")
	}
}
