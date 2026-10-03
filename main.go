package main

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type config struct {
	listen, pgAddr, pgDatabase, pgUser, pgPassword, pgSSLMode, allowedOrigin string
	pgAllowedAddrs                                                           map[string]struct{}
	pgRootCAs                                                                *x509.CertPool
	consoleEnabled                                                           bool
	queryTimeout                                                             time.Duration
	wsIdleTimeout, wsWriteTimeout                                            time.Duration
	upstreamSlots                                                            chan struct{}
	cancelSlots                                                              chan struct{}
	httpSlots                                                                chan struct{}
	maxHTTPRowBytes, maxHTTPBufferedBytes, maxHTTPResponseBytes              int64
	oidc                                                                     *oidcGate
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	timeout, err := time.ParseDuration(env("HERMIT_QUERY_TIMEOUT", "30s"))
	if err != nil || timeout <= 0 {
		slog.Error("invalid HERMIT_QUERY_TIMEOUT")
		os.Exit(1)
	}
	wsIdleTimeout, err := time.ParseDuration(env("HERMIT_WS_IDLE_TIMEOUT", "30m"))
	if err != nil || wsIdleTimeout <= 0 {
		slog.Error("invalid HERMIT_WS_IDLE_TIMEOUT")
		os.Exit(1)
	}
	wsWriteTimeout, err := time.ParseDuration(env("HERMIT_WS_WRITE_TIMEOUT", "30s"))
	if err != nil || wsWriteTimeout <= 0 {
		slog.Error("invalid HERMIT_WS_WRITE_TIMEOUT")
		os.Exit(1)
	}
	maxConnections, err := strconv.Atoi(env("HERMIT_MAX_CONNECTIONS", "32"))
	if err != nil || maxConnections <= 0 {
		slog.Error("invalid HERMIT_MAX_CONNECTIONS")
		os.Exit(1)
	}
	maxHTTPQueries, err := positiveIntEnv("HERMIT_MAX_HTTP_QUERIES", 8)
	if err != nil || maxHTTPQueries > maxConnections {
		slog.Error("invalid HERMIT_MAX_HTTP_QUERIES")
		os.Exit(1)
	}
	maxHTTPRowMiB, err := positiveIntEnv("HERMIT_HTTP_MAX_ROW_MIB", 8)
	if err != nil {
		slog.Error("invalid HERMIT_HTTP_MAX_ROW_MIB")
		os.Exit(1)
	}
	maxHTTPBufferedMiB, err := positiveIntEnv("HERMIT_HTTP_MAX_BUFFERED_MIB", 4)
	if err != nil {
		slog.Error("invalid HERMIT_HTTP_MAX_BUFFERED_MIB")
		os.Exit(1)
	}
	maxHTTPResponseMiB, err := positiveIntEnv("HERMIT_HTTP_MAX_RESPONSE_MIB", 128)
	if err != nil {
		slog.Error("invalid HERMIT_HTTP_MAX_RESPONSE_MIB")
		os.Exit(1)
	}
	allowedAddrs, err := parseAllowedPGAddrs(os.Getenv("HERMIT_PG_ALLOWED_ADDRS"))
	if err != nil {
		slog.Error("invalid PostgreSQL routing configuration", "error", err)
		os.Exit(1)
	}
	defaultPGAddr := os.Getenv("HERMIT_PG_ADDR")
	if defaultPGAddr == "" && len(allowedAddrs) == 0 {
		defaultPGAddr = "127.0.0.1:5432"
	}
	if defaultPGAddr != "" {
		defaultPGAddr, err = canonicalPGAddr(defaultPGAddr)
		if err != nil {
			slog.Error("invalid HERMIT_PG_ADDR", "error", err)
			os.Exit(1)
		}
	}
	pgSSLMode := env("HERMIT_PG_SSLMODE", "require")
	if pgSSLMode != "require" && pgSSLMode != "disable" {
		slog.Error("HERMIT_PG_SSLMODE must be require or disable")
		os.Exit(1)
	}
	pgRootCAs, err := loadPGRootCAs(os.Getenv("HERMIT_PG_CA_FILE"))
	if err != nil {
		slog.Error("invalid HERMIT_PG_CA_FILE", "error", err)
		os.Exit(1)
	}
	cfg := config{
		listen:               env("HERMIT_LISTEN", ":8080"),
		pgAddr:               defaultPGAddr,
		pgAllowedAddrs:       allowedAddrs,
		pgDatabase:           env("HERMIT_PG_DATABASE", "postgres"),
		pgUser:               env("HERMIT_PG_USER", "postgres"),
		pgPassword:           os.Getenv("HERMIT_PG_PASSWORD"),
		pgSSLMode:            pgSSLMode,
		pgRootCAs:            pgRootCAs,
		allowedOrigin:        os.Getenv("HERMIT_ALLOWED_ORIGIN"),
		consoleEnabled:       strings.EqualFold(os.Getenv("HERMIT_CONSOLE"), "true"),
		queryTimeout:         timeout,
		wsIdleTimeout:        wsIdleTimeout,
		wsWriteTimeout:       wsWriteTimeout,
		upstreamSlots:        make(chan struct{}, maxConnections),
		cancelSlots:          make(chan struct{}, min(8, maxConnections)),
		httpSlots:            make(chan struct{}, maxHTTPQueries),
		maxHTTPRowBytes:      int64(maxHTTPRowMiB) << 20,
		maxHTTPBufferedBytes: int64(maxHTTPBufferedMiB) << 20,
		maxHTTPResponseBytes: int64(maxHTTPResponseMiB) << 20,
	}
	issuer := os.Getenv("HERMIT_OIDC_ISSUER")
	audience := os.Getenv("HERMIT_OIDC_AUDIENCE")
	if issuer != "" || audience != "" {
		cfg.oidc, err = newOIDCGate(context.Background(), issuer, audience, nil)
		if err != nil {
			slog.Error("OIDC configuration failed", "error", err)
			os.Exit(1)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /sql", gzipSQL(cfg.sql))
	mux.HandleFunc("OPTIONS /sql", cfg.preflight)
	mux.HandleFunc("GET /v1", cfg.websocket)
	mux.HandleFunc("GET /v2", cfg.websocket)
	if cfg.consoleEnabled {
		mux.HandleFunc("GET /", console)
		mux.HandleFunc("GET /console.mjs", consoleScript)
		mux.HandleFunc("GET /pgwire.mjs", consoleScript)
	}
	server := &http.Server{Addr: cfg.listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("hermit listening", "address", cfg.listen, "postgres", cfg.pgAddr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func positiveIntEnv(key string, fallback int) (int, error) {
	value, err := strconv.Atoi(env(key, strconv.Itoa(fallback)))
	if err != nil || value <= 0 || int64(value) > int64(^uint64(0)>>1)/(1<<20) {
		return 0, errors.New("invalid positive value")
	}
	return value, nil
}

func (c config) acquireUpstream() bool {
	select {
	case c.upstreamSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c config) releaseUpstream() {
	<-c.upstreamSlots
}

func (c config) acquireHTTP() bool {
	if c.httpSlots == nil {
		return true
	}
	select {
	case c.httpSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c config) releaseHTTP() {
	if c.httpSlots != nil {
		<-c.httpSlots
	}
}
