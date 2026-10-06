package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/proddata/barnacle/internal/gateway"
	"github.com/proddata/barnacle/internal/pgws"
	"github.com/proddata/barnacle/internal/sqlhttp"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	timeout, err := time.ParseDuration(env("BARNACLE_QUERY_TIMEOUT", "30s"))
	if err != nil || timeout <= 0 {
		slog.Error("invalid BARNACLE_QUERY_TIMEOUT")
		os.Exit(1)
	}
	httpReadTimeout, err := time.ParseDuration(env("BARNACLE_HTTP_READ_TIMEOUT", "15s"))
	if err != nil || httpReadTimeout <= 0 {
		slog.Error("invalid BARNACLE_HTTP_READ_TIMEOUT")
		os.Exit(1)
	}
	httpWriteTimeout, err := time.ParseDuration(env("BARNACLE_HTTP_WRITE_TIMEOUT", "60s"))
	if err != nil || httpWriteTimeout <= 0 {
		slog.Error("invalid BARNACLE_HTTP_WRITE_TIMEOUT")
		os.Exit(1)
	}
	httpIdleTimeout, err := time.ParseDuration(env("BARNACLE_HTTP_IDLE_TIMEOUT", "60s"))
	if err != nil || httpIdleTimeout <= 0 {
		slog.Error("invalid BARNACLE_HTTP_IDLE_TIMEOUT")
		os.Exit(1)
	}
	wsIdleTimeout, err := time.ParseDuration(env("BARNACLE_WS_IDLE_TIMEOUT", "30m"))
	if err != nil || wsIdleTimeout <= 0 {
		slog.Error("invalid BARNACLE_WS_IDLE_TIMEOUT")
		os.Exit(1)
	}
	wsWriteTimeout, err := time.ParseDuration(env("BARNACLE_WS_WRITE_TIMEOUT", "30s"))
	if err != nil || wsWriteTimeout <= 0 {
		slog.Error("invalid BARNACLE_WS_WRITE_TIMEOUT")
		os.Exit(1)
	}
	maxConnections, err := strconv.Atoi(env("BARNACLE_MAX_CONNECTIONS", "32"))
	if err != nil || maxConnections <= 0 {
		slog.Error("invalid BARNACLE_MAX_CONNECTIONS")
		os.Exit(1)
	}
	maxHTTPQueries, err := positiveIntEnv("BARNACLE_MAX_HTTP_QUERIES", 8)
	if err != nil || maxHTTPQueries > maxConnections {
		slog.Error("invalid BARNACLE_MAX_HTTP_QUERIES")
		os.Exit(1)
	}
	maxHTTPRowMiB, err := positiveIntEnv("BARNACLE_HTTP_MAX_ROW_MIB", 8)
	if err != nil {
		slog.Error("invalid BARNACLE_HTTP_MAX_ROW_MIB")
		os.Exit(1)
	}
	maxHTTPBufferedMiB, err := positiveIntEnv("BARNACLE_HTTP_MAX_BUFFERED_MIB", 4)
	if err != nil {
		slog.Error("invalid BARNACLE_HTTP_MAX_BUFFERED_MIB")
		os.Exit(1)
	}
	maxHTTPResponseMiB, err := positiveIntEnv("BARNACLE_HTTP_MAX_RESPONSE_MIB", 128)
	if err != nil {
		slog.Error("invalid BARNACLE_HTTP_MAX_RESPONSE_MIB")
		os.Exit(1)
	}
	allowedAddrs, err := gateway.ParseAllowedPGAddrs(os.Getenv("BARNACLE_PG_ALLOWED_ADDRS"))
	if err != nil {
		slog.Error("invalid PostgreSQL routing configuration", "error", err)
		os.Exit(1)
	}
	defaultPGAddr := os.Getenv("BARNACLE_PG_ADDR")
	if defaultPGAddr == "" && len(allowedAddrs) == 0 {
		defaultPGAddr = "127.0.0.1:5432"
	}
	if defaultPGAddr != "" {
		defaultPGAddr, err = gateway.CanonicalPGAddr(defaultPGAddr)
		if err != nil {
			slog.Error("invalid BARNACLE_PG_ADDR", "error", err)
			os.Exit(1)
		}
		if len(allowedAddrs) > 0 {
			_, anyAllowed := allowedAddrs["*"]
			if _, ok := allowedAddrs[defaultPGAddr]; !ok && !anyAllowed {
				slog.Error("BARNACLE_PG_ADDR must appear in BARNACLE_PG_ALLOWED_ADDRS when routing is enabled")
				os.Exit(1)
			}
		}
	}
	readyPGAddr := os.Getenv("BARNACLE_READY_PG_ADDR")
	if readyPGAddr != "" {
		readyPGAddr, err = gateway.CanonicalPGAddr(readyPGAddr)
		if err != nil {
			slog.Error("invalid BARNACLE_READY_PG_ADDR", "error", err)
			os.Exit(1)
		}
	}
	pgSSLMode := env("BARNACLE_PG_SSLMODE", "require")
	if pgSSLMode != "require" && pgSSLMode != "disable" {
		slog.Error("BARNACLE_PG_SSLMODE must be require or disable")
		os.Exit(1)
	}
	pgQueryExecMode := env("BARNACLE_PG_QUERY_EXEC_MODE", "exec")
	if pgQueryExecMode != "exec" && pgQueryExecMode != "cache_describe" && pgQueryExecMode != "cache_statement" {
		slog.Error("BARNACLE_PG_QUERY_EXEC_MODE must be exec, cache_describe, or cache_statement")
		os.Exit(1)
	}
	pgRootCAs, err := gateway.LoadPGRootCAs(os.Getenv("BARNACLE_PG_CA_FILE"), os.Getenv("BARNACLE_PG_EXTRA_CA_FILE"))
	if err != nil {
		slog.Error("invalid PostgreSQL CA file", "error", err)
		os.Exit(1)
	}
	allowedOrigins, err := gateway.ParseAllowedOrigins(os.Getenv("BARNACLE_ALLOWED_ORIGIN"))
	if err != nil {
		slog.Error("invalid BARNACLE_ALLOWED_ORIGIN", "error", err)
		os.Exit(1)
	}
	trustedProxies, err := gateway.ParseTrustedProxies(os.Getenv("BARNACLE_TRUSTED_PROXIES"))
	if err != nil {
		slog.Error("invalid BARNACLE_TRUSTED_PROXIES", "error", err)
		os.Exit(1)
	}
	cfg := gateway.Config{
		Listen:               env("BARNACLE_LISTEN", ":8080"),
		PGAddr:               defaultPGAddr,
		ReadyPGAddr:          readyPGAddr,
		PGAllowedAddrs:       allowedAddrs,
		PGSSLMode:            pgSSLMode,
		PGQueryExecMode:      pgQueryExecMode,
		PGTLSServerName:      os.Getenv("BARNACLE_PG_TLS_SERVER_NAME"),
		PGRootCAs:            pgRootCAs,
		AllowedOrigins:       allowedOrigins,
		TrustedProxies:       trustedProxies,
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
	issuer := os.Getenv("BARNACLE_OIDC_ISSUER")
	audience := os.Getenv("BARNACLE_OIDC_AUDIENCE")
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
	metricsSetting := os.Getenv("BARNACLE_METRICS")
	if metricsSetting != "" && !strings.EqualFold(metricsSetting, "true") && !strings.EqualFold(metricsSetting, "false") {
		slog.Error("BARNACLE_METRICS must be true or false")
		os.Exit(1)
	}
	metricsEnabled := strings.EqualFold(metricsSetting, "true")
	metricsListen := os.Getenv("BARNACLE_METRICS_LISTEN")
	if metricsListen == "" {
		metricsListen = "127.0.0.1:9090"
	}
	mux.HandleFunc("GET /metrics", http.NotFound)
	mux.HandleFunc("GET /v2", wsHandler.Serve)
	server := &http.Server{
		Addr: cfg.Listen, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    32 << 10,
	}
	var certificateReloader *publicCertificateReloader
	server.TLSConfig, certificateReloader, err = publicTLSConfig(os.Getenv("BARNACLE_TLS_CERT_FILE"), os.Getenv("BARNACLE_TLS_KEY_FILE"))
	if err != nil {
		slog.Error("invalid public TLS configuration", "error", err)
		os.Exit(1)
	}
	mainListener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		slog.Error("failed to listen", "address", cfg.Listen, "error", err)
		os.Exit(1)
	}
	var metricsServer *http.Server
	var metricsListener net.Listener
	if metricsEnabled {
		metricsListener, err = net.Listen("tcp", metricsListen)
		if err != nil {
			mainListener.Close()
			slog.Error("failed to listen for metrics", "address", metricsListen, "error", err)
			os.Exit(1)
		}
		metricsMux := http.NewServeMux()
		metricsMux.HandleFunc("GET /metrics", cfg.Metrics.Serve)
		metricsServer = &http.Server{
			Handler:           metricsMux,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       httpReadTimeout,
			WriteTimeout:      httpWriteTimeout,
			IdleTimeout:       httpIdleTimeout,
			MaxHeaderBytes:    32 << 10,
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if certificateReloader != nil {
		go certificateReloader.run(ctx, time.Minute)
	}
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		stop() // A second signal uses the operating system's default handling.
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		results := make(chan error, 3)
		go func() { results <- server.Shutdown(shutdown) }()
		go func() { results <- wsHandler.Shutdown(shutdown) }()
		count := 2
		if metricsServer != nil {
			count++
			go func() { results <- metricsServer.Shutdown(shutdown) }()
		}
		for range count {
			if err := <-results; err != nil {
				slog.Warn("shutdown did not complete cleanly", "error", err)
			}
		}
		close(shutdownDone)
	}()
	serveResults := make(chan error, 2)
	if server.TLSConfig != nil {
		go func() { serveResults <- server.ServeTLS(mainListener, "", "") }()
	} else {
		go func() { serveResults <- server.Serve(mainListener) }()
	}
	slog.Info("barnacle listening", "address", mainListener.Addr().String(), "postgres", cfg.PGAddr, "tls", server.TLSConfig != nil)
	if metricsServer != nil {
		go func() { serveResults <- metricsServer.Serve(metricsListener) }()
		slog.Info("metrics listening", "address", metricsListener.Addr().String())
	}
	serveErr := <-serveResults
	interrupted := ctx.Err() != nil
	stop()
	<-shutdownDone
	if !interrupted || !errors.Is(serveErr, http.ErrServerClosed) {
		slog.Error("server stopped", "error", serveErr)
		os.Exit(1)
	}
}

