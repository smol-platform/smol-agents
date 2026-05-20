package controllers

import (
	"testing"

	v1 "github.com/stigen/smol-agents/operator/api/v1"
)

func TestPlatform_setReady_Idempotent(t *testing.T) {
	r := &SmolAgentPlatformReconciler{}
	p := &v1.SmolAgentPlatform{}
	r.setReady(p, true, "Reconciled", "")
	first := p.Status.Conditions[0].LastTransitionTime
	r.setReady(p, true, "Reconciled", "")
	if !p.Status.Conditions[0].LastTransitionTime.Equal(&first) {
		t.Errorf("LastTransitionTime drifted on idempotent update")
	}
	if len(p.Status.Conditions) != 1 {
		t.Errorf("expected 1 condition, got %d", len(p.Status.Conditions))
	}
}

func TestPlatform_setReady_TransitionUpdates(t *testing.T) {
	r := &SmolAgentPlatformReconciler{}
	p := &v1.SmolAgentPlatform{}
	r.setReady(p, true, "Reconciled", "")
	first := p.Status.Conditions[0].LastTransitionTime
	// Sleep one nanosecond by mutating: any non-equal time should update.
	r.setReady(p, false, "ApplyFailed", "boom")
	if p.Status.Conditions[0].LastTransitionTime.Equal(&first) {
		t.Errorf("expected LastTransitionTime to update on status change")
	}
	if p.Status.Conditions[0].Reason != "ApplyFailed" {
		t.Errorf("reason = %q", p.Status.Conditions[0].Reason)
	}
}
