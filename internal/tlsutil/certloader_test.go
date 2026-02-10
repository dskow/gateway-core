package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// generateTestCert creates a self-signed cert/key pair and writes them to
// the given directory. Returns the file paths.
func generateTestCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyFile, keyPEM, 0o644); err != nil {
		t.Fatalf("write key: %v", err)
	}

	return certFile, keyFile
}

func TestCertLoader_InitialLoad(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := generateTestCert(t, dir)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cl, err := New(certFile, keyFile, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cl.Stop()

	cert, err := cl.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil certificate")
	}
}

func TestCertLoader_InvalidCert(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")

	os.WriteFile(certFile, []byte("invalid"), 0o644)
	os.WriteFile(keyFile, []byte("invalid"), 0o644)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	_, err := New(certFile, keyFile, logger)
	if err == nil {
		t.Fatal("expected error for invalid cert")
	}
}

func TestCertLoader_Reload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := generateTestCert(t, dir)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cl, err := New(certFile, keyFile, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cl.Stop()

	// Generate a new cert and overwrite the files.
	generateTestCert(t, dir)

	if err := cl.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	cert, err := cl.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate after reload: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil certificate after reload")
	}
}
