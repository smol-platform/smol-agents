package agentmodel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	"github.com/smol-platform/smol-agents/operator/internal/controllers/features"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
	"github.com/smol-platform/smol-agents/pkg/turnmodel"
)

const sessionSuffix = "-session"

// AgentSessionReconciler turns an AgentSession into a long-running, durable
// session worker: a 1-replica Deployment running `agent serve-session` over the
// agent's AgentFS workspace. It survives restarts with resume (a fresh pod's
// AgentFS init container restores the kopia-checkpointed state, and the worker
// reloads its turn log) and carries the same containment as a run pod (sandbox
// RuntimeClass + egress cage + secret broker). Turn delivery is Phase 4
// (gateway/NATS writes the worker's inbox); this stands up the durable session.
type AgentSessionReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	DefaultRunRuntimeClass string
	AllowHostRuntime       bool
	// CNIEnforcesNetworkPolicy declares the cluster CNI enforces NetworkPolicy;
	// default false reports the session egress floor as "unenforced" (rv1.2).
	CNIEnforcesNetworkPolicy bool

	// NATSURL, when set, routes a session's turns through NATS (the gateway
	// path) by injecting AGENTSESSION_NATS_URL/_KEY into the worker; empty
	// leaves the worker on its on-disk inbox.
	NATSURL string
	// NATSAccountSeed, when set, is the operator's NATS account signing seed used
	// to mint per-namespace worker credentials (M2.20). Empty leaves workers
	// connecting unauthenticated (today's behavior — no per-tenant ACL).
	NATSAccountSeed []byte
	// MaxConcurrentReconciles bounds parallel session reconciles (default 1).
	MaxConcurrentReconciles int
}

