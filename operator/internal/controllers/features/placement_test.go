package features

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/stigen/smol-agents/operator/api/v1"
)

// stubReader is a minimal client.Reader returning a fixed AgentNodePool
// list — avoids pulling the controller-runtime fake client for a pure
// resolution test.
type stubReader struct {
	pools []v1.AgentNodePool
	cm    *corev1.ConfigMap
}

func (s stubReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if c, ok := obj.(*corev1.ConfigMap); ok {
		if s.cm == nil {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name)
		}
		s.cm.DeepCopyInto(c)
		return nil
	}
	return nil
}

func (s stubReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l := list.(*v1.AgentNodePoolList)
	l.Items = append([]v1.AgentNodePool(nil), s.pools...)
	return nil
}

func kataAgent(rc string) *v1.SmolAgent {
	cr := &v1.SmolAgent{}
	cr.Name, cr.Namespace = "a", "ns"
	cr.Spec.Features.Sandbox.RuntimeClass = rc
	return cr
}

func pool(name, isolation string) v1.AgentNodePool {
	p := v1.AgentNodePool{}
	p.Name = name
	p.Spec.Isolation = isolation
	return p
}

func TestResolvePlacement_MatchByIsolation(t *testing.T) {
	r := stubReader{pools: []v1.AgentNodePool{pool("kata-arm64", "kata-fc")}}
	p, ok, err := ResolvePlacement(context.Background(), Env{CR: kataAgent("kata-fc"), Reader: r})
	if err != nil || !ok {
		t.Fatalf("want match, got ok=%v err=%v", ok, err)
	}
	if p.PoolName != "kata-arm64" || p.Isolation != "kata-fc" {
		t.Errorf("placement = %+v", p)
	}
}

func TestResolvePlacement_DefaultRuntimeClassMatches(t *testing.T) {
	r := stubReader{pools: []v1.AgentNodePool{pool("kata-arm64", "kata-fc")}}
	// Empty runtimeClass defaults to kata-fc.
	if _, ok, err := ResolvePlacement(context.Background(), Env{CR: kataAgent(""), Reader: r}); err != nil || !ok {
		t.Errorf("empty runtimeClass should default to kata-fc and match: ok=%v err=%v", ok, err)
	}
}

func TestResolvePlacement_DeterministicLowestName(t *testing.T) {
	r := stubReader{pools: []v1.AgentNodePool{pool("kata-z", "kata-fc"), pool("kata-a", "kata-fc")}}
	p, ok, _ := ResolvePlacement(context.Background(), Env{CR: kataAgent("kata-fc"), Reader: r})
	if !ok || p.PoolName != "kata-a" {
		t.Errorf("want lowest-name pool kata-a, got %+v ok=%v", p, ok)
	}
}

func TestResolvePlacement_GvisorNoPlacement(t *testing.T) {
	r := stubReader{pools: []v1.AgentNodePool{pool("g", "gvisor")}}
	if _, ok, err := ResolvePlacement(context.Background(), Env{CR: kataAgent("gvisor"), Reader: r}); err != nil || ok {
		t.Errorf("gvisor must not require metal placement: ok=%v err=%v", ok, err)
	}
}

func TestResolvePlacement_NoPoolNoMatch(t *testing.T) {
	r := stubReader{}
	if _, ok, err := ResolvePlacement(context.Background(), Env{CR: kataAgent("kata-fc"), Reader: r}); err != nil || ok {
		t.Errorf("no pool → no placement (fallback handled elsewhere): ok=%v err=%v", ok, err)
	}
}

func TestResolvePlacement_NilReader(t *testing.T) {
	if _, ok, err := ResolvePlacement(context.Background(), Env{CR: kataAgent("kata-fc")}); err != nil || ok {
		t.Errorf("nil reader → no placement: ok=%v err=%v", ok, err)
	}
}

func enabledFlags() *corev1.ConfigMap {
	return &corev1.ConfigMap{Data: map[string]string{
		"kubernetes.podspec-runtimeclassname": "enabled",
		"kubernetes.podspec-affinity":         "enabled",
		"kubernetes.podspec-tolerations":      "enabled",
		"kubernetes.podspec-nodeselector":     "enabled",
	}}
}

func TestMissingKnativePodspecFlags_AllEnabled(t *testing.T) {
	if m := MissingKnativePodspecFlags(context.Background(), stubReader{cm: enabledFlags()}); len(m) != 0 {
		t.Errorf("want none missing, got %v", m)
	}
}

func TestMissingKnativePodspecFlags_SomeMissing(t *testing.T) {
	cm := &corev1.ConfigMap{Data: map[string]string{"kubernetes.podspec-runtimeclassname": "enabled"}}
	if m := MissingKnativePodspecFlags(context.Background(), stubReader{cm: cm}); len(m) != 3 {
		t.Errorf("want 3 missing, got %v", m)
	}
}

func TestMissingKnativePodspecFlags_AbsentIsBestEffort(t *testing.T) {
	if m := MissingKnativePodspecFlags(context.Background(), stubReader{}); m != nil {
		t.Errorf("absent config-features → nil (best-effort), got %v", m)
	}
}
