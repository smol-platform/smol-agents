package main

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestFollowRun_TerminalOutcomes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := amv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mk := func(state pure.Phase, output string) *amv1.AgentRun {
		r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "d"}}
		r.Spec = pure.AgentRunSpec{AgentRef: "a", Input: []byte(`{}`)}
		r.Status.State = state
		if output != "" {
			r.Status.Output = []byte(output)
		}
		return r
	}
	key := types.NamespacedName{Namespace: "d", Name: "r1"}

	// Completed → exit 0.
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mk(pure.PhaseCompleted, `{"ok":true}`)).Build()
	if code := followRun(context.Background(), cli, key); code != 0 {
		t.Fatalf("completed run must exit 0, got %d", code)
	}

	// Failed → exit 1.
	cli = fake.NewClientBuilder().WithScheme(scheme).WithObjects(mk(pure.PhaseFailed, "")).Build()
	if code := followRun(context.Background(), cli, key); code != 1 {
		t.Fatalf("failed run must exit 1, got %d", code)
	}

	// Missing run → exit 1 (not a hang).
	cli = fake.NewClientBuilder().WithScheme(scheme).Build()
	if code := followRun(context.Background(), cli, key); code != 1 {
		t.Fatalf("missing run must exit 1, got %d", code)
	}

	// Non-terminal + expired context → exit 1 via timeout branch.
	cli = fake.NewClientBuilder().WithScheme(scheme).WithObjects(mk(pure.PhaseRunning, "")).Build()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if code := followRun(ctx, cli, key); code != 1 {
		t.Fatalf("timed-out watch must exit 1, got %d", code)
	}
}
