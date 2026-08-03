package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func newTestResolver(t *testing.T, grants ...*amv1.AttachGrant) *k8sGrantResolver {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(sch)
	for _, g := range grants {
		builder = builder.WithObjects(g)
	}
	return &k8sGrantResolver{c: builder.Build(), log: slog.New(slog.NewTextHandler(testWriter{t}, nil))}
}

// testWriter routes slog output through t.Log so a failing test shows it.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// knative-agents-md9: a grant that fails pure.ValidateAttachGrant (here, an
// invalid role) must be skipped even though the operator ships with
// ENABLE_WEBHOOKS=false in some deployments and so never validated it at
// admission time. A separate, valid grant for the same agent/subject must
// still resolve.
func TestK8sGrantResolver_SkipsInvalidGrant_ResolvesValid(t *testing.T) {
	future := metav1.NewTime(time.Now().Add(time.Hour))

	invalid := &amv1.AttachGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-role", Namespace: "tenant-a"},
		Spec: pure.AttachGrantSpec{
			AgentRef: "claude", Subject: "alice", Role: "root", ExpiresAt: &future,
		},
	}
	valid := &amv1.AttachGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "good-viewer", Namespace: "tenant-a"},
		Spec: pure.AttachGrantSpec{
			AgentRef: "claude", Subject: "alice", Role: pure.AttachRoleViewer, ExpiresAt: &future,
		},
	}

	r := newTestResolver(t, invalid, valid)
	role, name, ok := r.Resolve(context.Background(), "tenant-a", "claude", "alice", time.Now())
	if !ok {
		t.Fatal("expected the valid grant to resolve")
	}
	if role != pure.AttachRoleViewer || name != "good-viewer" {
		t.Errorf("role=%q name=%q, want viewer/good-viewer (bad-role grant must be skipped)", role, name)
	}
}

// knative-agents-md9: a grant with a cross-namespace agentRef ("other-ns/agent")
// also fails ValidateAttachGrant. The Resolve query deliberately passes the
// same slash-bearing string as the agent parameter so the naive
// AgentRef!=agent equality filter alone would NOT reject it — proving
// ValidateAttachGrant, not that unrelated filter, is what closes this hole.
func TestK8sGrantResolver_SkipsCrossNamespaceAgentRef(t *testing.T) {
	future := metav1.NewTime(time.Now().Add(time.Hour))
	invalid := &amv1.AttachGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-ns", Namespace: "tenant-a"},
		Spec: pure.AttachGrantSpec{
			AgentRef: "other-ns/claude", Subject: "alice", Role: pure.AttachRoleDriver, ExpiresAt: &future,
		},
	}

	r := newTestResolver(t, invalid)
	_, _, ok := r.Resolve(context.Background(), "tenant-a", "other-ns/claude", "alice", time.Now())
	if ok {
		t.Error("cross-namespace agentRef must never resolve, even if it matched the queried agent string")
	}
}

// Only an invalid grant present → no resolution at all.
func TestK8sGrantResolver_AllInvalid_NoResolve(t *testing.T) {
	future := metav1.NewTime(time.Now().Add(time.Hour))
	invalid := &amv1.AttachGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "no-subject", Namespace: "tenant-a"},
		Spec: pure.AttachGrantSpec{
			AgentRef: "claude", Subject: "", Role: pure.AttachRoleViewer, ExpiresAt: &future,
		},
	}
	r := newTestResolver(t, invalid)
	_, _, ok := r.Resolve(context.Background(), "tenant-a", "claude", "", time.Now())
	if ok {
		t.Error("a grant with no subject must never resolve")
	}
}
