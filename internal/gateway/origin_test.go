package gateway

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	for _, raw := range []string{"", "127.0.0.1", "127.0.0.1, 2001:db8::1", "10.2.0.0/16, 2001:db8::/32"} {
		if _, err := ParseTrustedProxies(raw); err != nil {
			t.Errorf("ParseTrustedProxies(%q): %v", raw, err)
		}
	}
	for _, raw := range []string{" ", ",127.0.0.1", "127.0.0.1,", "*", "0.0.0.0/0", "::/0", "localhost", "127.0.0.1:80", "fe80::1%eth0", "::ffff:127.0.0.1/128", "10.0.0.0/33"} {
		if _, err := ParseTrustedProxies(raw); err == nil || !strings.Contains(err.Error(), "HERMIT_TRUSTED_PROXIES entry") {
			t.Errorf("wanted startup error for %q, got %v", raw, err)
		}
	}
}

func TestForwardedSchemeRequiresTrustedImmediatePeer(t *testing.T) {
	prefixes, err := ParseTrustedProxies("127.0.0.1, 10.2.0.0/16, 2001:db8::/32")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{TrustedProxies: prefixes}
	for _, tc := range []struct {
		name, peer, origin string
		forwarded          []string
		tls                bool
		allow              bool
	}{
		{"untrusted spoof", "192.0.2.1:9000", "https://api.example", []string{"https"}, false, false},
		{"no configured proxies", "127.0.0.1:9000", "https://api.example", []string{"https"}, false, false},
		{"trusted exact address", "127.0.0.1:9000", "https://api.example", []string{"https"}, false, true},
		{"trusted CIDR", "10.2.3.4:9000", "https://api.example", []string{"https"}, false, true},
		{"trusted IPv6", "[2001:db8::5]:9000", "https://api.example", []string{"https"}, false, true},
		{"outside CIDR", "10.3.3.4:9000", "https://api.example", []string{"https"}, false, false},
		{"trusted http", "127.0.0.1:9000", "http://api.example", []string{"http"}, false, true},
		{"trusted http denies https", "127.0.0.1:9000", "https://api.example", []string{"http"}, false, false},
		{"duplicate header", "127.0.0.1:9000", "https://api.example", []string{"https", "http"}, false, false},
		{"comma list", "127.0.0.1:9000", "https://api.example", []string{"https,http"}, false, false},
		{"unknown scheme", "127.0.0.1:9000", "https://api.example", []string{"ftp"}, false, false},
		{"capitalized scheme", "127.0.0.1:9000", "https://api.example", []string{"HTTPS"}, false, false},
		{"missing peer port", "127.0.0.1", "https://api.example", []string{"https"}, false, false},
		{"direct TLS", "192.0.2.1:9000", "https://api.example", []string{"http"}, true, true},
		{"trusted TLS cannot downgrade", "127.0.0.1:9000", "https://api.example", []string{"http"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "http://api.example/sql", nil)
			req.RemoteAddr = tc.peer
			req.Header.Set("Origin", tc.origin)
			for _, value := range tc.forwarded {
				req.Header.Add("X-Forwarded-Proto", value)
			}
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			active := cfg
			if tc.name == "no configured proxies" {
				active.TrustedProxies = nil
			}
			if got := active.OriginAllowed(req); got != tc.allow {
				t.Errorf("OriginAllowed = %t, want %t", got, tc.allow)
			}
			w := httptest.NewRecorder()
			active.Preflight(w, req)
			wantStatus := http.StatusForbidden
			if tc.allow {
				wantStatus = http.StatusNoContent
			}
			if w.Code != wantStatus {
				t.Errorf("preflight = %d, want %d", w.Code, wantStatus)
			}
		})
	}
}

func TestParseAllowedOrigins(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      int
	}{
		{"empty", "", 0},
		{"single", "https://prod.example.com", 1},
		{"multiple with spaces", " https://prod.example.com , https://stage.example.com:8443 ", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAllowedOrigins(tc.raw)
			if err != nil || len(got) != tc.want {
				t.Fatalf("ParseAllowedOrigins(%q) = %v, %v; want %d origins", tc.raw, got, err, tc.want)
			}
		})
	}
	for _, raw := range []string{
		" ", "https://prod.example.com,", ",https://prod.example.com", "https://prod.example.com,,https://stage.example.com",
		"*", "null", "https://*.example.com", "ftp://example.com", "https://example.com/",
		"https://example.com/path", "https://example.com?query=1", "https://example.com#fragment",
		"https://user@example.com", "https://example.com:99999", "https://example.com:",
	} {
		t.Run("invalid "+raw, func(t *testing.T) {
			if _, err := ParseAllowedOrigins(raw); err == nil || !strings.Contains(err.Error(), "HERMIT_ALLOWED_ORIGIN entry") {
				t.Fatalf("wanted clear startup error for %q, got %v", raw, err)
			}
		})
	}
}

func TestOriginPolicyAndPreflight(t *testing.T) {
	origins, err := ParseAllowedOrigins("https://prod.example.com, https://stage.example.com:8443")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{AllowedOrigins: origins}
	for _, tc := range []struct {
		origin string
		allow  bool
	}{
		{"", true},
		{"http://localhost:8080", true},
		{"https://prod.example.com", true},
		{"https://stage.example.com:8443", true},
		{"https://elsewhere.example", false},
		{"https://prod.example.com/", false},
		{"https://prod.example.com.evil", false},
	} {
		req := httptest.NewRequest(http.MethodOptions, "http://localhost:8080/sql", nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if got := cfg.OriginAllowed(req); got != tc.allow {
			t.Errorf("OriginAllowed(%q) = %t, want %t", tc.origin, got, tc.allow)
		}
		w := httptest.NewRecorder()
		cfg.Preflight(w, req)
		if got := w.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Vary for %q = %q", tc.origin, got)
		}
		if tc.allow {
			if w.Code != http.StatusNoContent {
				t.Errorf("preflight %q = %d", tc.origin, w.Code)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != tc.origin {
				t.Errorf("allow origin for %q = %q", tc.origin, got)
			}
		} else if w.Code != http.StatusForbidden || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("denied preflight %q = %d, headers %v", tc.origin, w.Code, w.Header())
		}
	}
	req := httptest.NewRequest(http.MethodOptions, "http://localhost:8080/sql", nil)
	req.Header.Add("Origin", "https://prod.example.com")
	req.Header.Add("Origin", "https://stage.example.com:8443")
	if cfg.OriginAllowed(req) {
		t.Fatal("multiple Origin headers accepted")
	}
}
