package gateway

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const oidcCookieName = "barnacle_access_token"

var ErrInvalidToken = errors.New("invalid access token")

type OIDCGate struct {
	issuer, audience, jwksURL string
	client                    *http.Client
	mu                        sync.Mutex
	keys                      map[string]*rsa.PublicKey
	refreshed                 time.Time
	unknownRefreshed          time.Time
}

type oidcMetadata struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

type jwkSet struct {
	Keys []struct {
		Kty, Kid, Use, Alg, N, E string
	} `json:"keys"`
}

func NewOIDCGate(ctx context.Context, issuer, audience string, client *http.Client) (*OIDCGate, error) {
	if issuer == "" || audience == "" {
		return nil, errors.New("OIDC issuer and audience are both required")
	}
	if err := checkOIDCURL(issuer); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many OIDC redirects")
			}
			return checkOIDCURL(req.URL.String())
		}}
	}
	parsed, _ := url.Parse(issuer)
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/.well-known/openid-configuration"
	var metadata oidcMetadata
	if err := fetchOIDCJSON(ctx, client, parsed.String(), &metadata); err != nil {
		return nil, fmt.Errorf("OIDC discovery failed: %w", err)
	}
	if metadata.Issuer != issuer {
		return nil, errors.New("OIDC discovery issuer mismatch")
	}
	if err := checkOIDCURL(metadata.JWKSURI); err != nil {
		return nil, err
	}
	gate := &OIDCGate{issuer: issuer, audience: audience, jwksURL: metadata.JWKSURI, client: client}
	if err := gate.refresh(ctx); err != nil {
		return nil, fmt.Errorf("OIDC keys unavailable: %w", err)
	}
	return gate, nil
}

func checkOIDCURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return errors.New("invalid OIDC URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()) {
		return nil // Local issuer integration tests only.
	}
	return errors.New("OIDC URLs must use HTTPS")
}

func fetchOIDCJSON(ctx context.Context, client *http.Client, address string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("OIDC JSON document too large")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid OIDC JSON document")
	}
	return nil
}

func (g *OIDCGate) refresh(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refreshLocked(ctx)
}

func (g *OIDCGate) refreshLocked(ctx context.Context) error {
	var set jwkSet
	if err := fetchOIDCJSON(ctx, g.client, g.jwksURL, &set); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, item := range set.Keys {
		if item.Kty != "RSA" || item.Kid == "" || item.Use != "" && item.Use != "sig" || item.Alg != "" && item.Alg != "RS256" {
			continue
		}
		modulus, errN := base64.RawURLEncoding.DecodeString(item.N)
		exponent, errE := base64.RawURLEncoding.DecodeString(item.E)
		if errN != nil || errE != nil || len(exponent) == 0 || len(exponent) > 4 {
			continue
		}
		n := new(big.Int).SetBytes(modulus)
		e := new(big.Int).SetBytes(exponent).Int64()
		if n.BitLen() < 2048 || n.BitLen() > 8192 || e < 3 || e > 1<<31-1 || e%2 == 0 {
			continue
		}
		if _, duplicate := keys[item.Kid]; duplicate {
			return errors.New("duplicate OIDC key ID")
		}
		keys[item.Kid] = &rsa.PublicKey{N: n, E: int(e)}
	}
	if len(keys) == 0 {
		return errors.New("no supported OIDC signing keys")
	}
	g.keys = keys
	g.refreshed = time.Now()
	return nil
}

func (g *OIDCGate) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.refreshed) >= 5*time.Minute {
		if err := g.refreshLocked(ctx); err != nil {
			return nil, err // Fail closed when the key cache is stale.
		}
	}
	if key := g.keys[kid]; key != nil {
		return key, nil
	}
	if time.Since(g.unknownRefreshed) < time.Second {
		return nil, ErrInvalidToken
	}
	g.unknownRefreshed = time.Now()
	if err := g.refreshLocked(ctx); err != nil {
		return nil, err
	}
	if key := g.keys[kid]; key != nil {
		return key, nil
	}
	return nil, ErrInvalidToken
}

func (g *OIDCGate) verify(ctx context.Context, token string) error {
	if len(token) == 0 || len(token) > 16<<10 {
		return ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ErrInvalidToken
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ErrInvalidToken
	}
	var header struct {
		Alg, Typ, Kid string
		Crit          json.RawMessage
	}
	if json.Unmarshal(headerBytes, &header) != nil || header.Alg != "RS256" || header.Kid == "" || len(header.Crit) > 0 {
		return ErrInvalidToken
	}
	if typ := strings.ToLower(header.Typ); typ != "at+jwt" && typ != "application/at+jwt" {
		return ErrInvalidToken
	}
	key, err := g.key(ctx, header.Kid)
	if err != nil {
		return ErrInvalidToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ErrInvalidToken
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return ErrInvalidToken
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrInvalidToken
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(claimsBytes, &claims) != nil {
		return ErrInvalidToken
	}
	var issuer, subject, clientID, jti string
	if json.Unmarshal(claims["iss"], &issuer) != nil || issuer != g.issuer ||
		json.Unmarshal(claims["sub"], &subject) != nil || subject == "" ||
		json.Unmarshal(claims["client_id"], &clientID) != nil || clientID == "" ||
		json.Unmarshal(claims["jti"], &jti) != nil || jti == "" || !audienceMatches(claims["aud"], g.audience) {
		return ErrInvalidToken
	}
	now := time.Now().Unix()
	exp, valid := numericDate(claims["exp"])
	if !valid || now >= exp+30 {
		return ErrInvalidToken
	}
	iat, valid := numericDate(claims["iat"])
	if !valid || iat > now+30 || exp <= iat {
		return ErrInvalidToken
	}
	if raw := claims["nbf"]; len(raw) > 0 {
		nbf, valid := numericDate(raw)
		if !valid || nbf > now+30 {
			return ErrInvalidToken
		}
	}
	return nil
}

func audienceMatches(raw json.RawMessage, expected string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == expected
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	for _, audience := range many {
		if audience == expected {
			return true
		}
	}
	return false
}

func numericDate(raw json.RawMessage) (int64, bool) {
	value, err := strconv.ParseInt(string(raw), 10, 64)
	return value, err == nil
}

func bearerToken(header string) string {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) != token || token == "" {
		return ""
	}
	return token
}

func (c Config) AuthorizeHTTP(w http.ResponseWriter, r *http.Request) bool {
	if c.OIDC == nil {
		return true
	}
	if err := c.OIDC.verify(r.Context(), bearerToken(r.Header.Get("Authorization"))); err == nil {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{"message": ErrInvalidToken.Error(), "code": "BARNACLE_ERROR"})
	return false
}

func (c Config) AuthorizeWebSocket(w http.ResponseWriter, r *http.Request) bool {
	if c.OIDC == nil {
		return true
	}
	cookie, err := r.Cookie(oidcCookieName)
	if err == nil && c.OIDC.verify(r.Context(), cookie.Value) == nil {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	http.Error(w, "invalid access token", http.StatusUnauthorized)
	return false
}
