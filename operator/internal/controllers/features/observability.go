package features

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stigen/smol-agents/operator/pkg/features"
)

// ObservabilityReconciler is config-only: the agent runtime reads
// observability settings from its ConfigMap (already owned by
// Identity). This reconciler validates and reports readiness.
type ObservabilityReconciler struct{}

func (ObservabilityReconciler) Name() features.Feature { return features.Observability }

func (r ObservabilityReconciler) Reconcile(_ context.Context, env Env) (Result, []client.Object, error) {
	cr := env.CR
	obs := cr.Spec.Features.Observability
	res := Result{Feature: r.Name(), Enabled: obs.Enabled}
	if !res.Enabled {
		res.Reason = "Disabled"
		return res, nil, nil
	}
	res.Ready = true
	res.Reason = "Reconciled"
	if obs.OTLPEndpoint == "" {
		// No-op providers; still Ready, but flag in message.
		res.Message = "no otlpEndpoint configured; metrics/traces are no-op"
	}
	return res, nil, nil
}
