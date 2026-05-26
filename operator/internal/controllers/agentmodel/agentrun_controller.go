package agentmodel

import (
	"context"
	"encoding/json"
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

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// memoryFSRetriever resolves a MemoryRetriever by name in the same namespace as
// the run and returns its filesystem mount input for AttachMemoryFS. It returns
// (zero, false, nil) when no retriever ref is set. It returns (zero, false, err)
// on a hard API error, and (zero, false, nil) for NotFound (treated as Pending).
func memoryFSRetriever(
	ctx context.Context,
	c client.Reader,
	run *amv1.AgentRun,
) (builders.MemoryMountInput, bool, error) {
	ref := run.Spec.MemoryRetrieverRef
	if ref == "" {
		return builders.MemoryMountInput{}, false, nil
	}

	retriever := &amv1.MemoryRetriever{}
	key := types.NamespacedName{Namespace: run.Namespace, Name: ref}
	if err := c.Get(ctx, key, retriever); err != nil {
		if apierrors.IsNotFound(err) {
			// Recoverable: the retriever may not exist yet; caller marks Pending.
			return builders.MemoryMountInput{}, false, nil
		}
		return builders.MemoryMountInput{}, false, fmt.Errorf("get MemoryRetriever %q: %w", key, err)
	}

	// Only filesystem retrievers with mounting enabled produce a volume.
	if retriever.Spec.Mount == nil || !retriever.Spec.Mount.Enabled {
		return builders.MemoryMountInput{}, false, nil
	}

	// Resolve the first filesystem MemoryStore to get its AgentFSSpec.
	var agentFS *pure.AgentFSSpec
	for _, storeName := range retriever.Spec.Stores {
		store := &amv1.MemoryStore{}
		sk := types.NamespacedName{Namespace: run.Namespace, Name: storeName}
		if err := c.Get(ctx, sk, store); err != nil {
			continue // tolerate missing stores; skip non-filesystem ones
		}
		if store.Spec.Kind == pure.MemoryStoreFilesystem && store.Spec.AgentFS != nil {
			agentFS = store.Spec.AgentFS
			break
		}
	}
	if agentFS == nil {
		// Retriever references no filesystem store — nothing to mount.
		return builders.MemoryMountInput{}, false, nil
	}

	input := builders.MemoryMountInput{
		AgentFS: agentFS,
		Mount:   retriever.Spec.Mount,
	}
	return input, input.MountEnabled(), nil
}

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
		// Render the spec the run pod executes (`agent run` reads it) before the
		// pod, so the mounted ConfigMap exists when the pod schedules.
		if err := r.ensureRunSpec(ctx, run, agent); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure run spec: %w", err)
		}

		desired := builders.BuildAgentRunPod(run, agent)

		// Attach filesystem MemoryRetriever mount if referenced (R-MEM-FS-2).
		mountInput, mountEnabled, mountErr := memoryFSRetriever(ctx, r.Client, run)
		if mountErr != nil {
			return ctrl.Result{}, fmt.Errorf("resolve memory retriever: %w", mountErr)
		}
		if mountEnabled {
			builders.AttachMemoryFS(desired, mountInput)
			logger.Info("attached memory AgentFS volume", "retriever", run.Spec.MemoryRetrieverRef)
		} else if run.Spec.MemoryRetrieverRef != "" && !mountEnabled {
			// Retriever referenced but not yet resolvable — stay Pending.
			r.markPending(run, "MemoryRetrieverPending",
				fmt.Sprintf("MemoryRetriever %q not ready for mounting", run.Spec.MemoryRetrieverRef))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, r.Status().Update(ctx, run)
		}

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
		r.foldRunResult(run, pod)
	case corev1.PodFailed:
		r.markTerminal(run, pure.PhaseFailed, terminationReason(pod))
		r.foldRunResult(run, pod)
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

// ensureRunSpec creates (once) the ConfigMap the run pod mounts, carrying the
// Agent + AgentRunSpec the `agent run` entrypoint executes. Owned by the run so
// it is garbage-collected with it. The spec is immutable per run, so an existing
// ConfigMap is left as-is.
func (r *AgentRunReconciler) ensureRunSpec(ctx context.Context, run *amv1.AgentRun, agent *amv1.Agent) error {
	cm, err := builders.BuildRunSpecConfigMap(run, agent)
	if err != nil {
		return err
	}
	if err := ctrl.SetControllerReference(run, cm, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.ConfigMap{}
	getErr := r.Get(ctx, types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}, existing)
	if apierrors.IsNotFound(getErr) {
		return r.Create(ctx, cm)
	}
	return getErr
}

// foldRunResult parses the run container's termination message (the RunResult
// the `agent run` entrypoint emits) into AgentRun.Status: output, usage, and —
// when the runtime reports a more specific phase than the Pod (e.g. Expired on
// a budget cap, which still exits 0) — the phase itself.
func (r *AgentRunReconciler) foldRunResult(run *amv1.AgentRun, pod *corev1.Pod) {
	rr, ok := runResultFromPod(pod)
	if !ok {
		return
	}
	run.Status.Output = rr.Output
	run.Status.Usage = rr.Usage
	if rr.Error != "" && run.Status.TerminationReason == "" {
		run.Status.TerminationReason = rr.Error
	}
	if rr.Phase != "" {
		run.Status.State = rr.Phase
	}
}

// runResultFromPod reads the RunResult JSON the run container wrote to its
// termination message. Matches the execution container (agent|harness), not the
// AgentFS / memory / secret-proxy sidecars.
func runResultFromPod(pod *corev1.Pod) (agentruntime.RunResult, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "agent" && cs.Name != "harness" {
			continue
		}
		if cs.State.Terminated == nil || cs.State.Terminated.Message == "" {
			continue
		}
		var rr agentruntime.RunResult
		if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &rr); err == nil {
			return rr, true
		}
	}
	return agentruntime.RunResult{}, false
}
