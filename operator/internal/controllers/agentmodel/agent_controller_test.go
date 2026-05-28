package agentmodel

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// newAgentReconcilerForTest builds an in-memory reconciler with the given
// starting objects. The Agent has a status subresource so r.Status().Update
// must be declared on the fake client.
func newAgentReconcilerForTest(t *testing.T, initial ...client.Object) *AgentReconciler {
	t.Helper()
	sch := runtime.NewScheme()
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("amv1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(initial...).
		WithStatusSubresource(&amv1.Agent{}).
		Build()
	return &AgentReconciler{Client: c, Scheme: sch}
}

func harnessAgent(name, ns string) *amv1.Agent {
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	a.Spec.Mode = pure.ModeHarness
	a.Spec.Instructions = "be terse"
	a.Spec.Budget = pure.Budget{MaxSteps: 1, MaxTokens: 1024, MaxWallClockSeconds: 60, MaxToolCalls: 0}
	a.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessHermes, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}
	return a
}

func TestToPure_RoundTrip(t *testing.T) {
	a := &amv1.Agent{}
	a.Spec.Model.ProviderRef = "openai"
	a.Spec.Model.Name = "gpt-4"
	a.Spec.Instructions = "be helpful"
	a.Spec.Budget = pure.Budget{MaxSteps: 10, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 5}

	got := toPure(a)
	if got.Spec.Model.Name != "gpt-4" {
		t.Errorf("model.Name lost: %q", got.Spec.Model.Name)
	}
	if got.Spec.Budget.MaxSteps != 10 {
		t.Errorf("budget.MaxSteps lost: %d", got.Spec.Budget.MaxSteps)
	}

	// Pure validate accepts the same.
	if err := pure.ValidateAgent(got); err != nil {
		t.Errorf("toPure-then-validate rejected valid agent: %v", err)
	}
}

func TestSetStatus_RecordsAllFields(t *testing.T) {
	r := &AgentReconciler{}
	a := &amv1.Agent{}
	a.Generation = 5
	r.setStatus(a, "Ready", "Reconciled", "all good")
	if a.Status.Phase != "Ready" {
		t.Errorf("phase = %q", a.Status.Phase)
	}
	if a.Status.Reason != "Reconciled" {
		t.Errorf("reason = %q", a.Status.Reason)
	}
	if a.Status.ObservedGeneration != 5 {
		t.Errorf("gen = %d", a.Status.ObservedGeneration)
	}
}

func TestAgentDeepCopy_PreservesContents(t *testing.T) {
	a := &amv1.Agent{}
	a.Spec.Model.Name = "claude"
	a.Spec.Instructions = "x"
	a.Spec.Budget = pure.Budget{MaxSteps: 1, MaxTokens: 100, MaxWallClockSeconds: 1, MaxToolCalls: 0}
	a.Spec.Tools = []pure.ToolRef{{Name: "search"}}
	cp := a.DeepCopy()
	if cp.Spec.Model.Name != "claude" {
		t.Errorf("model name lost in deepcopy")
	}
	// Verify list independence.
	cp.Spec.Tools[0].Name = "mutated"
	if a.Spec.Tools[0].Name == "mutated" {
		// JSON round-trip would isolate; for shallow copy we accept the
		// shared slice (matches generated DeepCopy when Spec is a value).
		// Verify a fresh deepcopy doesn't propagate the mutation back.
		fresh := a.DeepCopy()
		if fresh.Spec.Tools[0].Name == "mutated" {
			t.Error("deepcopy shared the slice")
		}
	}
}

