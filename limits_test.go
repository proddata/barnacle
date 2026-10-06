package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proddata/barnacle/internal/gateway"
	"github.com/proddata/barnacle/internal/pgws"
	"github.com/proddata/barnacle/internal/sqlhttp"
)

func TestUpstreamLimitCoversHTTPAndWebSocket(t *testing.T) {
	cfg := gateway.Config{
		PGAddr:        "127.0.0.1:1",
		PGSSLMode:     "disable",
		UpstreamSlots: make(chan struct{}, 1),
	}
	if !cfg.AcquireUpstream() || cfg.AcquireUpstream() {
		t.Fatal("connection limit was not enforced")
	}

	httpRequest := httptest.NewRequest(http.MethodPost, "/sql", strings.NewReader(`{"query":"select 1"}`))
	httpRequest.Header.Set("Neon-Connection-String", "postgres://user:password@localhost/db")
	httpResponse := httptest.NewRecorder()
	sqlhttp.New(&cfg).Serve(httpResponse, httpRequest)
	if httpResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP status = %d, want 503", httpResponse.Code)
	}

	wsRequest := httptest.NewRequest(http.MethodGet, "/v2", nil)
	wsRequest.Header.Set("Upgrade", "websocket")
	wsRequest.Header.Set("Connection", "Upgrade")
	wsRequest.Header.Set("Sec-WebSocket-Version", "13")
	wsRequest.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	wsResponse := httptest.NewRecorder()
	pgws.New(&cfg).Serve(wsResponse, wsRequest)
	if wsResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("WebSocket status = %d, want 503", wsResponse.Code)
	}

	cfg.ReleaseUpstream()
	if !cfg.AcquireUpstream() {
		t.Fatal("connection slot was not released")
	}
	cfg.ReleaseUpstream()
}

func TestHTTPQueryLimit(t *testing.T) {
	cfg := gateway.Config{HTTPSlots: make(chan struct{}, 2)}
	if !cfg.AcquireHTTP() || !cfg.AcquireHTTP() || cfg.AcquireHTTP() {
		t.Fatal("HTTP query limit was not enforced")
	}
	cfg.ReleaseHTTP()
	if !cfg.AcquireHTTP() {
		t.Fatal("HTTP query slot was not released")
	}
	cfg.ReleaseHTTP()
	cfg.ReleaseHTTP()
}
