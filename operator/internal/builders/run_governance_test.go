package builders

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestApplySessionResources(t *testing.T) {
	// limits-only must leave the container's default requests untouched (merge).
	c := &corev1.Container{Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
	}}
	ApplySessionResources(c, &pure.ResourceRequirements{Limits: map[string]string{"memory": "512Mi"}})
	if got := c.Resources.Limits.Memory(); got.String() != "512Mi" {
		t.Errorf("limit memory = %s, want 512Mi", got.String())
	}
	if got := c.Resources.Requests.Cpu(); got.String() != "100m" {
		t.Errorf("default request cpu = %s, want preserved 100m", got.String())
	}
}

func TestApplySessionResources_NilNoOp(t *testing.T) {
	c := &corev1.Container{Resources: corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
	}}
	ApplySessionResources(c, nil)
	if got := c.Resources.Limits.Cpu(); got.String() != "1" {
		t.Errorf("nil resources must be a no-op; limit cpu = %s", got.String())
	}
}

func TestApplySessionResources_BadQuantitySkipped(t *testing.T) {
	c := &corev1.Container{}
	// "500x" does not parse → that entry is dropped, "1" survives.
	ApplySessionResources(c, &pure.ResourceRequirements{Requests: map[string]string{"cpu": "1", "memory": "500x"}})
	if got := c.Resources.Requests.Cpu(); got.String() != "1" {
		t.Errorf("valid request cpu = %s, want 1", got.String())
	}
	if _, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		t.Error("unparseable memory quantity must be skipped")
	}
}

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
