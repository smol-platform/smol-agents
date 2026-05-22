package features

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/smol-platform/smol-agents/operator/pkg/features"
)

// SecretsReconciler reports the secrets feature's readiness. The actual
// sidecar is a Pod-spec concern handled by the workload reconciler;
// here we just gate readiness on configuration being valid.
type SecretsReconciler struct{}

func (SecretsReconciler) Name() features.Feature { return features.Secrets }

func (r SecretsReconciler) Reconcile(_ context.Context, env Env) (Result, []client.Object, error) {
	cr := env.CR
	res := Result{Feature: r.Name(), Enabled: cr.Spec.Features.Secrets.Enabled}
	if !res.Enabled {
		res.Reason = "Disabled"
		return res, nil, nil
	}
	if cr.Spec.Features.Secrets.MaxLeaseTTLSeconds < 0 {
		res.Reason = "InvalidConfig"
		res.Message = "secrets.maxLeaseTTLSeconds must be ≥ 0"
		return res, nil, nil
	}
	res.Ready = true
	res.Reason = "Reconciled"
	return res, nil, nil
}
