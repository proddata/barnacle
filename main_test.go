package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublicTLSConfig(t *testing.T) {
	if cfg, reloader, err := publicTLSConfig("", ""); err != nil || cfg != nil || reloader != nil {
		t.Fatalf("plaintext default: config = %v, error = %v", cfg, err)
	}
	for _, files := range [][2]string{{"cert.pem", ""}, {"", "key.pem"}} {
		if _, _, err := publicTLSConfig(files[0], files[1]); err == nil || !strings.Contains(err.Error(), "must both be set") {
			t.Fatalf("incomplete TLS pair %v: %v", files, err)
		}
	}
	if _, _, err := publicTLSConfig("missing-cert.pem", "missing-key.pem"); err == nil {
		t.Fatal("missing certificate pair accepted")
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), 0600); err != nil {
		t.Fatal(err)
	}
	_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongKeyBytes, err := x509.MarshalPKCS8PrivateKey(wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	wrongKeyFile := filepath.Join(dir, "wrong.key")
	if err := os.WriteFile(wrongKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: wrongKeyBytes}), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := publicTLSConfig(certFile, wrongKeyFile); err == nil {
		t.Fatal("mismatched certificate and key accepted")
	}
	cfg, _, err := publicTLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("minimum TLS version = %d", cfg.MinVersion)
	}
	parsedCertificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsedCertificate)
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	serverResult := make(chan error, 1)
	go func() { serverResult <- tls.Server(serverConn, cfg).Handshake() }()
	client := tls.Client(clientConn, &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err := client.Handshake(); err != nil {
		t.Fatalf("TLS client handshake: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("TLS server handshake: %v", err)
	}
}

func TestPublicTLSCertificateReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	firstCert, firstKey := testTLSKeyPair(t, 1)
	secondCert, secondKey := testTLSKeyPair(t, 2)
	writeTLSFile(t, certFile, firstCert)
	writeTLSFile(t, keyFile, firstKey)
	cfg, reloader, err := publicTLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := handshakeCertificateSerial(t, cfg); got != 1 {
		t.Fatalf("initial certificate serial = %d", got)
	}

	// Rotation may expose one new file before the other. Keep serving the old pair.
	writeTLSFile(t, certFile, secondCert)
	reloader.reload()
	if got := handshakeCertificateSerial(t, cfg); got != 1 {
		t.Fatalf("certificate during incomplete rotation = %d", got)
	}
	writeTLSFile(t, keyFile, secondKey)
	reloader.reload()
	if got := handshakeCertificateSerial(t, cfg); got != 2 {
		t.Fatalf("rotated certificate serial = %d", got)
	}

	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	reloader.reload()
	if got := handshakeCertificateSerial(t, cfg); got != 2 {
		t.Fatalf("certificate with missing key = %d", got)
	}
	writeTLSFile(t, keyFile, firstKey)
	writeTLSFile(t, certFile, firstCert)
	reloader.reload()
	if got := handshakeCertificateSerial(t, cfg); got != 1 {
		t.Fatalf("certificate after recovery = %d", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reloader.run(ctx, 5*time.Millisecond); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	writeTLSFile(t, certFile, secondCert)
	writeTLSFile(t, keyFile, secondKey)
	deadline := time.After(time.Second)
	for {
		if got := handshakeCertificateSerial(t, cfg); got == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("background reload did not pick up rotated certificate")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func testTLSKeyPair(t *testing.T, serial int64) ([]byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
}

func writeTLSFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
}

func handshakeCertificateSerial(t *testing.T, cfg *tls.Config) int64 {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	serverResult := make(chan error, 1)
	go func() { serverResult <- tls.Server(serverConn, cfg).Handshake() }()
	client := tls.Client(clientConn, &tls.Config{ServerName: "localhost", InsecureSkipVerify: true}) // Test reads the peer certificate directly.
	if err := client.Handshake(); err != nil {
		t.Fatalf("TLS client handshake: %v", err)
	}
	serial := client.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
	if err := <-serverResult; err != nil {
		t.Fatalf("TLS server handshake: %v", err)
	}
	return serial
}
