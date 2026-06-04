package agentmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	"github.com/smol-platform/smol-agents/operator/internal/controllers/features"
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

	// DefaultRunRuntimeClass is the sandbox RuntimeClass applied to run pods when
	// the Agent does not override it. Empty falls back to kata-fc. Set from
	// --default-run-runtime-class.
	DefaultRunRuntimeClass string
	// AllowHostRuntime permits runc (shared host kernel) on run pods; otherwise
	// a runc selection is a fail-closed R-SBX-1 policy violation. Set from
	// --allow-host-runtime (dev/CI clusters without a sandbox runtime).
	AllowHostRuntime bool
	// MaxConcurrentReconciles bounds parallel run reconciles (default 1).
	MaxConcurrentReconciles int
	// RunDeadlineMultiplier scales the run's wall-clock budget into the pod's
	// ActiveDeadlineSeconds hard backstop (0 → 1.5). Set from
	// --run-deadline-multiplier.
	RunDeadlineMultiplier float64
	// DefaultApprovalTimeout expires an un-decided pre-run approval when the
	// Agent's ApprovalPolicy sets no timeout (0 → 1h). Set from
	// --default-approval-timeout.
	DefaultApprovalTimeout time.Duration
}

// SetupWithManager wires the controller; Owns(Pod) so we react to Pod
// status changes immediately. Owns the egress NetworkPolicy so it is GC'd
// with the run.
func (r *AgentRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mc := r.MaxConcurrentReconciles
	if mc < 1 {
		mc = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&amv1.AgentRun{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Pod{}).
		Owns(&networkingv1.NetworkPolicy{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: mc}).
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
			return r.updateRunStatus(ctx, run, ctrl.Result{})
		}
		return ctrl.Result{}, err
	}

	// Cancellation: if spec.cancel is set and we're not yet terminal,
	// stamp Cancelled and (best-effort) delete the Pod.
	if run.Spec.Cancel && !run.Status.State.Terminal() {
		_ = r.deletePod(ctx, run)
		r.markTerminal(run, pure.PhaseCancelled, "cancel:requested")
		return r.updateRunStatus(ctx, run, ctrl.Result{})
	}

	// Ensure the Pod exists.
	pod := &corev1.Pod{}
	err = r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, pod)
	if apierrors.IsNotFound(err) {
		// Pre-run approval gate (M5): hold the run in RequiresAction until a
		// human approves via spec.decision, before any pod or cost exists. The
		// cancel check above still wins. handled ⇒ status is set, return now.
		if handled, gres, gerr := r.preRunApproval(ctx, run, agent); handled {
			return gres, gerr
		}

		// Resolve run-pod isolation fail-closed before any prep work: a policy
		// violation (runc without operator opt-in) fails the run; a not-yet-
		// registered hardened RuntimeClass holds it Pending rather than
		// scheduling an unisolated pod (R-SBX-1 on the run datapath).
		sbClass, sbPending, sbFailed := r.resolveRunSandbox(ctx, agent)
		if sbFailed != "" {
			r.markTerminal(run, pure.PhaseFailed, "sandbox:"+sbFailed)
			return r.updateRunStatus(ctx, run, ctrl.Result{})
		}
		if sbPending != "" {
			r.markPending(run, "SandboxNotReady", sbPending)
			return r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: 15 * time.Second})
		}

		// Resolve node placement for the sandbox class. A KVM class with no
		// matching AgentNodePool holds the run Pending (fail-closed) rather than
		// scheduling a kata pod that can never run — unless PlacementFallback is
		// "Schedule" (a dev/unlabelled-cluster escape hatch). R-PROV-2 / D3.
		placement, _, plErr := features.ResolvePlacementForClass(ctx, r.Client, sbClass)
		if plErr != nil {
			return ctrl.Result{}, fmt.Errorf("resolve placement: %w", plErr)
		}
		if placement == nil && builders.RequiresKVM(sbClass) && run.Spec.PlacementFallback != "Schedule" {
			r.markPending(run, "NoKVMCapacity",
				fmt.Sprintf("no AgentNodePool provides isolation %q", sbClass))
			return r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: 30 * time.Second})
		}

		// Resolve the loop-mode ModelProvider + gather every secret the broker
		// must serve (harness env secretRef + the provider API key). A missing
		// source Secret / ModelProvider keeps the run Pending and retries.
		provider, brokerValues, prepErr := r.prepareRun(ctx, run, agent)
		if prepErr != nil {
			r.markPending(run, "RunPrepPending", prepErr.Error())
			return r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: 10 * time.Second})
		}

		// Render the spec the run pod executes (`agent run` reads it) before the
		// pod, so the mounted ConfigMap exists when the pod schedules.
		if err := r.ensureRunSpec(ctx, run, agent, provider); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure run spec: %w", err)
		}

		// Generate the per-run broker config (resolved values + a uid-keyed
		// policy) before the pod mounts it.
		if len(brokerValues) > 0 {
			if err := r.ensureBrokerConfig(ctx, run, brokerValues); err != nil {
				return ctrl.Result{}, fmt.Errorf("ensure broker config: %w", err)
			}
		}

		desired := builders.BuildAgentRunPod(run, agent)
		// Pin the resolved sandbox RuntimeClass so the run executes under a real
		// isolation boundary (default kata-fc), not the cluster-default runtime.
		builders.ApplyRunSandbox(desired, sbClass)

		// Bind the pod to its kata node pool (no-op for runc / no pool) and set a
		// hard ActiveDeadlineSeconds backstop from the effective wall-clock budget
		// (BudgetOverride wins over the Agent's budget).
		builders.ApplyRunPodPlacement(desired, placement)
		effWall := agent.Spec.Budget.MaxWallClockSeconds
		if run.Spec.BudgetOverride != nil && run.Spec.BudgetOverride.MaxWallClockSeconds > 0 {
			effWall = run.Spec.BudgetOverride.MaxWallClockSeconds
		}
		builders.ApplyRunDeadline(desired, effWall, r.RunDeadlineMultiplier)

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
			return r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: 10 * time.Second})
		}

		// Attach the in-pod secret broker when there are secrets to serve
		// (harness env secretRef and/or the loop provider key).
		if len(brokerValues) > 0 {
			builders.AttachSecretBroker(desired, run.Name)
		}

		// Cage egress: a default-deny NetworkPolicy (blocks instance-metadata /
		// arbitrary outbound) must exist before the pod schedules.
		if err := r.ensureRunEgressPolicy(ctx, run); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure egress policy: %w", err)
		}

		if err := ctrl.SetControllerReference(run, desired, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set controller ref: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, fmt.Errorf("create pod: %w", err)
		}
		r.markRunning(run)
		logger.Info("created run pod", "agent", agent.Name)
		return r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: 5 * time.Second})
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
		r.foldRunResult(ctx, run, pod)
	case corev1.PodFailed:
		r.markTerminal(run, pure.PhaseFailed, terminationReason(pod))
		r.foldRunResult(ctx, run, pod)
	}

	// poll Pod state every 5s until terminal
	res := ctrl.Result{}
	if !run.Status.State.Terminal() {
		res = ctrl.Result{RequeueAfter: 5 * time.Second}
	}
	return r.updateRunStatus(ctx, run, res)
}