func publicTLSConfig(certFile, keyFile string) (*tls.Config, *publicCertificateReloader, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, nil, errors.New("BARNACLE_TLS_CERT_FILE and BARNACLE_TLS_KEY_FILE must both be set")
	}
	certificate, certHash, keyHash, err := loadPublicKeyPair(certFile, keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("load public TLS certificate and key: %w", err)
	}
	reloader := &publicCertificateReloader{
		certFile: certFile, keyFile: keyFile,
		certHash: certHash, keyHash: keyHash,
	}
	reloader.certificate.Store(certificate)
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: reloader.getCertificate}, reloader, nil
}

type publicCertificateReloader struct {
	mu          sync.Mutex
	certFile    string
	keyFile     string
	certificate atomic.Pointer[tls.Certificate]
	certHash    [sha256.Size]byte
	keyHash     [sha256.Size]byte
	lastError   string
}

func loadPublicKeyPair(certFile, keyFile string) (*tls.Certificate, [sha256.Size]byte, [sha256.Size]byte, error) {
	certPEM, keyPEM, err := readPublicKeyPairFiles(certFile, keyFile)
	if err != nil {
		return nil, [sha256.Size]byte{}, [sha256.Size]byte{}, err
	}
	certHash, keyHash := sha256.Sum256(certPEM), sha256.Sum256(keyPEM)
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, certHash, keyHash, err
	}
	return &certificate, certHash, keyHash, nil
}

