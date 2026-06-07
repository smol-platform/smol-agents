package builders

import (
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// ApplyRunPodPlacement binds a raw AgentRun / AgentSession pod to its node pool:
// nodeAffinity onto the pool label, the isolation toleration, and the
// do-not-disrupt annotation a live Firecracker microVM needs (R-PROV-5). No-op
// when p is nil or carries no PoolName. Idempotent — the affinity is set and
// the toleration/annotation are deduped so re-applying does not accumulate.
func ApplyRunPodPlacement(pod *corev1.Pod, p *NodePlacement) {
	if p == nil || p.PoolName == "" {
		return
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	pod.Spec.Affinity.NodeAffinity = placementNodeAffinity(*p)
	tol := placementToleration(*p)
	if !hasToleration(pod.Spec.Tolerations, tol) {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, tol)
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[DoNotDisruptAnnotation] = "true"
}

func hasToleration(ts []corev1.Toleration, t corev1.Toleration) bool {
	for _, e := range ts {
		if e.Key == t.Key && e.Value == t.Value && e.Effect == t.Effect && e.Operator == t.Operator {
			return true
		}
	}
	return false
}

// ApplyRunDeadline sets pod.Spec.ActiveDeadlineSeconds as a kernel-enforced hard
// backstop on a run's wall-clock budget: ceil(maxWallClockSeconds * multiplier),
// minimum 1. The multiplier (default 1.5 when <= 0) leaves headroom so the
// in-process budget context fires first and produces a RunResult, with the pod
// deadline only as the floor for a wedged process. No-op when
// maxWallClockSeconds <= 0 (no budget to enforce).
func ApplyRunDeadline(pod *corev1.Pod, maxWallClockSeconds int32, multiplier float64) {
	if maxWallClockSeconds <= 0 {
		return
	}
	if multiplier <= 0 {
		multiplier = 1.5
	}
	secs := int64(math.Ceil(float64(maxWallClockSeconds) * multiplier))
	if secs < 1 {
		secs = 1
	}
	pod.Spec.ActiveDeadlineSeconds = &secs
}

// ApplySessionResources merges an AgentSession's pure quantity-string resource
// mirror onto the worker container (M1.11). Only a side that the spec provides
// overrides the container's default — so limits-only leaves the default requests
// intact, and vice-versa. No-op when r is nil. An unparseable quantity is skipped
// defensively (the AgentSession webhook rejects those at admission). A session
// has no wall-clock deadline, so sizing the worker is done here, not via budget.
func ApplySessionResources(c *corev1.Container, r *pure.ResourceRequirements) {
	if r == nil {
		return
	}
	if rl := toResourceList(r.Limits); rl != nil {
		c.Resources.Limits = rl
	}
	if rl := toResourceList(r.Requests); rl != nil {
		c.Resources.Requests = rl
	}
}

// toResourceList parses a name→quantity-string map into a corev1.ResourceList,
// skipping any value that does not parse. Returns nil for an empty/all-invalid
// map so callers can leave the existing side untouched.
func toResourceList(m map[string]string) corev1.ResourceList {
	if len(m) == 0 {
		return nil
	}
	rl := corev1.ResourceList{}
	for k, v := range m {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			continue
		}
		rl[corev1.ResourceName(k)] = q
	}
	if len(rl) == 0 {
		return nil
	}
	return rl
}
