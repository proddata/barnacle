package sqlhttp

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/proddata/hermit/internal/gateway"
)

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
func TestOIDCConnectionRequiresOAuth(t *testing.T) {
	cfg := Handler{Config: &gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable", OIDC: &gateway.OIDCGate{}}}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	req.Header.Set("Neon-Connection-String", "postgres://app:legacy-password@remote.example/appdb?require_auth=scram-sha-256")
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
