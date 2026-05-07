package secrets

import (
	"context"
	"errors"
	"net"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// PeerAttestor resolves a connection's peer process to a SPIFFE ID.
// On Linux this uses SO_PEERCRED + the SPIRE workload API. On other
// platforms it returns ErrUnsupportedOS.
//
// Implements R-SEC-1.
type PeerAttestor interface {
	Attest(ctx context.Context, conn net.Conn) (spiffeid.ID, error)
}

// FixedPeerAttestor returns a fixed SPIFFE ID; useful for tests.
type FixedPeerAttestor struct {
	ID  spiffeid.ID
	Err error
}

func (f FixedPeerAttestor) Attest(ctx context.Context, conn net.Conn) (spiffeid.ID, error) {
	if f.Err != nil {
		return spiffeid.ID{}, f.Err
	}
	return f.ID, nil
}

// MultiAttestor tries each underlying attestor in order, returning the
// first success.
type MultiAttestor []PeerAttestor

func (m MultiAttestor) Attest(ctx context.Context, conn net.Conn) (spiffeid.ID, error) {
	if len(m) == 0 {
		return spiffeid.ID{}, errors.New("secrets: no attestors configured")
	}
	var lastErr error
	for _, a := range m {
		id, err := a.Attest(ctx, conn)
		if err == nil {
			return id, nil
		}
		lastErr = err
	}
	return spiffeid.ID{}, lastErr
}
