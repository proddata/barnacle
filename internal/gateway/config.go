package gateway

import (
	"crypto/x509"
	"net/netip"
	"time"
)

type Config struct {
	Listen, PGAddr, ReadyPGAddr                                 string
	PGDatabase, PGUser, PGPassword, PGSSLMode, PGTLSServerName  string
	PGQueryExecMode                                             string
	PGAllowedAddrs                                              map[string]struct{}
	AllowedOrigins                                              map[string]struct{}
	TrustedProxies                                              []netip.Prefix
	PGRootCAs                                                   *x509.CertPool
	QueryTimeout, WSIdleTimeout, WSWriteTimeout                 time.Duration
	UpstreamSlots, CancelSlots, HTTPSlots, ReadySlots           chan struct{}
	MaxHTTPRowBytes, MaxHTTPBufferedBytes, MaxHTTPResponseBytes int64
	OIDC                                                        *OIDCGate
	Metrics                                                     *Metrics
}

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
