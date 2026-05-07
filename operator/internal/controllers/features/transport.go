package features

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stigen/knative-agents/operator/pkg/features"
)

// TransportPrivateReconciler reports readiness for the in-mesh SPIFFE
// mTLS feature. The actual listener lives in the agent runtime; here we
// validate config and gate it on Identity being ready.
type TransportPrivateReconciler struct{}

func (TransportPrivateReconciler) Name() features.Feature { return features.TransportPrivate }

func (r TransportPrivateReconciler) Reconcile(_ context.Context, env Env) (Result, []client.Object, error) {
	cr := env.CR
	res := Result{Feature: r.Name(), Enabled: cr.Spec.Features.Transport.Private.Enabled}
	if !res.Enabled {
		res.Reason = "Disabled"
		return res, nil, nil
	}
	if !cr.Spec.Features.Identity.Enabled {
		res.Reason = "PrerequisitesUnmet"
		res.Message = "transport.private requires identity feature"
		return res, nil, nil
	}
	res.Ready = true
	res.Reason = "Reconciled"
	return res, nil, nil
}

// TransportPublicReconciler reports readiness for the gateway-fronted
// public mTLS feature. Disabled by default; when enabled requires
// certPath + keyPath.
type TransportPublicReconciler struct{}

func (TransportPublicReconciler) Name() features.Feature { return features.TransportPublic }

func (r TransportPublicReconciler) Reconcile(_ context.Context, env Env) (Result, []client.Object, error) {
	cr := env.CR
	pub := cr.Spec.Features.Transport.Public
	res := Result{Feature: r.Name(), Enabled: pub.Enabled}
	if !res.Enabled {
		res.Reason = "Disabled"
		return res, nil, nil
	}
	if pub.CertPath == "" || pub.KeyPath == "" {
		res.Reason = "PrerequisitesUnmet"
		res.Message = "transport.public.{certPath,keyPath} are required when enabled (R-MTL-2)"
		return res, nil, nil
	}
	res.Ready = true
	res.Reason = "Reconciled"
	return res, nil, nil
}
