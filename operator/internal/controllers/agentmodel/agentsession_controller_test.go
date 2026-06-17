package agentmodel

import (
	"context"
	"strings"
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

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	opv1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestAgentSessionReconcile_LaunchesDurableWorker(t *testing.T) {
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, amv1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, nodev1.AddToScheme, opv1.AddToScheme,
	} {
		if err := add(sch); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}

	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sess-a", Namespace: "t"}}
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessGenericHTTP, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}
	agent.Spec.Storage = &pure.StorageSpec{Kind: pure.StorageAgentFS, AgentFS: &pure.AgentFSSpec{SizeGiB: 1, MountPath: "/var/agentfs"}}

	session := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "t", Generation: 1}}
	session.Spec.AgentRef = "sess-a"

	kataRC := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata-fc"}, Handler: "kata-fc"}
	// A kata AgentNodePool so the worker placement resolves (M1.11) instead of
	// holding the session Pending/NoKVMCapacity.
	kataPool := &opv1.AgentNodePool{ObjectMeta: metav1.ObjectMeta{Name: "kata-pool"}, Spec: opv1.AgentNodePoolSpec{Isolation: "kata-fc"}}

	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(agent, session, kataRC, kataPool).
		WithStatusSubresource(&amv1.AgentSession{}).
		Build()
	r := &AgentSessionReconciler{Client: c, Scheme: sch}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "s1"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// A 1-replica session Deployment running serve-session under kata-fc, owned by the session.
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s1-session"}, &dep); err != nil {
		t.Fatalf("session Deployment not created: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", dep.Spec.Replicas)
	}
	if len(dep.OwnerReferences) == 0 || dep.OwnerReferences[0].Name != "s1" {
		t.Errorf("Deployment not owned by the session: %+v", dep.OwnerReferences)
	}
	mainC := dep.Spec.Template.Spec.Containers[0]
	if got := strings.Join(mainC.Command, " "); !strings.Contains(got, "serve-session") || !strings.Contains(got, "--agent-ref=sess-a") {
		t.Errorf("worker command = %q, want serve-session --agent-ref=sess-a", got)
	}
	if dep.Spec.Template.Spec.RuntimeClassName == nil || *dep.Spec.Template.Spec.RuntimeClassName != "kata-fc" {
		t.Errorf("session pod not pinned to kata-fc: %v", dep.Spec.Template.Spec.RuntimeClassName)
	}
	if dep.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("Deployment pod RestartPolicy = %v, want Always", dep.Spec.Template.Spec.RestartPolicy)
	}

	// Run-spec ConfigMap (worker reads agent.json) + egress cage, present.
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: builders.RunSpecConfigMapName("s1-session")}, &cm); err != nil {
		t.Errorf("run-spec ConfigMap not created: %v", err)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s1-session-egress"}, &np); err != nil {
		t.Errorf("egress NetworkPolicy not created: %v", err)
	}

	// Status reflects Pending (Deployment has no available replicas in the fake).
	var got amv1.AgentSession
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != pure.PhasePending || got.Status.ObservedGeneration != 1 {
		t.Errorf("status = %+v, want Pending/observedGen=1", got.Status)
	}
}

