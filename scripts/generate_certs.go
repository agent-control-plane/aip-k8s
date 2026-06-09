//go:build ignore

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run generate_certs.go <output-dir>")
		os.Exit(1)
	}
	outDir := os.Args[1]
	if err := os.MkdirAll(outDir, 0755); err != nil {
		fmt.Printf("failed to create directory: %v\n", err)
		os.Exit(1)
	}

	// 1. Generate CA
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		fmt.Printf("failed to generate CA key: %v\n", err)
		os.Exit(1)
	}

	caTemplate := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"AIP E2E"},
			CommonName:   "Keycloak Local CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caBytes, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		fmt.Printf("failed to create CA certificate: %v\n", err)
		os.Exit(1)
	}

	// 2. Generate Keycloak Cert
	kcKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		fmt.Printf("failed to generate Keycloak key: %v\n", err)
		os.Exit(1)
	}

	kcTemplate := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"AIP E2E"},
			CommonName:   "keycloak.keycloak.svc.cluster.local",
		},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().AddDate(2, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{
			"keycloak.keycloak.svc.cluster.local",
			"keycloak.keycloak.svc",
			"localhost",
		},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
		},
	}

	kcBytes, err := x509.CreateCertificate(rand.Reader, &kcTemplate, &caTemplate, &kcKey.PublicKey, caKey)
	if err != nil {
		fmt.Printf("failed to create Keycloak certificate: %v\n", err)
		os.Exit(1)
	}

	// Write files
	writePEM(filepath.Join(outDir, "ca.crt"), "CERTIFICATE", caBytes)
	writePEM(filepath.Join(outDir, "ca.key"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(caKey))
	writePEM(filepath.Join(outDir, "keycloak.crt"), "CERTIFICATE", kcBytes)
	writePEM(filepath.Join(outDir, "keycloak.key"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(kcKey))

	fmt.Println("Successfully generated certificates in:", outDir)
}

func writePEM(path, pemType string, bytes []byte) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("failed to create file %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close()

	err = pem.Encode(f, &pem.Block{Type: pemType, Bytes: bytes})
	if err != nil {
		fmt.Printf("failed to encode PEM to %s: %v\n", path, err)
		os.Exit(1)
	}
}