// updateRunStatus persists run.Status and maps the outcome onto the (Result,
// error) the reconcile returns. An optimistic-lock conflict is benign and
// expected: the cached AgentRun can lag a just-written status (e.g. the Pod
// watch reconcile racing the periodic 5s poll), so a conflict means the object
// already advanced — requeue and let the next reconcile read fresh, rather than
// surfacing a noisy "Reconciler error". onSuccess is returned verbatim when the
// write lands.
func (r *AgentRunReconciler) updateRunStatus(ctx context.Context, run *amv1.AgentRun, onSuccess ctrl.Result) (ctrl.Result, error) {
	if err := r.Status().Update(ctx, run); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return onSuccess, nil
}

func (r *AgentRunReconciler) markPending(run *amv1.AgentRun, reason, msg string) {
	run.Status.State = pure.PhasePending
	run.Status.TerminationReason = msg
	run.Status.StartedAt = nil
}

func (r *AgentRunReconciler) markRequiresAction(run *amv1.AgentRun, pa *pure.PendingAction) {
	run.Status.State = pure.PhaseRequiresAction
	run.Status.PendingAction = pa
	run.Status.TerminationReason = "" // non-terminal; clear any stale Pending hint
}

// approvalTimeout resolves the effective pre-run approval TTL: the Agent's
// override, else the operator default, else 1h.
func (r *AgentRunReconciler) approvalTimeout(agent *amv1.Agent) time.Duration {
	if agent.Spec.Approval != nil && agent.Spec.Approval.ApprovalTimeoutSeconds > 0 {
		return time.Duration(agent.Spec.Approval.ApprovalTimeoutSeconds) * time.Second
	}
	if r.DefaultApprovalTimeout > 0 {
		return r.DefaultApprovalTimeout
	}
	return time.Hour
}

