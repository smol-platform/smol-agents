package transport

import (
	"crypto/tls"
	"net"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// peerConn is a net.Conn that lazily exposes the peer SPIFFE ID after
// the TLS handshake completes.
type peerConn struct {
	net.Conn
	extractor func(net.Conn) (spiffeIDOrZero, bool)
}

// PeerSPIFFEID returns the peer SPIFFE ID, blocking on the TLS handshake
// if it has not yet completed.
func (p *peerConn) PeerSPIFFEID() (spiffeid.ID, bool) {
	if tc, ok := p.Conn.(*tls.Conn); ok {
		// Force handshake — TLS handshake may be deferred until first I/O.
		if err := tc.Handshake(); err != nil {
			return spiffeid.ID{}, false
		}
		state := tc.ConnectionState()
		return PeerFromTLSState(&state)
	}
	if p.extractor != nil {
		v, ok := p.extractor(p.Conn)
		if !ok {
			return spiffeid.ID{}, false
		}
		id, err := spiffeid.FromString(v.idString)
		if err != nil {
			return spiffeid.ID{}, false
		}
		return id, true
	}
	return spiffeid.ID{}, false
}

// Underlying returns the wrapped net.Conn.
func (p *peerConn) Underlying() net.Conn { return p.Conn }
