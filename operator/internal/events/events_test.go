package events

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/stigen/knative-agents/operator/pkg/features"
)

func TestRecorder_NilSafe(t *testing.T) {
	var r *Recorder
	r.FeatureReady(&corev1.Pod{}, features.Identity)
	r.FeaturePrereqMissing(&corev1.Pod{}, features.Sandbox, "x")
	r.ReconcileFailed(&corev1.Pod{}, nil)
	r.RolloutPaused(&corev1.Pod{})
	r.PolicyViolation(&corev1.Pod{}, "x")
	// no panic = pass
}

func TestRecorder_EmitsEvents(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	r := NewRecorder(fake)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	r.FeatureReady(pod, features.Identity)
	r.FeaturePrereqMissing(pod, features.EBPF, "missing platform")
	r.ReconcileFailed(pod, errBoom("oops"))
	r.RolloutPaused(pod)
	r.PolicyViolation(pod, "runc requires override")

	wantSub := []string{
		ReasonFeatureReady,
		ReasonFeaturePrereqMissing,
		ReasonReconcileFailed,
		ReasonRolloutPaused,
		ReasonPolicyViolation,
	}
	for _, want := range wantSub {
		select {
		case e := <-fake.Events:
			if !strings.Contains(e, want) {
				t.Errorf("event missing %q: %s", want, e)
			}
		default:
			t.Errorf("no event emitted for %q", want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func errBoom(s string) error { return errString(s) }
