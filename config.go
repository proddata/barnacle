package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/proddata/barnacle/internal/gateway"
)

// settingNames is the one list of environment variables the server reads.
// loadSettings panics on a name missing from it, strict mode treats any other
// BARNACLE_* name as a typo, and tests keep it in step with the code and with
// the table in docs/deployment.md.
var settingNames = []string{
	"BARNACLE_ALLOWED_ORIGIN",
	"BARNACLE_HTTP_IDLE_TIMEOUT",
	"BARNACLE_HTTP_MAX_BUFFERED_MIB",
	"BARNACLE_HTTP_MAX_MEMORY_MIB",
	"BARNACLE_HTTP_MAX_REQUEST_MIB",
	"BARNACLE_HTTP_MAX_RESPONSE_MIB",
	"BARNACLE_HTTP_MAX_ROW_MIB",
	"BARNACLE_HTTP_READ_TIMEOUT",
	"BARNACLE_HTTP_STATEMENT_TIMEOUT",
	"BARNACLE_LISTEN",
	"BARNACLE_MAX_CLIENT_CONNECTIONS",
	"BARNACLE_MAX_CONNECTIONS",
	"BARNACLE_MAX_HTTP_QUERIES",
	"BARNACLE_METRICS",
	"BARNACLE_METRICS_LISTEN",
	"BARNACLE_METRICS_MAX_CONNECTIONS",
	"BARNACLE_PG_ADDR",
	"BARNACLE_PG_ALLOWED_ADDRS",
	"BARNACLE_PG_CA_FILE",
	"BARNACLE_PG_EXTRA_CA_FILE",
	"BARNACLE_PG_QUERY_EXEC_MODE",
	"BARNACLE_PG_SSLMODE",
	"BARNACLE_PG_TLS_SERVER_NAME",
	"BARNACLE_QUERY_TIMEOUT",
	"BARNACLE_READY_PG_ADDR",
	"BARNACLE_TLS_CERT_FILE",
	"BARNACLE_TLS_KEY_FILE",
	"BARNACLE_TRUSTED_PROXIES",
	"BARNACLE_WS_ALLOW_TRUST_AUTH",
	"BARNACLE_WS_IDLE_TIMEOUT",
	"BARNACLE_WS_WRITE_TIMEOUT",
}

// settings is everything main derives from the environment before it opens a
// listener. Building it has no side effects beyond reading the certificate and
// CA files, so -check-config can run it against a real deployment's
// environment.
type settings struct {
	cfg                   gateway.Config
	httpReadTimeout       time.Duration
	httpWriteTimeout      time.Duration
	httpIdleTimeout       time.Duration
	httpMemoryBytes       int64
	httpMemorySource      string
	metricsEnabled        bool
	metricsListen         string
	maxClientConnections  int
	metricsMaxConnections int
	tlsConfig             *tls.Config
	certificateReload     *publicCertificateReloader
}

// envReader collects every configuration problem so an operator sees them all
// in one run instead of fixing them one restart at a time.
type envReader struct {
	getenv func(string) string
	read   map[string]bool
	errs   []error
}

func (r *envReader) failf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

func (r *envReader) env(key, fallback string) string {
	if v := r.getenv(key); v != "" {
		return v
	}
	return fallback
}

// duration records an error when the value does not parse or is below min.
func (r *envReader) duration(key, fallback string, min time.Duration) time.Duration {
	value, err := time.ParseDuration(r.env(key, fallback))
	if err != nil || value < min {
		r.failf("invalid %s", key)
		return 0
	}
	return value
}

// boolean accepts only true or false so a typo cannot silently relax a policy.
func (r *envReader) boolean(key string, fallback bool) bool {
	switch value := strings.ToLower(r.getenv(key)); value {
	case "":
		return fallback
	case "true":
		return true
	case "false":
		return false
	default:
		r.failf("%s must be true or false", key)
		return fallback
	}
}

func (r *envReader) choice(key, fallback string, choices ...string) string {
	value := r.env(key, fallback)
	if !slices.Contains(choices, value) {
		r.failf("%s must be %s", key, strings.Join(choices, ", "))
	}
	return value
}

func (r *envReader) positiveInt(key string, fallback int) int {
	value, err := strconv.Atoi(r.env(key, strconv.Itoa(fallback)))
	if err != nil || value <= 0 {
		r.failf("invalid %s", key)
		return fallback
	}
	return value
}

