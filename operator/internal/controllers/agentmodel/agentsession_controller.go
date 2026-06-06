package agentmodel

import (
	"context"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	"github.com/smol-platform/smol-agents/operator/internal/controllers/features"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
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

	// NATSURL, when set, routes a session's turns through NATS (the gateway
	// path) by injecting AGENTSESSION_NATS_URL/_KEY into the worker; empty
	// leaves the worker on its on-disk inbox.
	NATSURL string
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
		WithOptions(controller.Options{MaxConcurrentReconciles: mc}).
		Complete(r)
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
		return ctrl.Result{}, fmt.Errorf("resolve bound networks: %w", err)
	}
	// Surface the egress posture for observability (M1.19).
	session.Status.Networks = netPlan.Networks
	session.Status.EgressEnforcement = egressEnforcementLabel(netPlan)
	np := builders.BuildAgentSessionEgressPolicyWithPlan(synthetic.Name, session.Namespace,
		map[string]string{"agents.smol-agents.ai/run": synthetic.Name}, netPlan)
	// M1.18: allow the kube-apiserver endpoints (blocked by the default floor on
	// public-IP clusters) so a session worker can reach the apiserver.
	if rule := apiserverEgressRule(ctx, r.Client); rule != nil {
		np.Spec.Egress = append(np.Spec.Egress, *rule)
	}
	if err := r.ensureOwned(ctx, session, np); err != nil {
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
	cmd := []string{"/agent", "serve-session", "--dir=" + builders.RunSpecMountPath, "--agent-ref=" + session.Spec.AgentRef}
	if session.Spec.IdleTimeoutSeconds > 0 {
		cmd = append(cmd, fmt.Sprintf("--idle-timeout=%ds", session.Spec.IdleTimeoutSeconds))
	}
	// Turn-scaling knobs (M2.18). Accessors default-preserve serial behavior, so
	// only render when the operator actually opted into concurrency / a bound.
	if n := session.Spec.ConcurrentTurns(); n > 1 {
		cmd = append(cmd, fmt.Sprintf("--max-concurrent-turns=%d", n))
	}
	if h := session.Spec.HistoryLimit(); h > 0 {
		cmd = append(cmd, fmt.Sprintf("--history-limit=%d", h))
	}
	pod.Spec.Containers[0].Command = cmd
	// NATS turn transport (gateway path); without it the worker uses its on-disk
	// inbox. AGENTSESSION_KEY mirrors sessionqueue.SessionKey(ns, name).
	if r.NATSURL != "" {
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "AGENTSESSION_NATS_URL", Value: r.NATSURL},
			corev1.EnvVar{Name: "AGENTSESSION_KEY", Value: sessionqueue.SessionKey(session.Namespace, session.Name)},
		)
	}
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways // required for a Deployment template

	deploy := sessionDeployment(session, synthetic.Name, pod)
	if err := r.ensureDeployment(ctx, session, deploy); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure session deployment: %w", err)
	}

	phase := pure.PhasePending
	reason, message := "WorkerStarting", "session worker Deployment not yet Available"
	if r.deployAvailable(ctx, deploy.Namespace, deploy.Name) {
		phase, reason, message = pure.PhaseRunning, "Reconciled", ""
	}
	logger.Info("reconciled session", "phase", phase)
	// Requeue while not yet available so phase advances even without a Deployment
	// status event reaching us.
	requeue := time.Duration(0)
	if phase == pure.PhasePending {
		requeue = 15 * time.Second
	}
	return r.writeStatus(ctx, session, phase, reason, message, requeue)
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
