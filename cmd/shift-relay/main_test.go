package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testKeyPair issues a short-lived self-signed certificate and returns its
// PEM halves — enough to exercise the loader contract without fixtures.
func testKeyPair(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "shift-relay-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestDefaultListenPrecedence(t *testing.T) {
	t.Run("explicit env wins", func(t *testing.T) {
		t.Setenv("SHIFT_RELAY_LISTEN", "127.0.0.1:9443")
		t.Setenv("PORT", "8080")
		if got := defaultListen(); got != "127.0.0.1:9443" {
			t.Fatalf("defaultListen() = %q, want the explicit environment value", got)
		}
	})
	t.Run("platform port without env", func(t *testing.T) {
		t.Setenv("SHIFT_RELAY_LISTEN", "")
		t.Setenv("PORT", "8080")
		if got := defaultListen(); got != ":8080" {
			t.Fatalf("defaultListen() = %q, want the platform port", got)
		}
	})
	t.Run("built-in default otherwise", func(t *testing.T) {
		t.Setenv("SHIFT_RELAY_LISTEN", "")
		t.Setenv("PORT", "")
		if got := defaultListen(); got != ":9443" {
			t.Fatalf("defaultListen() = %q, want the built-in default", got)
		}
	})
}

func TestReadPEMOrFilePrefersInlinePEM(t *testing.T) {
	certPEM, _ := testKeyPair(t)
	material, err := readPEMOrFile(string(certPEM))
	if err != nil {
		t.Fatal(err)
	}
	if string(material) != string(certPEM) {
		t.Fatal("inline PEM was not returned verbatim")
	}
	if label := sourceLabel(string(certPEM)); label != "inline PEM" {
		t.Fatalf("sourceLabel() = %q, want inline PEM", label)
	}
}

func TestReadPEMOrFileReadsPaths(t *testing.T) {
	certPEM, _ := testKeyPair(t)
	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	material, err := readPEMOrFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(material) != string(certPEM) {
		t.Fatal("file contents were not returned")
	}
	if label := sourceLabel(path); label != path {
		t.Fatalf("sourceLabel() = %q, want the path itself", label)
	}
}

func TestLoadKeyPairAcceptsPEMOrFiles(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, pair := range map[string][2]string{
		"inline PEM": {string(certPEM), string(keyPEM)},
		"file paths": {certPath, keyPath},
		"mixed":      {string(certPEM), keyPath},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadKeyPair(pair[0], pair[1]); err != nil {
				t.Fatalf("loadKeyPair: %v", err)
			}
		})
	}

	t.Run("missing key names its source", func(t *testing.T) {
		_, err := loadKeyPair(string(certPEM), filepath.Join(dir, "absent.key"))
		if err == nil {
			t.Fatal("expected an error for a missing key file")
		}
		if !strings.Contains(err.Error(), "absent.key") {
			t.Fatalf("error %q does not name the missing source", err)
		}
	})
}