// mib rejects values whose byte count would overflow int64.
func (r *envReader) mib(key string, fallback int) int {
	value := r.positiveInt(key, fallback)
	if int64(value) > math.MaxInt64/(1<<20) {
		r.failf("invalid %s", key)
		return fallback
	}
	return value
}

func (r *envReader) check(key string, err error) {
	if err != nil {
		r.failf("invalid %s: %w", key, err)
	}
}

// loadSettings builds the settings from getenv. With strict set, environ (in
// os.Environ form) is also scanned and any BARNACLE_* name the server never
// reads is an error, which catches typos that would otherwise leave a default
// in place.
func loadSettings(lookup func(string) string, environ []string, strict bool) (*settings, error) {
	read := map[string]bool{}
	getenv := func(key string) string {
		if !slices.Contains(settingNames, key) {
			panic("setting " + key + " is missing from settingNames")
		}
		read[key] = true
		return lookup(key)
	}
	r := &envReader{getenv: getenv, read: read}
	s := &settings{}

	timeout := r.duration("BARNACLE_QUERY_TIMEOUT", "10m", 1)
	statementTimeout := r.duration("BARNACLE_HTTP_STATEMENT_TIMEOUT", "60s", time.Millisecond)
	s.httpReadTimeout = r.duration("BARNACLE_HTTP_READ_TIMEOUT", "15s", 1)
	// Give the SQL handler's request deadline time to finish before the HTTP
	// server cuts off its response write.
	s.httpWriteTimeout = timeout + time.Second
	if s.httpWriteTimeout <= timeout {
		r.failf("BARNACLE_QUERY_TIMEOUT is too large")
	}
	s.httpIdleTimeout = r.duration("BARNACLE_HTTP_IDLE_TIMEOUT", "60s", 1)
	wsIdleTimeout := r.duration("BARNACLE_WS_IDLE_TIMEOUT", "30m", 1)
	wsWriteTimeout := r.duration("BARNACLE_WS_WRITE_TIMEOUT", "30s", 1)
	maxConnections := r.positiveInt("BARNACLE_MAX_CONNECTIONS", 32)
	s.maxClientConnections = r.positiveInt("BARNACLE_MAX_CLIENT_CONNECTIONS", 256)
	s.metricsMaxConnections = r.positiveInt("BARNACLE_METRICS_MAX_CONNECTIONS", 16)
	maxHTTPQueries := r.positiveInt("BARNACLE_MAX_HTTP_QUERIES", 8)
	if maxHTTPQueries > maxConnections {
		r.failf("invalid BARNACLE_MAX_HTTP_QUERIES: exceeds BARNACLE_MAX_CONNECTIONS")
	}
	maxHTTPRequestMiB := r.mib("BARNACLE_HTTP_MAX_REQUEST_MIB", 10)
	maxHTTPRowMiB := r.mib("BARNACLE_HTTP_MAX_ROW_MIB", 8)
	maxHTTPBufferedMiB := r.mib("BARNACLE_HTTP_MAX_BUFFERED_MIB", 10)
	maxHTTPResponseMiB := r.mib("BARNACLE_HTTP_MAX_RESPONSE_MIB", 2048)
	// An explicit setting wins; otherwise follow the memory limit the service
	// runs under, so raising that limit raises the budget with it.
	s.httpMemoryBytes, s.httpMemorySource = gateway.DefaultMemoryBudget(96 << 20)
	if getenv("BARNACLE_HTTP_MAX_MEMORY_MIB") != "" {
		s.httpMemoryBytes, s.httpMemorySource = int64(r.mib("BARNACLE_HTTP_MAX_MEMORY_MIB", 96))<<20, "BARNACLE_HTTP_MAX_MEMORY_MIB"
	}

	allowedAddrs, err := gateway.ParseAllowedPGAddrs(getenv("BARNACLE_PG_ALLOWED_ADDRS"))
	r.check("BARNACLE_PG_ALLOWED_ADDRS", err)
	defaultPGAddr := getenv("BARNACLE_PG_ADDR")
	if defaultPGAddr == "" && len(allowedAddrs) == 0 {
		defaultPGAddr = "127.0.0.1:5432"
	}
	if defaultPGAddr != "" {
		defaultPGAddr, err = gateway.CanonicalPGAddr(defaultPGAddr)
		if err != nil {
			r.check("BARNACLE_PG_ADDR", err)
		} else if len(allowedAddrs) > 0 {
			_, anyAllowed := allowedAddrs["*"]
			if _, ok := allowedAddrs[defaultPGAddr]; !ok && !anyAllowed {
				r.failf("BARNACLE_PG_ADDR must appear in BARNACLE_PG_ALLOWED_ADDRS when routing is enabled")
			}
		}
	}
	readyPGAddr := getenv("BARNACLE_READY_PG_ADDR")
	if readyPGAddr != "" {
		readyPGAddr, err = gateway.CanonicalPGAddr(readyPGAddr)
		r.check("BARNACLE_READY_PG_ADDR", err)
	}
	pgSSLMode := r.choice("BARNACLE_PG_SSLMODE", "require", "require", "disable")
	pgQueryExecMode := r.choice("BARNACLE_PG_QUERY_EXEC_MODE", "exec", "exec", "cache_describe", "cache_statement")
	pgRootCAs, err := gateway.LoadPGRootCAs(getenv("BARNACLE_PG_CA_FILE"), getenv("BARNACLE_PG_EXTRA_CA_FILE"))
	r.check("PostgreSQL CA file", err)
	allowedOrigins, err := gateway.ParseAllowedOrigins(getenv("BARNACLE_ALLOWED_ORIGIN"))
	r.check("BARNACLE_ALLOWED_ORIGIN", err)
	trustedProxies, err := gateway.ParseTrustedProxies(getenv("BARNACLE_TRUSTED_PROXIES"))
	r.check("BARNACLE_TRUSTED_PROXIES", err)

	metricsSetting := getenv("BARNACLE_METRICS")
	if metricsSetting != "" && !strings.EqualFold(metricsSetting, "true") && !strings.EqualFold(metricsSetting, "false") {
		r.failf("BARNACLE_METRICS must be true or false")
	}
	s.metricsEnabled = metricsSetting == "" || strings.EqualFold(metricsSetting, "true")
	s.metricsListen = getenv("BARNACLE_METRICS_LISTEN")

	allowTrust := r.boolean("BARNACLE_WS_ALLOW_TRUST_AUTH", false)
	s.tlsConfig, s.certificateReload, err = publicTLSConfig(getenv("BARNACLE_TLS_CERT_FILE"), getenv("BARNACLE_TLS_KEY_FILE"))
	if err != nil {
		r.failf("invalid public TLS configuration: %w", err)
	}

	// These channels bound concurrent database work across both transports.
	// Cancellation uses a separate, smaller budget because it opens a new
	// PostgreSQL connection to stop abandoned WebSocket queries.
	s.cfg = gateway.Config{
		Listen:               r.env("BARNACLE_LISTEN", ":8080"),
		PGAddr:               defaultPGAddr,
		ReadyPGAddr:          readyPGAddr,
		PGAllowedAddrs:       allowedAddrs,
		PGSSLMode:            pgSSLMode,
		PGQueryExecMode:      pgQueryExecMode,
		AllowTrustLogin:      allowTrust,
		PGTLSServerName:      getenv("BARNACLE_PG_TLS_SERVER_NAME"),
		PGRootCAs:            pgRootCAs,
		AllowedOrigins:       allowedOrigins,
		TrustedProxies:       trustedProxies,
		QueryTimeout:         timeout,
		StatementTimeout:     statementTimeout,
		WSIdleTimeout:        wsIdleTimeout,
		WSWriteTimeout:       wsWriteTimeout,
		UpstreamSlots:        make(chan struct{}, maxConnections),
		CancelSlots:          make(chan struct{}, min(8, maxConnections)),
		HTTPSlots:            make(chan struct{}, maxHTTPQueries),
		ReadySlots:           make(chan struct{}, 1),
		MaxHTTPRequestBytes:  int64(maxHTTPRequestMiB) << 20,
		MaxHTTPRowBytes:      int64(maxHTTPRowMiB) << 20,
		MaxHTTPBufferedBytes: int64(maxHTTPBufferedMiB) << 20,
		MaxHTTPResponseBytes: int64(maxHTTPResponseMiB) << 20,
		HTTPMemory:           gateway.NewMemoryBudget(s.httpMemoryBytes),
		Metrics:              &gateway.Metrics{},
	}

	if strict {
		var unknown []string
		for _, entry := range environ {
			name, _, _ := strings.Cut(entry, "=")
			if strings.HasPrefix(name, "BARNACLE_") && !slices.Contains(settingNames, name) {
				unknown = append(unknown, name)
			}
		}
		slices.Sort(unknown)
		for _, name := range slices.Compact(unknown) {
			r.failf("unknown setting %s", name)
		}
	}

	if len(r.errs) > 0 {
		return nil, errors.Join(r.errs...)
	}
	return s, nil
}
