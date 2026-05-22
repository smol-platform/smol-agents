package controllers

import (
	"testing"
	"time"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/operator/pkg/features"
)

func TestAggregatePhase_Ready(t *testing.T) {
	r := []FeatureResult{
		{Feature: features.Identity, Enabled: true, Ready: true},
		{Feature: features.Sandbox, Enabled: true, Ready: true},
		{Feature: features.TransportPublic, Enabled: false, Ready: false},
	}
	if AggregatePhase(r) != "Ready" {
		t.Errorf("got %s, want Ready", AggregatePhase(r))
	}
}

func TestAggregatePhase_Pending(t *testing.T) {
	r := []FeatureResult{
		{Feature: features.Identity, Enabled: true, Ready: false},
		{Feature: features.Sandbox, Enabled: true, Ready: true},
	}
	if AggregatePhase(r) != "Pending" {
		t.Errorf("got %s, want Pending", AggregatePhase(r))
	}
}

func TestAggregatePhase_FailedTrumpsPending(t *testing.T) {
	r := []FeatureResult{
		{Feature: features.Identity, Enabled: true, Ready: false, Err: errBoom("nope")},
	}
	if AggregatePhase(r) != "Failed" {
		t.Errorf("got %s, want Failed", AggregatePhase(r))
	}
}

func TestAggregatePhase_DisabledFeatureCannotFail(t *testing.T) {
	r := []FeatureResult{
		// Even if Err is set on a disabled feature we ignore it.
		{Feature: features.Identity, Enabled: false, Ready: false, Err: errBoom("ignored")},
		{Feature: features.Sandbox, Enabled: true, Ready: true},
	}
	if AggregatePhase(r) != "Ready" {
		t.Errorf("got %s, want Ready", AggregatePhase(r))
	}
}

func TestApplyFeatureResults_PopulatesEverything(t *testing.T) {
	cr := &v1.SmolAgent{}
	cr.Generation = 7
	now := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)

	results := []FeatureResult{
		{Feature: features.Identity, Enabled: true, Ready: true, Mode: "strict", Reason: "Reconciled"},
		{Feature: features.Sandbox, Enabled: true, Ready: false, Reason: "PrerequisitesUnmet", Message: "RuntimeClass missing"},
		{Feature: features.TransportPublic, Enabled: false, Ready: false},
	}
	ApplyFeatureResults(cr, results, now)

	if cr.Status.Phase != "Pending" {
		t.Errorf("phase=%s, want Pending", cr.Status.Phase)
	}
	if cr.Status.ObservedGeneration != 7 {
		t.Errorf("observedGen=%d, want 7", cr.Status.ObservedGeneration)
	}
	if cr.Status.Features[string(features.Identity)].Mode != "strict" {
		t.Errorf("identity.mode = %q", cr.Status.Features[string(features.Identity)].Mode)
	}
	if cr.Status.Features[string(features.TransportPublic)].Reason != "Disabled" {
		t.Errorf("disabled feature reason = %q", cr.Status.Features[string(features.TransportPublic)].Reason)
	}
	// Conditions: 3 features + 1 aggregate.
	if len(cr.Status.Conditions) != 4 {
		t.Errorf("conditions=%d, want 4", len(cr.Status.Conditions))
	}
}

func TestApplyFeatureResults_Idempotent(t *testing.T) {
	cr := &v1.SmolAgent{}
	cr.Generation = 1
	r := []FeatureResult{{Feature: features.Identity, Enabled: true, Ready: true, Reason: "Reconciled"}}
	t1 := time.Unix(1000, 0)
	ApplyFeatureResults(cr, r, t1)
	originalTransition := cr.Status.Conditions[0].LastTransitionTime
	// Second apply with same result + later time should NOT bump LastTransitionTime.
	t2 := time.Unix(2000, 0)
	ApplyFeatureResults(cr, r, t2)
	if !cr.Status.Conditions[0].LastTransitionTime.Equal(&originalTransition) {
		t.Errorf("LastTransitionTime changed without status change")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func errBoom(s string) error { return errString(s) }
