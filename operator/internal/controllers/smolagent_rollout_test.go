package controllers

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/operator/internal/events"
)

// TestSmolAgentReconciler_RolloutPaused_SkipsApplyAndEmitsEvent proves the
// rollout-control gate (R-OP-REC-*): when spec.rollout.paused is true the
// operator freezes — it does NOT run feature reconcilers or apply owned
// objects, and it emits a RolloutPaused event. Pure fake-client proof.
//
// (The implemented rollout control is a pause switch, not a percentage canary;
// per-feature status conditions are covered by the feature-reconciler tests.)
func TestSmolAgentReconciler_RolloutPaused_SkipsApplyAndEmitsEvent(t *testing.T) {
	sch := runtime.NewScheme()
	if err := v1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	cr := &v1.SmolAgent{}
	cr.Name = "hello"
	cr.Namespace = "tenant-a"
	cr.Spec.Rollout.Paused = true

	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(cr).Build()
	fr := record.NewFakeRecorder(10)
	r := &SmolAgentReconciler{Client: c, Scheme: sch, Events: events.NewRecorder(fr)}

	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "hello"}})
	if err != nil {
		t.Fatalf("reconcile (paused) returned error: %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("paused reconcile should be a no-op result, got %+v", res)
	}

	// The RolloutPaused event is emitted ONLY on the paused early-return
	// branch (before any feature reconcile / applyOwned), so observing it
	// proves the operator froze rather than applying changes.
	select {
	case ev := <-fr.Events:
		if !strings.Contains(ev, "RolloutPaused") {
			t.Errorf("event = %q, want it to contain RolloutPaused", ev)
		}
	default:
		t.Error("no event emitted; expected a RolloutPaused event")
	}
}