func (r *AgentSessionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mc := r.MaxConcurrentReconciles
	if mc < 1 {
		mc = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&amv1.AgentSession{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&networkingv1.NetworkPolicy{}).
		// Re-reconcile bound sessions when their AgentNetwork changes (M1.16): a
		// resident worker's egress cage is updated in place, so a tightened (or
		// loosened) network applies to the live session.
		Watches(&amv1.AgentNetwork{}, handler.EnqueueRequestsFromMapFunc(r.sessionsForNetwork)).
		WithOptions(controller.Options{MaxConcurrentReconciles: mc}).
		Complete(r)
}

// sessionsForNetwork maps an AgentNetwork change to reconcile requests for the
// AgentSessions bound to it (their Agent matches the network's selector). M1.16.
func (r *AgentSessionReconciler) sessionsForNetwork(ctx context.Context, obj client.Object) []reconcile.Request {
	an, ok := obj.(*amv1.AgentNetwork)
	if !ok {
		return nil
	}
	agents := agentsBoundToNetwork(ctx, r.Client, an)
	if len(agents) == 0 {
		return nil
	}
	var sessions amv1.AgentSessionList
	if err := r.List(ctx, &sessions, client.InNamespace(an.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range sessions.Items {
		s := &sessions.Items[i]
		if !agents[s.Spec.AgentRef] {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: s.Namespace, Name: s.Name}})
	}
	return reqs
}

func (r *AgentSessionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agentsession", req.NamespacedName)

	session := &amv1.AgentSession{}
	if err := r.Get(ctx, req.NamespacedName, session); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	agent := &amv1.Agent{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: session.Namespace, Name: session.Spec.AgentRef}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return r.writeStatus(ctx, session, pure.PhasePending, "AgentMissing",
				fmt.Sprintf("agentRef %q not found", session.Spec.AgentRef), 15*time.Second)
		}
		return ctrl.Result{}, err
	}

	// Fail-closed sandbox resolution (same policy as runs).
	sbClass, sbPending, sbFailed := resolveSandbox(ctx, r.Client, agent.Spec.Sandbox.RuntimeClass, r.DefaultRunRuntimeClass, r.AllowHostRuntime)
	if sbFailed != "" {
		return r.writeStatus(ctx, session, pure.PhaseFailed, "SandboxFailed", sbFailed, 0)
	}
	if sbPending != "" {
		return r.writeStatus(ctx, session, pure.PhasePending, "SandboxNotReady", sbPending, 15*time.Second)
	}
	// D3 (M3.15): refuse danger permission/sandbox flags unless the resolved
	// class is a kata microVM — the same fail-closed gate as the run datapath.
	if v := dangerFlagViolation(agent, sbClass); v != "" {
		return r.writeStatus(ctx, session, pure.PhaseFailed, "DangerFlagsRefused", v, 0)
	}

	// A synthetic AgentRun (name+namespace carrier) drives the shared run-pod /
	// run-spec / broker builders; ownership is the SESSION's, set via ensureOwned.
	synthetic := &amv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: session.Name + sessionSuffix, Namespace: session.Namespace},
		Spec:       pure.AgentRunSpec{AgentRef: session.Spec.AgentRef},
	}

	provider, brokerValues, err := gatherRunSecrets(ctx, r.Client, agent, session.Namespace, nil)
	if err != nil {
		return r.writeStatus(ctx, session, pure.PhasePending, "SecretMissing", err.Error(), 10*time.Second)
	}

	// Run-spec ConfigMap (agent.json + provider.json) the worker reads.
	cm, err := builders.BuildRunSpecConfigMap(synthetic, agent, provider)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureOwned(ctx, session, cm); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure session spec: %w", err)
	}

	// Broker config secret (only when there are secrets to serve).
	if len(brokerValues) > 0 {
		sec, err := builders.BuildBrokerConfigSecret(synthetic, brokerValues)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.ensureOwned(ctx, session, sec); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure session broker config: %w", err)
		}
	}

	// Egress cage selecting the worker pods, with any bound AgentNetwork
	// allow-list layered on (M1.16) — empty plan = the default-deny floor.
	netPlan, err := resolveBoundNetworks(ctx, r.Client, agent)
	if err != nil {
		// A NetworkPlan compose conflict is a spec-level error — hold the session
		// Pending (fail-closed, visible) until the bound AgentNetworks are fixed
		// (which re-reconciles it via the Watch).
		if errors.Is(err, ErrNetworkConflict) {
			return r.writeStatus(ctx, session, pure.PhasePending, "NetworkConflict", err.Error(), 30*time.Second)
		}
		return ctrl.Result{}, fmt.Errorf("resolve bound networks: %w", err)
	}
	// Surface the egress posture for observability (M1.19).
	session.Status.Networks = netPlan.Networks
	session.Status.EgressEnforcement = egressEnforcementLabel(netPlan, r.CNIEnforcesNetworkPolicy)
	np := builders.BuildAgentSessionEgressPolicyWithPlan(synthetic.Name, session.Namespace,
		map[string]string{"agents.smol-agents.ai/run": synthetic.Name}, netPlan)
	// M1.18: allow the kube-apiserver endpoints (blocked by the default floor on
	// public-IP clusters) so a session worker can reach the apiserver.
	if rule := apiserverEgressRule(ctx, r.Client); rule != nil {
		np.Spec.Egress = append(np.Spec.Egress, *rule)
	}
	// Update-in-place (not create-only) so a changed AgentNetwork re-cages the
	// live worker (M1.16).
	if err := r.ensureEgressPolicy(ctx, session, np); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure session egress: %w", err)
	}

	// Build the run pod (security + AgentFS + run-spec volume), then turn it into
	// a long-running session worker: serve-session command, microVM class, broker.
	pod := builders.BuildAgentRunPod(synthetic, agent)
	builders.ApplyRunSandbox(pod, sbClass)
	// Bind the resident session worker to its kata node pool (M1.11). A KVM class
	// with no matching AgentNodePool holds the session Pending (fail-closed)
	// rather than scheduling a pod that can never run. No deadline — the idle
	// timeout, not a wall-clock budget, bounds a session worker.
	placement, _, plErr := features.ResolvePlacementForClass(ctx, r.Client, sbClass)
	if plErr != nil {
		return ctrl.Result{}, fmt.Errorf("resolve placement: %w", plErr)
	}
	if placement == nil && builders.RequiresKVM(sbClass) {
		return r.writeStatus(ctx, session, pure.PhasePending, "NoKVMCapacity",
			fmt.Sprintf("no AgentNodePool matches kata class %q", sbClass), 30*time.Second)
	}
	builders.ApplyRunPodPlacement(pod, placement)
	if len(brokerValues) > 0 {
		builders.AttachSecretBroker(pod, synthetic.Name)
	}
	pod.Spec.Containers[0].Command = sessionWorkerCommand(session)
	// Right-size the resident worker (M1.11). A session has no wall-clock deadline,
	// so worker sizing is expressed here rather than via a run budget.
	builders.ApplySessionResources(&pod.Spec.Containers[0], session.Spec.Resources)
	// Tier-2 datapath seam (no-op in Phase 1; Tier-1 is the egress NetworkPolicy).
	builders.AttachAgentNetwork(pod, netPlan)
	// NATS turn transport (gateway path); without it the worker uses its on-disk
	// inbox. AGENTSESSION_KEY mirrors sessionqueue.SessionKey(ns, name).
	if r.NATSURL != "" {
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "AGENTSESSION_NATS_URL", Value: r.NATSURL},
			corev1.EnvVar{Name: "AGENTSESSION_KEY", Value: sessionqueue.SessionKey(session.Namespace, session.Name)},
		)
		// Per-namespace NATS credential (M2.20): with an account seed configured,
		// authenticate the worker with its namespace-scoped creds so a compromised
		// worker can only touch its own tenant's turn subjects. Off (unauthed) when
		// no seed is set — today's behavior.
		if len(r.NATSAccountSeed) > 0 {
			credsSecret, cerr := r.ensureWorkerCreds(ctx, session.Namespace)
			if cerr != nil {
				return r.writeStatus(ctx, session, pure.PhasePending, "NATSCredsPending", cerr.Error(), 15*time.Second)
			}
			attachNATSCreds(pod, credsSecret)
		}
	}
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways // required for a Deployment template

	deploy := sessionDeployment(session, synthetic.Name, pod)
	if err := r.ensureDeployment(ctx, session, deploy); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure session deployment: %w", err)
	}

	phase := pure.PhasePending
	reason, message := "WorkerStarting", "session worker Deployment not yet Available"
	requeue := 15 * time.Second // Pending: poll for the worker to become Available
	if r.deployAvailable(ctx, deploy.Namespace, deploy.Name) {
		phase, reason, message = pure.PhaseRunning, "Reconciled", ""
		// M2.19: mirror the worker's live usage/turn counters into status
		// (best-effort), and keep refreshing them on a ~30s cadence.
		r.mirrorWorkerStatus(ctx, session, deploy.Name)
		requeue = 30 * time.Second
	}
	logger.Info("reconciled session", "phase", phase)
	return r.writeStatus(ctx, session, phase, reason, message, requeue)
}

