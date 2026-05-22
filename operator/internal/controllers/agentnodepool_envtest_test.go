//go:build envtest

package controllers_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

// TestAgentNodePool_Reconcile_KarpenterMissingDegraded drives the real
// reconcile through envtest: create an AgentNodePool, and since the
// Karpenter CRDs are not installed in the test apiserver the controller
// cannot apply the NodePool/EC2NodeClass and must report
// Degraded / KarpenterMissing (rather than hot-looping or going Ready).
func TestAgentNodePool_Reconcile_KarpenterMissingDegraded(t *testing.T) {
	e := setupEnv(t)
	applyPlatform(t, e)

	anp := &v1.AgentNodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "kata-arm64"},
		Spec: v1.AgentNodePoolSpec{
			Isolation: "kata-fc",
			Arch:      "arm64",
			Bootstrap: v1.NodeBootstrap{Mode: "UserData", Distro: "al2023"},
		},
	}
	if err := e.cli.Create(e.ctx, anp); err != nil {
		t.Fatalf("create AgentNodePool: %v", err)
	}

	waitForANP(t, e, types.NamespacedName{Name: "kata-arm64"}, func(a *v1.AgentNodePool) bool {
		return a.Status.Phase == "Degraded" && condReason(a, "KarpenterSynced") == "KarpenterMissing"
	})
}

func waitForANP(t *testing.T, e *envContext, key types.NamespacedName, pred func(*v1.AgentNodePool) bool) {
	t.Helper()
	for i := 0; i < 30; i++ {
		got := &v1.AgentNodePool{}
		if err := e.cli.Get(e.ctx, key, got); err == nil && pred(got) {
			return
		}
		<-roundtrip()
	}
	t.Fatalf("timeout waiting for AgentNodePool predicate on %s", key)
}

func condReason(a *v1.AgentNodePool, condType string) string {
	for _, c := range a.Status.Conditions {
		if c.Type == condType {
			return c.Reason
		}
	}
	return ""
}

// TestAgentNodePool_Reconcile_ClusterAutoscalerReady drives the CAS provider
// path: it needs no Karpenter CRDs (CAS scales an external ASG), so the
// operator just emits the node-group ConfigMap and the pool goes Ready —
// a deterministic happy-path reconcile through envtest.
func TestAgentNodePool_Reconcile_ClusterAutoscalerReady(t *testing.T) {
	e := setupEnv(t)
	applyPlatform(t, e)
	makeNamespace(t, e, "smol-agents-system")

	anp := &v1.AgentNodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "ca-kata"},
		Spec: v1.AgentNodePoolSpec{
			Isolation: "kata-fc",
			Arch:      "arm64",
			Provider:  "ClusterAutoscaler",
			Bootstrap: v1.NodeBootstrap{Mode: "UserData", Distro: "al2023"},
		},
	}
	if err := e.cli.Create(e.ctx, anp); err != nil {
		t.Fatalf("create AgentNodePool: %v", err)
	}

	waitForANP(t, e, types.NamespacedName{Name: "ca-kata"}, func(a *v1.AgentNodePool) bool {
		return a.Status.Phase == "Ready" && condReason(a, "NodeGroupRendered") == "ClusterAutoscaler"
	})

	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: "smol-agents-system", Name: "anp-ca-kata-clusterautoscaler"}
	if err := e.cli.Get(e.ctx, key, cm); err != nil {
		t.Fatalf("CAS node-group ConfigMap not created: %v", err)
	}
	if cm.Data["provider"] != "cluster-autoscaler" {
		t.Errorf("ConfigMap provider = %q", cm.Data["provider"])
	}
}
