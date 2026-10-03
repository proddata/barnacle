package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func TestWebSocketUpstreamTLSVerifiesCertificate(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverCert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames:  []string{"localhost"},
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
	cfg := Config{PGSSLMode: "require", PGRootCAs: roots}
	conn, err := cfg.DialPostgres(listener.Addr().String())
	if err == nil {
		conn.Close()
		t.Fatal("accepted a certificate for localhost when dialing 127.0.0.1")
	}
	if !strings.Contains(err.Error(), "IP SANs") {
		t.Fatalf("wanted hostname verification failure, got %v", err)
	}
	<-done
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
