package features

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
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
	return ResolvePlacementForClass(ctx, env.Reader, env.CR.Spec.Features.Sandbox.RuntimeClass)
}

// ResolvePlacementForClass is the SmolAgent-CR-independent core: it resolves the
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

// knativePodspecFlags are the config-features flags Knative requires before
// it honours the runtimeClassName / affinity / tolerations / nodeSelector
// our placement stamps onto a revision template. Without them Knative
// silently drops those fields and the kata pod schedules unisolated.
var knativePodspecFlags = []string{
	"kubernetes.podspec-runtimeclassname",
	"kubernetes.podspec-affinity",
	"kubernetes.podspec-tolerations",
	"kubernetes.podspec-nodeselector",
}

// MissingKnativePodspecFlags returns the required Knative feature flags not
// set to "enabled" in knative-serving/config-features. Best-effort: returns
// nil when the ConfigMap can't be read (Knative absent or a non-standard
// namespace), so the operator never blocks on a check it cannot make.
func MissingKnativePodspecFlags(ctx context.Context, reader client.Reader) []string {
	if reader == nil {
		return nil
	}
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: "knative-serving", Name: "config-features"}
	if err := reader.Get(ctx, key, cm); err != nil {
		return nil
	}
	var missing []string
	for _, f := range knativePodspecFlags {
		if cm.Data[f] != "enabled" {
			missing = append(missing, f)
		}
	}
	return missing
}
