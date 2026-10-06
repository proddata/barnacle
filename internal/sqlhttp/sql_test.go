package sqlhttp

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/proddata/barnacle/internal/gateway"
)

func TestDBErrorIdentifiesTLSVerificationFailure(t *testing.T) {
	response := httptest.NewRecorder()
	dbError(response, fmt.Errorf("connect: %w", x509.UnknownAuthorityError{}))
	var body struct{ Message, Code string }
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 502 || body.Code != "BARNACLE_UPSTREAM_TLS_VERIFICATION_FAILED" || body.Message == "" {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestConnectionConfigLocksUpstreamAndForwardsBearer(t *testing.T) {
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGUser: "default_user", PGDatabase: "default_db", PGSSLMode: "disable"}}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	req.Header.Set("Neon-Connection-String", "postgres://app@remote.example:6543/appdb")
	req.Header.Set("Authorization", "Bearer test-token")
	pgcfg, err := cfg.connectionConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if pgcfg.Host != "127.0.0.1" || pgcfg.Port != 5432 || pgcfg.User != "app" || pgcfg.Database != "appdb" || pgcfg.Password != "test-token" || len(pgcfg.Fallbacks) != 0 {
		t.Fatalf("unexpected config: host=%s port=%d user=%s database=%s fallbacks=%d", pgcfg.Host, pgcfg.Port, pgcfg.User, pgcfg.Database, len(pgcfg.Fallbacks))
	}
	if pgcfg.OAuthTokenProvider == nil {
		t.Fatal("bearer token was not configured for OAUTHBEARER")
	}
	token, err := pgcfg.OAuthTokenProvider(context.Background())
	if err != nil || token != "test-token" {
		t.Fatalf("OAuth token = %q, %v", token, err)
	}
}

func TestConnectionConfigUsesAllowedRouteAndTLSName(t *testing.T) {
	allowed, err := gateway.ParseAllowedPGAddrs("db.internal:5432")
	if err != nil {
		t.Fatal(err)
	}
	cfg := New(&gateway.Config{PGAllowedAddrs: allowed, PGSSLMode: "require"})
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	req.Header.Set("Neon-Connection-String", "postgres://app:secret@db.internal/app")
	pgcfg, err := cfg.connectionConfig(req)
	if err != nil || pgcfg.Host != "db.internal" || pgcfg.Port != 5432 || pgcfg.TLSConfig.ServerName != "db.internal" {
		t.Fatalf("HTTP route = %#v, %v", pgcfg, err)
	}
}

func TestConnectionConfigQueryExecMode(t *testing.T) {
	for _, tc := range []struct {
		configured string
		want       pgx.QueryExecMode
	}{
		{"", pgx.QueryExecModeExec},
		{"exec", pgx.QueryExecModeExec},
		{"cache_describe", pgx.QueryExecModeCacheDescribe},
		{"cache_statement", pgx.QueryExecModeCacheStatement},
	} {
		cfg := New(&gateway.Config{PGAddr: "postgres:5432", PGSSLMode: "disable", PGQueryExecMode: tc.configured})
		req := httptest.NewRequest("POST", "http://localhost/sql", nil)
		req.Header.Set("Neon-Connection-String", "postgres://app:secret@postgres:5432/app")
		got, err := cfg.connectionConfig(req)
		if err != nil || got.DefaultQueryExecMode != tc.want {
			t.Fatalf("mode %q = %v, %v; want %v", tc.configured, got.DefaultQueryExecMode, err, tc.want)
		}
	}
}
func TestConnectionConfigRequiresCredential(t *testing.T) {
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGUser: "app", PGDatabase: "app", PGSSLMode: "disable"}}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	if _, err := cfg.connectionConfig(req); err == nil {
		t.Fatal("accepted anonymous request")
	}
	req.Header.Set("Neon-Connection-String", "postgres://app@remote.example/app")
	if _, err := cfg.connectionConfig(req); err == nil {
		t.Fatal("accepted passwordless connection string")
	}
}
func TestConnectionConfigRejectsCredentialOverridesAndFileOptions(t *testing.T) {
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable"}}
	for _, raw := range []string{
		"postgres://app:secret@remote.example/app?user=other",
		"postgres://app:secret@remote.example/app?password=other",
		"postgres://app:secret@remote.example/app?servicefile=/etc/passwd&service=app",
		"postgres://app:secret@remote.example/app?passfile=/etc/passwd",
		"postgres://app:secret@remote.example/app?sslcert=/etc/passwd",
		"postgres://app:secret@remote.example/app?require_auth=none",
		"postgres://app:secret@remote.example/app?require_auth=md5&require_auth=scram-sha-256",
		"postgres://:secret@remote.example/app",
	} {
		req := httptest.NewRequest("POST", "http://localhost/sql", nil)
		req.Header.Set("Neon-Connection-String", raw)
		if _, err := cfg.connectionConfig(req); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("connectionConfig(%q) error = %v", raw, err)
		}
	}
}

