// Package policy is the single implementation of "what AgentPolicy governs
// this namespace" shared by the admission webhook
// (operator/internal/webhooks/agentpolicy_gate_webhook.go) and the reconcile
// backstop (operator/internal/controllers/agentmodel/policy.go). Before
// knative-agents-7dm the two carried near-identical list+compose copies —
// exactly the divergence class this epic exists to close — so any change to
// the composition or fail-closed behavior had to be made twice and could
// silently drift.
package policy

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	purev1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// Effective composes the namespace's own AgentPolicies (as ComposePolicies
// always has) and, only when that composition is Empty (no AgentPolicy in ns
// constrains anything), falls back to the platform baseline named by
// baseline (knative-agents-7dm: "default-deny baseline when no policy
// present", D1). baseline.Name == "" disables the fallback outright — the
// zero value of a struct field or an unset --platform-agent-policy flag —
// which reproduces pre-7dm behavior exactly (Empty ⇒ callers skip
// enforcement).
//
// A namespace with its OWN constraining policy is never blended with the
// baseline: ComposePolicies UNIONS allow-lists, so folding a baseline into an
// already-constrained namespace would WIDEN it (permit providers/tools the
// namespace itself never allow-listed) — the opposite of governance intent.
// Opting into any policy means the namespace owns its policy outright.
//
// Both a List error on the namespace's own policies and a Get error on a
// configured-but-unreadable baseline (including NotFound) are returned as an
// error rather than folded into an Empty result: a misconfigured baseline or
// a transient apiserver hiccup must fail CLOSED, exactly like the List-error
// path from knative-agents-2mi, never silently degrade to "no governance".
// The two error cases are not distinguished in the return value — every
// caller today (the webhook's apierrors.NewInternalError deny, the
// reconciler's Pending/PolicyUnavailable + requeue) treats them identically.
func Effective(ctx context.Context, c client.Client, ns string, baseline types.NamespacedName) (purev1.EffectivePolicy, error) {
	var list amv1.AgentPolicyList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return purev1.EffectivePolicy{}, fmt.Errorf("list AgentPolicy in namespace %q: %w", ns, err)
	}
	pol := make([]purev1.AgentPolicy, 0, len(list.Items))
	for i := range list.Items {
		pol = append(pol, purev1.AgentPolicy{Name: list.Items[i].Name, Spec: list.Items[i].Spec})
	}
	eff := purev1.ComposePolicies(pol)
	if !eff.Empty || baseline.Name == "" {
		return eff, nil
	}

	var base amv1.AgentPolicy
	if err := c.Get(ctx, baseline, &base); err != nil {
		return purev1.EffectivePolicy{}, fmt.Errorf("get platform baseline AgentPolicy %s: %w", baseline, err)
	}
	return purev1.ComposePolicies([]purev1.AgentPolicy{{Name: base.Name, Spec: base.Spec}}), nil
}
