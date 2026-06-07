package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/operator/internal/controllers/features"
	"github.com/smol-platform/smol-agents/operator/internal/events"
	"github.com/smol-platform/smol-agents/operator/internal/metrics"
	flib "github.com/smol-platform/smol-agents/operator/pkg/features"
)

// SmolAgentReconciler is the top-level controller. It dispatches to
// each FeatureReconciler in registration order and folds results into
// Status.
type SmolAgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Reconcilers is the ordered set the controller dispatches to.
	// Defaulted to All() when nil.
	Reconcilers []features.FeatureReconciler

	// PlatformName is the singleton SmolAgentPlatform name. Defaults
	// to "default" if unset.
	PlatformName string

	// Events is the per-controller event recorder. nil-safe.
	Events *events.Recorder
}

// SetupWithManager wires the controller. R-OP-REC-1, R-OP-REC-3.
func (r *SmolAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Reconcilers == nil {
		r.Reconcilers = AllFeatureReconcilers()
	}
	if r.PlatformName == "" {
		r.PlatformName = "default"
	}
	if r.Events == nil {
		r.Events = events.NewRecorder(mgr.GetEventRecorderFor("smolagent-controller"))
	}
	// Owns() registers ownership for every namespaced resource a
	// FeatureReconciler may produce. controller-runtime watches them
	// and re-queues the parent CR when any owned object is mutated by
	// another field manager — that's our drift detector. R-OP-REC-3.
	//
	// We use GenerationChangedPredicate on the parent so churn on
	// status-only updates doesn't cause loop amplification.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.SmolAgent{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&nodev1.RuntimeClass{}).
		Complete(r)
}

