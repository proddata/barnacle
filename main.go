// Barnacle serves SQL over HTTP and PostgreSQL wire traffic over WebSocket.
// PostgreSQL authenticates clients; this process enforces routing, transport,
// origin, and resource policies around those connections.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/proddata/barnacle/internal/pgws"
	"github.com/proddata/barnacle/internal/sqlhttp"
)

func main() {
	checkConfig := flag.Bool("check-config", false, "validate the BARNACLE_* environment and exit without listening")
	flag.Parse()
	s, err := loadSettings(os.Getenv)
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if *checkConfig {
		fmt.Println("configuration ok")
		return
	}
	cfg := s.cfg
	httpMemoryBytes, httpMemorySource := s.httpMemoryBytes, s.httpMemorySource
	metricsEnabled, metricsListen := s.metricsEnabled, s.metricsListen
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", cfg.Ready)
	sqlHandler := sqlhttp.New(&cfg)
	wsHandler := pgws.New(&cfg)
	mux.HandleFunc("POST /sql", cfg.Metrics.MeasureSQL(sqlhttp.Gzip(sqlHandler.Serve)))
	mux.HandleFunc("OPTIONS /sql", cfg.Preflight)
	if metricsEnabled && metricsListen == "" {
		mux.HandleFunc("GET /metrics", cfg.Metrics.Serve)
	} else {
		mux.HandleFunc("GET /metrics", http.NotFound)
	}
	mux.HandleFunc("GET /v2", wsHandler.Serve)
	server := &http.Server{
		Addr: cfg.Listen, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       s.httpReadTimeout,
		WriteTimeout:      s.httpWriteTimeout,
		IdleTimeout:       s.httpIdleTimeout,
		MaxHeaderBytes:    32 << 10,
	}
	server.TLSConfig = s.tlsConfig
	certificateReloader := s.certificateReload
	mainListener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		slog.Error("failed to listen", "address", cfg.Listen, "error", err)
		os.Exit(1)
	}
	var metricsServer *http.Server
	var metricsListener net.Listener
	if metricsEnabled && metricsListen != "" {
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
			ReadTimeout:       s.httpReadTimeout,
			WriteTimeout:      s.httpWriteTimeout,
			IdleTimeout:       s.httpIdleTimeout,
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
	slog.Info("buffered HTTP result memory budget", "mib", httpMemoryBytes>>20, "source", httpMemorySource)
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
