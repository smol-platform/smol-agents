package agentbench

import (
	"context"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// Handle is an opaque reference to a submitted workload that Collect can resolve
// to a terminal RunStatus.
type Handle struct {
	// Name is the AgentRun name (run driver) or the turn id (gateway driver).
	Name string
	// Namespace is where the workload lives.
	Namespace string
	// Cold marks the first submission to an agent (for cold-start timing).
	Cold bool
	// extra carries driver-private state (e.g. the gateway session key).
	extra map[string]string
}

// Driver submits a case workload and collects its terminal RunStatus. The two
// shipped impls (runDriver, gatewayDriver) measure latency at the SAME boundary
// (status.startedAt/endedAt), so a case is comparable across drivers.
type Driver interface {
	// Kind returns the driver discriminator.
	Kind() DriverKind
	// Submit creates the workload (an AgentRun CR or a gateway turn) for one
	// sample of the case and returns a handle.
	Submit(ctx context.Context, c BenchCase, ns, prompt string, sampleIdx int) (Handle, error)
	// Collect waits for the workload to reach a terminal state and returns the
	// folded RunStatus.
	Collect(ctx context.Context, h Handle) (pure.RunStatus, error)
}
