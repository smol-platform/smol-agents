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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func mgwScheme() *runtime.Scheme {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = appsv1.AddToScheme(sch)
	_ = networkingv1.AddToScheme(sch)
	_ = nodev1.AddToScheme(sch)
	_ = amv1.AddToScheme(sch)
	return sch
}

func newGateway() *amv1.ModelGateway {
	return &amv1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes", Namespace: "tenant-a", Generation: 1},
		Spec: pure.ModelGatewaySpec{
			Provider: "hermes",
			Image:    "nousresearch/hermes-agent:latest",
			Config:   "model:\n  provider: zai\n  model: glm-4.6\n",
		},
	}
}

func reconcileGateway(t *testing.T, r *ModelGatewayReconciler, c client.Client) amv1.ModelGateway {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "hermes"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got amv1.ModelGateway
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "hermes"}, &got); err != nil {
		t.Fatalf("get gateway: %v", err)
	}
	return got
}

// With kata-fc registered + a valid spec, the reconciler renders the owned
// objects and surfaces the endpoint (Pending until the Deployment is available).
func TestModelGatewayReconcile_RendersAndExposesEndpoint(t *testing.T) {
	sch := mgwScheme()
	kata := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata-fc"}, Handler: "kata-fc"}
	c := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&amv1.ModelGateway{}).
		WithObjects(newGateway(), kata).Build()
	r := &ModelGatewayReconciler{Client: c, Scheme: sch, DefaultRunRuntimeClass: "kata-fc"}

	got := reconcileGateway(t, r, c)
	if got.Status.Endpoint != "http://mgw-hermes.tenant-a.svc:8642" {
		t.Errorf("endpoint = %q", got.Status.Endpoint)
	}
	if got.Status.Phase != "Pending" || got.Status.Reason != "Deploying" {
		t.Errorf("phase=%s/%s, want Pending/Deploying (no available replicas in fake)", got.Status.Phase, got.Status.Reason)
	}
	// Owned objects exist + the gateway pod is pinned to kata-fc.
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: builders.ModelGatewayName(&got)}, &dep); err != nil {
		t.Fatalf("deployment not rendered: %v", err)
	}
	if dep.Spec.Template.Spec.RuntimeClassName == nil || *dep.Spec.Template.Spec.RuntimeClassName != "kata-fc" {
		t.Errorf("deployment runtimeClass = %v, want kata-fc", dep.Spec.Template.Spec.RuntimeClassName)
	}
	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: builders.ModelGatewayName(&got)}, &svc); err != nil {
		t.Errorf("service not rendered: %v", err)
	}
}

// A bad spec fails closed.
func TestModelGatewayReconcile_InvalidSpec(t *testing.T) {
	sch := mgwScheme()
	gw := newGateway()
	gw.Spec.Image = "" // required
	c := fake.NewClientBuilder().WithScheme(sch).WithStatusSubresource(&amv1.ModelGateway{}).WithObjects(gw).Build()
	r := &ModelGatewayReconciler{Client: c, Scheme: sch, DefaultRunRuntimeClass: "kata-fc"}
	got := reconcileGateway(t, r, c)
	if got.Status.Phase != "Failed" || got.Status.Reason != "InvalidSpec" {
		t.Errorf("phase=%s/%s, want Failed/InvalidSpec", got.Status.Phase, got.Status.Reason)
	}
}

// The RCE gateway must not schedule unisolated: runc without --allow-host-runtime
// fails closed; an unregistered kata RuntimeClass holds it Pending.
func TestModelGatewayReconcile_SandboxFailClosed(t *testing.T) {
	sch := mgwScheme()
	// runc without allow-host-runtime → Failed.
	gw := newGateway()
	gw.Spec.Sandbox.RuntimeClass = "runc"
	c := fake.NewClientBuilder().WithScheme(sch).WithStatusSubresource(&amv1.ModelGateway{}).WithObjects(gw).Build()
	r := &ModelGatewayReconciler{Client: c, Scheme: sch, DefaultRunRuntimeClass: "kata-fc"}
	if got := reconcileGateway(t, r, c); got.Status.Phase != "Failed" || got.Status.Reason != "SandboxRefused" {
		t.Errorf("runc no-allow: phase=%s/%s, want Failed/SandboxRefused", got.Status.Phase, got.Status.Reason)
	}

	// kata-fc not registered → Pending (refuse to run unisolated).
	c2 := fake.NewClientBuilder().WithScheme(sch).WithStatusSubresource(&amv1.ModelGateway{}).WithObjects(newGateway()).Build()
	r2 := &ModelGatewayReconciler{Client: c2, Scheme: sch, DefaultRunRuntimeClass: "kata-fc"}
	if got := reconcileGateway(t, r2, c2); got.Status.Phase != "Pending" || got.Status.Reason != "SandboxPending" {
		t.Errorf("unregistered kata: phase=%s/%s, want Pending/SandboxPending", got.Status.Phase, got.Status.Reason)
	}
}
