package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// PublicConfig configures a public (gateway-fronted) mTLS endpoint.
// Implements R-MTL-2.
type PublicConfig struct {
	// Addr is the listen address (host:port).
	Addr string

	// CertPath / KeyPath point to a PEM cert chain and private key from a
	// public CA. R-MTL-2 acceptance #1.
	CertPath string
	KeyPath  string

	// ClientCAPath, if set, enables mutual auth: clients must present a
	// chain anchored to this CA bundle.
	ClientCAPath string

	// BoundSPIFFEID, if non-empty, asserts the server's identity in
	// service-to-service interactions; it is set in request logs but does
	// not affect TLS chain validation. R-MTL-2 acceptance #2.
	BoundSPIFFEID spiffeid.ID

	// MinVersion defaults to tls.VersionTLS13.
	MinVersion uint16
}

// PublicListener returns a net.Listener serving the public-facing mTLS
// endpoint described by cfg.
func PublicListener(ctx context.Context, cfg PublicConfig) (net.Listener, error) {
	if cfg.Addr == "" {
		return nil, errors.New("transport: PublicListener requires Addr")
	}
	if cfg.CertPath == "" || cfg.KeyPath == "" {
		return nil, errors.New("transport: PublicListener requires CertPath and KeyPath (R-MTL-2)")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("transport: load public cert: %w", err)
	}
	min := cfg.MinVersion
	if min == 0 {
		min = tls.VersionTLS13
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   min,
	}
	if cfg.ClientCAPath != "" {
		pool, err := loadCAPool(cfg.ClientCAPath)
		if err != nil {
			return nil, fmt.Errorf("transport: load client CA: %w", err)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	tcp, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("transport: tcp listen: %w", err)
	}
	return &peerListener{
		Listener:  tls.NewListener(tcp, tlsCfg),
		extractor: extractFromTLS,
	}, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("no certs found in PEM")
	}
	return pool, nil
}
