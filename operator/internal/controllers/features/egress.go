package features

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/smol-platform/smol-agents/operator/internal/builders"
	"github.com/smol-platform/smol-agents/operator/pkg/features"
	"github.com/smol-platform/smol-agents/pkg/agentnet/plan"
)

// EgressFloorReconciler installs the default-ON egress cage for a served
// SmolAgent's pods (M1.17, D3). Unlike most features it has no enable flag — the
// floor is always on, so an agent that egresses anywhere but DNS / in-cluster /
// public 80/443 must declare it. The metadata IP (169.254.169.254) is blocked.
// The bound-AgentNetwork allow-list for the serving path is a follow-up (the
// run/session datapaths already layer it); serving gets the floor today.
type EgressFloorReconciler struct{}

func (EgressFloorReconciler) Name() features.Feature { return features.EgressFloor }

func (r EgressFloorReconciler) Reconcile(_ context.Context, env Env) (Result, []client.Object, error) {
	np := builders.BuildSmolAgentEgressPolicy(env.CR, plan.NetworkPlan{})
	return Result{
		Feature: r.Name(),
		Enabled: true,
		Ready:   true,
		Reason:  "Reconciled",
		Message: "default-deny serving egress floor (metadata blocked; DNS+in-cluster+80/443 allowed)",
	}, []client.Object{np}, nil
}
