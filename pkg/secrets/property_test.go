package secrets

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"pgregory.net/rapid"
)

// Property: every issued lease's principal is allowed by the policy.
// Implements the verification side of R-VRF-2.
func TestProperty_LeaseImpliesAuthorized(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		policy := NewStaticPolicy()
		backend := NewStaticBackend()
		td := spiffeid.RequireTrustDomainFromString("stigen.ai")

		// Generate a small principal/secret universe.
		principalNames := rapid.SliceOfN(rapid.StringMatching(`[a-z]{1,8}`), 1, 4).Draw(t, "principals")
		secretNames := rapid.SliceOfN(rapid.StringMatching(`[a-z]{1,8}`), 1, 4).Draw(t, "secrets")

		ids := make([]spiffeid.ID, 0, len(principalNames))
		for _, n := range principalNames {
			id, err := spiffeid.FromPath(td, "/ns/agents/sa/"+n)
			if err != nil {
				continue
			}
			ids = append(ids, id)
			for _, s := range secretNames {
				backend.Set(id, s, []byte(n+":"+s))
			}
		}
		if len(ids) == 0 {
			t.Skip("no valid principals generated")
		}

		// Grant a random subset.
		grants := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) [2]int {
			return [2]int{
				rapid.IntRange(0, len(ids)-1).Draw(t, "i"),
				rapid.IntRange(0, len(secretNames)-1).Draw(t, "j"),
			}
		}), 0, 16).Draw(t, "grants")
		for _, g := range grants {
			policy.Grant(ids[g[0]], secretNames[g[1]])
		}

		// Drive the server through a few requests and check invariants.
		s := &Server{
			SocketPath:  "", // skipped — we call dispatch directly
			MaxLeaseTTL: 5 * time.Minute,
			DefaultTTL:  time.Minute,
			Backend:     backend,
			Policy:      policy,
			Logger:      slog.Default(),
			Now:         time.Now,
			conns:       map[*serverConn]struct{}{},
			issued:      map[string]Lease{},
		}

		ops := rapid.IntRange(0, 32).Draw(t, "ops")
		for k := 0; k < ops; k++ {
			callerIdx := rapid.IntRange(0, len(ids)-1).Draw(t, "caller")
			secretIdx := rapid.IntRange(0, len(secretNames)-1).Draw(t, "secret")
			ttl := time.Duration(rapid.IntRange(1, int(s.MaxLeaseTTL/time.Second)+10).Draw(t, "ttl")) * time.Second
			resp := s.dispatch(context.Background(), ids[callerIdx], request{Kind: reqLease, Name: secretNames[secretIdx], TTL: ttl})

			// Invariant 1: if a lease was issued, the policy must allow.
			if resp.Lease != nil {
				if !policy.Allowed(ids[callerIdx], secretNames[secretIdx]) {
					t.Fatalf("Property violated: lease issued without policy allow (id=%s name=%s)",
						ids[callerIdx], secretNames[secretIdx])
				}
				// Invariant 2: TTL ≤ MaxLeaseTTL.
				if resp.Lease.TTL > s.MaxLeaseTTL {
					t.Fatalf("Property violated: TTL %v > max %v", resp.Lease.TTL, s.MaxLeaseTTL)
				}
				// Invariant 3: ExpiresAt > Issued.
				if !resp.Lease.ExpiresAt.After(resp.Lease.Issued) {
					t.Fatalf("Property violated: ExpiresAt %v not after Issued %v",
						resp.Lease.ExpiresAt, resp.Lease.Issued)
				}
			}

			// Invariant 4: TTL > MaxLeaseTTL must produce ErrTTLExceeded.
			if ttl > s.MaxLeaseTTL && resp.Lease != nil {
				t.Fatalf("Property violated: oversized TTL accepted (%v > %v)", ttl, s.MaxLeaseTTL)
			}
		}
	})
}

// Property: errResponseWrap preserves errors.Is semantics across the wire.
func TestProperty_ErrorRoundTrip(t *testing.T) {
	cases := []error{
		ErrUnauthorized, ErrNotFound, ErrBackendDown, ErrPeerNotSpiffe,
		ErrTTLExceeded, ErrLeaseExpired, ErrInvalidRequest,
	}
	for _, e := range cases {
		resp := errResponseWrap(e)
		got := errorFromCode(resp.ErrorCode, resp.ErrorMessage)
		if !errors.Is(got, e) {
			t.Errorf("round trip lost: %v → code=%s → %v", e, resp.ErrorCode, got)
		}
	}
}
