// Package policy implements the deny-by-default memory access control engine.
//
// A call (SPIFFE identity, operation, namespace) is allowed only when an
// explicit MemoryGrant in the retriever's policy matches all three fields.
// There is no default-allow path — an empty policy grants nothing.
// Implements R-MEM-AUTH-2, R-MEM-SEC-1.
package policy

import (
	"strings"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/memory"
)

// Checker evaluates whether a (caller, op, namespace) tuple is permitted by
// a retriever's policy. It is stateless and safe for concurrent use.
type Checker struct{}

// Allow returns nil when the caller holds an explicit grant for the requested
// operation on the requested namespace. It returns a typed PermissionDenied
// error otherwise — fail-closed, never a default-allow fallback.
//
// callerSPIFFEID is the full SPIFFE URI (e.g. spiffe://td/ns/foo/sa/bar).
// op must be one of MemoryOpRead / MemoryOpWrite / MemoryOpDelete.
// namespace is the memory namespace being accessed.
// grants is the MemoryRetriever.Policy slice.
//
// Implements R-MEM-AUTH-2: deny-by-default; read/write/delete independently grantable.
func (Checker) Allow(callerSPIFFEID string, op v1.MemoryOperation, namespace string, grants []v1.MemoryGrant) error {
	for _, g := range grants {
		if !matchesIdentity(g.Identity, callerSPIFFEID) {
			continue
		}
		if !containsOp(g.Operations, op) {
			continue
		}
		if !matchesNamespace(g.Namespaces, namespace) {
			continue
		}
		return nil // explicit grant found
	}
	return memory.PermissionDenied("no grant matches identity=" + callerSPIFFEID +
		" op=" + string(op) + " namespace=" + namespace)
}

// matchesIdentity returns true when grant.Identity matches callerSPIFFEID.
//
// Matching rules (mirrors the MemoryGrant.Identity doc):
//   - Exact match: "spiffe://td/ns/foo/sa/bar" matches only that ID.
//   - Prefix match: "spiffe://td/ns/foo/" (trailing "/") matches any ID
//     whose string representation starts with that prefix.
func matchesIdentity(pattern, callerSPIFFEID string) bool {
	if pattern == "" {
		return false
	}
	// Prefix grant: pattern ends with "/"
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(callerSPIFFEID, pattern)
	}
	return pattern == callerSPIFFEID
}

// containsOp reports whether ops contains target.
func containsOp(ops []v1.MemoryOperation, target v1.MemoryOperation) bool {
	for _, o := range ops {
		if o == target {
			return true
		}
	}
	return false
}

// matchesNamespace returns true when the grant's namespace list covers ns.
// ["*"] is a wildcard that grants access to any namespace.
func matchesNamespace(namespaces []string, ns string) bool {
	for _, n := range namespaces {
		if n == "*" || n == ns {
			return true
		}
	}
	return false
}
