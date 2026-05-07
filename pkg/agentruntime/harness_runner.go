package agentruntime

import (
	"context"
	"encoding/json"

	v1 "github.com/stigen/knative-agents/pkg/agentmodel/v1"
	"github.com/stigen/knative-agents/pkg/agentruntime/harness"
)

// RegistryRunner adapts a harness.Registry to the executor's
// HarnessRunner interface. Production callers do
//
//	exec := agentruntime.New()
//	exec.Harness = agentruntime.NewRegistryRunner(harness.Default())
//
// Tests can wire a stub Registry instead.
type RegistryRunner struct {
	Registry *harness.Registry
}

// NewRegistryRunner returns a HarnessRunner backed by r.
func NewRegistryRunner(r *harness.Registry) *RegistryRunner {
	return &RegistryRunner{Registry: r}
}

// RunHarness satisfies HarnessRunner.
func (r *RegistryRunner) RunHarness(
	ctx context.Context, spec v1.HarnessSpec, instructions string,
	input json.RawMessage, workingDir string, env map[string]string,
	budget v1.Budget, seed int64,
) (output []byte, tokensIn int64, tokensOut int64, durationMs int64, err error) {
	if r == nil || r.Registry == nil {
		r.Registry = harness.Default()
	}
	h, err := r.Registry.For(spec.Kind)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	resp, err := h.Run(ctx, harness.Request{
		Spec:         spec,
		Instructions: instructions,
		Input:        input,
		WorkingDir:   workingDir,
		Env:          env,
		Budget:       budget,
		Seed:         seed,
	})
	return resp.Output, resp.TokensIn, resp.TokensOut, resp.DurationMs, err
}
