//go:build !linux

package secrets

import (
	"context"
	"errors"
	"net"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// SPIREPeerAttestor on non-Linux returns ErrUnsupportedOS. The broker
// only runs in production on Linux; other OSes are supported for unit
// tests via FixedPeerAttestor.
type SPIREPeerAttestor struct {
	WorkloadAPIAddr string
}

func NewSPIREPeerAttestor(addr string) (*SPIREPeerAttestor, error) {
	if addr == "" {
		return nil, errors.New("secrets: SPIRE workload API addr is required")
	}
	return &SPIREPeerAttestor{WorkloadAPIAddr: addr}, nil
}

func (a *SPIREPeerAttestor) Attest(ctx context.Context, conn net.Conn) (spiffeid.ID, error) {
	return spiffeid.ID{}, ErrUnsupportedOS
}

// Attest on non-Linux returns ErrUnsupportedOS (SO_PEERCRED is Linux-only). The
// pure ID logic (localPrincipal / LocalIDForUID) remains testable everywhere.
func (a LocalPeerAttestor) Attest(ctx context.Context, conn net.Conn) (spiffeid.ID, error) {
	return spiffeid.ID{}, ErrUnsupportedOS
}
