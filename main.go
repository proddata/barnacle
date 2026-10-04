package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/proddata/hermit/internal/gateway"
	"github.com/proddata/hermit/internal/pgws"
	"github.com/proddata/hermit/internal/sqlhttp"
)

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
	httpReadTimeout, err := time.ParseDuration(env("HERMIT_HTTP_READ_TIMEOUT", "15s"))
	if err != nil || httpReadTimeout <= 0 {
		slog.Error("invalid HERMIT_HTTP_READ_TIMEOUT")
		os.Exit(1)
	}
	httpIdleTimeout, err := time.ParseDuration(env("HERMIT_HTTP_IDLE_TIMEOUT", "60s"))
	if err != nil || httpIdleTimeout <= 0 {
		slog.Error("invalid HERMIT_HTTP_IDLE_TIMEOUT")
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
	allowedAddrs, err := gateway.ParseAllowedPGAddrs(os.Getenv("HERMIT_PG_ALLOWED_ADDRS"))
	if err != nil {
		slog.Error("invalid PostgreSQL routing configuration", "error", err)
		os.Exit(1)
	}
	defaultPGAddr := os.Getenv("HERMIT_PG_ADDR")
	if defaultPGAddr == "" && len(allowedAddrs) == 0 {
		defaultPGAddr = "127.0.0.1:5432"
	}
	if defaultPGAddr != "" {
		defaultPGAddr, err = gateway.CanonicalPGAddr(defaultPGAddr)
		if err != nil {
			slog.Error("invalid HERMIT_PG_ADDR", "error", err)
			os.Exit(1)
		}
		if len(allowedAddrs) > 0 {
			if _, ok := allowedAddrs[defaultPGAddr]; !ok {
				slog.Error("HERMIT_PG_ADDR must appear in HERMIT_PG_ALLOWED_ADDRS when routing is enabled")
				os.Exit(1)
			}
		}
	}
	readyPGAddr := os.Getenv("HERMIT_READY_PG_ADDR")
	if readyPGAddr != "" {
		readyPGAddr, err = gateway.CanonicalPGAddr(readyPGAddr)
		if err != nil {
			slog.Error("invalid HERMIT_READY_PG_ADDR", "error", err)
			os.Exit(1)
		}
	}
	pgSSLMode := env("HERMIT_PG_SSLMODE", "require")
	if pgSSLMode != "require" && pgSSLMode != "disable" {
		slog.Error("HERMIT_PG_SSLMODE must be require or disable")
		os.Exit(1)
	}
	pgRootCAs, err := gateway.LoadPGRootCAs(os.Getenv("HERMIT_PG_CA_FILE"))
	if err != nil {
		slog.Error("invalid HERMIT_PG_CA_FILE", "error", err)
		os.Exit(1)
	}
	cfg := gateway.Config{
		Listen:               env("HERMIT_LISTEN", ":8080"),
		PGAddr:               defaultPGAddr,
		ReadyPGAddr:          readyPGAddr,
		PGAllowedAddrs:       allowedAddrs,
		PGDatabase:           env("HERMIT_PG_DATABASE", "postgres"),
		PGUser:               env("HERMIT_PG_USER", "postgres"),
		PGPassword:           os.Getenv("HERMIT_PG_PASSWORD"),
		PGSSLMode:            pgSSLMode,
		PGRootCAs:            pgRootCAs,
		AllowedOrigin:        os.Getenv("HERMIT_ALLOWED_ORIGIN"),
		QueryTimeout:         timeout,
		WSIdleTimeout:        wsIdleTimeout,
		WSWriteTimeout:       wsWriteTimeout,
		UpstreamSlots:        make(chan struct{}, maxConnections),
		CancelSlots:          make(chan struct{}, min(8, maxConnections)),
		HTTPSlots:            make(chan struct{}, maxHTTPQueries),
		ReadySlots:           make(chan struct{}, 1),
		MaxHTTPRowBytes:      int64(maxHTTPRowMiB) << 20,
		MaxHTTPBufferedBytes: int64(maxHTTPBufferedMiB) << 20,
		MaxHTTPResponseBytes: int64(maxHTTPResponseMiB) << 20,
		Metrics:              &gateway.Metrics{},
	}
	issuer := os.Getenv("HERMIT_OIDC_ISSUER")
	audience := os.Getenv("HERMIT_OIDC_AUDIENCE")
	if issuer != "" || audience != "" {
		cfg.OIDC, err = gateway.NewOIDCGate(context.Background(), issuer, audience, nil)
		if err != nil {
			slog.Error("OIDC configuration failed", "error", err)
			os.Exit(1)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", cfg.Ready)
	sqlHandler := sqlhttp.New(&cfg)
	wsHandler := pgws.New(&cfg)
	mux.HandleFunc("POST /sql", cfg.Metrics.MeasureSQL(sqlhttp.Gzip(sqlHandler.Serve)))
	mux.HandleFunc("OPTIONS /sql", cfg.Preflight)
	if strings.EqualFold(os.Getenv("HERMIT_METRICS"), "true") {
		mux.HandleFunc("GET /metrics", cfg.Metrics.Serve)
	} else {
		mux.HandleFunc("GET /metrics", http.NotFound)
	}
	mux.HandleFunc("GET /v1", wsHandler.Serve)
	mux.HandleFunc("GET /v2", wsHandler.Serve)
	server := &http.Server{
		Addr: cfg.Listen, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    32 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		stop() // A second signal uses the operating system's default handling.
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		results := make(chan error, 2)
		go func() { results <- server.Shutdown(shutdown) }()
		go func() { results <- wsHandler.Shutdown(shutdown) }()
		for range 2 {
			if err := <-results; err != nil {
				slog.Warn("shutdown did not complete cleanly", "error", err)
			}
		}
		close(shutdownDone)
	}()
	slog.Info("hermit listening", "address", cfg.Listen, "postgres", cfg.PGAddr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		stop()
		<-shutdownDone
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
	<-shutdownDone
}

func positiveIntEnv(key string, fallback int) (int, error) {
	value, err := strconv.Atoi(env(key, strconv.Itoa(fallback)))
	if err != nil || value <= 0 || int64(value) > int64(^uint64(0)>>1)/(1<<20) {
		return 0, errors.New("invalid positive value")
	}
	return value, nil
}
