package agentmodel

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func bindScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return sch
}

// proxyNet builds an identityProxy AgentNetwork binding one http resource on
// localPort→gateway; two with the same port but different gateways conflict.
func proxyNet(name, ns string, sel map[string]string, port int32, gw string) *amv1.AgentNetwork {
	an := &amv1.AgentNetwork{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	an.Spec = pure.AgentNetworkSpec{
		Kind:          "identityProxy",
		AgentSelector: sel,
		IdentityProxy: &pure.IdentityProxySpec{
			Resources: []pure.ResourceTarget{{Name: name, Kind: "http", LocalPort: port, Gateway: gw}},
		},
	}
	return an
}

func TestResolveBoundNetworks_ConflictWrapsSentinel(t *testing.T) {
	sch := bindScheme(t)
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "t", Labels: map[string]string{"team": "x"}}}
	n1 := proxyNet("n1", "t", map[string]string{"team": "x"}, 8080, "https://a")
	n2 := proxyNet("n2", "t", map[string]string{"team": "x"}, 8080, "https://b") // same port, different gateway
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(agent, n1, n2).Build()

	_, err := resolveBoundNetworks(context.Background(), c, agent)
	if err == nil {
		t.Fatal("expected a conflict error from two networks clashing on localPort")
	}
	if !errors.Is(err, ErrNetworkConflict) {
		t.Errorf("a compose conflict must wrap ErrNetworkConflict (so the caller holds Pending), got %v", err)
	}
}

func TestResolveBoundNetworks_NoConflict(t *testing.T) {
	sch := bindScheme(t)
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "t", Labels: map[string]string{"team": "x"}}}
	n1 := proxyNet("n1", "t", map[string]string{"team": "x"}, 8080, "https://a")
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(agent, n1).Build()

	p, err := resolveBoundNetworks(context.Background(), c, agent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Networks) != 1 || p.Networks[0] != "n1" {
		t.Errorf("bound networks = %v, want [n1]", p.Networks)
	}
}

func TestAgentsBoundToNetwork(t *testing.T) {
	sch := bindScheme(t)
	match := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "t", Labels: map[string]string{"team": "x"}}}
	noMatch := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "nm", Namespace: "t", Labels: map[string]string{"team": "y"}}}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(match, noMatch).Build()

	an := proxyNet("n", "t", map[string]string{"team": "x"}, 8080, "https://a")
	got := agentsBoundToNetwork(context.Background(), c, an)
	if !got["m"] || got["nm"] {
		t.Errorf("bound set = %v, want only m", got)
	}
	an.Spec.AgentSelector = nil
	if got := agentsBoundToNetwork(context.Background(), c, an); len(got) != 0 {
		t.Errorf("empty selector must bind nothing, got %v", got)
	}
}

func TestRunsForNetwork_EnqueuesBoundNonTerminal(t *testing.T) {
	sch := bindScheme(t)
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "t", Labels: map[string]string{"team": "x"}}}
	an := proxyNet("n", "t", map[string]string{"team": "x"}, 8080, "https://a")

	running := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r-run", Namespace: "t"}}
	running.Spec.AgentRef = "a1"
	running.Status.State = pure.PhaseRunning // non-terminal → enqueued
	done := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r-done", Namespace: "t"}}
	done.Spec.AgentRef = "a1"
	done.Status.State = pure.PhaseCompleted // terminal → skipped (do not re-cage)
	other := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r-other", Namespace: "t"}}
	other.Spec.AgentRef = "a2" // unbound agent → skipped

	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(agent, an, running, done, other).Build()
	r := &AgentRunReconciler{Client: c, Scheme: sch}
	reqs := r.runsForNetwork(context.Background(), an)
	if len(reqs) != 1 || reqs[0].Name != "r-run" {
		t.Errorf("runsForNetwork = %v, want only [r-run]", reqs)
	}
}

func TestSessionsForNetwork_EnqueuesBound(t *testing.T) {
	sch := bindScheme(t)
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "t", Labels: map[string]string{"team": "x"}}}
	an := proxyNet("n", "t", map[string]string{"team": "x"}, 8080, "https://a")
	bound := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s-bound", Namespace: "t"}}
	bound.Spec.AgentRef = "a1"
	unbound := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s-unbound", Namespace: "t"}}
	unbound.Spec.AgentRef = "a2"

	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(agent, an, bound, unbound).Build()
	r := &AgentSessionReconciler{Client: c, Scheme: sch}
	reqs := r.sessionsForNetwork(context.Background(), an)
	if len(reqs) != 1 || reqs[0].Name != "s-bound" {
		t.Errorf("sessionsForNetwork = %v, want only [s-bound]", reqs)
	}
}
