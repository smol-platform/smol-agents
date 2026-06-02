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
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestAgentSessionReconcile_LaunchesDurableWorker(t *testing.T) {
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, amv1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, nodev1.AddToScheme,
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

	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(agent, session, kataRC).
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
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "s2-session"}, &appsv1.Deployment{}); err == nil {
		t.Error("no Deployment should be created for a fail-closed session")
	}
}
