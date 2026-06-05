package agentmodel

import (
	"context"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentnet/plan"
)

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
		return plan.NetworkPlan{}, err
	}
	p.Networks = names
	return p, nil
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
