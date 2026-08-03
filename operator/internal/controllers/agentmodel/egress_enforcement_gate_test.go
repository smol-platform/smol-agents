package agentmodel

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// knative-agents-7p3: --require-egress-enforcement fail-closed mode.
//
// egressAllowOnlyNet builds an identityProxy AgentNetwork that binds (so it
// populates NetworkPlan.Networks) via a Tier-1 allow-list only — no
// Resources, no eBPF Enforcement — so checkTier2Wired lets it through (see
// TestCheckTier2Wired). Used for the "pod created normally" cases, where the
// run/session must get all the way to a scheduled pod/worker.
func egressAllowOnlyNet(name, ns string, sel map[string]string) *amv1.AgentNetwork {
	an := &amv1.AgentNetwork{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	an.Spec = pure.AgentNetworkSpec{
		Kind:          "identityProxy",
		AgentSelector: sel,
		IdentityProxy: &pure.IdentityProxySpec{
			Egress: pure.EgressPolicy{Allow: []pure.EgressRule{{CIDR: "203.0.113.0/24"}}},
		},
	}
	return an
}

// egressWireguardOnlyNet builds a wireguardMesh AgentNetwork that binds
// (populates NetworkPlan.Networks) but contributes NOTHING to the plan's
// AllowRules — plan.BuildNetworkPlan only inspects IdentityProxy specs
// (wireguard specs "contribute nothing to the egress plan", per its doc
// comment). Used to prove the gate's trigger is genuinely "a bound
// AgentNetwork exists" (netPlan.Networks) and not "the plan has allow
// rules" — the latter would silently miss this network.
func egressWireguardOnlyNet(name, ns string, sel map[string]string) *amv1.AgentNetwork {
	an := &amv1.AgentNetwork{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	an.Spec = pure.AgentNetworkSpec{
		Kind:          "wireguardMesh",
		AgentSelector: sel,
		WireGuardMesh: &pure.WireGuardSpec{Mode: "client", PrivateKeyRef: pure.AuthRef{SecretName: "wg-key"}},
	}
	return an
}

// runcHarnessAgent builds a harness-mode Agent pinned to the runc sandbox
// class, so a full pre-Pod Reconcile() can schedule a pod in these tests
// without registering a kata-fc RuntimeClass/AgentNodePool fixture (mirrors
// TestAgentSessionReconcile_RuncFailsClosed's setup) and without a
// ModelProvider (harness mode skips provider resolution in gatherRunSecrets).
func runcHarnessAgent(name, ns string, labels map[string]string) *amv1.Agent {
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
	a.Spec.Mode = pure.ModeHarness
	a.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessClaudeCode, Image: "img"}
	a.Spec.Sandbox.RuntimeClass = "runc"
	return a
}

// TestReconcile_RequireEgressEnforcement_BoundNetwork_UnenforcedCNI_HoldsPending
// is the gate's core security claim: flag on + a bound AgentNetwork + a CNI
// that cannot enforce NetworkPolicy must hold the run Pending/EgressUnenforced
// and must NOT create a pod — asserting the Pod is actually absent, not just
// that status says the right thing.
func TestReconcile_RequireEgressEnforcement_BoundNetwork_UnenforcedCNI_HoldsPending(t *testing.T) {
	agent := runcHarnessAgent("alice", "tenant-a", map[string]string{"team": "x"})
	an := egressAllowOnlyNet("net1", "tenant-a", map[string]string{"team": "x"})
	run := sampleRun()

	r := newRunReconcilerForTest(t, interceptor.Funcs{}, agent, an, run)
	r.AllowHostRuntime = true
	r.RequireEgressEnforcement = true
	// r.CNIEnforcesNetworkPolicy defaults false (the CNI cannot enforce it).

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getRun(t, r, run.Namespace, run.Name)
	if got.Status.State != pure.PhasePending {
		t.Fatalf("state = %s, want Pending", got.Status.State)
	}
	if got.Status.Reason != "EgressUnenforced" {
		t.Errorf("reason = %q, want EgressUnenforced", got.Status.Reason)
	}
	if runPodExists(r, run.Namespace, run.Name) {
		t.Error("a Pod was created despite the unenforceable bound AgentNetwork — this is the security claim the gate exists for")
	}
}

// TestReconcile_RequireEgressEnforcement_BoundNetwork_EnforcedCNI_CreatesPod:
// with an enforcing CNI declared, the same bound network must NOT hold the
// run — the CNI can actually enforce the cage, so there's nothing to fail
// closed over.
func TestReconcile_RequireEgressEnforcement_BoundNetwork_EnforcedCNI_CreatesPod(t *testing.T) {
	agent := runcHarnessAgent("alice", "tenant-a", map[string]string{"team": "x"})
	an := egressAllowOnlyNet("net1", "tenant-a", map[string]string{"team": "x"})
	run := sampleRun()

	r := newRunReconcilerForTest(t, interceptor.Funcs{}, agent, an, run)
	r.AllowHostRuntime = true
	r.RequireEgressEnforcement = true
	r.CNIEnforcesNetworkPolicy = true

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getRun(t, r, run.Namespace, run.Name)
	if got.Status.Reason == "EgressUnenforced" {
		t.Errorf("reason = %q, must not gate when the CNI enforces NetworkPolicy", got.Status.Reason)
	}
	if !runPodExists(r, run.Namespace, run.Name) {
		t.Error("no Pod created despite an enforcing CNI — the gate must not block an actually-enforceable run")
	}
}

// TestReconcile_RequireEgressEnforcement_NoBoundNetwork_CreatesPod proves the
// flag does NOT block the bare default-deny floor: with no bound
// AgentNetwork at all, a run must schedule normally on a non-enforcing CNI —
// gating every run on every non-enforcing cluster would make the flag
// unusable, and that's not what knative-agents-7p3 asks for.
func TestReconcile_RequireEgressEnforcement_NoBoundNetwork_CreatesPod(t *testing.T) {
	agent := runcHarnessAgent("alice", "tenant-a", nil)
	run := sampleRun()

	r := newRunReconcilerForTest(t, interceptor.Funcs{}, agent, run)
	r.AllowHostRuntime = true
	r.RequireEgressEnforcement = true
	// No AgentNetwork bound; CNIEnforcesNetworkPolicy defaults false.

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getRun(t, r, run.Namespace, run.Name)
	if got.Status.Reason == "EgressUnenforced" {
		t.Errorf("reason = %q, the bare floor (no bound AgentNetwork) must never be gated", got.Status.Reason)
	}
	if !runPodExists(r, run.Namespace, run.Name) {
		t.Error("no Pod created for a bare-floor run — the flag must not block every run on a non-enforcing cluster")
	}
}

// TestReconcile_RequireEgressEnforcementOff_BoundNetwork_UnenforcedCNI_CreatesPod
// proves strict backward compatibility: with the flag at its default (off),
// today's behavior is unchanged — a bound network on a non-enforcing CNI
// still only reports "unenforced" on status, it never blocks the pod.
func TestReconcile_RequireEgressEnforcementOff_BoundNetwork_UnenforcedCNI_CreatesPod(t *testing.T) {
	agent := runcHarnessAgent("alice", "tenant-a", map[string]string{"team": "x"})
	an := egressAllowOnlyNet("net1", "tenant-a", map[string]string{"team": "x"})
	run := sampleRun()

	r := newRunReconcilerForTest(t, interceptor.Funcs{}, agent, an, run)
	r.AllowHostRuntime = true
	// r.RequireEgressEnforcement defaults false; r.CNIEnforcesNetworkPolicy defaults false.

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getRun(t, r, run.Namespace, run.Name)
	if got.Status.Reason == "EgressUnenforced" {
		t.Errorf("reason = %q, the flag defaults to off — must not gate", got.Status.Reason)
	}
	if got.Status.EgressEnforcement != "unenforced" {
		t.Errorf("EgressEnforcement = %q, want unenforced (rv1.2 status-only reporting, unchanged)", got.Status.EgressEnforcement)
	}
	if !runPodExists(r, run.Namespace, run.Name) {
		t.Error("no Pod created with the flag off — this must be byte-for-byte the pre-7p3 behavior")
	}
}

// TestReconcile_RequireEgressEnforcement_WireguardOnlyNetwork_HoldsPending
// proves the chosen predicate: the gate reuses netPlan.Networks ("a bound
// AgentNetwork exists"), not AllowRules presence. A wireguardMesh-only bound
// network contributes zero AllowRules to the plan but is still a real
// binding the run's author intended to restrict egress with, so it must
// still gate — an AllowRules-based predicate would silently miss it.
func TestReconcile_RequireEgressEnforcement_WireguardOnlyNetwork_HoldsPending(t *testing.T) {
	agent := runcHarnessAgent("alice", "tenant-a", map[string]string{"team": "x"})
	an := egressWireguardOnlyNet("net1", "tenant-a", map[string]string{"team": "x"})
	run := sampleRun()

	r := newRunReconcilerForTest(t, interceptor.Funcs{}, agent, an, run)
	r.AllowHostRuntime = true
	r.RequireEgressEnforcement = true

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getRun(t, r, run.Namespace, run.Name)
	if got.Status.Reason != "EgressUnenforced" {
		t.Errorf("reason = %q, want EgressUnenforced (a wireguard-only bound network still counts as bound)", got.Status.Reason)
	}
	if runPodExists(r, run.Namespace, run.Name) {
		t.Error("a Pod was created for a wireguard-only bound network on a non-enforcing CNI")
	}
}

// TestReconcile_ExistingPod_RequireEgressEnforcement_StaysRunningSurfacesReason
// covers the already-running-pod interaction with knative-agents-1c5: the
// flag is admission-time only, so a live run whose bound AgentNetwork newly
// becomes unenforceable must NOT be retroactively stalled or killed — it
// stays Running, with the gap surfaced observability-only on Status.Reason
// (mirroring the ErrNetworkConflict carve-out), and its Pod is left alone.
func TestReconcile_ExistingPod_RequireEgressEnforcement_StaysRunningSurfacesReason(t *testing.T) {
	agent := runcHarnessAgent("alice", "tenant-a", map[string]string{"team": "x"})
	an := egressAllowOnlyNet("net1", "tenant-a", map[string]string{"team": "x"})
	run := sampleRun()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: run.Name, Namespace: run.Namespace},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r := newRunReconcilerForTest(t, interceptor.Funcs{}, agent, an, run, pod)
	r.RequireEgressEnforcement = true
	// r.CNIEnforcesNetworkPolicy defaults false.

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getRun(t, r, run.Namespace, run.Name)
	if got.Status.State != pure.PhaseRunning {
		t.Errorf("state = %s, want Running (the gate must never retroactively stall a live run)", got.Status.State)
	}
	if got.Status.Reason != "EgressUnenforced" {
		t.Errorf("reason = %q, want EgressUnenforced surfaced observability-only", got.Status.Reason)
	}
	if !runPodExists(r, run.Namespace, run.Name) {
		t.Error("the live run's Pod must not be deleted")
	}
}

