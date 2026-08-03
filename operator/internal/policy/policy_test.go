package policy

import (
	"context"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func fakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

// knative-agents-7dm: a policy-less namespace falls back to the platform
// baseline instead of being fully unconstrained.
func TestEffective_BaselineAppliedWhenNamespaceEmpty(t *testing.T) {
	baselinePolicy := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "floor", Namespace: "platform"},
		Spec:       pure.AgentPolicySpec{AllowedProviders: []string{"openai"}},
	}
	c := fakeClient(t, baselinePolicy)

	eff, err := Effective(context.Background(), c, "tenant-a", types.NamespacedName{Namespace: "platform", Name: "floor"})
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if eff.Empty {
		t.Fatal("want the baseline applied (not Empty) for a policy-less namespace")
	}
	if !eff.AllowsProvider("openai") || eff.AllowsProvider("anthropic") {
		t.Fatalf("want the baseline's allow-list in effect, got Providers=%v", eff.Providers)
	}
}

// A namespace that already constrains itself on any axis owns its policy
// outright — the baseline must NOT be blended in (ComposePolicies unions
// allow-lists, so blending would widen a namespace's self-chosen policy).
func TestEffective_BaselineNotAppliedWhenNamespaceConstrains(t *testing.T) {
	nsPolicy := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "own", Namespace: "tenant-a"},
		Spec:       pure.AgentPolicySpec{AllowedProviders: []string{"anthropic"}},
	}
	baselinePolicy := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "floor", Namespace: "platform"},
		Spec:       pure.AgentPolicySpec{AllowedProviders: []string{"openai"}},
	}
	c := fakeClient(t, nsPolicy, baselinePolicy)

	eff, err := Effective(context.Background(), c, "tenant-a", types.NamespacedName{Namespace: "platform", Name: "floor"})
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if eff.Empty {
		t.Fatal("want the namespace's own policy in effect (not Empty)")
	}
	if !eff.AllowsProvider("anthropic") {
		t.Fatal("want the namespace's own allow-list in effect")
	}
	if eff.AllowsProvider("openai") {
		t.Fatal("baseline must NOT be blended into an already-constrained namespace (would widen it)")
	}
}

// A configured-but-missing baseline must fail closed with an error, never
// silently degrade to "no governance" (knative-agents-2mi semantics extended
// to the baseline read).
func TestEffective_BaselineMissingReturnsError(t *testing.T) {
	c := fakeClient(t) // no objects at all — the baseline does not exist
	_, err := Effective(context.Background(), c, "tenant-a", types.NamespacedName{Namespace: "platform", Name: "floor"})
	if err == nil {
		t.Fatal("want an error when the configured baseline AgentPolicy is missing, got nil")
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want the underlying cause to be a NotFound, got %v", err)
	}
}

// A List error reading the namespace's own AgentPolicies must also fail
// closed, independent of whether a baseline is configured.
func TestEffective_NamespaceListErrorReturnsError(t *testing.T) {
	base := fakeClient(t)
	ic := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return fmt.Errorf("simulated apiserver hiccup")
		},
	})
	_, err := Effective(context.Background(), ic, "tenant-a", types.NamespacedName{Namespace: "platform", Name: "floor"})
	if err == nil {
		t.Fatal("want an error on a namespace AgentPolicy list failure, got nil")
	}
}

// No baseline configured + an empty namespace ⇒ Empty with no error: exactly
// the pre-7dm behavior, unchanged.
func TestEffective_NoBaselineNoNamespacePolicy_EmptyNoError(t *testing.T) {
	c := fakeClient(t)
	eff, err := Effective(context.Background(), c, "tenant-a", types.NamespacedName{})
	if err != nil {
		t.Fatalf("want no error, got %v", err)
	}
	if !eff.Empty {
		t.Fatal("want Empty when no namespace policy and no baseline configured")
	}
}
