// Package shared holds the cross-ring scenario logic and the Env
// interface that each ring's setup satisfies.
//
// The split-with-shared-scenarios is the central simplification of
// the fullstack-e2e architecture: every assertion is written once,
// then executed at every ring whose capabilities cover it. Scenarios
// MUST NOT branch on ring identity — they branch on capabilities so
// adding a new ring (or removing one) is purely a wiring change.
package shared

import (
	"context"
	"testing"
	"time"
)

// Env is the abstraction every ring's setup implements. The methods
// are intentionally narrow: anything more specific (eBPF map reads,
// SSM exec, kubectl raw) goes through the ring-specific helper that
// returns the Env so scenarios stay portable.
type Env interface {
	// Capabilities returns the ring's feature flags. Scenarios skip
	// themselves when their required caps aren't advertised.
	Capabilities() Caps

	// Ring is a short label ("l0", "l1", "l2") used in test output.
	Ring() string

	// Apply pushes a manifest (YAML) into the environment. For L0
	// (no Kubernetes) implementers translate manifests into compose
	// service config or fail with NotApplicable.
	Apply(ctx context.Context, manifest []byte) error

	// Exec runs a command inside a target container/pod and returns
	// the combined stdout+stderr.
	Exec(ctx context.Context, target ExecTarget, cmd ...string) (output []byte, err error)

	// WaitFor polls predicate until it returns true, the deadline
	// fires, or ctx is cancelled. Predicates are deliberately
	// stateless to avoid leaking ring-specific objects up.
	WaitFor(ctx context.Context, name string, deadline time.Duration, predicate func(ctx context.Context) bool) error

	// Cleanup is called by the test framework on exit. Implementers
	// MUST be idempotent: cleanup may run more than once on a flaky
	// CI runner.
	Cleanup(ctx context.Context) error

	// Endpoint resolves a logical service name to a ring-specific
	// dial address (host:port, URL, or socket path). Scenarios use
	// this so they don't have to know whether they're talking to a
	// compose service, a k8s ClusterIP, or a Pod via port-forward.
	//
	// Known logical names (each ring implements only what applies):
	//   "fake-llm"          → http URL, e.g. http://127.0.0.1:18080
	//   "fake-gateway-http" → https URL of the JWT-validating echo
	//   "fake-gateway-tcp"  → host:port of the mTLS echo
	//   "wg-hub"            → host:port (UDP) of the WireGuard server
	Endpoint(logical string) (addr string, ok bool)

	// SPIFFEWorkloadAPI returns the dial path for the SPIRE
	// workload-API socket reachable from the test process. Empty
	// string means "no SPIRE in this ring" (scenarios should gate
	// on CapSPIRE).
	SPIFFEWorkloadAPI() string

	// RunSpiffeProbe launches an in-cluster Pod running
	// cmd/spiffe-probe with the requested scenarios, captures its
	// log output, and returns the parsed lines. Lets SPIFFE
	// scenarios bypass the macOS-OrbStack in-VM-socket limitation
	// by running assertions inside the cluster's kernel.
	//
	// Rings that don't have a cluster (L0) return an error; SPIFFE
	// scenarios that depend on this self-skip via Capability gating
	// before getting here.
	RunSpiffeProbe(ctx context.Context, scenarios []string, args ...string) ([]ProbeLine, error)

	// RunEBPFProbe launches a privileged in-cluster Pod running
	// cmd/ebpf-probe with the requested scenarios. The probe
	// loads bpf/programs/egress_redirect.bpf.o, attaches to its
	// own cgroup, populates the LPM trie / hash maps with the
	// args-driven policy, and verifies the kernel egress filter
	// behaves as advertised. Returns the parsed OK/FAIL lines
	// just like RunSpiffeProbe.
	//
	// Rings without privileged pod support (L0, kind without
	// extra mounts) self-skip via the CapEBPF gate.
	RunEBPFProbe(ctx context.Context, scenarios []string, args ...string) ([]ProbeLine, error)
}

// ProbeLine is one parsed result line from cmd/spiffe-probe's stdout.
type ProbeLine struct {
	OK       bool
	Scenario string
	Detail   string
}

// ExecTarget identifies a process to exec into. Different rings map
// the fields differently:
//
//	L0 (compose): Container = service name; Namespace ignored.
//	L1/L2 (k8s):  Namespace + Pod selectors; Container is the
//	              container-name within the Pod.
type ExecTarget struct {
	Namespace string
	Pod       string // pod name or label selector "app=foo"
	Container string // container/service name
}

// Scenario is the unit of cross-ring testing.
type Scenario struct {
	// ID matches an R-E2E-SCN-* requirement so the coverage gate
	// can map test → requirement.
	ID string

	// Name is the human-readable label.
	Name string

	// Requires lists the capabilities the ring must advertise. If
	// any are missing, the scenario is skipped (not failed).
	Requires Caps

	// Run executes the scenario against the given Env. Implementers
	// may use t.Helper, t.Errorf, etc. — the runner is a normal
	// `*testing.T` subtest.
	Run func(t *testing.T, env Env)
}

// RunAll executes every scenario whose Requires is a subset of
// env.Capabilities() as a t.Run subtest. Scenarios with unmet
// capabilities are reported as skipped, not failed.
func RunAll(t *testing.T, env Env, scenarios []Scenario) {
	t.Helper()
	caps := env.Capabilities()
	for _, s := range scenarios {
		t.Run(s.ID+"/"+s.Name, func(t *testing.T) {
			if !caps.Has(s.Requires) {
				t.Skipf("ring %s missing capabilities: have=%s, need=%s",
					env.Ring(), caps, s.Requires)
				return
			}
			s.Run(t, env)
		})
	}
}
