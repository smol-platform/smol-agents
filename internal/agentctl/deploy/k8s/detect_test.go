package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDetectAutoscalers_NeitherPresent(t *testing.T) {
	disc := newDiscoveryFake([]string{"v1", "apps/v1"})
	c := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	got, err := detectAutoscalers(context.Background(), disc, c)
	if err != nil {
		t.Fatalf("detectAutoscalers: %v", err)
	}
	if got.Karpenter || got.ClusterAutoscaler {
		t.Errorf("expected neither autoscaler, got %+v", got)
	}
}

func TestDetectAutoscalers_BothPresent(t *testing.T) {
	disc := newDiscoveryFake([]string{"v1", "apps/v1", "karpenter.sh/v1"})
	c := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-autoscaler", Namespace: "kube-system"},
			Status:     appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 2},
		}).
		Build()

	got, err := detectAutoscalers(context.Background(), disc, c)
	if err != nil {
		t.Fatalf("detectAutoscalers: %v", err)
	}
	if !got.Karpenter {
		t.Errorf("Karpenter not detected; detail=%q", got.KarpenterDetail)
	}
	if got.KarpenterDetail != "API group karpenter.sh/v1" {
		t.Errorf("KarpenterDetail = %q, want 'API group karpenter.sh/v1'", got.KarpenterDetail)
	}
	if !got.ClusterAutoscaler {
		t.Errorf("ClusterAutoscaler not detected; detail=%q", got.CADetail)
	}
	if got.CADetail == "" {
		t.Errorf("CADetail empty")
	}
}

func TestDetectAutoscalers_KarpenterOnly(t *testing.T) {
	disc := newDiscoveryFake([]string{"karpenter.sh/v1beta1"})
	c := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	got, err := detectAutoscalers(context.Background(), disc, c)
	if err != nil {
		t.Fatalf("detectAutoscalers: %v", err)
	}
	if !got.Karpenter {
		t.Errorf("Karpenter not detected")
	}
	if got.ClusterAutoscaler {
		t.Errorf("ClusterAutoscaler false-positive")
	}
}

// newDiscoveryFake builds a fake DiscoveryInterface whose ServerGroups reports
// the listed GroupVersions (in addition to core groups already wired by the
// fake clientset).
func newDiscoveryFake(extraGVs []string) discovery.DiscoveryInterface {
	cs := fake.NewSimpleClientset()
	for _, gv := range extraGVs {
		cs.Fake.Resources = append(cs.Fake.Resources, &metav1.APIResourceList{GroupVersion: gv})
	}
	return cs.Discovery()
}
