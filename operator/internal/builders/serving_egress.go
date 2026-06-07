package builders

import (
	networkingv1 "k8s.io/api/networking/v1"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/pkg/agentnet/plan"
)

// BuildSmolAgentEgressPolicy renders the default-ON egress floor for a served
// SmolAgent's pods (M1.17, D3): the same cage the run/session datapaths use,
// selecting the Knative revision pods via Selector(cr) (the labels
// BuildKnativeService stamps on the revision template). An empty plan is the
// default-deny floor — DNS + in-cluster + public 80/443, with the
// 169.254.169.254 metadata IP blocked; a bound NetworkPlan only ever tightens
// it. Default-ON, no opt-in flag (D3): served agents that need non-80/443
// egress must declare it explicitly.
func BuildSmolAgentEgressPolicy(cr *v1.SmolAgent, p plan.NetworkPlan) *networkingv1.NetworkPolicy {
	return BuildEgressPolicyWithPlan(cr.Name+"-serving-egress", cr.Namespace, "serving", Selector(cr), p)
}
