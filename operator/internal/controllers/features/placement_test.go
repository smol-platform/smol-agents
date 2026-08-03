package features

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

// stubReader is a minimal client.Reader returning a fixed AgentNodePool
// list — avoids pulling the controller-runtime fake client for a pure
// resolution test.
type stubReader struct {
	pools []v1.AgentNodePool
}

func (s stubReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return nil
}

func (s stubReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l := list.(*v1.AgentNodePoolList)
	l.Items = append([]v1.AgentNodePool(nil), s.pools...)
	return nil
}

func pool(name, isolation string) v1.AgentNodePool {
	p := v1.AgentNodePool{}
	p.Name = name
	p.Spec.Isolation = isolation
	return p
}

func TestResolvePlacementForClass_MatchByIsolation(t *testing.T) {
	r := stubReader{pools: []v1.AgentNodePool{pool("kata-arm64", "kata-fc")}}
	p, ok, err := ResolvePlacementForClass(context.Background(), r, "kata-fc")
	if err != nil || !ok {
		t.Fatalf("want match, got ok=%v err=%v", ok, err)
	}
	if p.PoolName != "kata-arm64" || p.Isolation != "kata-fc" {
		t.Errorf("placement = %+v", p)
	}
}

func TestResolvePlacementForClass_DefaultRuntimeClassMatches(t *testing.T) {
	r := stubReader{pools: []v1.AgentNodePool{pool("kata-arm64", "kata-fc")}}
	// Empty runtimeClass defaults to kata-fc.
	if _, ok, err := ResolvePlacementForClass(context.Background(), r, ""); err != nil || !ok {
		t.Errorf("empty runtimeClass should default to kata-fc and match: ok=%v err=%v", ok, err)
	}
}

func TestResolvePlacementForClass_DeterministicLowestName(t *testing.T) {
	r := stubReader{pools: []v1.AgentNodePool{pool("kata-z", "kata-fc"), pool("kata-a", "kata-fc")}}
	p, ok, _ := ResolvePlacementForClass(context.Background(), r, "kata-fc")
	if !ok || p.PoolName != "kata-a" {
		t.Errorf("want lowest-name pool kata-a, got %+v ok=%v", p, ok)
	}
}

func TestResolvePlacementForClass_GvisorNoPlacement(t *testing.T) {
	r := stubReader{pools: []v1.AgentNodePool{pool("g", "gvisor")}}
	if _, ok, err := ResolvePlacementForClass(context.Background(), r, "gvisor"); err != nil || ok {
		t.Errorf("gvisor must not require metal placement: ok=%v err=%v", ok, err)
	}
}

func TestResolvePlacementForClass_NoPoolNoMatch(t *testing.T) {
	r := stubReader{}
	if _, ok, err := ResolvePlacementForClass(context.Background(), r, "kata-fc"); err != nil || ok {
		t.Errorf("no pool → no placement (fallback handled elsewhere): ok=%v err=%v", ok, err)
	}
}

func TestResolvePlacementForClass_NilReader(t *testing.T) {
	if _, ok, err := ResolvePlacementForClass(context.Background(), nil, "kata-fc"); err != nil || ok {
		t.Errorf("nil reader → no placement: ok=%v err=%v", ok, err)
	}
}
