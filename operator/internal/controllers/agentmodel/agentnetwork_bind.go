package agentmodel

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentnet/plan"
)

// ErrNetworkConflict marks a NetworkPlan compose conflict (clashing
// localPort/localAddr/TTS, or a metadata-overlapping allow CIDR) — a spec-level
// error in the namespace's AgentNetworks that requeuing cannot fix. Callers hold
// the run/session Pending/NetworkConflict (visible, fail-closed) rather than
// caging it wrong or erroring in a tight loop. A transient List error is NOT
// wrapped with this, so it still surfaces as a normal requeue-able error.
var ErrNetworkConflict = errors.New("network conflict")

// resolveBoundNetworks composes the NetworkPlan for an agent from every
// AgentNetwork in its namespace whose agentSelector is a subset of the agent's
// labels. An AgentNetwork with an empty selector binds nothing (it is available
// but inert). A cross-network conflict (clashing localPort/TTS, metadata CIDR)
// surfaces as an error so the caller holds the run rather than caging it wrong.
func resolveBoundNetworks(ctx context.Context, c client.Client, agent *amv1.Agent) (plan.NetworkPlan, error) {
	var list amv1.AgentNetworkList
	if err := c.List(ctx, &list, client.InNamespace(agent.Namespace)); err != nil {
		return plan.NetworkPlan{}, err
	}
	var bound []pure.AgentNetworkSpec
	var names []string
	for i := range list.Items {
		an := &list.Items[i]
		if len(an.Spec.AgentSelector) == 0 {
			continue
		}
		if labels.SelectorFromSet(an.Spec.AgentSelector).Matches(labels.Set(agent.Labels)) {
			bound = append(bound, an.Spec)
			names = append(names, an.Name)
		}
	}
	p, err := plan.BuildNetworkPlan(bound)
	if err != nil {
		return plan.NetworkPlan{}, fmt.Errorf("%w: %w", ErrNetworkConflict, err)
	}
	p.Networks = names
	return p, nil
}

// agentsBoundToNetwork returns the names of Agents in the AgentNetwork's namespace
// whose labels match its (non-empty) agentSelector — the set whose runs/sessions a
// change to this network must re-reconcile (M1.16 Watches). An empty selector or a
// List error yields nil (the network binds nothing, or we re-reconcile nothing on
// a transient error — the level-triggered loop recovers).
func agentsBoundToNetwork(ctx context.Context, c client.Client, an *amv1.AgentNetwork) map[string]bool {
	if len(an.Spec.AgentSelector) == 0 {
		return nil
	}
	var agents amv1.AgentList
	if err := c.List(ctx, &agents, client.InNamespace(an.Namespace)); err != nil {
		return nil
	}
	sel := labels.SelectorFromSet(an.Spec.AgentSelector)
	out := map[string]bool{}
	for i := range agents.Items {
		if sel.Matches(labels.Set(agents.Items[i].Labels)) {
			out[agents.Items[i].Name] = true
		}
	}
	return out
}

// egressEnforcementLabel summarizes a plan's egress posture for status (M1.19):
// "tiered" when a bound AgentNetwork layers an allow-list on top of the floor,
// else "default-deny" (the floor only).
func egressEnforcementLabel(p plan.NetworkPlan) string {
	if len(p.AllowRules) > 0 {
		return "tiered"
	}
	return "default-deny"
}
