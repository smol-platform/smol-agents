package agentmodel

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// AgentNetworkReconciler validates an AgentNetwork CR, resolves the
// secrets it references (so callers fail fast if a key is missing),
// counts how many Agents in the namespace match the selector, and
// reports Status.Phase. It does NOT inject sidecars itself — the
// AgentRun reconciler reads bound AgentNetworks and renders them via
// builders.BuildAgentRunPod (R-AN-PROXY-3).
type AgentNetworkReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager wires the controller. Watching Secrets makes the
// `Pending: SecretMissing → Ready` transition automatic — no spec
// bump required when the broker secret appears.
func (r *AgentNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&amv1.AgentNetwork{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToAgentNetworks)).
		Complete(r)
}

// secretToAgentNetworks maps a Secret event to the AgentNetworks in
// the same namespace whose WireGuardMesh.PrivateKeyRef points at it.
func (r *AgentNetworkReconciler) secretToAgentNetworks(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &amv1.AgentNetworkList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		an := &list.Items[i]
		if an.Spec.WireGuardMesh != nil && an.Spec.WireGuardMesh.PrivateKeyRef.SecretName == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: an.Namespace, Name: an.Name,
			}})
		}
	}
	return reqs
}

// Reconcile is the per-AgentNetwork entrypoint.
func (r *AgentNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agentnetwork", req.NamespacedName)

	an := &amv1.AgentNetwork{}
	if err := r.Get(ctx, req.NamespacedName, an); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	prev := an.Status // value copy — Status is primitives only

	if err := pure.ValidateAgentNetwork(an.Spec); err != nil {
		r.setStatus(an, "Failed", "InvalidSpec", err.Error())
		return ctrl.Result{}, r.statusUpdateIfChanged(ctx, an, prev)
	}

	// Per-kind status fields.
	switch an.Spec.Kind {
	case pure.NetworkIdentityProxy:
		an.Status.ProxyResourceCount = int32(len(an.Spec.IdentityProxy.Resources))
		an.Status.WGPeerCount = 0
	case pure.NetworkWireGuardMesh:
		an.Status.WGPeerCount = int32(len(an.Spec.WireGuardMesh.Peers))
		an.Status.ProxyResourceCount = 0

		// WireGuard requires the broker private key — fail fast if
		// the Secret is missing rather than waiting until the agent
		// boots and crashloops on Start().
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: an.Namespace,
			Name:      an.Spec.WireGuardMesh.PrivateKeyRef.SecretName,
		}, secret)
		if apierrors.IsNotFound(err) {
			r.setStatus(an, "Pending", "SecretMissing",
				fmt.Sprintf("secret %q not found", an.Spec.WireGuardMesh.PrivateKeyRef.SecretName))
			return ctrl.Result{}, r.statusUpdateIfChanged(ctx, an, prev)
		}
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Empty selector binds zero (network available but unbound) per R-AN-API-2.
	bound := int32(0)
	if len(an.Spec.AgentSelector) > 0 {
		agents := &amv1.AgentList{}
		if err := r.List(ctx, agents,
			client.InNamespace(an.Namespace),
			client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(an.Spec.AgentSelector)},
		); err != nil {
			return ctrl.Result{}, fmt.Errorf("list agents: %w", err)
		}
		bound = int32(len(agents.Items))
	}
	an.Status.BoundAgents = bound

	r.setStatus(an, "Ready", "Reconciled", "")
	logger.Info("agentnetwork ready",
		"kind", an.Spec.Kind,
		"bound", bound,
		"resources", an.Status.ProxyResourceCount,
		"peers", an.Status.WGPeerCount,
	)
	return ctrl.Result{}, r.statusUpdateIfChanged(ctx, an, prev)
}

func (r *AgentNetworkReconciler) setStatus(an *amv1.AgentNetwork, phase, reason, msg string) {
	an.Status.Phase = phase
	an.Status.Reason = reason
	an.Status.Message = msg
	an.Status.ObservedGeneration = an.Generation
}

// statusUpdateIfChanged skips the API write when status is byte-identical.
// Saves apiserver load on requeues that don't actually change anything.
func (r *AgentNetworkReconciler) statusUpdateIfChanged(ctx context.Context, an *amv1.AgentNetwork, prev pure.AgentNetworkStatus) error {
	if equality.Semantic.DeepEqual(prev, an.Status) {
		return nil
	}
	return r.Status().Update(ctx, an)
}
