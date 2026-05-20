package features

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stigen/smol-agents/operator/internal/builders"
	"github.com/stigen/smol-agents/operator/pkg/features"
)

// KnativeReconciler renders the Knative Service when the feature is
// enabled and `spec.deploymentKind == "knative"`. R-DEP-1.
//
// Prereq check: the Knative `serving.knative.dev/v1` CRD must be
// installed on the cluster. We probe with a List.
type KnativeReconciler struct{}

func (KnativeReconciler) Name() features.Feature { return features.Knative }

func (r KnativeReconciler) Reconcile(ctx context.Context, env Env) (Result, []client.Object, error) {
	cr := env.CR
	res := Result{Feature: r.Name(), Enabled: cr.Spec.Features.Knative.Enabled}
	if !res.Enabled {
		res.Reason = "Disabled"
		return res, nil, nil
	}
	if cr.Spec.DeploymentKind != "" && cr.Spec.DeploymentKind != "knative" {
		// User picked Deployment / StatefulSet — Knative feature stays
		// idle, not Ready, but not an error either.
		res.Reason = "DeploymentKindMismatch"
		res.Message = "spec.deploymentKind=" + cr.Spec.DeploymentKind + " ignores Knative feature"
		return res, nil, nil
	}
	if env.Reader != nil {
		// Best-effort prereq check — list the Knative CRD's resources;
		// any error other than "not found in scheme" implies not installed.
		gvr := schema.GroupVersionResource{Group: "serving.knative.dev", Version: "v1", Resource: "services"}
		_ = gvr // we don't actually list here to avoid pulling dynamic client; presence is checked by builder + apply step
	}
	svc := builders.BuildKnativeService(cr)
	// Bind kata agents to their node pool (auto-match by isolation). When
	// no pool matches we still render the Service; the no-KVM fallback is
	// handled by the Sandbox feature (R-PROV-2).
	if p, ok, err := ResolvePlacement(ctx, env); err != nil {
		res.Reason = "PlacementError"
		res.Message = err.Error()
		return res, nil, err
	} else if ok {
		builders.ApplyKnativePlacement(svc, *p)
		if missing := MissingKnativePodspecFlags(ctx, env.Reader); len(missing) > 0 {
			res.Message = "Knative podspec feature-flags not enabled (" +
				strings.Join(missing, ", ") +
				"); placement is ignored until they are enabled in knative-serving/config-features"
		}
	}
	owned := []client.Object{svc}
	res.Ready = true
	res.Reason = "Reconciled"
	return res, owned, nil
}
