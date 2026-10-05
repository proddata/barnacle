package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// CanonicalPGAddr accepts one TCP destination, never a URL or a host list.
func CanonicalPGAddr(addr string) (string, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil || host == "" || strings.ContainsAny(host, "/\\@?#% \t\r\n") {
		return "", errors.New("expected PostgreSQL host:port")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", errors.New("invalid PostgreSQL port")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		host = strings.ToLower(host)
	}
	return net.JoinHostPort(host, strconv.FormatUint(port, 10)), nil
}

func ParseAllowedPGAddrs(raw string) (map[string]struct{}, error) {
	if raw == "" {
		return nil, nil
	}
	if strings.TrimSpace(raw) == "*" {
		return map[string]struct{}{"*": {}}, nil
	}
	allowed := make(map[string]struct{})
	for _, entry := range strings.Split(raw, ",") {
		if strings.TrimSpace(entry) == "*" {
			return nil, errors.New("HERMIT_PG_ALLOWED_ADDRS must use * alone")
		}
		addr, err := CanonicalPGAddr(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf("invalid HERMIT_PG_ALLOWED_ADDRS entry %q: %w", entry, err)
		}
		allowed[addr] = struct{}{}
	}
	return allowed, nil
}

func (c Config) UpstreamAddr(requested string) (string, error) {
	if len(c.PGAllowedAddrs) == 0 {
		return c.PGAddr, nil
	}
	_, allowAny := c.PGAllowedAddrs["*"]
	if requested == "" {
		if c.PGAddr != "" {
			if _, ok := c.PGAllowedAddrs[c.PGAddr]; ok || allowAny {
				return c.PGAddr, nil
			}
			return "", errors.New("default PostgreSQL address not allowed")
		}
		return "", errors.New("PostgreSQL address required")
	}
	addr, err := CanonicalPGAddr(requested)
	if err != nil {
		return "", err
	}
	if allowAny {
		return addr, nil
	}
	if _, ok := c.PGAllowedAddrs[addr]; !ok {
		return "", errors.New("PostgreSQL address not allowed")
	}
	return addr, nil
}

func (c Config) HTTPUpstreamAddr(raw string) (string, error) {
	if len(c.PGAllowedAddrs) == 0 || raw == "" {
		return c.UpstreamAddr("")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("invalid PostgreSQL connection string")
	}
	if u.Host == "" {
		return c.UpstreamAddr("")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return c.UpstreamAddr(net.JoinHostPort(u.Hostname(), port))
}