// preRunApproval implements the M5 pre-run approval gate. It returns handled=true
// (with the result/err to return) when the run is waiting for, denied by, or has
// expired an approval — NO pod is created in those cases. handled=false means the
// run is ungated or approved and reconcile should proceed to create the pod.
func (r *AgentRunReconciler) preRunApproval(ctx context.Context, run *amv1.AgentRun, agent *amv1.Agent) (handled bool, res ctrl.Result, err error) {
	gated := agent.Spec.Approval != nil && agent.Spec.Approval.RequireApprovalBeforeRun
	if run.Spec.RequireApprovalBeforeRun != nil {
		gated = *run.Spec.RequireApprovalBeforeRun // per-run override
	}
	if !gated {
		return false, ctrl.Result{}, nil
	}

	// A decision only counts when its token matches the pending one (or none has
	// been minted yet) — a stale/mismatched token is ignored, keeping the run
	// parked rather than acting on an approval for a different state.
	tokenMatches := run.Status.PendingAction == nil ||
		(run.Spec.Decision != nil && run.Spec.Decision.Token == run.Status.PendingAction.Token)

	if d := run.Spec.Decision; d != nil && tokenMatches {
		if d.Approve {
			run.Status.PendingAction = nil // clear the gate; pod-create proceeds
			return false, ctrl.Result{}, nil
		}
		r.markTerminal(run, pure.PhaseCancelled, "decision:denied:"+d.Reason)
		out, e := r.updateRunStatus(ctx, run, ctrl.Result{})
		return true, out, e
	}

	// No matching decision yet — mint the pending token on first entry.
	if run.Status.PendingAction == nil {
		token := string(run.UID)
		if token == "" {
			token = run.Name
		}
		r.markRequiresAction(run, &pure.PendingAction{
			Kind:        "pre-run",
			Token:       token,
			RequestedAt: metav1.Now(),
			Reason:      "awaiting human approval before run",
		})
		out, e := r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: r.approvalTimeout(agent)})
		return true, out, e
	}

	// Already pending — expire on TTL.
	if !run.Status.PendingAction.RequestedAt.IsZero() &&
		time.Since(run.Status.PendingAction.RequestedAt.Time) >= r.approvalTimeout(agent) {
		r.markTerminal(run, pure.PhaseExpired, "approval:timeout")
		out, e := r.updateRunStatus(ctx, run, ctrl.Result{})
		return true, out, e
	}

	// Still waiting (incl. a stale/mismatched decision token) — requeue near TTL.
	out, e := r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: 30 * time.Second})
	return true, out, e
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
	// Terminal state owns TerminationReason — overwrite any stale Pending/
	// Running hint (e.g. "Pod is Pending" left by an earlier markPending).
	// foldRunResult may refine this further with the runtime's own reason.
	run.Status.TerminationReason = reason
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

// prepareRun resolves the loop-mode ModelProvider (rendered into the run spec)
// and gathers every secret the broker must serve: each harness env secretRef
// value plus the provider API key. Values are keyed by lease name (the
// SecretRef.SecretName the runtime leases by). Harness mode returns a nil
// provider; agents with no secrets return an empty map.
func (r *AgentRunReconciler) prepareRun(ctx context.Context, run *amv1.AgentRun, agent *amv1.Agent) (*builders.RunProvider, map[string][]byte, error) {
	return gatherRunSecrets(ctx, r.Client, agent, run.Namespace, run.Spec.Inputs)
}

