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

	// PlatformName is the singleton KnativeAgentPlatform whose
	// nodeProvisioning block supplies cluster-level defaults. Defaults to
	// "default".
	PlatformName string

	// Namespace is where ClusterAutoscaler node-group ConfigMaps are
	// written. Defaults to "knative-agents-system".
	Namespace string
}

// SetupWithManager wires the controller.
//
// We intentionally do NOT Owns() the Karpenter types: their CRDs may be
// absent when the manager starts, which would block the controller cache.
// Cascade deletion is handled by ownerReferences instead; reconcile is
// triggered by changes to the AgentNodePool itself.
func (r *AgentNodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.PlatformName == "" {
		r.PlatformName = "default"
	}
	if r.Namespace == "" {
		r.Namespace = "knative-agents-system"
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.AgentNodePool{}).
		Complete(r)
}

// Reconcile dispatches to the configured node-provisioning backend.
func (r *AgentNodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	anp := &v1.AgentNodePool{}
	if err := r.Get(ctx, req.NamespacedName, anp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defaults := r.resolveDefaults(ctx)
	if anp.Spec.Provider == "ClusterAutoscaler" {
		return r.reconcileClusterAutoscaler(ctx, anp, defaults)
	}
	return r.reconcileKarpenter(ctx, anp, defaults)
}

// reconcileKarpenter renders + server-side-applies the owned Karpenter
// NodePool + EC2NodeClass.
func (r *AgentNodePoolReconciler) reconcileKarpenter(ctx context.Context, anp *v1.AgentNodePool, defaults builders.KarpenterDefaults) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
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

// reconcileClusterAutoscaler emits the externally-managed node-group spec as
// an owned ConfigMap. CAS scales the matching ASG (which the cluster's IaC
// creates from this spec); the operator cannot create the ASG itself, so
// there is no in-cluster node-group object to sync.
func (r *AgentNodePoolReconciler) reconcileClusterAutoscaler(ctx context.Context, anp *v1.AgentNodePool, defaults builders.KarpenterDefaults) (ctrl.Result, error) {
	cm := builders.BuildClusterAutoscalerConfigMap(anp, r.Namespace, defaults)
	if err := ctrl.SetControllerReference(anp, cm, r.Scheme); err != nil {
		r.setCondition(anp, "Ready", metav1.ConditionFalse, "OwnerRefFailed", err.Error())
		anp.Status.Phase = "Degraded"
		return ctrl.Result{}, r.statusUpdate(ctx, anp)
	}
	if err := r.apply(ctx, cm); err != nil {
		r.setCondition(anp, "NodeGroupRendered", metav1.ConditionFalse, "ApplyFailed", err.Error())
		r.setCondition(anp, "Ready", metav1.ConditionFalse, "ApplyFailed", "node-group ConfigMap not written")
		anp.Status.Phase = "Degraded"
		if uerr := r.statusUpdate(ctx, anp); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{}, err
	}

	anp.Status.NodePoolName = ""
	anp.Status.NodeClassName = cm.Name
	r.setCondition(anp, "NodeGroupRendered", metav1.ConditionTrue, "ClusterAutoscaler",
		"node group is externally managed; required ASG config in ConfigMap "+r.Namespace+"/"+cm.Name)
	r.setCondition(anp, "Ready", metav1.ConditionTrue, "Reconciled", "")
	anp.Status.Phase = "Ready"
	return ctrl.Result{}, r.statusUpdate(ctx, anp)
}

// resolveDefaults sources cluster-level provisioning defaults from the
// singleton KnativeAgentPlatform's nodeProvisioning block (subnet/SG
// discovery tags, node IAM role, the existing join snippet / base AMI).
// If the Platform is absent we return minimal defaults; the resulting
// EC2NodeClass will lack selectors and Karpenter will reject it, surfaced
// as ApplyFailed on the AgentNodePool.
func (r *AgentNodePoolReconciler) resolveDefaults(ctx context.Context) builders.KarpenterDefaults {
	d := builders.KarpenterDefaults{AMIFamily: "Custom"}
	name := r.PlatformName
	if name == "" {
		name = "default"
	}
	p := &v1.KnativeAgentPlatform{}
	if err := r.Get(ctx, client.ObjectKey{Name: name}, p); err != nil {
		return d
	}
	np := p.Spec.NodeProvisioning
	if np.AMIFamily != "" {
		d.AMIFamily = np.AMIFamily
	}
	d.Role = np.Role
	d.SubnetSelectorTags = np.SubnetSelectorTags
	d.SecurityGroupSelectorTags = np.SecurityGroupSelectorTags
	d.BaseAMISelector = np.BaseAMISelector
	d.JoinUserData = np.JoinUserData
	return d
}

func (r *AgentNodePoolReconciler) apply(ctx context.Context, o client.Object) error {
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
