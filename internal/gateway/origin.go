package gateway

import (
	"net/http"
	"net/url"
)

func (c Config) AllowOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	w.Header().Add("Vary", "Origin")
	if c.OriginAllowed(r) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		return true
	}
	http.Error(w, "origin denied", http.StatusForbidden)
	return false
}
func (c Config) Preflight(w http.ResponseWriter, r *http.Request) {
	if !c.AllowOrigin(w, r) {
		return
	}
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Neon-Connection-String, Neon-Array-Mode, Neon-Raw-Text-Output, Neon-Batch-Read-Only, Neon-Batch-Isolation-Level, Neon-Batch-Deferrable")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func (c Config) OriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if c.AllowedOrigin != "" && origin == c.AllowedOrigin {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host != r.Host {
		return false
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return parsed.Scheme == scheme
}
