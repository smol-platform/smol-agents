package transport

import (
	"context"
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

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

func TestPeerCtx(t *testing.T) {
	id := spiffeid.RequireFromString("spiffe://smol-agents.ai/ns/agents/sa/a")
	ctx := WithPeer(context.Background(), id)
	got, ok := PeerID(ctx)
	if !ok {
		t.Fatal("PeerID lost")
	}
	if got != id {
		t.Errorf("got %s, want %s", got, id)
	}
}

func TestPeerID_Missing(t *testing.T) {
	_, ok := PeerID(context.Background())
	if ok {
		t.Fatal("PeerID should be absent on bare ctx")
	}
}

func TestPublicListener_RequiresPaths(t *testing.T) {
	_, err := PublicListener(context.Background(), PublicConfig{Addr: "127.0.0.1:0"})
	if err == nil {
		t.Fatal("expected error: missing cert/key")
	}
}

func TestPublicListener_BadCert(t *testing.T) {
	_, err := PublicListener(context.Background(), PublicConfig{
		Addr: "127.0.0.1:0", CertPath: "/no/such/file", KeyPath: "/no/such/file",
	})
	if err == nil {
		t.Fatal("expected error loading bad cert")
	}
}

func TestPublicListener_Loads(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedPair(t, dir)
	l, err := PublicListener(context.Background(), PublicConfig{
		Addr: "127.0.0.1:0", CertPath: certPath, KeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("PublicListener: %v", err)
	}
	defer l.Close()
}

func writeSelfSignedPair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
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
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
