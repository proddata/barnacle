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
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable"}}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	req.Header.Set("Neon-Connection-String", "postgres://app@remote.example:6543/appdb")
	req.Header.Set("Authorization", "Bearer test-token")
	pgcfg, err := cfg.connectionConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if pgcfg.Host != "127.0.0.1" || pgcfg.Port != 5432 || pgcfg.User != "app" || pgcfg.Database != "appdb" || pgcfg.Password != "" || pgcfg.RequireAuth != "oauth" || len(pgcfg.Fallbacks) != 0 {
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

func TestNeonConnectionHeaderAndBodyOptions(t *testing.T) {
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable"}}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
	pgcfg, err := cfg.connectionConfig(req)
	if err != nil || pgcfg.User != "app" || pgcfg.Database != "appdb" {
		t.Fatalf("Neon connection = %#v, %v", pgcfg, err)
	}
	req.Header.Add("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
	if _, err := cfg.connectionConfig(req); err == nil || !strings.Contains(err.Error(), "one Neon-Connection-String") {
		t.Fatalf("accepted duplicate connection headers: %v", err)
	}
	req.Header.Del("Neon-Connection-String")
	req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
	req.Header.Set("Neon-Array-Mode", "true")
	req.Header.Set("Neon-Batch-Read-Only", "true")
	rawText, isolation := true, "Serializable"
	options, err := optionsForRequest(req, queryRequest{Options: &queryOptions{RawText: &rawText, IsolationLevel: &isolation}}, true)
	if err != nil || !options.arrayMode || !options.rawText || options.tx.AccessMode != pgx.ReadOnly || options.tx.IsoLevel != pgx.Serializable {
		t.Fatalf("Neon and body options = %#v, %v", options, err)
	}
	arrayMode := false
	if _, err := optionsForRequest(req, queryRequest{Options: &queryOptions{ArrayMode: &arrayMode}}, true); err == nil {
		t.Fatal("accepted conflicting array mode aliases")
	}
	neon := httptest.NewRequest("POST", "http://localhost/sql", nil)
	options, err = optionsForRequest(neon, queryRequest{Options: &queryOptions{RawText: &rawText}}, true)
	if err != nil || options.arrayMode || !options.rawText {
		t.Fatalf("Neon options changed = %#v, %v", options, err)
	}
	neon.Header.Set("Neon-Array-Mode", "true")
	neon.Header.Set("Neon-Batch-Read-Only", "true")
	options, err = optionsForRequest(neon, queryRequest{}, true)
	if err != nil || !options.arrayMode || options.tx.AccessMode != pgx.ReadOnly {
		t.Fatalf("Neon header options = %#v, %v", options, err)
	}
	neon.Header.Add("Neon-Array-Mode", "false")
	if _, err := optionsForRequest(neon, queryRequest{}, true); err == nil {
		t.Fatal("accepted duplicate Neon headers")
	}
	neon.Header.Del("Neon-Array-Mode")
	neon.Header.Set("Neon-Array-Mode", "yes")
	if _, err := optionsForRequest(neon, queryRequest{}, true); err == nil {
		t.Fatal("accepted invalid boolean header")
	}
}

func TestResponseMode(t *testing.T) {
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	if streaming, err := responseMode(req, false); streaming || err != nil {
		t.Fatalf("default mode = %t, %v", streaming, err)
	}
	for value, want := range map[string]bool{"buffered": false, "STREAMING": true} {
		req.Header.Set("Barnacle-Response-Mode", value)
		if streaming, err := responseMode(req, false); streaming != want || err != nil {
			t.Errorf("mode %q = %t, %v", value, streaming, err)
		}
	}
	for _, value := range []string{"unknown", "buffered, streaming"} {
		req.Header.Set("Barnacle-Response-Mode", value)
		if _, err := responseMode(req, false); err == nil {
			t.Errorf("accepted mode %q", value)
		}
	}
	req.Header.Set("Barnacle-Response-Mode", "streaming")
	if _, err := responseMode(req, true); err == nil {
		t.Fatal("accepted response mode on a batch")
	}
	req.Header.Add("Barnacle-Response-Mode", "buffered")
	if _, err := responseMode(req, false); err == nil {
		t.Fatal("accepted duplicate response mode headers")
	}
}

func TestLegacyOptionHeadersRejected(t *testing.T) {
	removedBatch := httptest.NewRequest("POST", "http://localhost/sql", nil)
	removedBatch.Header.Set("Barnacle-Batch-Mode", "stop-on-error")
	if err := rejectLegacyHeaders(removedBatch); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("removed batch mode rejection = %v", err)
	}
	for _, name := range []string{"Connection-String", "Array-Mode", "Raw-Text-Output", "Batch-Read-Only", "Batch-Isolation-Level", "Batch-Deferrable"} {
		req := httptest.NewRequest("POST", "http://localhost/sql", nil)
		req.Header.Set(name, "value")
		if err := rejectLegacyHeaders(req); err == nil || !strings.Contains(err.Error(), "Neon-"+name) {
			t.Errorf("%s rejection = %v", name, err)
		}
	}
}

func TestConnectionConfigBasicAuth(t *testing.T) {
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable"}}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	req.Header.Set("Neon-Connection-String", "postgres://remote.example/appdb")
	req.SetBasicAuth("app", "secret")
	pgcfg, err := cfg.connectionConfig(req)
	if err != nil || pgcfg.User != "app" || pgcfg.Password != "secret" || pgcfg.Database != "appdb" || pgcfg.Host != "127.0.0.1" || pgcfg.OAuthTokenProvider != nil || pgcfg.RequireAuth != "password,md5,scram-sha-256" {
		t.Fatalf("Basic connection config = %#v, %v", pgcfg, err)
	}
	req.Header.Set("Neon-Connection-String", "postgres://app:other@remote.example/appdb")
	if _, err := cfg.connectionConfig(req); err == nil {
		t.Fatal("accepted Basic credentials with URL credentials")
	}
	req.Header.Set("Neon-Connection-String", "postgres://remote.example/appdb")
	for _, malformed := range []string{"Basic", "Basic ", "Basic invalid", "Digest anything"} {
		req.Header.Set("Authorization", malformed)
		if _, err := cfg.connectionConfig(req); err == nil {
			t.Errorf("accepted malformed Authorization %q", malformed)
		}
	}
}

func TestConnectionBodyIsRejected(t *testing.T) {
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	_, err := decodeRequest(req, []byte(`{"connection":{"host":"remote.example","database":"appdb"},"query":"select 1"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "connection must be supplied") {
		t.Fatalf("connection body error = %v", err)
	}
}

func TestRequestBodyLimitAndEarlyAdmission(t *testing.T) {
	const limit = 10 << 20
	slots := make(chan struct{}, 1)
	cfg := Handler{Config: &gateway.Config{
		PGAddr:              "127.0.0.1:5432",
		PGSSLMode:           "disable",
		QueryTimeout:        time.Minute,
		HTTPSlots:           slots,
		MaxHTTPRequestBytes: limit,
		Metrics:             &gateway.Metrics{},
	}}
	base := `{"query":"select 1"}`
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"at limit", base + strings.Repeat(" ", limit-len(base)), 503},
		{"over limit", base + strings.Repeat(" ", limit-len(base)+1), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "http://localhost/sql", strings.NewReader(tc.body))
			req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
			cfg.Serve(response, req)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
			if tc.want == 503 && !strings.Contains(response.Body.String(), "postgres connection limit") {
				t.Fatalf("at-limit upload did not reach upstream admission: %s", response.Body.String())
			}
			if len(slots) != 0 {
				t.Fatal("HTTP slot was not released")
			}
		})
	}
	slots <- struct{}{}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	req.Body = unreadableBody{}
	response := httptest.NewRecorder()
	cfg.Serve(response, req)
	if response.Code != 503 {
		t.Fatalf("full-slot status = %d, want 503", response.Code)
	}
}

type unreadableBody struct{}

func (unreadableBody) Read([]byte) (int, error) { panic("request body read before admission") }
func (unreadableBody) Close() error             { return nil }

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
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable"}}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	if _, err := cfg.connectionConfig(req); err == nil {
		t.Fatal("accepted anonymous request")
	}
	req.Header.Set("Authorization", "Bearer test-token")
	if _, err := cfg.connectionConfig(req); err == nil {
		t.Fatal("accepted bearer request without connection string")
	}
	req.Header.Del("Authorization")
	req.Header.Set("Neon-Connection-String", "postgres://app@remote.example/app")
	if _, err := cfg.connectionConfig(req); err == nil {
		t.Fatal("accepted passwordless connection string")
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Neon-Connection-String", "postgres://app@remote.example/")
	if _, err := cfg.connectionConfig(req); err == nil {
		t.Fatal("accepted connection string without database")
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
func TestBearerConnectionRequiresOAuth(t *testing.T) {
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable"}}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	req.Header.Set("Neon-Connection-String", "postgres://app:legacy-password@remote.example/appdb?require_auth=md5")
	req.Header.Set("Authorization", "Bearer signed-access-token")
	pgcfg, err := cfg.connectionConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if pgcfg.Password != "" || pgcfg.RequireAuth != "oauth" || pgcfg.OAuthTokenProvider == nil {
		t.Fatalf("bearer connection permits non-OAuth authentication: password set=%t require_auth=%q provider set=%t",
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
