package gateway

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// ParseTrustedProxies accepts only literal IP addresses and CIDR prefixes.
// Hostnames would make the trust boundary depend on DNS after startup.
func ParseTrustedProxies(raw string) ([]netip.Prefix, error) {
	if raw == "" {
		return nil, nil
	}
	var prefixes []netip.Prefix
	for index, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		var prefix netip.Prefix
		var err error
		if addr, addrErr := netip.ParseAddr(entry); addrErr == nil && addr.Zone() == "" {
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		} else {
			prefix, err = netip.ParsePrefix(entry)
		}
		if entry == "" || err != nil || !prefix.IsValid() || prefix.Bits() == 0 || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("BARNACLE_TRUSTED_PROXIES entry %d %q must be an IP address or CIDR prefix", index+1, entry)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func ParseAllowedOrigins(raw string) (map[string]struct{}, error) {
	if raw == "" {
		return nil, nil
	}
	origins := make(map[string]struct{})
	for index, part := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(part)
		parsed, err := url.Parse(origin)
		if origin == "" || err != nil || !validConfiguredOrigin(origin, parsed) {
			return nil, fmt.Errorf("BARNACLE_ALLOWED_ORIGIN entry %d %q must be an exact http(s) origin without a path, query, fragment, or wildcard", index+1, origin)
		}
		origins[origin] = struct{}{}
	}
	return origins, nil
}

func validConfiguredOrigin(origin string, parsed *url.URL) bool {
	if parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		strings.ContainsAny(origin, "*#?\\ ") || strings.HasSuffix(parsed.Host, ":") {
		return false
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return parsed.Scheme+"://"+parsed.Host == origin
}

func (c Config) AllowOrigin(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "Origin")
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if c.OriginAllowed(r) {
		w.Header().Set("Access-Control-Allow-Origin", origins[0])
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
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Connection-String, Neon-Connection-String, Array-Mode, Neon-Array-Mode, Raw-Text-Output, Neon-Raw-Text-Output, Batch-Read-Only, Neon-Batch-Read-Only, Batch-Isolation-Level, Neon-Batch-Isolation-Level, Batch-Deferrable, Neon-Batch-Deferrable")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func (c Config) OriginAllowed(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 || origins[0] == "" {
		return false
	}
	scheme, valid := c.requestScheme(r)
	if !valid {
		return false
	}
	origin := origins[0]
	if _, ok := c.AllowedOrigins[origin]; ok {
		return true
	}
	return origin == scheme+"://"+r.Host
}

func (c Config) requestScheme(r *http.Request) (string, bool) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if !c.trustedProxy(r.RemoteAddr) {
		return scheme, true
	}
	values := r.Header.Values("X-Forwarded-Proto")
	if len(values) == 0 {
		return scheme, true
	}
	if len(values) != 1 || (values[0] != "http" && values[0] != "https") {
		return "", false
	}
	if r.TLS != nil {
		return "https", true
	}
	return values[0], true
}

func (c Config) trustedProxy(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range c.TrustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
