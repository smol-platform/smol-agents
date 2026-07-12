package controllers

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
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

func TestAgentNodePool_resolveDefaults_FromSpec(t *testing.T) {
	anp := &v1.AgentNodePool{}
	anp.Spec.AMIFamily = "Custom"
	anp.Spec.Role = "KarpenterNodeRole-k0s"
	anp.Spec.SubnetSelectorTags = map[string]string{"karpenter.sh/discovery": "k0s"}
	anp.Spec.JoinUserData = "#!/bin/bash\nk0s install worker\n"
	anp.Spec.BaseAMISelector = []v1.AMISelectorTerm{{Tags: map[string]string{"k0s-join": "true"}}}

	d := resolveDefaults(anp)
	if d.Role != "KarpenterNodeRole-k0s" || d.JoinUserData == "" || len(d.BaseAMISelector) != 1 {
		t.Errorf("defaults not sourced from spec: %+v", d)
	}
	if d.SubnetSelectorTags["karpenter.sh/discovery"] != "k0s" {
		t.Errorf("subnet tags = %v", d.SubnetSelectorTags)
	}
}

func TestAgentNodePool_resolveDefaults_SpecEmpty(t *testing.T) {
	anp := &v1.AgentNodePool{}
	d := resolveDefaults(anp)
	if d.AMIFamily != "Custom" {
		t.Errorf("empty spec should still default amiFamily=Custom, got %q", d.AMIFamily)
	}
	if d.Role != "" {
		t.Errorf("empty spec should yield empty role, got %q", d.Role)
	}
}
