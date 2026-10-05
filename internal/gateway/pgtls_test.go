package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadPGRootCAsAddsExtraFile(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(5), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/ca.pem"
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	roots, err := LoadPGRootCAs("", path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
		t.Fatalf("extra CA not trusted: %v", err)
	}
}

func TestUpstreamTLSVerifiesConfiguredServerName(t *testing.T) {
	const certName = "db.service.example"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverCert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: certName},
		DNSNames:  []string{certName},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, serverCert, serverCert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	for _, tc := range []struct {
		name, serverName, wantError string
	}{
		{"matching name", certName, ""},
		{"wrong name", "other.service.example", "certificate is valid for"},
		{"unset name", "", "IP SANs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var request [8]byte
				if _, err := io.ReadFull(conn, request[:]); err != nil || request != pgSSLRequest {
					return
				}
				if _, err := conn.Write([]byte{'S'}); err != nil {
					return
				}
				secured := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
				_ = secured.Handshake()
			}()
			cfg := Config{PGSSLMode: "require", PGRootCAs: roots, PGTLSServerName: tc.serverName}
			conn, err := cfg.DialPostgres(listener.Addr().String())
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("dial with matching certificate name: %v", err)
				}
				conn.Close()
			} else {
				if err == nil {
					conn.Close()
					t.Fatal("accepted a certificate with the wrong server name")
				}
				if !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("wanted %q, got %v", tc.wantError, err)
				}
			}
			<-done
		})
	}
}

func TestWebSocketUpstreamTLSRejectsPlaintext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var request [8]byte
		if _, err := io.ReadFull(conn, request[:]); err == nil && request == pgSSLRequest {
			_, _ = conn.Write([]byte{'N'})
		}
	}()
	if conn, err := (Config{PGSSLMode: "require"}).DialPostgres(listener.Addr().String()); err == nil {
		conn.Close()
		t.Fatal("accepted a plaintext PostgreSQL server")
	}
	<-done
}
