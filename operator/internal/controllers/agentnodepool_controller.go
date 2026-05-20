package controllers

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/stigen/knative-agents/operator/api/v1"
	"github.com/stigen/knative-agents/operator/internal/builders"
)

const anpFieldOwner = "knative-agents-operator"

// AgentNodePoolReconciler compiles each AgentNodePool into a Karpenter
// NodePool + EC2NodeClass (the kata/devmapper layer composed onto the
// existing deployment's node-join). See docs/design/agent-platform.md.
// Implements R-PROV-1, R-PROV-3.
type AgentNodePoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager wires the controller.
//
// We intentionally do NOT Owns() the Karpenter types: their CRDs may be
// absent when the manager starts, which would block the controller cache.
// Cascade deletion is handled by ownerReferences instead; reconcile is
// triggered by changes to the AgentNodePool itself.
func (r *AgentNodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.AgentNodePool{}).
		Complete(r)
}

// Reconcile renders and server-side-applies the owned Karpenter objects.
func (r *AgentNodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agentnodepool", req.Name)

	anp := &v1.AgentNodePool{}
	if err := r.Get(ctx, req.NamespacedName, anp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	defaults := r.resolveDefaults(ctx)
	npName, ncName := builders.KarpenterNames(anp)
	objs := []*unstructured.Unstructured{
		builders.BuildKarpenterEC2NodeClass(anp, defaults),
		builders.BuildKarpenterNodePool(anp, ncName),
	}

	for _, o := range objs {
		if err := ctrl.SetControllerReference(anp, o, r.Scheme); err != nil {
			r.setCondition(anp, "Ready", metav1.ConditionFalse, "OwnerRefFailed", err.Error())
			anp.Status.Phase = "Degraded"
			return ctrl.Result{}, r.statusUpdate(ctx, anp)
		}
		if err := r.apply(ctx, o); err != nil {
			reason := "ApplyFailed"
			if meta.IsNoMatchError(err) {
				// Karpenter CRDs not installed: stay Degraded and wait
				// rather than hot-looping; a CR change re-triggers us.
				reason = "KarpenterMissing"
				logger.Info("Karpenter CRDs absent; cannot provision", "kind", o.GetKind())
			}
			r.setCondition(anp, "KarpenterSynced", metav1.ConditionFalse, reason, err.Error())
			r.setCondition(anp, "Ready", metav1.ConditionFalse, reason, "Karpenter objects not synced")
			anp.Status.Phase = "Degraded"
			if uerr := r.statusUpdate(ctx, anp); uerr != nil {
				return ctrl.Result{}, uerr
			}
			if reason == "KarpenterMissing" {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}

	anp.Status.NodePoolName = npName
	anp.Status.NodeClassName = ncName
	r.setCondition(anp, "KarpenterSynced", metav1.ConditionTrue, "Synced", "")
	r.setCondition(anp, "Ready", metav1.ConditionTrue, "Reconciled", "")
	anp.Status.Phase = "Ready"
	return ctrl.Result{}, r.statusUpdate(ctx, anp)
}

// resolveDefaults sources cluster-level provisioning defaults.
// TODO(P1): read from KnativeAgentPlatform.spec.nodeProvisioning (subnet/SG
// discovery tags, node IAM role, the existing join snippet / base AMI) once
// that field is added. Until then the kata layer is composed onto an empty
// join, which is enough to exercise the reconcile + status path.
func (r *AgentNodePoolReconciler) resolveDefaults(_ context.Context) builders.KarpenterDefaults {
	return builders.KarpenterDefaults{AMIFamily: "Custom"}
}

func (r *AgentNodePoolReconciler) apply(ctx context.Context, o *unstructured.Unstructured) error {
	return r.Patch(ctx, o, client.Apply,
		client.FieldOwner(anpFieldOwner), client.ForceOwnership)
}

func (r *AgentNodePoolReconciler) statusUpdate(ctx context.Context, anp *v1.AgentNodePool) error {
	anp.Status.ObservedGeneration = anp.Generation
	return r.Status().Update(ctx, anp)
}

// setCondition upserts a condition by type, preserving LastTransitionTime
// when the status is unchanged (matches the platform reconciler's pattern).
func (r *AgentNodePoolReconciler) setCondition(anp *v1.AgentNodePool, condType string, status metav1.ConditionStatus, reason, msg string) {
	c := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.NewTime(time.Now()),
		ObservedGeneration: anp.Generation,
	}
	for i, existing := range anp.Status.Conditions {
		if existing.Type == c.Type {
			if existing.Status == c.Status {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			anp.Status.Conditions[i] = c
			return
		}
	}
	anp.Status.Conditions = append(anp.Status.Conditions, c)
}
