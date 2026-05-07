package features

import (
	"context"
	"fmt"

	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stigen/knative-agents/operator/internal/builders"
	"github.com/stigen/knative-agents/operator/pkg/features"
	pkgsandbox "github.com/stigen/knative-agents/pkg/sandbox"
)

// SandboxReconciler enforces RuntimeClass selection. R-SBX-1.
//
// Default runtimeClass is "kata-fc" (Kata Containers + Firecracker
// microVMs). For runtime classes the operator knows how to
// auto-install ("kata-fc", "gvisor") it owns the RuntimeClass object
// when the cluster lacks one.
type SandboxReconciler struct{}

func (SandboxReconciler) Name() features.Feature { return features.Sandbox }

func (r SandboxReconciler) Reconcile(ctx context.Context, env Env) (Result, []client.Object, error) {
	cr := env.CR
	res := Result{Feature: r.Name(), Enabled: cr.Spec.Features.Sandbox.Enabled}
	if !res.Enabled {
		res.Reason = "Disabled"
		return res, nil, nil
	}
	rc := cr.Spec.Features.Sandbox.RuntimeClass
	if rc == "" {
		rc = string(pkgsandbox.KindKataFC)
	}
	kind := pkgsandbox.ParseKind(rc)
	if kind == pkgsandbox.KindRunc && !cr.Spec.Features.Sandbox.AllowHostEscape {
		res.Reason = "PolicyViolation"
		res.Message = "runtimeClass=runc requires allowHostEscape=true (R-SBX-1)"
		return res, nil, fmt.Errorf("sandbox: %s", res.Message)
	}

	// Auto-provision the RuntimeClass for kinds we know how to install.
	if env.Reader != nil {
		var owned client.Object
		switch kind {
		case pkgsandbox.KindKataFC:
			owned = builders.BuildRuntimeClassKataFC()
		case pkgsandbox.KindGVisor:
			owned = builders.BuildRuntimeClassGVisor()
		}
		if owned != nil {
			var existing nodev1.RuntimeClass
			err := env.Reader.Get(ctx, types.NamespacedName{Name: owned.GetName()}, &existing)
			if err != nil && !apierrors.IsNotFound(err) {
				return res, nil, err
			}
			if apierrors.IsNotFound(err) {
				res.Ready = true
				res.Reason = "Reconciled"
				res.Mode = string(kind)
				return res, []client.Object{owned}, nil
			}
		}
	}
	res.Ready = true
	res.Reason = "Reconciled"
	res.Mode = string(kind)
	return res, nil, nil
}
