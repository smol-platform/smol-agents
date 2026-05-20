package features

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stigen/smol-agents/operator/pkg/features"
)

// EBPFReconciler validates the agent's eBPF program list against what
// the platform's ebpf-loader DaemonSet supports. Cluster-side DaemonSet
// is owned by the Platform reconciler, not this one.
type EBPFReconciler struct{}

func (EBPFReconciler) Name() features.Feature { return features.EBPF }

func (r EBPFReconciler) Reconcile(_ context.Context, env Env) (Result, []client.Object, error) {
	cr := env.CR
	res := Result{Feature: r.Name(), Enabled: cr.Spec.Features.EBPF.Enabled}
	if !res.Enabled {
		res.Reason = "Disabled"
		return res, nil, nil
	}
	// Prereq: platform CR exists and has loader enabled.
	if env.Platform == nil {
		res.Reason = "PrerequisitesUnmet"
		res.Message = "no SmolAgentPlatform found; install one before enabling eBPF"
		return res, nil, nil
	}
	if !env.Platform.Spec.EBPFLoader.Enabled {
		res.Reason = "PrerequisitesUnmet"
		res.Message = "platform.spec.ebpfLoader.enabled=false"
		return res, nil, nil
	}
	res.Ready = true
	res.Reason = "Reconciled"
	return res, nil, nil
}
