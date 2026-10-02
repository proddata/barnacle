package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func main() {
	const dir = "/certs"
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err == nil {
		return // Keep the CA stable across ordinary Compose restarts.
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Hermit disposable PostgreSQL CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	check(err)
	server := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "postgres"},
		DNSNames:  []string{"postgres"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, ca, &serverKey.PublicKey, caKey)
	check(err)
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	check(err)
	write("server.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}), 0600)
	check(os.Chown(filepath.Join(dir, "server.key"), 70, 70)) // postgres:17-alpine user
	write("server.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), 0644)
	write("ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0644)
}

func serial() *big.Int {
	value, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	check(err)
	return value
}

func write(name string, data []byte, mode os.FileMode) {
	check(os.WriteFile(filepath.Join("/certs", name), data, mode))
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