// Reconcile is the per-CR entrypoint.
func (r *SmolAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("smolagent", req.NamespacedName)

	startedAt := time.Now()
	defer func() {
		metrics.ReconcileDuration.WithLabelValues("smolagent").
			Observe(time.Since(startedAt).Seconds())
	}()

	cr := &v1.SmolAgent{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if cr.Spec.Rollout.Paused {
		logger.Info("rollout paused; skipping")
		r.Events.RolloutPaused(cr)
		return ctrl.Result{}, nil
	}

	platform, platErr := r.fetchPlatform(ctx)
	if platErr != nil && !errors.Is(platErr, errPlatformAbsent) {
		return ctrl.Result{}, platErr
	}

	results, objects := r.runReconcilers(ctx, cr, platform)

	for _, obj := range objects {
		if err := r.applyOwned(ctx, cr, obj); err != nil {
			r.Events.ReconcileFailed(cr, err)
			results = append(results, FeatureResult{
				Feature: flib.Feature("apply"),
				Enabled: true, Ready: false,
				Reason: "ApplyFailed", Message: err.Error(),
				Err: err,
			})
			break
		}
	}

	r.recordMetrics(cr, results)
	r.recordTransitionEvents(cr, results)

	ApplyFeatureResults(cr, results, time.Now())
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	return ctrl.Result{}, nil
}

// recordMetrics updates per-feature gauges + the error counter.
func (r *SmolAgentReconciler) recordMetrics(cr *v1.SmolAgent, results []FeatureResult) {
	for _, res := range results {
		metrics.Record(cr.Namespace, cr.Name, string(res.Feature), res.Enabled, res.Ready, res.Err)
	}
}

// recordTransitionEvents emits a Kubernetes Event when a feature
// crosses Ready or hits a prereq failure that wasn't already in the
// previous Status. Idempotent across reconciles thanks to the
// previous-status comparison.
func (r *SmolAgentReconciler) recordTransitionEvents(cr *v1.SmolAgent, results []FeatureResult) {
	if r.Events == nil {
		return
	}
	prev := cr.Status.Features
	for _, res := range results {
		old, hadOld := prev[string(res.Feature)]
		switch {
		case res.Err != nil:
			r.Events.ReconcileFailed(cr, res.Err)
		case res.Ready && (!hadOld || !old.Ready):
			r.Events.FeatureReady(cr, res.Feature)
		case !res.Ready && res.Reason == "PrerequisitesUnmet" && (!hadOld || old.Reason != "PrerequisitesUnmet"):
			r.Events.FeaturePrereqMissing(cr, res.Feature, res.Message)
		case !res.Ready && res.Reason == "PolicyViolation":
			r.Events.PolicyViolation(cr, res.Message)
		}
	}
}

// runReconcilers calls every registered FeatureReconciler and gathers
// their results. R-OP-REC-4: a single-feature failure does not abort
// the loop.
func (r *SmolAgentReconciler) runReconcilers(ctx context.Context, cr *v1.SmolAgent, platform *v1.SmolAgentPlatform) ([]FeatureResult, []client.Object) {
	results := make([]FeatureResult, 0, len(r.Reconcilers))
	objects := []client.Object{}
	env := features.Env{CR: cr, Platform: platform, Reader: r.Client, Scheme: r.Scheme}
	for _, fr := range r.Reconcilers {
		res, owned, err := fr.Reconcile(ctx, env)
		if err != nil {
			res.Err = err
		}
		results = append(results, res)
		if res.Ready && err == nil {
			objects = append(objects, owned...)
		}
	}
	return results, objects
}

// applyOwned uses CreateOrUpdate (R-OP-REC-2). Sets controller reference
// when the resource is namespaced and lives in the CR's namespace.
func (r *SmolAgentReconciler) applyOwned(ctx context.Context, cr *v1.SmolAgent, desired client.Object) error {
	desired = desired.DeepCopyObject().(client.Object)
	current := desired.DeepCopyObject().(client.Object)
	mut := func() error {
		// Set OwnerReference for namespaced objects in the CR's namespace.
		if desired.GetNamespace() == cr.Namespace {
			if err := ctrl.SetControllerReference(cr, current, r.Scheme); err != nil {
				return err
			}
		}
		return nil
	}
	// Naïve apply: get-or-create. A real operator would use SSA; this
	// keeps tests light.
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if err != nil { // NotFound → create
		if mErr := mut(); mErr != nil {
			return mErr
		}
		copyInto(current, desired)
		return r.Create(ctx, current)
	}
	if mErr := mut(); mErr != nil {
		return mErr
	}
	copyInto(current, desired)
	return r.Update(ctx, current)
}

// copyInto is a deliberately-simple replace; the production operator
// will use server-side apply, but for unit-testable correctness this is
// sufficient.
func copyInto(dst, src client.Object) {
	dst.SetName(src.GetName())
	dst.SetNamespace(src.GetNamespace())
	dst.SetLabels(src.GetLabels())
	dst.SetAnnotations(src.GetAnnotations())
}

var errPlatformAbsent = errors.New("controllers: SmolAgentPlatform not found")

func (r *SmolAgentReconciler) fetchPlatform(ctx context.Context) (*v1.SmolAgentPlatform, error) {
	p := &v1.SmolAgentPlatform{}
	err := r.Get(ctx, client.ObjectKey{Name: r.PlatformName}, p)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, errPlatformAbsent
		}
		return nil, err
	}
	return p, nil
}

// AllFeatureReconcilers returns the registered reconcilers in dispatch
// order. The ordering is deliberate:
//  1. Identity   — owned ConfigMap is consumed by every other feature
//  2. Sandbox    — gates Pod template runtimeClassName
//  3. Secrets    — broker sidecar joins Pod template
//  4. EBPF       — host-side prereq check (DaemonSet owned by Platform)
//  5. Transport.Private / Public — agent-side listener config
//  6. Knative    — renders Knative Service when DeploymentKind=knative
//  7. Observability — config-only; reads from agent ConfigMap
func AllFeatureReconcilers() []features.FeatureReconciler {
	return []features.FeatureReconciler{
		features.IdentityReconciler{},
		features.SandboxReconciler{},
		features.SecretsReconciler{},
		features.EBPFReconciler{},
		features.TransportPrivateReconciler{},
		features.TransportPublicReconciler{},
		features.KnativeReconciler{},
		features.ObservabilityReconciler{},
		features.EgressFloorReconciler{},
	}
}