// mirrorWorkerStatus scrapes a Running worker pod's /status endpoint and folds the
// SessionSummary into status (usage/turns/failedTurns/lastTurnTime). Best-effort:
// no reachable pod / a scrape error leaves the prior status untouched (M2.19).
func (r *AgentSessionReconciler) mirrorWorkerStatus(ctx context.Context, session *amv1.AgentSession, deployName string) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(session.Namespace),
		client.MatchingLabels{"agents.smol-agents.ai/run": deployName}); err != nil {
		return
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			continue
		}
		if sum, err := fetchSessionSummary(ctx, p.Status.PodIP); err == nil {
			applySummaryToStatus(&session.Status, sum)
			return
		}
	}
}

// applySummaryToStatus folds a worker SessionSummary into AgentSessionStatus
// field-wise (Usage is verbatim CumulativeUsage — NOT Usage.Add). Pure + tested.
func applySummaryToStatus(st *pure.AgentSessionStatus, sum turnmodel.SessionSummary) {
	st.Usage = sum.Usage
	st.Turns = int64(sum.Turns)
	st.FailedTurns = int64(sum.FailedTurns)
	if sum.LastTurnTime != nil {
		t := metav1.NewTime(*sum.LastTurnTime)
		st.LastTurnTime = &t
	}
}

// fetchSessionSummary GETs the worker's /status endpoint (a short-timeout,
// in-cluster read of the session's own non-secret counters).
func fetchSessionSummary(ctx context.Context, podIP string) (turnmodel.SessionSummary, error) {
	url := "http://" + podIP + turnmodel.SessionStatusPort + "/status"
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return turnmodel.SessionSummary{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return turnmodel.SessionSummary{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return turnmodel.SessionSummary{}, fmt.Errorf("session status: http %d", resp.StatusCode)
	}
	var sum turnmodel.SessionSummary
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&sum); err != nil {
		return turnmodel.SessionSummary{}, err
	}
	return sum, nil
}

