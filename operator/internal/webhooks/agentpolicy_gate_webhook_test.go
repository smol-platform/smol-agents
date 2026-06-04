package webhooks

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func gateWith(t *testing.T, objs ...client.Object) *agentPolicyGate {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	return &agentPolicyGate{client: c}
}

func TestAgentPolicyGate_AgentProvider(t *testing.T) {
	policy := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "t"},
		Spec:       pure.AgentPolicySpec{AllowedProviders: []string{"openai"}},
	}
	g := gateWith(t, policy)

	bad := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	bad.Spec.Model = pure.ModelRef{ProviderRef: "anthropic", Name: "m"}
	if err := g.checkAgent(context.Background(), bad); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("disallowed provider must be Invalid, got %v", err)
	}
	good := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	good.Spec.Model = pure.ModelRef{ProviderRef: "openai", Name: "m"}
	if err := g.checkAgent(context.Background(), good); err != nil {
		t.Fatalf("conforming provider must pass: %v", err)
	}
}

func TestAgentPolicyGate_FailOpenNoPolicies(t *testing.T) {
	g := gateWith(t) // no policies → fail open
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	a.Spec.Model = pure.ModelRef{ProviderRef: "anything", Name: "m"}
	if err := g.checkAgent(context.Background(), a); err != nil {
		t.Fatalf("no policies must fail open, got %v", err)
	}
}

func TestAgentPolicyGate_RunBudgetOverride(t *testing.T) {
	policy := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "t"},
		Spec:       pure.AgentPolicySpec{MaxBudget: &pure.Budget{MaxTokens: 1000, MaxSteps: 10, MaxWallClockSeconds: 60, MaxToolCalls: 5}},
	}
	g := gateWith(t, policy)

	over := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "t"}}
	over.Spec.AgentRef = "a"
	over.Spec.BudgetOverride = &pure.Budget{MaxTokens: 2000, MaxSteps: 10, MaxWallClockSeconds: 60, MaxToolCalls: 5}
	if err := g.checkRun(context.Background(), over); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("over-cap budgetOverride must be Invalid, got %v", err)
	}
	within := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "t"}}
	within.Spec.AgentRef = "a"
	within.Spec.BudgetOverride = &pure.Budget{MaxTokens: 500, MaxSteps: 10, MaxWallClockSeconds: 60, MaxToolCalls: 5}
	if err := g.checkRun(context.Background(), within); err != nil {
		t.Fatalf("within-cap override must pass: %v", err)
	}
}
