//go:build !linux

package secrets

import (
	"errors"
	"net"
)

// PeerCallerClass is a non-Linux stub: without /proc the broker treats every
// caller as the agent (it only runs on Linux in production).
func PeerCallerClass(net.Conn, ProcAncestry) CallerClass { return CallerAgent }

// ProcfsAncestry is a non-Linux stub: /proc process ancestry is unavailable, so
// the interactive-caller classifier degrades to "agent" (the broker's other
// controls still apply). The broker only runs on Linux in production.
type ProcfsAncestry struct{}

// Ancestry always errors off-Linux; callers treat that as CallerAgent.
func (ProcfsAncestry) Ancestry(int) ([]string, error) {
	return nil, errors.New("secrets: /proc ancestry unavailable off-linux")
}
