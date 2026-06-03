package agentbench

import (
	"sort"
	"sync"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// VerdictKind is the outcome of an oracle check.
type VerdictKind string

const (
	VerdictPass    VerdictKind = "pass"
	VerdictFail    VerdictKind = "fail"
	VerdictSkip    VerdictKind = "skip"    // a required capability/precondition was absent
	VerdictBlocked VerdictKind = "blocked" // negative oracle held as expected (gap still present)
)

// Verdict is what an oracle returns: an outcome plus human-readable evidence
// (the proof, never a bare boolean).
type Verdict struct {
	Kind     VerdictKind `json:"verdict"`
	Evidence string      `json:"evidence"`
}

// CollectCtx threads everything an oracle needs beyond the RunStatus: the case,
// the per-run nonce, secret values (for non-leak/reach oracles), the node
// kernel (for isolation), prior-run output (fs_roundtrip), and the harness kind
// (kind-aware secret_absent). It is data-only so oracles stay pure + testable.
type CollectCtx struct {
	// Case is the case under check.
	Case BenchCase
	// Nonce is the fresh per-run nonce substituted into the prompt (empty when
	// the case did not request one).
	Nonce string
	// HarnessKind is the resolved harness kind of the referenced Agent (e.g.
	// "hermes", "claude-code", "generic-cli", "loop" for loop-mode). Drives the
	// kind-aware secret_absent inversion.
	HarnessKind string
	// SecretValues are cleartext secret values to grep for (secret_absent) or
	// reflect (secret_reach). Keyed by a caller-chosen label.
	SecretValues map[string]string
	// NodeKernel is the kubelet-reported node kernel version (isolation_kernel).
	NodeKernel string
	// PriorOutput is a prior run's RunStatus.Output (fs_roundtrip threads run-N
	// output into run-N+1).
	PriorOutput []byte
	// KataAvailable reports whether a kata-fc RuntimeClass is present in the
	// cluster; isolation_kernel SKIPs (not fails) when false.
	KataAvailable bool
}

// Oracle checks a terminal RunStatus against a case-specific assertion.
type OracleImpl interface {
	// Kind returns the registry key this impl handles.
	Kind() string
	// Check evaluates the status and returns a Verdict + evidence.
	Check(status pure.RunStatus, c CollectCtx) Verdict
}

var (
	registryMu sync.RWMutex
	registry   = map[string]OracleImpl{}
)

// register adds an oracle impl to the package registry. Called from init().
func register(o OracleImpl) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[o.Kind()] = o
}

// IsRegistered reports whether an oracle kind has a registered impl.
func IsRegistered(kind string) bool {
	registryMu.RLock()
	defer registryMu.RUnlock()
	_, ok := registry[kind]
	return ok
}

// LookupOracle returns the impl for kind.
func LookupOracle(kind string) (OracleImpl, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	o, ok := registry[kind]
	return o, ok
}

// RegisteredKinds returns a sorted list of all registered oracle kinds.
func RegisteredKinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
