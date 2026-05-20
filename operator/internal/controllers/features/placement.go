package features

import (
	"context"
	"sort"

	v1 "github.com/stigen/knative-agents/operator/api/v1"
	"github.com/stigen/knative-agents/operator/internal/builders"
)

// ResolvePlacement finds the AgentNodePool a sandboxed agent must be
// scheduled onto, auto-matching the pool's isolation to the agent's
// sandbox runtimeClass.
//
// Returns (nil, false) when no placement is needed or possible:
//   - the runtimeClass needs no bare-metal pool (gvisor / runc), or
//   - there is no Reader (unit tests), or
//   - no AgentNodePool matches the isolation — the caller decides the
//     fallback (gVisor / Failed). See R-PROV-2.
//
// When several pools provide the same isolation, the lowest name wins so
// the choice is deterministic across reconciles.
func ResolvePlacement(ctx context.Context, env Env) (*builders.NodePlacement, bool, error) {
	rc := env.CR.Spec.Features.Sandbox.RuntimeClass
	if rc == "" {
		rc = "kata-fc"
	}
	if !builders.RequiresKVM(rc) || env.Reader == nil {
		return nil, false, nil
	}

	list := &v1.AgentNodePoolList{}
	if err := env.Reader.List(ctx, list); err != nil {
		return nil, false, err
	}
	matches := make([]string, 0, len(list.Items))
	for _, anp := range list.Items {
		if anp.Spec.Isolation == rc {
			matches = append(matches, anp.Name)
		}
	}
	if len(matches) == 0 {
		return nil, false, nil
	}
	sort.Strings(matches)
	return &builders.NodePlacement{PoolName: matches[0], Isolation: rc}, true, nil
}
