package agentmodel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

// wbb: when a workflow reaches END it emits exactly one
// com.smol-agents.workflow.completed CloudEvent carrying the final node's output,
// and a re-reconcile of the Completed workflow does not re-emit.
func TestAgentWorkflowReconcile_EmitsResultOnCompletion(t *testing.T) {
	sch := teamScheme(t)

	var posts int32
	var gotType, gotID, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		gotType = r.Header.Get("Ce-Type")
		gotID = r.Header.Get("Ce-Id")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	wf := wfFixture()
	wf.Spec.ResultSink = srv.URL
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(wf).
		WithStatusSubresource(&amv1.AgentWorkflow{}, &amv1.AgentRun{}).Build()
	r := &AgentWorkflowReconciler{Client: c, Scheme: sch, ResultSinkClient: srv.Client()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "wf"}}

	// Drive the DAG to END: research (score 90) → review → END.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	completeRun(t, c, wfChild(t, c, "research"), `{"score":90}`, 10)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	completeRun(t, c, wfChild(t, c, "review"), `{"result":"done"}`, 20)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}

	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("posts = %d on completion, want 1", posts)
	}
	if gotType != "com.smol-agents.workflow.completed" {
		t.Errorf("ce-type = %q", gotType)
	}
	if gotID != "wf-uid" {
		t.Errorf("ce-id = %q, want the workflow UID", gotID)
	}
	if gotBody != `{"result":"done"}` {
		t.Errorf("body = %s, want the END-reaching node's output", gotBody)
	}

	// A re-reconcile of the Completed workflow must not re-emit (annotation guard).
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 4: %v", err)
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Errorf("posts = %d after re-reconcile, want still 1 (guard)", posts)
	}
}

// A workflow with no resultSink emits nothing.
func TestAgentWorkflowReconcile_NoSinkNoEmit(t *testing.T) {
	sch := teamScheme(t)
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	wf := wfFixture() // no ResultSink
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(wf).
		WithStatusSubresource(&amv1.AgentWorkflow{}, &amv1.AgentRun{}).Build()
	r := &AgentWorkflowReconciler{Client: c, Scheme: sch, ResultSinkClient: srv.Client()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "wf"}}

	_, _ = r.Reconcile(context.Background(), req)
	completeRun(t, c, wfChild(t, c, "research"), `{"score":90}`, 10)
	_, _ = r.Reconcile(context.Background(), req)
	completeRun(t, c, wfChild(t, c, "review"), `{}`, 20)
	_, _ = r.Reconcile(context.Background(), req)
	if atomic.LoadInt32(&posts) != 0 {
		t.Errorf("no-sink workflow emitted; posts = %d, want 0", posts)
	}
}
