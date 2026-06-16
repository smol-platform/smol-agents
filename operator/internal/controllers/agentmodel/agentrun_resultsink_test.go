package agentmodel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func resultSinkScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = amv1.AddToScheme(sch)
	return sch
}

// wbb: a completed run emits one CloudEvent to the Agent's sink (ce-id = run UID,
// body = output), stamps the guard annotation, and never re-emits on a
// re-reconcile of the terminal run. A no-sink Agent emits nothing.
func TestEmitResultOnce(t *testing.T) {
	sch := resultSinkScheme(t)

	var posts int32
	var gotID, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		gotID = r.Header.Get("Ce-Id")
		gotType = r.Header.Get("Ce-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	run := &amv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "t", UID: types.UID("run-uid-1")},
		Spec:       pure.AgentRunSpec{AgentRef: "a"},
	}
	run.Status.State = pure.PhaseCompleted
	run.Status.Output = json.RawMessage(`{"answer":42}`)
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	agent.Spec.ResultSink = srv.URL

	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(run).Build()
	r := &AgentRunReconciler{Client: c, Scheme: sch, ResultSinkClient: srv.Client()}

	r.emitResultOnce(context.Background(), run, agent)
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("posts = %d, want 1", posts)
	}
	if gotID != "run-uid-1" {
		t.Errorf("ce-id = %q, want the run UID (stable id)", gotID)
	}
	if gotType != "com.smol-agents.run.completed" {
		t.Errorf("ce-type = %q", gotType)
	}
	if gotBody != `{"answer":42}` {
		t.Errorf("body = %s, want the run output", gotBody)
	}
	if run.Annotations[resultEmittedAnnotation] != "true" {
		t.Fatal("result-emitted annotation must be stamped after a successful emit")
	}

	// Re-reconcile of the terminal run → guarded, NO second POST.
	r.emitResultOnce(context.Background(), run, agent)
	if atomic.LoadInt32(&posts) != 1 {
		t.Errorf("posts = %d after re-emit, want still 1 (annotation guard)", posts)
	}

	// An Agent with no sink never emits.
	noSink := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "t"}}
	run2 := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: "t", UID: "u2"}}
	run2.Status.State = pure.PhaseCompleted
	r.emitResultOnce(context.Background(), run2, noSink)
	if atomic.LoadInt32(&posts) != 1 {
		t.Errorf("no-sink Agent emitted; posts = %d, want 1", posts)
	}
}

// A failing sink leaves the annotation unset so the next reconcile retries
// (at-least-once).
func TestEmitResultOnce_FailureRetries(t *testing.T) {
	sch := resultSinkScheme(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	run := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "t", UID: "u1"}}
	run.Status.State = pure.PhaseCompleted
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	agent.Spec.ResultSink = srv.URL

	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(run).Build()
	r := &AgentRunReconciler{Client: c, Scheme: sch, ResultSinkClient: srv.Client()}

	r.emitResultOnce(context.Background(), run, agent)
	if run.Annotations[resultEmittedAnnotation] == "true" {
		t.Error("a failed emit must NOT stamp the annotation (so the next reconcile retries)")
	}
}