func readPublicKeyPairFiles(certFile, keyFile string) ([]byte, []byte, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read private key: %w", err)
	}
	return certPEM, keyPEM, nil
}

func (r *publicCertificateReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.certificate.Load(), nil
}

func (r *publicCertificateReloader) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reload()
		}
	}
}

func (r *publicCertificateReloader) reload() {
	r.mu.Lock()
	defer r.mu.Unlock()
	certPEM, keyPEM, err := readPublicKeyPairFiles(r.certFile, r.keyFile)
	var certHash, keyHash [sha256.Size]byte
	if err == nil {
		certHash, keyHash = sha256.Sum256(certPEM), sha256.Sum256(keyPEM)
		if certHash == r.certHash && keyHash == r.keyHash {
			r.lastError = ""
			return
		}
	}
	var certificate tls.Certificate
	if err == nil {
		certificate, err = tls.X509KeyPair(certPEM, keyPEM)
	}
	if err != nil {
		if message := err.Error(); message != r.lastError {
			slog.Error("failed to reload public TLS certificate and key; keeping previous pair", "error", err)
			r.lastError = message
		}
		return
	}
	r.lastError = ""
	r.certificate.Store(&certificate)
	r.certHash, r.keyHash = certHash, keyHash
	slog.Info("reloaded public TLS certificate and key")
}

func positiveIntEnv(key string, fallback int) (int, error) {
	value, err := strconv.Atoi(env(key, strconv.Itoa(fallback)))
	if err != nil || value <= 0 || int64(value) > int64(^uint64(0)>>1)/(1<<20) {
		return 0, errors.New("invalid positive value")
	}
	return value, nil
}
