package transport

import (
	"context"
	"crypto/tls"
	"errors"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// peerCtxKey is the unexported context key for the peer SVID.
type peerCtxKey struct{}

// WithPeer returns a context decorated with the peer SPIFFE ID.
func WithPeer(ctx context.Context, id spiffeid.ID) context.Context {
	return context.WithValue(ctx, peerCtxKey{}, id)
}

// PeerID returns the SPIFFE ID of the connection peer attached by a
// transport listener, and ok==true if present.
func PeerID(ctx context.Context) (spiffeid.ID, bool) {
	id, ok := ctx.Value(peerCtxKey{}).(spiffeid.ID)
	return id, ok
}

// PeerFromTLSState extracts the SPIFFE ID from a *tls.ConnectionState if the
// peer presented an SVID. Returns ok==false if the peer cert is missing
// or doesn't carry a SPIFFE URI SAN.
//
// R-MTL-1 acceptance #2: connection exposes peer SPIFFE ID.
func PeerFromTLSState(state *tls.ConnectionState) (spiffeid.ID, bool) {
	if state == nil || len(state.PeerCertificates) == 0 {
		return spiffeid.ID{}, false
	}
	id, err := x509svid.IDFromCert(state.PeerCertificates[0])
	if err != nil {
		return spiffeid.ID{}, false
	}
	return id, true
}

// ErrNoPeerID is returned when a handler expects a SPIFFE peer but no
// SPIFFE ID could be extracted.
var ErrNoPeerID = errors.New("transport: no SPIFFE peer ID on connection")
