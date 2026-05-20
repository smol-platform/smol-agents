package controllers

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/stigen/knative-agents/operator/api/v1"
)

// TestAgentNodePool_setCondition_UpsertStableTimestamp covers the pure
// status logic: upsert by type, preserve LastTransitionTime when the status
// is unchanged, and append distinct types. (Reconcile itself needs envtest
// + the Karpenter CRDs — tracked as a follow-up per the design's testing
// strategy.)
func TestAgentNodePool_setCondition_UpsertStableTimestamp(t *testing.T) {
	r := &AgentNodePoolReconciler{}
	anp := &v1.AgentNodePool{}
	anp.Generation = 2

	r.setCondition(anp, "Ready", metav1.ConditionFalse, "Pending", "first")
	if len(anp.Status.Conditions) != 1 {
		t.Fatalf("want 1 condition, got %d", len(anp.Status.Conditions))
	}
	first := anp.Status.Conditions[0].LastTransitionTime

	// Same status → timestamp preserved, reason/message updated in place.
	r.setCondition(anp, "Ready", metav1.ConditionFalse, "StillPending", "second")
	if len(anp.Status.Conditions) != 1 {
		t.Fatalf("upsert must not append same type, got %d", len(anp.Status.Conditions))
	}
	if !anp.Status.Conditions[0].LastTransitionTime.Equal(&first) {
		t.Error("LastTransitionTime changed despite unchanged status")
	}
	if anp.Status.Conditions[0].Reason != "StillPending" {
		t.Errorf("reason = %q, want StillPending", anp.Status.Conditions[0].Reason)
	}

	// Status flip is recorded.
	r.setCondition(anp, "Ready", metav1.ConditionTrue, "Reconciled", "")
	if anp.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Error("status not flipped to True")
	}

	// Distinct type appends.
	r.setCondition(anp, "KarpenterSynced", metav1.ConditionTrue, "Synced", "")
	if len(anp.Status.Conditions) != 2 {
		t.Fatalf("want 2 conditions after distinct type, got %d", len(anp.Status.Conditions))
	}
}

// stubPlatformClient is a minimal client.Client that answers Get for the
// singleton platform — enough to test resolveDefaults without envtest.
type stubPlatformClient struct {
	client.Client
	platform *v1.KnativeAgentPlatform
}

func (s stubPlatformClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if s.platform == nil {
		return apierrors.NewNotFound(schema.GroupResource{
			Group: "agents.stigen.ai", Resource: "knativeagentplatforms",
		}, key.Name)
	}
	if p, ok := obj.(*v1.KnativeAgentPlatform); ok {
		s.platform.DeepCopyInto(p)
	}
	return nil
}

func TestAgentNodePool_resolveDefaults_FromPlatform(t *testing.T) {
	plat := &v1.KnativeAgentPlatform{}
	plat.Name = "default"
	plat.Spec.NodeProvisioning = v1.NodeProvisioningSpec{
		AMIFamily:          "Custom",
		Role:               "KarpenterNodeRole-k0s",
		SubnetSelectorTags: map[string]string{"karpenter.sh/discovery": "k0s"},
		JoinUserData:       "#!/bin/bash\nk0s install worker\n",
		BaseAMISelector:    []v1.AMISelectorTerm{{Tags: map[string]string{"k0s-join": "true"}}},
	}
	r := &AgentNodePoolReconciler{Client: stubPlatformClient{platform: plat}, PlatformName: "default"}

	d := r.resolveDefaults(context.Background())
	if d.Role != "KarpenterNodeRole-k0s" || d.JoinUserData == "" || len(d.BaseAMISelector) != 1 {
		t.Errorf("defaults not sourced from platform: %+v", d)
	}
	if d.SubnetSelectorTags["karpenter.sh/discovery"] != "k0s" {
		t.Errorf("subnet tags = %v", d.SubnetSelectorTags)
	}
}

func TestAgentNodePool_resolveDefaults_PlatformAbsent(t *testing.T) {
	r := &AgentNodePoolReconciler{Client: stubPlatformClient{}, PlatformName: "default"}
	d := r.resolveDefaults(context.Background())
	if d.AMIFamily != "Custom" {
		t.Errorf("absent platform should still default amiFamily=Custom, got %q", d.AMIFamily)
	}
	if d.Role != "" {
		t.Errorf("absent platform should yield empty role, got %q", d.Role)
	}
}
