package features

import (
	"context"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
)

// ResolvePlacementForClass resolves the
// AgentNodePool for a bare sandbox runtimeClass string, which is all the
// AgentRun / AgentSession path has. Empty rc defaults to kata-fc. Returns
// (nil, false) when no placement is needed or possible (non-KVM class, nil
// reader, or no matching pool); lowest pool name wins for determinism. R-PROV-2.
func ResolvePlacementForClass(ctx context.Context, reader client.Reader, rc string) (*builders.NodePlacement, bool, error) {
	if rc == "" {
		rc = "kata-fc"
	}
	if !builders.RequiresKVM(rc) || reader == nil {
		return nil, false, nil
	}

	list := &v1.AgentNodePoolList{}
	if err := reader.List(ctx, list); err != nil {
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
