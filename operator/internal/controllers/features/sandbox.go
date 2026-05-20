package features

import (
	"context"
	"fmt"

	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stigen/smol-agents/operator/internal/builders"
	"github.com/stigen/smol-agents/operator/pkg/features"
	pkgsandbox "github.com/stigen/smol-agents/pkg/sandbox"
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

	// gVisor fallback (R-PROV-2): a kata isolation needs a metal node from a
	// matching AgentNodePool. If none exists, either fall back to gVisor
	// (when the platform allows it) or hold the agent NotReady — never
	// schedule a "kata" pod with no microVM-capable node behind it.
	if builders.RequiresKVM(rc) && env.Reader != nil {
		_, hasPool, err := ResolvePlacement(ctx, env)
		if err != nil {
			return res, nil, err
		}
		if !hasPool {
			if env.Platform == nil || !env.Platform.Spec.NodeProvisioning.AllowGvisorFallback {
				res.Reason = "NoKVMCapacity"
				res.Message = fmt.Sprintf("no AgentNodePool provides isolation %q and gVisor fallback is disabled", rc)
				return res, nil, fmt.Errorf("sandbox: %s", res.Message)
			}
			// Switch the effective runtimeClass in-memory so downstream
			// feature reconcilers (Knative runs after Sandbox) render gvisor
			// and skip the kata node placement.
			rc = string(pkgsandbox.KindGVisor)
			cr.Spec.Features.Sandbox.RuntimeClass = rc
			kind = pkgsandbox.KindGVisor
			res.Message = "no kata AgentNodePool; fell back to gVisor (R-PROV-2)"
		}
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
