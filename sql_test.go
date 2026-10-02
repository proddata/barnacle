package main

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestConnectionConfigLocksUpstreamAndForwardsBearer(t *testing.T) {
	cfg := config{pgAddr: "127.0.0.1:5432", pgUser: "default_user", pgDatabase: "default_db", pgSSLMode: "disable"}
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
func TestConnectionConfigRequiresCredential(t *testing.T) {
	cfg := config{pgAddr: "127.0.0.1:5432", pgUser: "app", pgDatabase: "app", pgSSLMode: "disable"}
	req := httptest.NewRequest("POST", "http://localhost/sql", nil)
	if _, err := cfg.connectionConfig(req); err == nil {
		t.Fatal("accepted anonymous request")
	}
	req.Header.Set("Neon-Connection-String", "postgres://app@remote.example/app")
	if _, err := cfg.connectionConfig(req); err == nil {
		t.Fatal("accepted passwordless connection string")
	}
}
func TestOriginPolicy(t *testing.T) {
	cfg := config{allowedOrigin: "https://app.example.com"}
	req := httptest.NewRequest("POST", "http://localhost:8080/sql", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	if !cfg.originAllowed(req) {
		t.Fatal("same origin denied")
	}
	req.Header.Set("Origin", "https://app.example.com")
	if !cfg.originAllowed(req) {
		t.Fatal("configured origin denied")
	}
	req.Header.Set("Origin", "https://elsewhere.example")
	if cfg.originAllowed(req) {
		t.Fatal("unconfigured origin allowed")
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
