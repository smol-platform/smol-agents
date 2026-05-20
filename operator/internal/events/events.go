// Package events typed helpers around record.EventRecorder. Each
// helper emits a Kubernetes Event with a stable Reason string so SREs
// can search for them. R-OP-OBS-2.
package events

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	"github.com/stigen/smol-agents/operator/pkg/features"
)

// Reason names — kept here so test asserts can reference them.
const (
	ReasonFeatureEnabled       = "FeatureEnabled"
	ReasonFeaturePrereqMissing = "FeaturePrereqMissing"
	ReasonFeatureReady         = "FeatureReady"
	ReasonReconcileFailed      = "ReconcileFailed"
	ReasonRolloutPaused        = "RolloutPaused"
	ReasonRolloutResumed       = "RolloutResumed"
	ReasonPolicyViolation      = "PolicyViolation"
)

// Recorder is a thin wrapper that types Reason strings so callers can't
// drift from the canonical set above.
type Recorder struct {
	r record.EventRecorder
}

// NewRecorder wraps a controller-runtime / client-go EventRecorder.
func NewRecorder(r record.EventRecorder) *Recorder {
	return &Recorder{r: r}
}

// FeatureReady emits a Normal event when a feature transitions to Ready.
func (e *Recorder) FeatureReady(obj runtime.Object, f features.Feature) {
	if e == nil || e.r == nil {
		return
	}
	e.r.Event(obj, corev1.EventTypeNormal, ReasonFeatureReady,
		fmt.Sprintf("feature %s reconciled to Ready", f))
}

// FeaturePrereqMissing is a Warning event explaining why a feature is
// not Ready.
func (e *Recorder) FeaturePrereqMissing(obj runtime.Object, f features.Feature, message string) {
	if e == nil || e.r == nil {
		return
	}
	e.r.Eventf(obj, corev1.EventTypeWarning, ReasonFeaturePrereqMissing,
		"feature %s prerequisites unmet: %s", f, message)
}

// ReconcileFailed marks a reconcile loop error.
func (e *Recorder) ReconcileFailed(obj runtime.Object, err error) {
	if e == nil || e.r == nil {
		return
	}
	e.r.Eventf(obj, corev1.EventTypeWarning, ReasonReconcileFailed,
		"reconcile failed: %v", err)
}

// RolloutPaused emits when spec.rollout.paused becomes true.
func (e *Recorder) RolloutPaused(obj runtime.Object) {
	if e == nil || e.r == nil {
		return
	}
	e.r.Event(obj, corev1.EventTypeNormal, ReasonRolloutPaused,
		"rollout paused; operator is not applying changes")
}

// PolicyViolation reports a configuration that the validating webhook
// caught at admission OR that the reconciler rejected at reconcile time.
func (e *Recorder) PolicyViolation(obj runtime.Object, message string) {
	if e == nil || e.r == nil {
		return
	}
	e.r.Event(obj, corev1.EventTypeWarning, ReasonPolicyViolation, message)
}