// n20: changing the bound ModelProvider (here: adding chatPath) must re-render
// the live session worker's provider.json in place AND roll the worker (the
// pod-template runspec-hash changes) so it re-reads the new config — create-only
// previously pinned the stale ConfigMap for the session's lifetime.
func TestAgentSessionReconcile_ModelProviderChangeRerendersAndRolls(t *testing.T) {
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, amv1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, nodev1.AddToScheme, opv1.AddToScheme,
	} {
		if err := add(sch); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}

	provider := &amv1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "zai", Namespace: "t"}}
	provider.Spec.Kind = "openai"
	provider.Spec.Endpoint = "https://api.z.ai" // chatPath initially unset

	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "loop-a", Namespace: "t"}}
	agent.Spec.Mode = pure.ModeLoop
	agent.Spec.Model = pure.ModelRef{ProviderRef: "zai", Name: "glm-4.6"}
	agent.Spec.Instructions = "do things"
	agent.Spec.Budget = pure.Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 60}
	agent.Spec.Storage = &pure.StorageSpec{Kind: pure.StorageAgentFS, AgentFS: &pure.AgentFSSpec{SizeGiB: 1, MountPath: "/var/agentfs"}}

	session := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "t", Generation: 1}}
	session.Spec.AgentRef = "loop-a"

	kataRC := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata-fc"}, Handler: "kata-fc"}
	kataPool := &opv1.AgentNodePool{ObjectMeta: metav1.ObjectMeta{Name: "kata-pool"}, Spec: opv1.AgentNodePoolSpec{Isolation: "kata-fc"}}

	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(provider, agent, session, kataRC, kataPool).
		WithStatusSubresource(&amv1.AgentSession{}).
		Build()
	r := &AgentSessionReconciler{Client: c, Scheme: sch}

	rec := func() {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "s1"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	cmName := builders.RunSpecConfigMapName("s1-session")
	getProviderJSON := func() string {
		t.Helper()
		var cm corev1.ConfigMap
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: cmName}, &cm); err != nil {
			t.Fatalf("run-spec ConfigMap: %v", err)
		}
		return cm.Data["provider.json"]
	}
	getHash := func() string {
		t.Helper()
		var dep appsv1.Deployment
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s1-session"}, &dep); err != nil {
			t.Fatalf("session Deployment: %v", err)
		}
		return dep.Spec.Template.Annotations[runSpecHashAnnotation]
	}

	rec()
	before := getProviderJSON()
	hash1 := getHash()
	if strings.Contains(before, "chatPath") {
		t.Fatalf("provider.json unexpectedly has chatPath before the change: %s", before)
	}
	if hash1 == "" {
		t.Fatal("worker pod template missing the runspec-hash annotation")
	}

	// Operator-side change: the bound ModelProvider gains a chatPath.
	var mp amv1.ModelProvider
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "zai"}, &mp); err != nil {
		t.Fatal(err)
	}
	mp.Spec.ChatPath = "/api/coding/paas/v4/chat/completions"
	if err := c.Update(context.Background(), &mp); err != nil {
		t.Fatal(err)
	}

	rec()
	after := getProviderJSON()
	hash2 := getHash()
	if !strings.Contains(after, "/api/coding/paas/v4/chat/completions") {
		t.Errorf("provider.json was NOT re-rendered in place with the new chatPath: %s", after)
	}
	if hash1 == hash2 {
		t.Errorf("runspec-hash did not change, so the worker would not roll: %s", hash2)
	}

	// Idempotent: a no-change reconcile keeps the same hash (no rollout churn).
	rec()
	if getHash() != hash2 {
		t.Errorf("runspec-hash changed on a no-op reconcile (would churn the worker)")
	}
}

// M2.18: when the AgentSession opts into turn scaling, the controller renders
// the worker's --max-concurrent-turns / --history-limit flags from the spec
// accessors; an unset session (above) renders neither (serial default).
func TestAgentSessionReconcile_RendersTurnScaling(t *testing.T) {
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, amv1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, nodev1.AddToScheme, opv1.AddToScheme,
	} {
		if err := add(sch); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}

	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sess-a", Namespace: "t"}}
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessGenericHTTP, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}
	agent.Spec.Storage = &pure.StorageSpec{Kind: pure.StorageAgentFS, AgentFS: &pure.AgentFSSpec{SizeGiB: 1, MountPath: "/var/agentfs"}}

	session := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "t", Generation: 1}}
	session.Spec.AgentRef = "sess-a"
	session.Spec.MaxConcurrentTurns = 4
	session.Spec.TurnHistoryLimit = 50

	kataRC := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata-fc"}, Handler: "kata-fc"}
	kataPool := &opv1.AgentNodePool{ObjectMeta: metav1.ObjectMeta{Name: "kata-pool"}, Spec: opv1.AgentNodePoolSpec{Isolation: "kata-fc"}}

	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(agent, session, kataRC, kataPool).
		WithStatusSubresource(&amv1.AgentSession{}).
		Build()
	r := &AgentSessionReconciler{Client: c, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "s1"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s1-session"}, &dep); err != nil {
		t.Fatalf("session Deployment not created: %v", err)
	}
	got := strings.Join(dep.Spec.Template.Spec.Containers[0].Command, " ")
	if !strings.Contains(got, "--max-concurrent-turns=4") {
		t.Errorf("command = %q, want --max-concurrent-turns=4", got)
	}
	if !strings.Contains(got, "--history-limit=50") {
		t.Errorf("command = %q, want --history-limit=50", got)
	}
}