func TestAgentRun_Marshalable(t *testing.T) {
	r := &amv1.AgentRun{}
	r.Spec.AgentRef = "alice"
	r.Spec.Input = json.RawMessage(`{"q":"hi"}`)
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !contains(string(out), `"agentRef":"alice"`) {
		t.Errorf("marshal lost agentRef: %s", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ── Reconcile-level tests (issues #1 / #2 follow-ups) ────────────────────────

// Harness-mode agent has no Model/ProviderRef; the controller used to stamp it
// Pending/ProviderMissing forever. Should now reach Ready.
func TestReconcile_HarnessAgent_NoProvider_ReachesReady(t *testing.T) {
	a := harnessAgent("alice", "tenant-a")
	r := newAgentReconcilerForTest(t, a)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: a.Name, Namespace: a.Namespace}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &amv1.Agent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: a.Name, Namespace: a.Namespace}, got); err != nil {
		t.Fatalf("get back: %v", err)
	}
	if got.Status.Phase != "Ready" {
		t.Errorf("phase = %q, want Ready (harness agents have no Model to resolve); reason=%q msg=%q",
			got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
	if got.Status.ResolvedProvider != "" {
		t.Errorf("ResolvedProvider = %q, want empty for harness mode", got.Status.ResolvedProvider)
	}
}

// Loop-mode agent referencing a non-existent ModelProvider should stay Pending
// with ProviderMissing — this path must keep working (we only skip provider
// resolution for harness mode, not for loop).
func TestReconcile_LoopAgent_ProviderRefMissing_StaysPending(t *testing.T) {
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "tenant-a"}}
	a.Spec.Mode = pure.ModeLoop
	a.Spec.Model = pure.ModelRef{ProviderRef: "ghost", Name: "gpt-4"}
	a.Spec.Instructions = "x"
	a.Spec.Budget = pure.Budget{MaxSteps: 1, MaxTokens: 100, MaxWallClockSeconds: 10, MaxToolCalls: 0}

	r := newAgentReconcilerForTest(t, a)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: a.Name, Namespace: a.Namespace}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &amv1.Agent{}
	_ = r.Get(context.Background(), types.NamespacedName{Name: a.Name, Namespace: a.Namespace}, got)
	if got.Status.Phase != "Pending" || got.Status.Reason != "ProviderMissing" {
		t.Errorf("loop with absent providerRef should be Pending/ProviderMissing; got phase=%q reason=%q",
			got.Status.Phase, got.Status.Reason)
	}
}

// Reconciling an Agent must create the ServiceAccount that AgentRun pods run
// as (the platform SmolAgent controller used to be the only creator, leaving
// runtime-only flows broken at pod-create time).
func TestReconcile_CreatesServiceAccount_WithOwnerRef(t *testing.T) {
	a := harnessAgent("alice", "tenant-a")
	r := newAgentReconcilerForTest(t, a)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: a.Name, Namespace: a.Namespace}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	sa := &corev1.ServiceAccount{}
	err := r.Get(context.Background(), types.NamespacedName{Name: builders.AgentSAName(a.Name), Namespace: a.Namespace}, sa)
	if err != nil {
		t.Fatalf("SA not created (want %s/%s): %v", a.Namespace, builders.AgentSAName(a.Name), err)
	}

	// Owned by the Agent so it's garbage-collected with it.
	if len(sa.OwnerReferences) == 0 || sa.OwnerReferences[0].Name != a.Name || sa.OwnerReferences[0].Kind != "Agent" {
		t.Errorf("SA missing controller-ref to its Agent; got %+v", sa.OwnerReferences)
	}
}

// Two reconciles must not double-create the SA.
func TestReconcile_ServiceAccount_Idempotent(t *testing.T) {
	a := harnessAgent("alice", "tenant-a")
	r := newAgentReconcilerForTest(t, a)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: a.Name, Namespace: a.Namespace}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	// One and only one SA in the namespace.
	list := &corev1.ServiceAccountList{}
	if err := r.List(context.Background(), list, client.InNamespace(a.Namespace)); err != nil {
		t.Fatalf("list: %v", err)
	}
	hits := 0
	for _, sa := range list.Items {
		if sa.Name == builders.AgentSAName(a.Name) {
			hits++
		}
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 agent SA, got %d", hits)
	}
}

// If the SA is deleted out-of-band, the next reconcile re-creates it.
func TestReconcile_RecreatesSA_WhenDeleted(t *testing.T) {
	a := harnessAgent("alice", "tenant-a")
	r := newAgentReconcilerForTest(t, a)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: a.Name, Namespace: a.Namespace}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: builders.AgentSAName(a.Name), Namespace: a.Namespace}}
	if err := r.Delete(context.Background(), sa); err != nil {
		t.Fatalf("delete SA: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	got := &corev1.ServiceAccount{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: builders.AgentSAName(a.Name), Namespace: a.Namespace}, got); err != nil {
		t.Errorf("SA not recreated after delete: %v", err)
	}
}