// sessionSchemeWithRunc mirrors TestAgentSessionReconcile_RuncFailsClosed's
// scheme: no kata-fc RuntimeClass/AgentNodePool fixture is needed since these
// tests pin runc + AllowHostRuntime.
func sessionSchemeWithRunc(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, amv1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, nodev1.AddToScheme,
	} {
		if err := add(sch); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return sch
}

// TestAgentSessionReconcile_RequireEgressEnforcement_BoundNetwork_UnenforcedCNI_HoldsPending
// is the session-side equivalent of the run gate's core security claim: flag
// on + a bound AgentNetwork + a non-enforcing CNI must hold the session
// Pending/EgressUnenforced and must NOT create the worker Deployment.
func TestAgentSessionReconcile_RequireEgressEnforcement_BoundNetwork_UnenforcedCNI_HoldsPending(t *testing.T) {
	sch := sessionSchemeWithRunc(t)

	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t", Labels: map[string]string{"team": "x"}}}
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessGenericHTTP, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}
	agent.Spec.Sandbox.RuntimeClass = "runc"
	an := egressAllowOnlyNet("net1", "t", map[string]string{"team": "x"})
	session := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "t", Generation: 1}}
	session.Spec.AgentRef = "a"

	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(agent, an, session).
		WithStatusSubresource(&amv1.AgentSession{}).Build()
	r := &AgentSessionReconciler{
		Client: c, Scheme: sch,
		AllowHostRuntime:         true,
		RequireEgressEnforcement: true,
		// CNIEnforcesNetworkPolicy defaults false.
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "s1"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got amv1.AgentSession
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != pure.PhasePending {
		t.Errorf("phase = %s, want Pending", got.Status.Phase)
	}
	if got.Status.Reason != "EgressUnenforced" {
		t.Errorf("reason = %q, want EgressUnenforced", got.Status.Reason)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s1-session"}, &appsv1.Deployment{}); err == nil {
		t.Error("a worker Deployment was created despite the unenforceable bound AgentNetwork — the security claim of this gate")
	}
}

// TestAgentSessionReconcile_RequireEgressEnforcement_NoBoundNetwork_CreatesWorker
// proves the session-side bare floor is not blocked either.
func TestAgentSessionReconcile_RequireEgressEnforcement_NoBoundNetwork_CreatesWorker(t *testing.T) {
	sch := sessionSchemeWithRunc(t)

	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessGenericHTTP, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}
	agent.Spec.Sandbox.RuntimeClass = "runc"
	session := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "t", Generation: 1}}
	session.Spec.AgentRef = "a"

	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(agent, session).
		WithStatusSubresource(&amv1.AgentSession{}).Build()
	r := &AgentSessionReconciler{
		Client: c, Scheme: sch,
		AllowHostRuntime:         true,
		RequireEgressEnforcement: true,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "s1"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got amv1.AgentSession
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Reason == "EgressUnenforced" {
		t.Errorf("reason = %q, the bare floor (no bound AgentNetwork) must never be gated", got.Status.Reason)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s1-session"}, &appsv1.Deployment{}); err != nil {
		t.Error("no worker Deployment created for a bare-floor session")
	}
}
