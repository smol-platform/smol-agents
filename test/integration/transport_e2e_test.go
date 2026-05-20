//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stigen/smol-agents/pkg/transport"
)

// TestIntegration_PublicListenerHandshake validates that a real TLS client
// can complete a handshake against PublicListener.
func TestIntegration_PublicListenerHandshake(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir)

	ln, err := transport.PublicListener(context.Background(), transport.PublicConfig{
		Addr:     "127.0.0.1:0",
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("PublicListener: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	serverDone := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer c.Close()
		// Trigger handshake via a read; client side will write 0 bytes
		// after handshake, which returns EOF here — that's fine.
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 1)
		_, _ = c.Read(buf)
		serverDone <- nil
	}()

	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func writeSelfSigned(t *testing.T, dir string) (cert, key string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert = filepath.Join(dir, "tls.crt")
	key = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}
