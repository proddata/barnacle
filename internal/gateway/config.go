// Package gateway holds the limits and trust decisions shared by the HTTP SQL
// endpoint and the PostgreSQL wire protocol WebSocket endpoint.
package gateway

import (
	"crypto/x509"
	"net/netip"
	"time"
)

// Config is assembled at startup. Handlers share its admission channels so an
// HTTP request and a WebSocket session draw from the same PostgreSQL budget.
type Config struct {
	Listen, PGAddr, ReadyPGAddr                                                      string
	PGSSLMode, PGTLSServerName                                                       string
	PGQueryExecMode                                                                  string
	PGAllowedAddrs                                                                   map[string]struct{}
	AllowedOrigins                                                                   map[string]struct{}
	TrustedProxies                                                                   []netip.Prefix
	PGRootCAs                                                                        *x509.CertPool
	QueryTimeout, StatementTimeout, WSIdleTimeout, WSWriteTimeout                    time.Duration
	UpstreamSlots, CancelSlots, HTTPSlots, ReadySlots                                chan struct{}
	MaxHTTPRequestBytes, MaxHTTPRowBytes, MaxHTTPBufferedBytes, MaxHTTPResponseBytes int64
	Metrics                                                                          *Metrics
}

// AcquireUpstream reserves one database connection without waiting. Refusing
// excess work before dialing prevents the gateway from exceeding its budget.
func (c Config) AcquireUpstream() bool {
	select {
	case c.UpstreamSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c Config) ReleaseUpstream() {
	<-c.UpstreamSlots
}

func (c Config) AcquireHTTP() bool {
	if c.HTTPSlots == nil {
		return true
	}
	select {
	case c.HTTPSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c Config) ReleaseHTTP() {
	if c.HTTPSlots != nil {
		<-c.HTTPSlots
	}
}