// ensureOwned sets the session as controller-owner and creates obj if absent
// (idempotent — the run-spec/broker/egress objects are stable for a session).
func (r *AgentSessionReconciler) ensureOwned(ctx context.Context, session *amv1.AgentSession, obj client.Object) error {
	if err := ctrl.SetControllerReference(session, obj, r.Scheme); err != nil {
		return err
	}
	existing := obj.DeepCopyObject().(client.Object)
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	return err
}

// ensureEgressPolicy creates or UPDATES the session worker's egress NetworkPolicy
// (unlike ensureOwned, which is create-only for the stable run-spec/broker
// objects). Update-in-place lets a changed AgentNetwork re-cage a live session
// worker without recreating it (M1.16).
func (r *AgentSessionReconciler) ensureEgressPolicy(ctx context.Context, session *amv1.AgentSession, np *networkingv1.NetworkPolicy) error {
	if err := ctrl.SetControllerReference(session, np, r.Scheme); err != nil {
		return err
	}
	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKeyFromObject(np), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, np)
	}
	if err != nil {
		return err
	}
	existing.Spec = np.Spec
	return r.Update(ctx, existing)
}

// ensureDeployment creates or updates the session worker Deployment.
func (r *AgentSessionReconciler) ensureDeployment(ctx context.Context, session *amv1.AgentSession, deploy *appsv1.Deployment) error {
	if err := ctrl.SetControllerReference(session, deploy, r.Scheme); err != nil {
		return err
	}
	existing := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKeyFromObject(deploy), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, deploy)
	}
	if err != nil {
		return err
	}
	existing.Spec = deploy.Spec
	return r.Update(ctx, existing)
}

func (r *AgentSessionReconciler) deployAvailable(ctx context.Context, ns, name string) bool {
	cur := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, cur); err != nil {
		return false
	}
	return cur.Status.AvailableReplicas > 0
}

func (r *AgentSessionReconciler) writeStatus(ctx context.Context, session *amv1.AgentSession, phase pure.Phase, reason, message string, requeue time.Duration) (ctrl.Result, error) {
	session.Status.Phase = phase
	session.Status.Reason = reason
	session.Status.Message = message
	session.Status.ObservedGeneration = session.Generation
	if err := r.Status().Update(ctx, session); err != nil {
		// A conflict means the object advanced; requeue and re-read rather than
		// surfacing a noisy error.
		return ctrl.Result{RequeueAfter: time.Second}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// sessionDeployment wraps the worker pod into a 1-replica Deployment so the
// session survives node loss / crash (the new pod resumes from the AgentFS
// checkpoint). Phase 4 swaps this for a Knative Service for scale-to-zero.
func sessionDeployment(session *amv1.AgentSession, name string, pod *corev1.Pod) *appsv1.Deployment {
	selector := map[string]string{"agents.smol-agents.ai/run": name}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: session.Namespace, Labels: pod.Labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: pod.Labels},
				Spec:       pod.Spec,
			},
		},
	}
}

// sessionWorkerCommand renders the `agent serve-session` command line for a
// session worker, including the M2.18 turn-scaling knobs. The spec accessors
// default-preserve today's behavior (serial, unbounded history, no per-turn
// cap), so each knob is only rendered when the operator opted into it — keeping
// a default session's command identical to before.
func sessionWorkerCommand(session *amv1.AgentSession) []string {
	cmd := []string{"/agent", "serve-session", "--dir=" + builders.RunSpecMountPath, "--agent-ref=" + session.Spec.AgentRef}
	if session.Spec.IdleTimeoutSeconds > 0 {
		cmd = append(cmd, fmt.Sprintf("--idle-timeout=%ds", session.Spec.IdleTimeoutSeconds))
	}
	if n := session.Spec.ConcurrentTurns(); n > 1 {
		cmd = append(cmd, fmt.Sprintf("--max-concurrent-turns=%d", n))
	}
	if h := session.Spec.HistoryLimit(); h > 0 {
		cmd = append(cmd, fmt.Sprintf("--history-limit=%d", h))
	}
	// M2.18 per-turn deadline: the worker abandons a turn whose wall-clock
	// exceeds this (turnCtx = min(TurnTimeout, budget)). Opt-in; 0 = no cap.
	if d := session.Spec.TurnDeliveryTimeoutSeconds; d > 0 {
		cmd = append(cmd, fmt.Sprintf("--turn-timeout=%ds", d))
	}
	return cmd
}
