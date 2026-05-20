// Package controllers hosts the operator's reconcilers and the
// status aggregation that turns per-feature results into the CR's
// top-level Phase + Conditions.
package controllers

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/stigen/smol-agents/operator/api/v1"
	"github.com/stigen/smol-agents/operator/pkg/features"
)

// FeatureResult aliases features.Result so existing imports keep
// compiling. The canonical type lives in pkg/features (R-OP-FF-3).
type FeatureResult = features.Result

// AggregatePhase derives the overall Phase from per-feature results.
//
//	Phase=Failed       — any non-disabled feature returned an error
//	Phase=Pending      — at least one enabled feature is not Ready and no error
//	Phase=Ready        — every enabled feature is Ready
//	Phase=Reconciling  — observed generation < spec generation (caller injects)
func AggregatePhase(results []FeatureResult) string {
	anyError := false
	allEnabledReady := true
	for _, r := range results {
		if r.Err != nil && r.Enabled {
			anyError = true
		}
		if r.Enabled && !r.Ready {
			allEnabledReady = false
		}
	}
	switch {
	case anyError:
		return "Failed"
	case allEnabledReady:
		return "Ready"
	default:
		return "Pending"
	}
}

// ApplyFeatureResults rewrites cr.Status.{Conditions,Features} given the
// reconcile results. The caller is responsible for persisting the CR.
func ApplyFeatureResults(cr *v1.SmolAgent, results []FeatureResult, now time.Time) {
	if cr.Status.Features == nil {
		cr.Status.Features = make(map[string]v1.FeatureStatus, len(results))
	}
	if cr.Status.Conditions == nil {
		cr.Status.Conditions = []metav1.Condition{}
	}
	mt := metav1.NewTime(now)

	for _, r := range results {
		condType := features.ConditionType(r.Feature)
		condStatus := metav1.ConditionFalse
		if r.Ready {
			condStatus = metav1.ConditionTrue
		}
		reason := r.Reason
		if reason == "" {
			if !r.Enabled {
				reason = "Disabled"
			} else if r.Ready {
				reason = "Reconciled"
			} else {
				reason = "Reconciling"
			}
		}
		setCondition(cr, metav1.Condition{
			Type:               condType,
			Status:             condStatus,
			Reason:             reason,
			Message:            r.Message,
			LastTransitionTime: mt,
			ObservedGeneration: cr.Generation,
		})
		cr.Status.Features[string(r.Feature)] = v1.FeatureStatus{
			Enabled:            r.Enabled,
			Ready:              r.Ready,
			Mode:               r.Mode,
			Reason:             reason,
			Message:            r.Message,
			LastTransitionTime: mt,
		}
	}

	// Aggregate "Ready" condition.
	phase := AggregatePhase(results)
	cr.Status.Phase = phase
	cr.Status.ObservedGeneration = cr.Generation
	aggStatus := metav1.ConditionFalse
	if phase == "Ready" {
		aggStatus = metav1.ConditionTrue
	}
	setCondition(cr, metav1.Condition{
		Type:               "Ready",
		Status:             aggStatus,
		Reason:             phase,
		LastTransitionTime: mt,
		ObservedGeneration: cr.Generation,
	})
}

// setCondition replaces the matching Type entry or appends.
func setCondition(cr *v1.SmolAgent, c metav1.Condition) {
	for i, existing := range cr.Status.Conditions {
		if existing.Type == c.Type {
			// Preserve LastTransitionTime if status hasn't changed.
			if existing.Status == c.Status && !existing.LastTransitionTime.Time.IsZero() {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			cr.Status.Conditions[i] = c
			return
		}
	}
	cr.Status.Conditions = append(cr.Status.Conditions, c)
}
