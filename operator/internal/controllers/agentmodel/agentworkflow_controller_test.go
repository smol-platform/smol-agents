package agentmodel

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func wfFixture() *amv1.AgentWorkflow {
	return &amv1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "t", UID: "wf-uid", Generation: 1},
		Spec: pure.AgentWorkflowSpec{
			Nodes: []pure.WorkflowNode{{Name: "research", AgentRef: "r-agent"}, {Name: "review", AgentRef: "c-agent"}},
			Edges: []pure.WorkflowEdge{
				{From: pure.WorkflowStart, To: "research"},
				{From: "research", To: "review", When: "score >= 80"},
				{From: "research", To: pure.WorkflowEnd, When: "score < 80"},
				{From: "review", To: pure.WorkflowEnd},
			},
		},
	}
}

func wfChild(t *testing.T, c client.Client, node string) *amv1.AgentRun {
	t.Helper()
	var runs amv1.AgentRunList
	if err := c.List(context.Background(), &runs, client.InNamespace("t")); err != nil {
		t.Fatalf("list: %v", err)
	}
	for i := range runs.Items {
		if runs.Items[i].Labels[WorkflowNodeLabel] == node {
			return &runs.Items[i]
		}
	}
	return nil
}

func completeRun(t *testing.T, c client.Client, run *amv1.AgentRun, output string, tokens int64) {
	t.Helper()
	run.Status.State = pure.PhaseCompleted
	run.Status.Output = []byte(output)
	run.Status.Usage = pure.Usage{Tokens: tokens}
	if err := c.Status().Update(context.Background(), run); err != nil {
		t.Fatalf("status update: %v", err)
	}
}

func TestAgentWorkflowReconcile_DAGWalkAndRouting(t *testing.T) {
	sch := teamScheme(t)
	wf := wfFixture()
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(wf).
		WithStatusSubresource(&amv1.AgentWorkflow{}, &amv1.AgentRun{}).Build()
	r := &AgentWorkflowReconciler{Client: c, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "wf"}}

	// Reconcile 1: START → research activated → research child created.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	research := wfChild(t, c, "research")
	if research == nil {
		t.Fatal("research node child should be created from START")
	}
	if wfChild(t, c, "review") != nil {
		t.Fatal("review must NOT be created before research completes")
	}

	// research completes with score 90 → the score>=80 edge to review is taken.
	completeRun(t, c, research, `{"score":90}`, 10)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	review := wfChild(t, c, "review")
	if review == nil {
		t.Fatal("review child should be created after research scores >= 80")
	}

	// review completes → edge review→END satisfied → workflow Completed.
	completeRun(t, c, review, `{}`, 20)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	var got amv1.AgentWorkflow
	if err := c.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("get wf: %v", err)
	}
	if got.Status.Phase != pure.PhaseCompleted {
		t.Fatalf("phase: want Completed, got %q (%s)", got.Status.Phase, got.Status.Reason)
	}
	if got.Status.CumulativeUsage.Tokens != 30 { // 10 + 20 field-wise
		t.Fatalf("usage roll-up: want 30, got %d", got.Status.CumulativeUsage.Tokens)
	}
}

func TestAgentWorkflowReconcile_LowScoreRoutesToEnd(t *testing.T) {
	sch := teamScheme(t)
	wf := wfFixture()
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(wf).
		WithStatusSubresource(&amv1.AgentWorkflow{}, &amv1.AgentRun{}).Build()
	r := &AgentWorkflowReconciler{Client: c, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "wf"}}

	_, _ = r.Reconcile(context.Background(), req)
	research := wfChild(t, c, "research")
	completeRun(t, c, research, `{"score":50}`, 5) // < 80 → routes straight to END
	_, _ = r.Reconcile(context.Background(), req)

	if wfChild(t, c, "review") != nil {
		t.Fatal("score 50 must NOT activate review (predicate score>=80 false)")
	}
	var got amv1.AgentWorkflow
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != pure.PhaseCompleted {
		t.Fatalf("low score should route to END (Completed), got %q", got.Status.Phase)
	}
}

func TestAgentWorkflowReconcile_InvalidSpecFailsClosed(t *testing.T) {
	sch := teamScheme(t)
	wf := &amv1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "t", Generation: 1},
		Spec:       pure.AgentWorkflowSpec{}, // no nodes
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(wf).
		WithStatusSubresource(&amv1.AgentWorkflow{}).Build()
	r := &AgentWorkflowReconciler{Client: c, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "bad"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got amv1.AgentWorkflow
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "bad"}, &got)
	if got.Status.Phase != pure.PhaseFailed || got.Status.Reason != "InvalidSpec" {
		t.Fatalf("want Failed/InvalidSpec, got %q/%q", got.Status.Phase, got.Status.Reason)
	}
}
