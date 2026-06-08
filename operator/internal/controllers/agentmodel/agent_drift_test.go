package agentmodel

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/smol-platform/smol-agents/operator/internal/builders"
)

// TestAgentReconciler_DriftHealing_RecreatesDeletedServiceAccount proves the
// controller's level-triggered drift healing (R-OP-REC-3): when the managed
// per-Agent ServiceAccount is deleted out from under the operator, the next
// reconcile recreates it. This is a pure fake-client proof — no live cluster.
func TestAgentReconciler_DriftHealing_RecreatesDeletedServiceAccount(t *testing.T) {
	ctx := context.Background()
	agent := harnessAgent("drifty", "tenant-a")
	r := newAgentReconcilerForTest(t, agent)

	saName := builders.AgentServiceAccount(agent).Name
	key := types.NamespacedName{Namespace: "tenant-a", Name: saName}

	// First reconcile creates the managed SA.
	reconcileAgent(t, r, "tenant-a", "drifty")
	if err := r.Get(ctx, key, &corev1.ServiceAccount{}); err != nil {
		t.Fatalf("managed SA %q not created on first reconcile: %v", saName, err)
	}

	// Drift: a human or another field manager deletes the managed SA.
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, key, sa); err != nil {
		t.Fatalf("get SA: %v", err)
	}
	if err := r.Delete(ctx, sa); err != nil {
		t.Fatalf("delete SA: %v", err)
	}
	if err := r.Get(ctx, key, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("SA should be gone after delete, got err=%v", err)
	}

	// Level-triggered reconcile heals the drift back to spec.
	reconcileAgent(t, r, "tenant-a", "drifty")
	if err := r.Get(ctx, key, &corev1.ServiceAccount{}); err != nil {
		t.Fatalf("drift not healed — managed SA still missing after reconcile: %v", err)
	}
}
