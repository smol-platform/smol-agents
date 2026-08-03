package agentmodel

import (
	"context"
	"fmt"
	"net/url"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// modelProviderKinds is the set of provider families the platform actually
// supports (the ModelProviderSpec.Kind doc comment). Only openai-compatible
// and local have a working loop-mode client (pkg/agentruntime/openaillm);
// bedrock/vertex are reserved and anthropic is reachable only via a
// compatible endpoint or a harness — but all five are legitimate spec values.
var modelProviderKinds = map[string]bool{
	"openai":    true,
	"anthropic": true,
	"bedrock":   true,
	"vertex":    true,
	"local":     true,
}

// ModelProviderReconciler validates a ModelProvider's spec + secret and
// publishes a Ready/Pending/Failed status, so a bad providerRef or missing
// secret surfaces once at the source object instead of as N buried Pending
// AgentRuns. It validates + reports only — it does not resolve the provider
// for a run (gatherRunSecrets in secrets.go still does that at run time).
type ModelProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager wires the controller. There is deliberately no Watches
// on Secrets (knative-agents-5jy: that would require a cluster-wide Secret
// informer, which the manager's cache no longer holds — see main.go and
// secretRequeueInterval in agentnetwork_controller.go); the
// `Pending: SecretMissing → Ready` flip instead self-heals via the periodic
// requeue set on that status path.
func (r *ModelProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&amv1.ModelProvider{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// Reconcile is the per-provider entrypoint.
func (r *ModelProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("modelprovider", req.NamespacedName)

	p := &amv1.ModelProvider{}
	if err := r.Get(ctx, req.NamespacedName, p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	prev := p.Status // value copy — Status is primitives only

	pureProvider := pure.ModelProvider{Name: p.Name, Spec: p.Spec}
	if err := pure.ValidateModelProvider(pureProvider); err != nil {
		r.setStatus(p, "Failed", "InvalidSpec", err.Error())
		return ctrl.Result{}, r.statusUpdateIfChanged(ctx, p, prev)
	}

	if !modelProviderKinds[p.Spec.Kind] {
		r.setStatus(p, "Failed", "InvalidSpec", fmt.Sprintf("spec.kind %q is not one of openai, anthropic, bedrock, vertex, local", p.Spec.Kind))
		return ctrl.Result{}, r.statusUpdateIfChanged(ctx, p, prev)
	}

	if p.Spec.Kind == "local" && p.Spec.Endpoint == "" {
		r.setStatus(p, "Failed", "InvalidSpec", "spec.endpoint is required when spec.kind=local")
		return ctrl.Result{}, r.statusUpdateIfChanged(ctx, p, prev)
	}

	if p.Spec.Endpoint != "" {
		u, err := url.Parse(p.Spec.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			r.setStatus(p, "Failed", "InvalidSpec", fmt.Sprintf("spec.endpoint %q must be an absolute http(s) URL", p.Spec.Endpoint))
			return ctrl.Result{}, r.statusUpdateIfChanged(ctx, p, prev)
		}
	}

	// Resolve the referenced Secret — fail fast (Pending, not the buried
	// per-run failure) rather than letting every consuming AgentRun rediscover
	// this at pod-build time.
	ref := p.Spec.SecretRef
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: ref.SecretName}, secret)
	if apierrors.IsNotFound(err) {
		r.setStatus(p, "Pending", "SecretMissing", fmt.Sprintf("secret %q not found", ref.SecretName))
		return ctrl.Result{RequeueAfter: secretRequeueInterval}, r.statusUpdateIfChanged(ctx, p, prev)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// AuthRef.Key is optional; when unset, the broker (readSecretKey in
	// secrets.go) falls back to "the secret's sole key" — so an empty Key only
	// resolves unambiguously when the secret holds exactly one key.
	if ref.Key != "" {
		if _, ok := secret.Data[ref.Key]; !ok {
			r.setStatus(p, "Pending", "SecretMissing", fmt.Sprintf("secret %q has no key %q", ref.SecretName, ref.Key))
			return ctrl.Result{RequeueAfter: secretRequeueInterval}, r.statusUpdateIfChanged(ctx, p, prev)
		}
	} else if len(secret.Data) != 1 {
		keys := make([]string, 0, len(secret.Data))
		for k := range secret.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		r.setStatus(p, "Pending", "SecretAmbiguous",
			fmt.Sprintf("secret %q has no spec.secretRef.key set, so it must hold exactly one key; found %d: %v", ref.SecretName, len(keys), keys))
		return ctrl.Result{RequeueAfter: secretRequeueInterval}, r.statusUpdateIfChanged(ctx, p, prev)
	}

	r.setStatus(p, "Ready", "Reconciled", "")
	logger.Info("modelprovider ready", "kind", p.Spec.Kind)
	return ctrl.Result{}, r.statusUpdateIfChanged(ctx, p, prev)
}

func (r *ModelProviderReconciler) setStatus(p *amv1.ModelProvider, phase, reason, msg string) {
	p.Status.Phase = phase
	p.Status.Reason = reason
	p.Status.Message = msg
	p.Status.ObservedGeneration = p.Generation
}

// statusUpdateIfChanged skips the API write when status is unchanged.
func (r *ModelProviderReconciler) statusUpdateIfChanged(ctx context.Context, p *amv1.ModelProvider, prev pure.ModelProviderStatus) error {
	if equality.Semantic.DeepEqual(prev, p.Status) {
		return nil
	}
	return r.Status().Update(ctx, p)
}
