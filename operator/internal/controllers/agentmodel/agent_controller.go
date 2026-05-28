// Package agentmodel hosts the controllers for the runtime.agents.smol-agents.ai
// CR family: Agent, Tool, ModelProvider, AgentRun, AgentSession, AgentPolicy.
package agentmodel

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// AgentReconciler validates an Agent CR's references (ModelProvider,
// Tools), enforces the budget, and reports Status.Phase. It does NOT
// produce any owned objects — Agent is a passive declaration; the
// running Pod is produced from an AgentRun by the Run reconciler.
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager wires the controller. We Own ServiceAccount so the
// per-Agent SA (created by ensureServiceAccount) is GC'd with the Agent and
// re-created if it goes missing.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&amv1.Agent{}).
		Owns(&corev1.ServiceAccount{}).
		Complete(r)
}

// Reconcile is the per-Agent entrypoint.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agent", req.NamespacedName)

	agent := &amv1.Agent{}
	if err := r.Get(ctx, req.NamespacedName, agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := pure.ValidateAgent(toPure(agent)); err != nil {
		r.setStatus(agent, "Failed", "InvalidSpec", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, agent)
	}

	// Ensure the per-Agent ServiceAccount that AgentRun pods execute as. The
	// platform SmolAgent controller creates a similarly-named SA for its
	// long-lived workload; the runtime-only path (Agent + AgentRun without a
	// SmolAgent) needs this so pod-create doesn't fail with "service account
	// not found".
	if err := r.ensureServiceAccount(ctx, agent); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure ServiceAccount: %w", err)
	}

	// Resolve ModelProvider — only when the Agent actually has one. Harness
	// agents (mode=harness) delegate generation to a sidecar/HTTP gateway and
	// have no Model field to look up; treating "no provider" as Pending would
	// strand them in a misleading ProviderMissing state forever.
	var providerName string
	if agent.Spec.Model.ProviderRef != "" {
		provider := &amv1.ModelProvider{}
		err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: agent.Spec.Model.ProviderRef}, provider)
		if err != nil {
			if apierrors.IsNotFound(err) {
				r.setStatus(agent, "Pending", "ProviderMissing",
					fmt.Sprintf("ModelProvider %q not found", agent.Spec.Model.ProviderRef))
				return ctrl.Result{}, r.Status().Update(ctx, agent)
			}
			return ctrl.Result{}, err
		}
		providerName = provider.Name
	} else if agent.Spec.Mode != pure.ModeHarness {
		// Loop-mode agents need a Model.ProviderRef; harness agents legitimately
		// don't.
		r.setStatus(agent, "Pending", "ProviderMissing",
			"spec.model.providerRef is required for loop-mode agents")
		return ctrl.Result{}, r.Status().Update(ctx, agent)
	}

	// Resolve every referenced Tool.
	resolved := make([]string, 0, len(agent.Spec.Tools))
	for _, ref := range agent.Spec.Tools {
		ns := ref.Namespace
		if ns == "" {
			ns = agent.Namespace
		}
		tool := &amv1.Tool{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, tool); err != nil {
			if apierrors.IsNotFound(err) {
				r.setStatus(agent, "Pending", "ToolMissing",
					fmt.Sprintf("Tool %q (ns=%s) not found", ref.Name, ns))
				return ctrl.Result{}, r.Status().Update(ctx, agent)
			}
			return ctrl.Result{}, err
		}
		resolved = append(resolved, tool.Name)
	}

	agent.Status.ResolvedTools = resolved
	agent.Status.ResolvedProvider = providerName
	r.setStatus(agent, "Ready", "Reconciled", "")
	logger.Info("agent ready", "tools", len(resolved), "provider", providerName)
	return ctrl.Result{}, r.Status().Update(ctx, agent)
}

// ensureServiceAccount creates (once) the SA AgentRun pods execute as. Owned
// by the Agent so it's GC'd when the Agent is deleted; existing SAs (e.g.
// pre-existing or also-owned by a SmolAgent) are left untouched.
func (r *AgentReconciler) ensureServiceAccount(ctx context.Context, agent *amv1.Agent) error {
	sa := builders.AgentServiceAccount(agent)
	if err := ctrl.SetControllerReference(agent, sa, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.ServiceAccount{}
	getErr := r.Get(ctx, types.NamespacedName{Namespace: sa.Namespace, Name: sa.Name}, existing)
	if apierrors.IsNotFound(getErr) {
		return r.Create(ctx, sa)
	}
	return getErr
}

// toPure unwraps the K8s wrapper into the pure Agent shape so the
// existing pkg/agentmodel/v1.ValidateAgent function can run.
func toPure(a *amv1.Agent) pure.Agent {
	return pure.Agent{Spec: a.Spec, Status: a.Status}
}

func (r *AgentReconciler) setStatus(a *amv1.Agent, phase, reason, msg string) {
	a.Status.Phase = phase
	a.Status.Reason = reason
	a.Status.Message = msg
	a.Status.ObservedGeneration = a.Generation
}
