package agentmodel

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentnet/plan"
)

func sandboxReconciler(t *testing.T, mut func(*AgentRunReconciler), initial ...client.Object) *AgentRunReconciler {
	t.Helper()
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, amv1.AddToScheme, nodev1.AddToScheme, networkingv1.AddToScheme,
	} {
		if err := add(sch); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(initial...).Build()
	r := &AgentRunReconciler{Client: c, Scheme: sch}
	if mut != nil {
		mut(r)
	}
	return r
}

func rcObj(name string) *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Handler: name}
}

func agentWithSandbox(rc string) *amv1.Agent {
	a := &amv1.Agent{}
	a.Spec.Sandbox.RuntimeClass = rc
	return a
}

// TestResolveRunSandbox covers the fail-closed run-pod isolation resolution:
// default kata-fc, the R-SBX-1 runc guard, and the refuse-to-run-unisolated hold
// when a hardened RuntimeClass isn't registered.
func TestResolveRunSandbox(t *testing.T) {
	cases := []struct {
		name       string
		registered []client.Object
		mut        func(*AgentRunReconciler)
		agentRC    string
		wantClass  string
		pendingHas string // "" => pending must be empty
		wantFailed string
	}{
		{"default-kata-not-registered → Pending", nil, nil, "", "", "not registered", ""},
		{"default-kata-registered → kata-fc", []client.Object{rcObj("kata-fc")}, nil, "", "kata-fc", "", ""},
		{"runc-without-optin → Failed", nil, nil, "runc", "", "", "runc-requires-allow-host-runtime"},
		{"runc-with-optin → runc", nil, func(r *AgentRunReconciler) { r.AllowHostRuntime = true }, "runc", "runc", "", ""},
		{"default-override-gvisor-registered → gvisor", []client.Object{rcObj("gvisor")}, func(r *AgentRunReconciler) { r.DefaultRunRuntimeClass = "gvisor" }, "", "gvisor", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sandboxReconciler(t, tc.mut, tc.registered...)
			class, pending, failed := r.resolveRunSandbox(context.Background(), agentWithSandbox(tc.agentRC))
			if class != tc.wantClass {
				t.Errorf("class = %q, want %q", class, tc.wantClass)
			}
			if failed != tc.wantFailed {
				t.Errorf("failed = %q, want %q", failed, tc.wantFailed)
			}
			if tc.pendingHas == "" && pending != "" {
				t.Errorf("unexpected pending = %q", pending)
			}
			if tc.pendingHas != "" && !strings.Contains(pending, tc.pendingHas) {
				t.Errorf("pending = %q, want contains %q", pending, tc.pendingHas)
			}
		})
	}
}

func TestEnsureRunEgressPolicy(t *testing.T) {
	run := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "tenant-a", UID: "uid-1"}}
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "tenant-a"}}
	r := sandboxReconciler(t, nil, run)

	if err := r.ensureRunEgressPolicy(context.Background(), run, agent); err != nil {
		t.Fatalf("ensureRunEgressPolicy: %v", err)
	}
	var np networkingv1.NetworkPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "r1-egress"}, &np); err != nil {
		t.Fatalf("egress NetworkPolicy not created: %v", err)
	}
	if len(np.OwnerReferences) == 0 || np.OwnerReferences[0].Name != "r1" {
		t.Errorf("egress NP not owned by the run: %+v", np.OwnerReferences)
	}
	// M1.19: with no bound AgentNetwork the run reports the default-deny floor.
	if run.Status.EgressEnforcement != "default-deny" || len(run.Status.Networks) != 0 {
		t.Errorf("status egress = %q networks=%v, want default-deny / none", run.Status.EgressEnforcement, run.Status.Networks)
	}
	// Idempotent.
	if err := r.ensureRunEgressPolicy(context.Background(), run, agent); err != nil {
		t.Fatalf("second ensureRunEgressPolicy: %v", err)
	}
}

// M1.19: the egress-posture label is "tiered" only when a bound AgentNetwork
// layers an allow-list on the floor.
func TestEgressEnforcementLabel(t *testing.T) {
	if got := egressEnforcementLabel(plan.NetworkPlan{}); got != "default-deny" {
		t.Errorf("empty plan = %q, want default-deny", got)
	}
	withAllow := plan.NetworkPlan{AllowRules: []pure.EgressRule{{CIDR: "203.0.113.0/24"}}}
	if got := egressEnforcementLabel(withAllow); got != "tiered" {
		t.Errorf("plan with allow rules = %q, want tiered", got)
	}
}
