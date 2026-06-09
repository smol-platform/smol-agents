package memory

import (
	"crypto/tls"
	"testing"
)

// TestQdrantTransportCreds verifies the dial uses (m)TLS when a TLS config is
// supplied (the in-cluster SPIFFE-mTLS path, x9i.3) and insecure otherwise
// (Qdrant Cloud / dev only).
func TestQdrantTransportCreds(t *testing.T) {
	if got := qdrantTransportCreds(QdrantConfig{}).Info().SecurityProtocol; got != "insecure" {
		t.Errorf("no TLS config: SecurityProtocol=%q, want insecure", got)
	}
	withTLS := QdrantConfig{TLS: &tls.Config{MinVersion: tls.VersionTLS13}}
	if got := qdrantTransportCreds(withTLS).Info().SecurityProtocol; got != "tls" {
		t.Errorf("with TLS config: SecurityProtocol=%q, want tls", got)
	}
}
