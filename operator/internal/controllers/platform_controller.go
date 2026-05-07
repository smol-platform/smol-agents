package controllers

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/stigen/knative-agents/operator/api/v1"
	"github.com/stigen/knative-agents/operator/internal/builders"
)

// KnativeAgentPlatformReconciler reconciles cluster-scoped platform
// state: the ebpf-loader DaemonSet, its ServiceAccount/ConfigMap, and
// (optionally) the cluster RuntimeClass for sandbox.
//
// Implements R-OP-API-2 (singleton platform CR) by treating any
// platform CR named other than `SingletonName` as a config error.
type KnativeAgentPlatformReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// SingletonName is the only acceptable name for a Platform CR.
	// Defaults to "default".
	SingletonName string

	// Namespace where the operator deploys cluster-wide resources
	// (ebpf-loader DaemonSet etc.). Defaults to "knative-agents-system".
	Namespace string
}

// SetupWithManager wires the controller.
func (r *KnativeAgentPlatformReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.SingletonName == "" {
		r.SingletonName = "default"
	}
	if r.Namespace == "" {
		r.Namespace = "knative-agents-system"
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.KnativeAgentPlatform{}).
		Complete(r)
}

// Reconcile owns the ebpf-loader stack at cluster scope.
func (r *KnativeAgentPlatformReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("platform", req.Name)

	cr := &v1.KnativeAgentPlatform{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// R-OP-API-2: enforce singleton.
	if cr.Name != r.SingletonName {
		logger.Info("rejecting non-singleton platform CR", "name", cr.Name, "want", r.SingletonName)
		r.setReady(cr, false, "InvalidName",
			fmt.Sprintf("only %q is permitted; rename or delete %q", r.SingletonName, cr.Name))
		return ctrl.Result{}, r.Status().Update(ctx, cr)
	}

	// Validate preset; unknown falls back to generic with a warning.
	preset := cr.Spec.EBPFLoader.Preset
	if preset == "" {
		preset = "generic"
	}
	if _, ok := builders.LoaderPresets[preset]; !ok {
		r.setReady(cr, false, "UnknownPreset",
			fmt.Sprintf("ebpfLoader.preset=%q is not in {generic,gke-cos,eks-bottlerocket,aks-mariner,k3s,openshift,talos}", preset))
		return ctrl.Result{}, r.Status().Update(ctx, cr)
	}

	if cr.Spec.EBPFLoader.Enabled {
		objs := []client.Object{
			builders.BuildEBPFLoaderServiceAccount(r.Namespace),
			builders.BuildEBPFLoaderConfigMap(cr, r.Namespace),
			builders.BuildEBPFLoaderDaemonSet(cr, r.Namespace, preset),
		}
		for _, o := range objs {
			if err := r.applyOwned(ctx, cr, o); err != nil {
				r.setReady(cr, false, "ApplyFailed", err.Error())
				_ = r.Status().Update(ctx, cr)
				return ctrl.Result{}, err
			}
		}
	}

	r.updateManagedTenants(ctx, cr)
	r.setReady(cr, true, "Reconciled", "")
	return ctrl.Result{}, r.Status().Update(ctx, cr)
}

func (r *KnativeAgentPlatformReconciler) updateManagedTenants(ctx context.Context, p *v1.KnativeAgentPlatform) {
	list := &v1.KnativeAgentList{}
	if err := r.List(ctx, list); err != nil {
		return
	}
	p.Status.ManagedTenants = int32(len(list.Items))
}

func (r *KnativeAgentPlatformReconciler) setReady(p *v1.KnativeAgentPlatform, ready bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	now := metav1.NewTime(time.Now())
	c := metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: p.Generation,
	}
	for i, existing := range p.Status.Conditions {
		if existing.Type == c.Type {
			if existing.Status == c.Status {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			p.Status.Conditions[i] = c
			p.Status.ObservedGeneration = p.Generation
			return
		}
	}
	p.Status.Conditions = append(p.Status.Conditions, c)
	p.Status.ObservedGeneration = p.Generation
}

func (r *KnativeAgentPlatformReconciler) applyOwned(ctx context.Context, owner *v1.KnativeAgentPlatform, desired client.Object) error {
	current := desired.DeepCopyObject().(client.Object)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		// Set the OwnerReference for cluster-scoped objects too — this
		// causes garbage collection when the platform CR is deleted.
		if err := ctrl.SetControllerReference(owner, desired, r.Scheme); err != nil {
			// Cluster-scoped objects can't be owned by a cluster-scoped
			// platform with namespaced owner; ignore the error since
			// SetControllerReference enforces same-namespace.
			_ = err
		}
		return r.Create(ctx, desired)
	}
	// Naive replace; SSA would be cleaner.
	desired.SetResourceVersion(current.GetResourceVersion())
	return r.Update(ctx, desired)
}