func TestConnectionConfigKeepsRequestCredentialsSeparate(t *testing.T) {
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable"}}
	for _, tc := range []struct{ user, password string }{{"alice", "first"}, {"bob", "second"}} {
		req := httptest.NewRequest("POST", "http://localhost/sql", nil)
		req.Header.Set("Neon-Connection-String", "postgres://"+tc.user+":"+tc.password+"@remote.example/app?sslmode=require")
		pgcfg, err := cfg.connectionConfig(req)
		if err != nil || pgcfg.User != tc.user || pgcfg.Password != tc.password {
			t.Fatalf("credentials for %s = %v, %v", tc.user, pgcfg, err)
		}
	}
}
func TestConnectionConfigCanRequirePasswordMethod(t *testing.T) {
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable"}}
	for _, method := range []string{"scram-sha-256", "md5"} {
		req := httptest.NewRequest("POST", "http://localhost/sql", nil)
		req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/app?require_auth="+method)
		pgcfg, err := cfg.connectionConfig(req)
		if err != nil || pgcfg.RequireAuth != method {
			t.Fatalf("required method %s = %#v, %v", method, pgcfg, err)
		}
	}
}
func TestOIDCConnectionRequiresOAuth(t *testing.T) {
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable", OIDC: &gateway.OIDCGate{}}}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	req.Header.Set("Neon-Connection-String", "postgres://app:legacy-password@remote.example/appdb?require_auth=md5")
	req.Header.Set("Authorization", "Bearer signed-access-token")
	pgcfg, err := cfg.connectionConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if pgcfg.Password != "" || pgcfg.RequireAuth != "oauth" || pgcfg.OAuthTokenProvider == nil {
		t.Fatalf("OIDC connection permits non-OAuth authentication: password set=%t require_auth=%q provider set=%t",
			pgcfg.Password != "", pgcfg.RequireAuth, pgcfg.OAuthTokenProvider != nil)
	}
	token, err := pgcfg.OAuthTokenProvider(context.Background())
	if err != nil || token != "signed-access-token" {
		t.Fatalf("OAuth token = %q, %v", token, err)
	}
}

func TestRequiredOAuthUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth pgproto3.BackendMessage
	}{
		{name: "SCRAM only", auth: &pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256"}}},
		{name: "trust only", auth: &pgproto3.AuthenticationOk{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			serverErr := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverErr <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				var length [4]byte
				if _, err := io.ReadFull(conn, length[:]); err != nil {
					serverErr <- err
					return
				}
				if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint32(length[:]))-4); err != nil {
					serverErr <- err
					return
				}
				packet, err := tc.auth.Encode(nil)
				if err == nil {
					_, err = conn.Write(packet)
				}
				serverErr <- err
			}()

			port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
			pgcfg, err := pgx.ParseConfig("postgres://app@127.0.0.1:" + port + "/app?sslmode=disable")
			if err != nil {
				t.Fatal(err)
			}
			pgcfg.RequireAuth = "oauth"
			pgcfg.OAuthTokenProvider = func(context.Context) (string, error) { return "signed-token", nil }
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := pgx.ConnectConfig(ctx, pgcfg)
			if conn != nil {
				conn.Close(context.Background())
				t.Fatal("accepted a PostgreSQL connection without OAuth")
			}
			if err == nil {
				t.Fatal("expected OAuth requirement error")
			}
			if err := <-serverErr; err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			dbConnectError(response, err, true)
			var body struct{ Message, Code string }
			if decodeErr := json.Unmarshal(response.Body.Bytes(), &body); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if response.Code != 502 || body.Code != "BARNACLE_UPSTREAM_OAUTH_UNAVAILABLE" || body.Message == "" {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if response.Header().Get("WWW-Authenticate") != "" {
				t.Fatal("upstream OAuth mismatch was reported as an invalid token")
			}
		})
	}
}

func TestBatchIsolation(t *testing.T) {
	for input, want := range map[string]pgx.TxIsoLevel{
		"ReadUncommitted": pgx.ReadUncommitted,
		"ReadCommitted":   pgx.ReadCommitted,
		"RepeatableRead":  pgx.RepeatableRead,
		"Serializable":    pgx.Serializable,
		"read committed":  pgx.ReadCommitted,
	} {
		got, err := batchIsolation(input)
		if err != nil || got != want {
			t.Fatalf("batchIsolation(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := batchIsolation("invalid"); err == nil {
		t.Fatal("accepted invalid isolation level")
	}
}

func TestCommandRowCount(t *testing.T) {
	for tag, want := range map[string]*int64{
		"CREATE TABLE": nil,
		"SELECT 0":     int64Pointer(0),
		"SELECT 3":     int64Pointer(3),
		"INSERT 0 2":   int64Pointer(2),
		"UPDATE 4":     int64Pointer(4),
	} {
		got := commandRowCount(tag)
		if got == nil && want == nil {
			continue
		}
		if got == nil || want == nil || *got != *want {
			t.Fatalf("commandRowCount(%q) = %v, want %v", tag, got, want)
		}
	}
}

func int64Pointer(value int64) *int64 { return &value }