// resolveRunSandbox picks the run pod's RuntimeClass, fail-closed. At most one
// of (class, pending, failed) is non-empty: failed is a hard R-SBX-1 violation
// (runc without operator opt-in); pending means the chosen hardened RuntimeClass
// is not registered yet, so we refuse to schedule an unisolated pod and wait. An
// empty Agent override falls back to --default-run-runtime-class, then kata-fc.
func (r *AgentRunReconciler) resolveRunSandbox(ctx context.Context, agent *amv1.Agent) (class, pending, failed string) {
	return resolveSandbox(ctx, r.Client, agent.Spec.Sandbox.RuntimeClass, r.DefaultRunRuntimeClass, r.AllowHostRuntime)
}

// ensureRunEgressPolicy creates the run pod's default-deny egress NetworkPolicy
// (idempotent), owned by the run so it is GC'd with it.
func (r *AgentRunReconciler) ensureRunEgressPolicy(ctx context.Context, run *amv1.AgentRun) error {
	np := builders.BuildAgentRunEgressPolicy(run)
	if err := ctrl.SetControllerReference(run, np, r.Scheme); err != nil {
		return err
	}
	var existing networkingv1.NetworkPolicy
	err := r.Get(ctx, types.NamespacedName{Namespace: np.Namespace, Name: np.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, np)
	}
	return err
}

// readSecret fetches one key from a k8s Secret (the sole key when key is empty).
func (r *AgentRunReconciler) readSecret(ctx context.Context, ns, name, key string) ([]byte, error) {
	return readSecretKey(ctx, r.Client, ns, name, key)
}

// ensureBrokerConfig creates (once) the run-owned Secret holding the
// secret-proxy config (static backend + policy) the broker sidecar reads.
func (r *AgentRunReconciler) ensureBrokerConfig(ctx context.Context, run *amv1.AgentRun, values map[string][]byte) error {
	sec, err := builders.BuildBrokerConfigSecret(run, values)
	if err != nil {
		return err
	}
	if err := ctrl.SetControllerReference(run, sec, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.Secret{}
	getErr := r.Get(ctx, types.NamespacedName{Namespace: sec.Namespace, Name: sec.Name}, existing)
	if apierrors.IsNotFound(getErr) {
		return r.Create(ctx, sec)
	}
	return getErr
}

// ensureRunSpec creates (once) the ConfigMap the run pod mounts, carrying the
// Agent + AgentRunSpec the `agent run` entrypoint executes. Owned by the run so
// it is garbage-collected with it. The spec is immutable per run, so an existing
// ConfigMap is left as-is.
func (r *AgentRunReconciler) ensureRunSpec(ctx context.Context, run *amv1.AgentRun, agent *amv1.Agent, provider *builders.RunProvider) error {
	cm, err := builders.BuildRunSpecConfigMap(run, agent, provider)
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
// a budget cap, which still exits 0) — the phase itself. The runtime's own
// reason (e.g. "budget:tokens") is the most specific signal we have and wins
// over any pod-level reason markTerminal set; a runtime error wins outright.
func (r *AgentRunReconciler) foldRunResult(ctx context.Context, run *amv1.AgentRun, pod *corev1.Pod) {
	rr, ok := runResultFromPod(pod)
	if !ok {
		return
	}
	// Apply any namespace RedactionPolicy to the cluster-facing record. This is
	// a disclosure control on Status only — the harness already observed the
	// raw data, so it is never containment (agentpolicy R1). TerminationReason
	// is a controlled signal and stays unredacted.
	pats := compileNamespaceRedaction(ctx, r.Client, run.Namespace)
	run.Status.Output = pure.RedactJSON(rr.Output, pats)
	run.Status.Steps = pure.RedactSteps(rr.Steps, pats)
	run.Status.Usage = rr.Usage
	switch {
	case rr.Error != "":
		run.Status.TerminationReason = rr.Error
	case rr.TerminationReason != "":
		run.Status.TerminationReason = rr.TerminationReason
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
