package agentmodel

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// DynamicCredentialBackendReconciler validates a DynamicCredentialBackend (D8),
// resolves its root-secret reference (fail-fast Pending: SecretMissing rather
// than letting the broker crashloop), and reports readiness. It validates +
// reports only — it does NOT inject the broker/proxy sidecars or mint anything
// (the mint datapath is M1.22-23 + the agentnetwork-datapath proxy injection).
// Grants are operator-owned, RBAC-locked.
type DynamicCredentialBackendReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager wires the controller. Watching Secrets makes the
// `Pending: SecretMissing → Ready` flip automatic when the root key appears.
func (r *DynamicCredentialBackendReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&amv1.DynamicCredentialBackend{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToBackends)).
		Complete(r)
}

// secretToBackends maps a Secret event to the backends in the same namespace
// whose root-key reference points at it.
func (r *DynamicCredentialBackendReconciler) secretToBackends(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &amv1.DynamicCredentialBackendList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		b := &list.Items[i]
		if b.Spec.GitHubApp != nil && b.Spec.GitHubApp.PrivateKeyRef.SecretName == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: b.Namespace, Name: b.Name}})
		}
	}
	return reqs
}

// Reconcile is the per-backend entrypoint.
func (r *DynamicCredentialBackendReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("dynamiccredentialbackend", req.NamespacedName)

	b := &amv1.DynamicCredentialBackend{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	prev := b.Status // value copy — Status is primitives only

	b.Status.GrantCount = len(b.Spec.Grants)

	if err := pure.ValidateDynamicCredentialBackend(b.Spec); err != nil {
		r.setStatus(b, "Failed", "InvalidSpec", err.Error())
		return ctrl.Result{}, r.statusUpdateIfChanged(ctx, b, prev)
	}

	// Resolve the root secret (+ the key when named) — fail fast if absent.
	if b.Spec.GitHubApp != nil {
		ref := b.Spec.GitHubApp.PrivateKeyRef
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: ref.SecretName}, secret)
		if apierrors.IsNotFound(err) {
			r.setStatus(b, "Pending", "SecretMissing", fmt.Sprintf("root secret %q not found", ref.SecretName))
			return ctrl.Result{}, r.statusUpdateIfChanged(ctx, b, prev)
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if ref.Key != "" {
			if _, ok := secret.Data[ref.Key]; !ok {
				r.setStatus(b, "Pending", "SecretMissing", fmt.Sprintf("root secret %q has no key %q", ref.SecretName, ref.Key))
				return ctrl.Result{}, r.statusUpdateIfChanged(ctx, b, prev)
			}
		}
	}

	r.setStatus(b, "Ready", "Reconciled", "")
	logger.Info("dynamiccredentialbackend ready", "credential", b.Spec.CredentialName, "grants", b.Status.GrantCount)
	return ctrl.Result{}, r.statusUpdateIfChanged(ctx, b, prev)
}

func (r *DynamicCredentialBackendReconciler) setStatus(b *amv1.DynamicCredentialBackend, phase, reason, msg string) {
	b.Status.Phase = phase
	b.Status.Reason = reason
	b.Status.Message = msg
	b.Status.ObservedGeneration = b.Generation
}

// statusUpdateIfChanged skips the API write when status is unchanged.
func (r *DynamicCredentialBackendReconciler) statusUpdateIfChanged(ctx context.Context, b *amv1.DynamicCredentialBackend, prev pure.DynamicCredentialBackendStatus) error {
	if equality.Semantic.DeepEqual(prev, b.Status) {
		return nil
	}
	return r.Status().Update(ctx, b)
}
