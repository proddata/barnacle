package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

var pgSSLRequest = [8]byte{0, 0, 0, 8, 4, 210, 22, 47}

func LoadPGRootCAs(paths ...string) (*x509.CertPool, error) {
	if len(paths) == 0 || len(paths) == 1 && paths[0] == "" {
		return nil, nil // Go's system trust store
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system CA certificates: %w", err)
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("load PostgreSQL CA file %q: %w", path, err)
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("PostgreSQL CA file %q contains no certificates", path)
		}
	}
	return roots, nil
}

func (c Config) PGTLSConfig(host string) *tls.Config {
	serverName := host
	if c.PGTLSServerName != "" {
		serverName = c.PGTLSServerName
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName, RootCAs: c.PGRootCAs}
}

func (c Config) DialPostgres(addr string) (net.Conn, error) {
	backend, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if c.PGSSLMode == "disable" {
		return backend, nil
	}
	if c.PGSSLMode != "require" {
		backend.Close()
		return nil, errors.New("HERMIT_PG_SSLMODE must be disable or require")
	}
	if err := backend.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		backend.Close()
		return nil, err
	}
	if err := writeFull(backend, pgSSLRequest[:]); err != nil {
		backend.Close()
		return nil, err
	}
	var response [1]byte
	if _, err := io.ReadFull(backend, response[:]); err != nil {
		backend.Close()
		return nil, err
	}
	if response[0] != 'S' {
		backend.Close()
		return nil, fmt.Errorf("PostgreSQL TLS required, server replied %q", response[0])
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		backend.Close()
		return nil, err
	}
	secured := tls.Client(backend, c.PGTLSConfig(host))
	if err := secured.Handshake(); err != nil {
		backend.Close()
		return nil, err
	}
	if err := secured.SetDeadline(time.Time{}); err != nil {
		secured.Close()
		return nil, err
	}
	return secured, nil
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
