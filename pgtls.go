package main

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

func loadPGRootCAs(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil // Go's system trust store
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("HERMIT_PG_CA_FILE contains no certificates")
	}
	return roots, nil
}

func (c config) pgTLSConfig(host string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, RootCAs: c.pgRootCAs}
}

func (c config) dialPostgres(addr string) (net.Conn, error) {
	backend, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if c.pgSSLMode == "disable" {
		return backend, nil
	}
	if c.pgSSLMode != "require" {
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
	secured := tls.Client(backend, c.pgTLSConfig(host))
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