// runc without --allow-host-runtime is a fail-closed policy violation for a session too.
func TestAgentSessionReconcile_RuncFailsClosed(t *testing.T) {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = amv1.AddToScheme(sch)
	_ = appsv1.AddToScheme(sch)
	_ = networkingv1.AddToScheme(sch)
	_ = nodev1.AddToScheme(sch)

	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessGenericHTTP, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}
	agent.Spec.Sandbox.RuntimeClass = "runc"
	session := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s2", Namespace: "t", Generation: 1}}
	session.Spec.AgentRef = "a"

	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(agent, session).
		WithStatusSubresource(&amv1.AgentSession{}).Build()
	r := &AgentSessionReconciler{Client: c, Scheme: sch} // AllowHostRuntime=false

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "s2"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got amv1.AgentSession
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s2"}, &got)
	if got.Status.Phase != pure.PhaseFailed {
		t.Errorf("runc session phase = %s, want Failed (R-SBX-1)", got.Status.Phase)
	}
	if got.Status.Reason != "SandboxFailed" {
		t.Errorf("runc session reason = %q, want SandboxFailed (a Failed session must surface why)", got.Status.Reason)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s2-session"}, &appsv1.Deployment{}); err == nil {
		t.Error("no Deployment should be created for a fail-closed session")
	}
}

// M3.15: danger permission flags (claude approvalMode=never) must fail closed on
// a shared-kernel class EVEN with --allow-host-runtime — runc is permitted as a
// class there, so the danger-flag microVM gate is the only thing that can (and
// must) fail it. The same posture on a kata microVM would be allowed.
func TestAgentSessionReconcile_DangerFlagsFailClosed(t *testing.T) {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = amv1.AddToScheme(sch)
	_ = appsv1.AddToScheme(sch)
	_ = networkingv1.AddToScheme(sch)
	_ = nodev1.AddToScheme(sch)

	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessClaudeCode, CLI: &pure.HarnessCLISpec{ApprovalMode: "never"}}
	agent.Spec.Sandbox.RuntimeClass = "runc"
	session := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s3", Namespace: "t", Generation: 1}}
	session.Spec.AgentRef = "a"

	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(agent, session).
		WithStatusSubresource(&amv1.AgentSession{}).Build()
	r := &AgentSessionReconciler{Client: c, Scheme: sch, AllowHostRuntime: true} // runc permitted as a class

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "s3"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got amv1.AgentSession
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s3"}, &got)
	if got.Status.Phase != pure.PhaseFailed {
		t.Errorf("danger flags on runc must fail closed (D3), phase = %s", got.Status.Phase)
	}
	if got.Status.Reason != "DangerFlagsRefused" {
		t.Errorf("danger session reason = %q, want DangerFlagsRefused", got.Status.Reason)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s3-session"}, &appsv1.Deployment{}); err == nil {
		t.Error("no Deployment should be created when danger flags are refused")
	}
}

// M1.11: a kata session whose RuntimeClass exists but has NO matching
// AgentNodePool is held Pending (fail-closed) — no worker Deployment.
func TestAgentSessionReconcile_NoKataPoolPending(t *testing.T) {
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, amv1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, nodev1.AddToScheme, opv1.AddToScheme,
	} {
		_ = add(sch)
	}
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "ka", Namespace: "t"}}
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessGenericHTTP, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}
	agent.Spec.Storage = &pure.StorageSpec{Kind: pure.StorageAgentFS, AgentFS: &pure.AgentFSSpec{SizeGiB: 1, MountPath: "/var/agentfs"}}
	session := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s3", Namespace: "t", Generation: 1}}
	session.Spec.AgentRef = "ka"
	kataRC := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata-fc"}, Handler: "kata-fc"}

	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(agent, session, kataRC). // RuntimeClass present, but NO AgentNodePool
		WithStatusSubresource(&amv1.AgentSession{}).Build()
	r := &AgentSessionReconciler{Client: c, Scheme: sch}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "s3"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got amv1.AgentSession
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s3"}, &got)
	if got.Status.Phase != pure.PhasePending {
		t.Errorf("kata session w/o pool → phase %s, want Pending", got.Status.Phase)
	}
	if got.Status.Reason != "NoKVMCapacity" {
		t.Errorf("kata-no-pool session reason = %q, want NoKVMCapacity", got.Status.Reason)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s3-session"}, &appsv1.Deployment{}); err == nil {
		t.Error("no Deployment should be created when placement can't resolve")
	}
}
