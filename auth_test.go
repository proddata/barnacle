package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proddata/hermit/internal/gateway"
	"github.com/proddata/hermit/internal/pgws"
	"github.com/proddata/hermit/internal/sqlhttp"
)

func TestEndpointsEnforceOIDCGate(t *testing.T) {
	cfg := gateway.Config{OIDC: &gateway.OIDCGate{}}
	for _, endpoint := range []struct {
		name, method, path string
		serve              http.HandlerFunc
	}{
		{"HTTP", http.MethodPost, "/sql", sqlhttp.New(&cfg).Serve},
		{"WebSocket", http.MethodGet, "/v2", pgws.New(&cfg).Serve},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			request := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(`{"query":"select 1"}`))
			response := httptest.NewRecorder()
			endpoint.serve(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
		})
	}
}
