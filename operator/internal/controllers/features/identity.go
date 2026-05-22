package features

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/smol-platform/smol-agents/operator/internal/builders"
	"github.com/smol-platform/smol-agents/operator/pkg/features"
)

// IdentityReconciler reconciles ConfigMap (agent.yaml block for identity)
// and the ClusterSPIFFEID. R-IDN-1, R-IDN-3.
type IdentityReconciler struct{}

func (IdentityReconciler) Name() features.Feature { return features.Identity }

func (r IdentityReconciler) Reconcile(_ context.Context, env Env) (Result, []client.Object, error) {
	cr := env.CR
	res := Result{Feature: r.Name(), Enabled: cr.Spec.Features.Identity.Enabled, Mode: cr.Spec.Features.Identity.Mode}
	if cr.Spec.Mode != "" {
		res.Mode = cr.Spec.Mode
	}
	if !res.Enabled {
		res.Reason = "Disabled"
		return res, nil, nil
	}
	if cr.Spec.TrustDomain == "" {
		res.Reason = "PrerequisitesUnmet"
		res.Message = "spec.trustDomain is required"
		return res, nil, nil
	}
	owned := []client.Object{
		builders.BuildAgentConfigMap(cr),
		builders.BuildServiceAccount(cr),
		builders.BuildClusterSPIFFEID(cr),
	}
	res.Ready = true
	res.Reason = "Reconciled"
	return res, owned, nil
}
