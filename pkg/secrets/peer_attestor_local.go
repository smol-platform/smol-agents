package secrets

import (
	"strconv"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// DefaultLocalTrustDomain is the synthetic trust domain LocalPeerAttestor mints
// identities under when SPIRE is unavailable.
const DefaultLocalTrustDomain = "local.smol-agents"

// LocalPeerAttestor authenticates a unix-domain-socket peer by its
// kernel-verified uid (SO_PEERCRED) and returns the synthetic SPIFFE ID
// spiffe://<TrustDomain>/uid/<uid>. It is the no-SPIRE fallback for in-pod
// brokers: the EmptyDir socket bounds access to the pod and the uid bounds it to
// the expected workload user within it. Weaker than SPIRE (no cryptographic
// per-workload identity), so a broker prefers SPIRE when present (see
// MultiAttestor). The Attest method is OS-specific (peer_attestor_{linux,other}.go).
type LocalPeerAttestor struct {
	// TrustDomain is the synthetic local trust domain; zero -> DefaultLocalTrustDomain.
	TrustDomain spiffeid.TrustDomain
}

// NewLocalPeerAttestor builds a LocalPeerAttestor (empty td -> default).
func NewLocalPeerAttestor(td string) (LocalPeerAttestor, error) {
	dom, err := spiffeid.TrustDomainFromString(localTDOrDefault(td))
	if err != nil {
		return LocalPeerAttestor{}, err
	}
	return LocalPeerAttestor{TrustDomain: dom}, nil
}

// LocalIDForUID is the principal LocalPeerAttestor produces for a uid. Operators
// generating a broker policy/backend for the local fallback use this so the
// config keys match what the attestor will mint at runtime.
func LocalIDForUID(td string, uid uint32) (spiffeid.ID, error) {
	dom, err := spiffeid.TrustDomainFromString(localTDOrDefault(td))
	if err != nil {
		return spiffeid.ID{}, err
	}
	return localPrincipal(dom, uid)
}

// localPrincipal builds the synthetic identity for a uid. Pure (no syscalls), so
// it is testable on any OS; the build-tagged Attest methods feed it SO_PEERCRED.
func localPrincipal(td spiffeid.TrustDomain, uid uint32) (spiffeid.ID, error) {
	return spiffeid.FromSegments(td, "uid", strconv.FormatUint(uint64(uid), 10))
}

func localTDOrDefault(td string) string {
	if td == "" {
		return DefaultLocalTrustDomain
	}
	return td
}
