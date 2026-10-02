package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamLimitCoversHTTPAndWebSocket(t *testing.T) {
	cfg := config{
		pgAddr:        "127.0.0.1:1",
		pgSSLMode:     "disable",
		upstreamSlots: make(chan struct{}, 1),
	}
	if !cfg.acquireUpstream() || cfg.acquireUpstream() {
		t.Fatal("connection limit was not enforced")
	}

	httpRequest := httptest.NewRequest(http.MethodPost, "/sql", strings.NewReader(`{"query":"select 1"}`))
	httpRequest.Header.Set("Neon-Connection-String", "postgres://user:password@localhost/db")
	httpResponse := httptest.NewRecorder()
	cfg.sql(httpResponse, httpRequest)
	if httpResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP status = %d, want 503", httpResponse.Code)
	}

	wsRequest := httptest.NewRequest(http.MethodGet, "/v2", nil)
	wsRequest.Header.Set("Upgrade", "websocket")
	wsRequest.Header.Set("Connection", "Upgrade")
	wsRequest.Header.Set("Sec-WebSocket-Version", "13")
	wsRequest.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	wsResponse := httptest.NewRecorder()
	cfg.websocket(wsResponse, wsRequest)
	if wsResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("WebSocket status = %d, want 503", wsResponse.Code)
	}

	cfg.releaseUpstream()
	if !cfg.acquireUpstream() {
		t.Fatal("connection slot was not released")
	}
	cfg.releaseUpstream()
}

func TestHTTPQueryLimit(t *testing.T) {
	cfg := config{httpSlots: make(chan struct{}, 2)}
	if !cfg.acquireHTTP() || !cfg.acquireHTTP() || cfg.acquireHTTP() {
		t.Fatal("HTTP query limit was not enforced")
	}
	cfg.releaseHTTP()
	if !cfg.acquireHTTP() {
		t.Fatal("HTTP query slot was not released")
	}
	cfg.releaseHTTP()
	cfg.releaseHTTP()
}
