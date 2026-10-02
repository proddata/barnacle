package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// canonicalPGAddr accepts one TCP destination, never a URL or a host list.
func canonicalPGAddr(addr string) (string, error) {
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

func parseAllowedPGAddrs(raw string) (map[string]struct{}, error) {
	if raw == "" {
		return nil, nil
	}
	allowed := make(map[string]struct{})
	for _, entry := range strings.Split(raw, ",") {
		addr, err := canonicalPGAddr(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf("invalid HERMIT_PG_ALLOWED_ADDRS entry %q: %w", entry, err)
		}
		allowed[addr] = struct{}{}
	}
	return allowed, nil
}

func (c config) upstreamAddr(requested string) (string, error) {
	if len(c.pgAllowedAddrs) == 0 {
		return c.pgAddr, nil
	}
	if requested == "" {
		if c.pgAddr != "" {
			return c.pgAddr, nil
		}
		return "", errors.New("PostgreSQL address required")
	}
	addr, err := canonicalPGAddr(requested)
	if err != nil {
		return "", err
	}
	if _, ok := c.pgAllowedAddrs[addr]; !ok {
		return "", errors.New("PostgreSQL address not allowed")
	}
	return addr, nil
}

func (c config) httpUpstreamAddr(raw string) (string, error) {
	if len(c.pgAllowedAddrs) == 0 || raw == "" {
		return c.upstreamAddr("")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("invalid PostgreSQL connection string")
	}
	if u.Host == "" {
		return c.upstreamAddr("")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return c.upstreamAddr(net.JoinHostPort(u.Hostname(), port))
}
