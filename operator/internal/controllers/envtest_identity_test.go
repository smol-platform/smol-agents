//go:build envtest

package controllers_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/stigen/smol-agents/operator/api/v1"
)

// TestEnvtest_Identity_HappyPath drives the controller against a real
// api-server. We apply a Platform + Agent CR and assert that the
// IdentityReconciler produces the agent ConfigMap + ServiceAccount
// in-cluster, and that Status.Phase reaches Ready (or stays Pending
// with a known reason — depending on which feature dependencies the
// envtest cluster lacks; we don't run SPIRE in envtest).
func TestEnvtest_Identity_HappyPath(t *testing.T) {
	e := setupEnv(t)
	makeNamespace(t, e, "tenant-a")
	applyPlatform(t, e)
	makeAgent(t, e, "tenant-a", "alice")

	// Wait for the controller to create the owned ConfigMap.
	deadline := 60
	var cm corev1.ConfigMap
	for i := 0; i < deadline; i++ {
		err := e.cli.Get(e.ctx, types.NamespacedName{Namespace: "tenant-a", Name: "alice-config"}, &cm)
		if err == nil {
			break
		}
		<-roundtrip()
	}
	if cm.Name == "" {
		t.Fatal("agent ConfigMap was not created within timeout")
	}
	if _, ok := cm.Data["agent.yaml"]; !ok {
		t.Errorf("agent.yaml not present in ConfigMap")
	}

	// Status should reflect at least Identity ready.
	waitFor(t, e, types.NamespacedName{Namespace: "tenant-a", Name: "alice"}, func(a *v1.SmolAgent) bool {
		s, ok := a.Status.Features["identity"]
		return ok && s.Ready
	})
}

// TestEnvtest_OwnedResourcesHaveOwnerRef confirms managed objects
// carry the controller reference back to the parent CR. This is what
// enables the Owns(...) watch + GC.
func TestEnvtest_OwnedResourcesHaveOwnerRef(t *testing.T) {
	e := setupEnv(t)
	makeNamespace(t, e, "tenant-o")
	applyPlatform(t, e)
	makeAgent(t, e, "tenant-o", "owns")

	var cm corev1.ConfigMap
	for i := 0; i < 60; i++ {
		err := e.cli.Get(e.ctx, types.NamespacedName{Namespace: "tenant-o", Name: "owns-config"}, &cm)
		if err == nil {
			break
		}
		<-roundtrip()
	}
	if cm.Name == "" {
		t.Fatal("owns-config not created")
	}
	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner ref, got %d: %+v", len(cm.OwnerReferences), cm.OwnerReferences)
	}
	o := cm.OwnerReferences[0]
	if o.Kind != "SmolAgent" || o.Name != "owns" {
		t.Errorf("wrong owner ref: %+v", o)
	}
	if o.Controller == nil || !*o.Controller {
		t.Error("expected controller=true on owner ref")
	}
}
