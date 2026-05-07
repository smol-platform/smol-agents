package secrets

import (
	"context"
	"errors"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// MaxLeaseTTL is the absolute upper bound on lease lifetime, regardless of
// per-call configuration. It can be lowered via Server.MaxLeaseTTL.
//
// Implements R-SEC-2 acceptance #1.
const MaxLeaseTTL = 15 * time.Minute

// Errors visible to clients. Concrete error values let callers branch.
var (
	ErrUnauthorized   = errors.New("secrets: unauthorized")
	ErrNotFound       = errors.New("secrets: not found")
	ErrBackendDown    = errors.New("secrets: backend unavailable")
	ErrPeerNotSpiffe  = errors.New("secrets: peer is not SPIFFE-attested")
	ErrTTLExceeded    = errors.New("secrets: requested TTL exceeds maximum")
	ErrLeaseExpired   = errors.New("secrets: lease expired")
	ErrUnsupportedOS  = errors.New("secrets: operation not supported on this OS")
	ErrInvalidRequest = errors.New("secrets: invalid request")
)

// Lease is the short-lived envelope returned by the broker for a secret.
//
// Implements R-SEC-2: TTL ≤ MaxLeaseTTL.
type Lease struct {
	Name      string        `json:"name"`
	Value     []byte        `json:"value"`
	Issued    time.Time     `json:"issued"`
	ExpiresAt time.Time     `json:"expires_at"`
	Audience  spiffeid.ID   `json:"audience"`
	TTL       time.Duration `json:"ttl"`
}

// Valid returns true if the lease has not yet expired.
func (l Lease) Valid(now time.Time) bool {
	return now.Before(l.ExpiresAt)
}

// Backend is the pluggable secret store. Implementations must be
// goroutine-safe.
//
// Implements R-SEC-3.
type Backend interface {
	// Fetch returns the raw secret material identified by name. Backends
	// SHOULD scope by principal so a misconfigured policy cannot leak.
	Fetch(ctx context.Context, principal spiffeid.ID, name string) ([]byte, error)

	// Close releases any resources held by the backend.
	Close() error
}

// Policy decides whether a principal may obtain a given secret.
type Policy interface {
	Allowed(principal spiffeid.ID, name string) bool
}

// AllowFunc is a Policy adapter for inline closures.
type AllowFunc func(principal spiffeid.ID, name string) bool

func (f AllowFunc) Allowed(p spiffeid.ID, n string) bool { return f(p, n) }

// StaticPolicy is a simple SPIFFE-ID → set-of-names policy.
type StaticPolicy struct {
	Allow map[string]map[string]struct{} // spiffeID.String() → name set
}

func NewStaticPolicy() *StaticPolicy {
	return &StaticPolicy{Allow: make(map[string]map[string]struct{})}
}

func (p *StaticPolicy) Grant(id spiffeid.ID, names ...string) {
	key := id.String()
	if p.Allow[key] == nil {
		p.Allow[key] = make(map[string]struct{})
	}
	for _, n := range names {
		p.Allow[key][n] = struct{}{}
	}
}

func (p *StaticPolicy) Allowed(id spiffeid.ID, name string) bool {
	if p == nil {
		return false
	}
	set, ok := p.Allow[id.String()]
	if !ok {
		return false
	}
	_, ok = set[name]
	return ok
}
