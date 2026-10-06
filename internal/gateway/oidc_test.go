package gateway

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type testOIDCIssuer struct {
	mu       sync.Mutex
	issuer   string
	keys     map[string]*rsa.PrivateKey
	broken   bool
	requests int
}

func (provider *testOIDCIssuer) serve(w http.ResponseWriter, r *http.Request) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": provider.issuer, "jwks_uri": provider.issuer + "/keys"})
	case "/keys":
		provider.requests++
		if provider.broken {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		keys := make([]map[string]string, 0, len(provider.keys))
		for kid, key := range provider.keys {
			keys = append(keys, map[string]string{
				"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	default:
		http.NotFound(w, r)
	}
}

func signedAccessToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any, typ string) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": typ, "kid": kid})
	payload, _ := json.Marshal(claims)
	message := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestOIDCAccessTokenGate(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := &testOIDCIssuer{keys: map[string]*rsa.PrivateKey{"first": key}}
	server := httptest.NewServer(http.HandlerFunc(provider.serve))
	defer server.Close()
	provider.issuer = server.URL
	gate, err := NewOIDCGate(t.Context(), server.URL, "barnacle-api", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	base := map[string]any{
		"iss": server.URL, "aud": []string{"other", "barnacle-api"},
		"sub": "person-1", "client_id": "test-client", "jti": "test-id",
		"iat": now, "nbf": now - 1, "exp": now + 60,
	}
	token := signedAccessToken(t, key, "first", base, "at+jwt")
	if err := gate.verify(t.Context(), token); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	config := Config{OIDC: gate, PGAddr: "127.0.0.1:1", PGSSLMode: "disable"}
	validHTTP := httptest.NewRequest(http.MethodPost, "/sql", nil)
	validHTTP.Header.Set("Authorization", "Bearer "+token)
	if !config.AuthorizeHTTP(httptest.NewRecorder(), validHTTP) {
		t.Fatal("valid HTTP access token rejected")
	}
	validWS := httptest.NewRequest(http.MethodGet, "/v2", nil)
	validWS.AddCookie(&http.Cookie{Name: oidcCookieName, Value: token})
	if !config.AuthorizeWebSocket(httptest.NewRecorder(), validWS) {
		t.Fatal("valid WebSocket access-token cookie rejected")
	}
	for name, change := range map[string]func(map[string]any){
		"issuer":          func(c map[string]any) { c["iss"] = "https://other.example" },
		"audience":        func(c map[string]any) { c["aud"] = "other" },
		"expired":         func(c map[string]any) { c["exp"] = now - 60 },
		"not yet valid":   func(c map[string]any) { c["nbf"] = now + 60 },
		"future issue":    func(c map[string]any) { c["iat"] = now + 60 },
		"missing subject": func(c map[string]any) { delete(c, "sub") },
	} {
		t.Run(name, func(t *testing.T) {
			claims := make(map[string]any, len(base))
			for k, v := range base {
				claims[k] = v
			}
			change(claims)
			if gate.verify(t.Context(), signedAccessToken(t, key, "first", claims, "at+jwt")) == nil {
				t.Fatal("invalid claims accepted")
			}
		})
	}
	if gate.verify(t.Context(), signedAccessToken(t, key, "first", base, "JWT")) == nil {
		t.Fatal("ID token type accepted as access token")
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if gate.verify(t.Context(), signedAccessToken(t, other, "first", base, "at+jwt")) == nil {
		t.Fatal("invalid signature accepted")
	}
	provider.mu.Lock()
	provider.keys = map[string]*rsa.PrivateKey{"second": other}
	provider.mu.Unlock()
	if err := gate.verify(t.Context(), signedAccessToken(t, other, "second", base, "at+jwt")); err != nil {
		t.Fatalf("rotated key rejected: %v", err)
	}
	provider.mu.Lock()
	provider.broken = true
	provider.mu.Unlock()
	gate.mu.Lock()
	gate.refreshed = time.Now().Add(-6 * time.Minute)
	gate.mu.Unlock()
	if gate.verify(t.Context(), signedAccessToken(t, other, "second", base, "at+jwt")) == nil {
		t.Fatal("stale key accepted when JWKS refresh failed")
	}

	request := httptest.NewRequest(http.MethodPost, "/sql", strings.NewReader(`{"query":"select 1"}`))
	request.Header.Set("Authorization", "Bearer invalid")
	response := httptest.NewRecorder()
	config.AuthorizeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("HTTP gate status = %d, want 401", response.Code)
	}
	wsRequest := httptest.NewRequest(http.MethodGet, "/v2", nil)
	wsResponse := httptest.NewRecorder()
	config.AuthorizeWebSocket(wsResponse, wsRequest)
	if wsResponse.Code != http.StatusUnauthorized {
		t.Fatalf("WebSocket gate status = %d, want 401", wsResponse.Code)
	}
}

func TestOIDCRejectsInsecureNonlocalIssuer(t *testing.T) {
	if err := checkOIDCURL("http://issuer.example/realms/one"); err == nil {
		t.Fatal("accepted insecure issuer")
	}
	if err := checkOIDCURL("https://issuer.example/realms/one"); err != nil {
		t.Fatal(err)
	}
}
