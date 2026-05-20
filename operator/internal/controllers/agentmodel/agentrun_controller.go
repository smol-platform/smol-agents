package agentmodel

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	amv1 "github.com/stigen/smol-agents/operator/api/agentmodel/v1"
	"github.com/stigen/smol-agents/operator/internal/builders"
	pure "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// AgentRunReconciler turns an AgentRun CR into a Pod and tracks its
// lifecycle. State machine mirrors pure.Phase: Pending → Running →
// Completed | Failed | Cancelled.
type AgentRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager wires the controller; Owns(Pod) so we react to Pod
// status changes immediately.
func (r *AgentRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&amv1.AgentRun{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Pod{}).
		Complete(r)
}

// Reconcile is the per-Run entrypoint.
func (r *AgentRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agentrun", req.NamespacedName)

	run := &amv1.AgentRun{}
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve the parent Agent.
	agent := &amv1.Agent{}
	err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.AgentRef}, agent)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.markPending(run, "AgentMissing", "spec.agentRef not found")
			return ctrl.Result{}, r.Status().Update(ctx, run)
		}
		return ctrl.Result{}, err
	}

	// Cancellation: if spec.cancel is set and we're not yet terminal,
	// stamp Cancelled and (best-effort) delete the Pod.
	if run.Spec.Cancel && !run.Status.State.Terminal() {
		_ = r.deletePod(ctx, run)
		r.markTerminal(run, pure.PhaseCancelled, "cancel:requested")
		return ctrl.Result{}, r.Status().Update(ctx, run)
	}

	// Ensure the Pod exists.
	pod := &corev1.Pod{}
	err = r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, pod)
	if apierrors.IsNotFound(err) {
		desired := builders.BuildAgentRunPod(run, agent)
		if err := ctrl.SetControllerReference(run, desired, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set controller ref: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, fmt.Errorf("create pod: %w", err)
		}
		r.markRunning(run)
		logger.Info("created run pod", "agent", agent.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.Status().Update(ctx, run)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Map Pod phase → AgentRun phase.
	switch pod.Status.Phase {
	case corev1.PodPending:
		r.markPending(run, "PodPending", "Pod is Pending")
	case corev1.PodRunning:
		r.markRunning(run)
	case corev1.PodSucceeded:
		r.markTerminal(run, pure.PhaseCompleted, "")
	case corev1.PodFailed:
		r.markTerminal(run, pure.PhaseFailed, terminationReason(pod))
	}

	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	if !run.Status.State.Terminal() {
		// poll Pod state every 5s until terminal
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *AgentRunReconciler) markPending(run *amv1.AgentRun, reason, msg string) {
	run.Status.State = pure.PhasePending
	run.Status.TerminationReason = msg
	run.Status.StartedAt = nil
}

func (r *AgentRunReconciler) markRunning(run *amv1.AgentRun) {
	if run.Status.State == pure.PhaseRunning {
		return
	}
	run.Status.State = pure.PhaseRunning
	now := metav1.Now()
	run.Status.StartedAt = &now
}

func (r *AgentRunReconciler) markTerminal(run *amv1.AgentRun, phase pure.Phase, reason string) {
	if run.Status.State.Terminal() && run.Status.State == phase {
		return
	}
	run.Status.State = phase
	now := metav1.Now()
	run.Status.EndedAt = &now
	if reason != "" {
		run.Status.TerminationReason = reason
	}
}

func (r *AgentRunReconciler) deletePod(ctx context.Context, run *amv1.AgentRun) error {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, pod); err != nil {
		return client.IgnoreNotFound(err)
	}
	return r.Delete(ctx, pod)
}

func terminationReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return "pod:" + cs.State.Terminated.Reason
		}
	}
	return "pod:Failed"
}
