package gateway

import (
	"net/http/httptest"
	"testing"
)

func TestOriginPolicy(t *testing.T) {
	cfg := Config{AllowedOrigin: "https://app.example.com"}
	req := httptest.NewRequest("POST", "http://localhost:8080/sql", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	if !cfg.OriginAllowed(req) {
		t.Fatal("same origin denied")
	}
	req.Header.Set("Origin", "https://app.example.com")
	if !cfg.OriginAllowed(req) {
		t.Fatal("configured origin denied")
	}
	req.Header.Set("Origin", "https://elsewhere.example")
	if cfg.OriginAllowed(req) {
		t.Fatal("unconfigured origin allowed")
	}
}
