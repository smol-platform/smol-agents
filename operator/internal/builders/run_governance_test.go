package builders

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestApplyRunPodPlacement(t *testing.T) {
	pod := &corev1.Pod{}
	p := &NodePlacement{PoolName: "kata-arm64", Isolation: "kata-fc"}

	ApplyRunPodPlacement(pod, p)
	na := pod.Spec.Affinity.NodeAffinity
	me := na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
	if me.Key != PoolLabelKey || me.Values[0] != "kata-arm64" {
		t.Errorf("nodeAffinity = %+v, want %s In [kata-arm64]", me, PoolLabelKey)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Value != "kata-fc" {
		t.Errorf("toleration = %+v", pod.Spec.Tolerations)
	}
	if pod.Annotations[DoNotDisruptAnnotation] != "true" {
		t.Errorf("do-not-disrupt annotation missing: %v", pod.Annotations)
	}

	// Idempotent: re-applying must not duplicate the toleration.
	ApplyRunPodPlacement(pod, p)
	if len(pod.Spec.Tolerations) != 1 {
		t.Errorf("toleration duplicated on re-apply: %+v", pod.Spec.Tolerations)
	}
}

func TestApplyRunPodPlacement_NilNoOp(t *testing.T) {
	pod := &corev1.Pod{}
	ApplyRunPodPlacement(pod, nil)
	ApplyRunPodPlacement(pod, &NodePlacement{})
	if pod.Spec.Affinity != nil || len(pod.Spec.Tolerations) != 0 || pod.Annotations != nil {
		t.Errorf("nil/empty placement must be a no-op, got %+v", pod.Spec)
	}
}

func TestApplyRunDeadline(t *testing.T) {
	cases := []struct {
		name string
		secs int32
		mult float64
		want int64 // -1 = expect unset
	}{
		{"30x1.5=45", 30, 1.5, 45},
		{"ceil-fractional", 10, 1.51, 16}, // ceil(15.1)=16
		{"default-mult-zero", 100, 0, 150},
		{"default-mult-negative", 100, -2, 150},
		{"min-1", 1, 0.01, 1},
		{"no-op-zero", 0, 1.5, -1},
		{"no-op-negative", -5, 1.5, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod := &corev1.Pod{}
			ApplyRunDeadline(pod, c.secs, c.mult)
			if c.want < 0 {
				if pod.Spec.ActiveDeadlineSeconds != nil {
					t.Errorf("want unset, got %d", *pod.Spec.ActiveDeadlineSeconds)
				}
				return
			}
			if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != c.want {
				t.Errorf("ActiveDeadlineSeconds = %v, want %d", pod.Spec.ActiveDeadlineSeconds, c.want)
			}
		})
	}
}
