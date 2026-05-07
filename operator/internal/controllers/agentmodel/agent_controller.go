// Package agentmodel hosts the controllers for the runtime.agents.stigen.ai
// CR family: Agent, Tool, ModelProvider, AgentRun, AgentSession, AgentPolicy.
package agentmodel

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	amv1 "github.com/stigen/knative-agents/operator/api/agentmodel/v1"
	pure "github.com/stigen/knative-agents/pkg/agentmodel/v1"
)

// AgentReconciler validates an Agent CR's references (ModelProvider,
// Tools), enforces the budget, and reports Status.Phase. It does NOT
// produce any owned objects — Agent is a passive declaration; the
// running Pod is produced from an AgentRun by the Run reconciler.
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager wires the controller.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&amv1.Agent{}).
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

	// Resolve ModelProvider.
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
	agent.Status.ResolvedProvider = provider.Name
	r.setStatus(agent, "Ready", "Reconciled", "")
	logger.Info("agent ready", "tools", len(resolved), "provider", provider.Name)
	return ctrl.Result{}, r.Status().Update(ctx, agent)
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
