package controllers

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
