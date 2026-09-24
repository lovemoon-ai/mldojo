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
	"testing"
	"time"

	"github.com/lovemoon-ai/mldojo/internal/config"
)

// certPair writes a throwaway self-signed certificate and returns both paths.
func certPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mldojo-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestServerTLS(t *testing.T) {
	cert, key := certPair(t)
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("no config means plain http", func(t *testing.T) {
		cfg, err := serverTLS(&config.API{})
		if err != nil || cfg != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", cfg, err)
		}
	})

	t.Run("cert and key enable tls", func(t *testing.T) {
		cfg, err := serverTLS(&config.API{TLSCert: cert, TLSKey: key})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("got nil config with a certificate configured")
		}
		if cfg.ClientAuth != 0 {
			t.Error("client certificates required without a client CA")
		}
	})

	// Half a configuration is the failure worth catching: it would otherwise
	// start as plain HTTP and look like it worked.
	t.Run("half a pair is refused", func(t *testing.T) {
		for _, c := range []*config.API{{TLSCert: cert}, {TLSKey: key}} {
			if _, err := serverTLS(c); err == nil {
				t.Errorf("accepted %+v", c)
			}
		}
	})

	t.Run("client CA without server TLS is refused", func(t *testing.T) {
		if _, err := serverTLS(&config.API{TLSClientCA: cert}); err == nil {
			t.Error("accepted mTLS without server TLS, which cannot work")
		}
	})

	t.Run("a bad certificate fails at startup, not on first request", func(t *testing.T) {
		if _, err := serverTLS(&config.API{TLSCert: junk, TLSKey: key}); err == nil {
			t.Error("accepted an unparseable certificate")
		}
	})

	t.Run("client CA turns on mTLS", func(t *testing.T) {
		cfg, err := serverTLS(&config.API{TLSCert: cert, TLSKey: key, TLSClientCA: cert})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ClientCAs == nil {
			t.Error("client CA pool is empty")
		}
		if cfg.ClientAuth == 0 {
			t.Error("client certificates are not required")
		}
	})

	t.Run("a client CA with no certificates in it is refused", func(t *testing.T) {
		if _, err := serverTLS(&config.API{TLSCert: cert, TLSKey: key, TLSClientCA: junk}); err == nil {
			t.Error("accepted a CA file holding no certificates")
		}
	})
}
