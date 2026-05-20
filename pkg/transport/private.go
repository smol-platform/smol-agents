package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/spiffe/go-spiffe/v2/spiffetls"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"

	"github.com/stigen/smol-agents/pkg/identity"
)

// PrivateConfig configures a private (in-mesh) mTLS endpoint.
// Implements R-MTL-1.
type PrivateConfig struct {
	// Addr is the listen or connect address (host:port).
	Addr string

	// Authorize is the SPIFFE authorizer. If nil, AuthorizeAny is used.
	Authorize identity.Authorizer
}

// PrivateListener returns a net.Listener whose accepted connections
// require the peer to present a SPIFFE X.509-SVID matching cfg.Authorize.
//
// The returned listener is a thin wrapper over spiffetls.ListenWithMode
// that also annotates each *peerConn with its peer ID.
func PrivateListener(ctx context.Context, src identity.Source, cfg PrivateConfig) (net.Listener, error) {
	if cfg.Addr == "" {
		return nil, errors.New("transport: PrivateListener requires Addr")
	}
	auth := cfg.Authorize
	if auth == nil {
		auth = identity.AuthorizeAny{TrustDomain: src.TrustDomain()}
	}
	mode := spiffetls.MTLSServerWithSourceOptions(
		auth.AsAuthorizer(),
		nil, // x509SourceOption — we attach via the source argument below
	)

	// spiffetls accepts a Source via WithBundleSource etc.; here we use
	// the simpler API: hand it our source and it will use both bundle and
	// SVID from the same workload API connection.
	tlsCfg := tlsconfig.MTLSServerConfig(src.X509Source(), src.X509Source(), auth.AsAuthorizer())

	tcp, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("transport: tcp listen: %w", err)
	}
	_ = mode // mode is unused in the simplified path; kept to document intent
	return &peerListener{
		Listener:  tls.NewListener(tcp, tlsCfg),
		extractor: extractFromTLS,
	}, nil
}

// PrivateDialer returns a function that dials Addr using mTLS with src.
// The peer must satisfy cfg.Authorize.
func PrivateDialer(src identity.Source, cfg PrivateConfig) func(ctx context.Context, network, addr string) (net.Conn, error) {
	auth := cfg.Authorize
	if auth == nil {
		auth = identity.AuthorizeAny{TrustDomain: src.TrustDomain()}
	}
	tlsCfg := tlsconfig.MTLSClientConfig(src.X509Source(), src.X509Source(), auth.AsAuthorizer())
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		raw, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		c := tls.Client(raw, tlsCfg)
		if err := c.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("transport: handshake: %w", err)
		}
		return c, nil
	}
}

// peerListener wraps a net.Listener and rewrites Accept to attach the peer
// SPIFFE ID to the returned conn (via peerConn).
type peerListener struct {
	net.Listener
	extractor func(net.Conn) (spiffeIDOrZero, bool)
}

type spiffeIDOrZero = struct {
	idString string
}

func (l *peerListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &peerConn{Conn: c, extractor: l.extractor}, nil
}

func extractFromTLS(c net.Conn) (spiffeIDOrZero, bool) {
	tc, ok := c.(*tls.Conn)
	if !ok {
		return spiffeIDOrZero{}, false
	}
	state := tc.ConnectionState()
	id, ok := PeerFromTLSState(&state)
	if !ok {
		return spiffeIDOrZero{}, false
	}
	return spiffeIDOrZero{idString: id.String()}, true
}
