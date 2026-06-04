package webhooks

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

var (
	agentGK    = schema.GroupKind{Group: "runtime.agents.smol-agents.ai", Kind: "Agent"}
	agentRunGK = schema.GroupKind{Group: "runtime.agents.smol-agents.ai", Kind: "AgentRun"}
)

// SetupAgentPolicyGateWebhook registers validating webhooks on Agent and
// AgentRun that enforce the namespace AgentPolicy allow-lists + budget caps at
// admission (the ValidatingWebhookConfiguration runs failurePolicy: Fail, D3).
// The Agent/AgentRun reconcilers run the same checks as a reconcile-time
// backstop. It fails open only on a transient list error or an empty effective
// policy — never wrongly denies on an apiserver hiccup.
func SetupAgentPolicyGateWebhook(mgr ctrl.Manager) error {
	g := &agentPolicyGate{client: mgr.GetClient()}
	if err := ctrl.NewWebhookManagedBy(mgr, &amv1.Agent{}).WithValidator(agentGate{g}).Complete(); err != nil {
		return err
	}
	return ctrl.NewWebhookManagedBy(mgr, &amv1.AgentRun{}).WithValidator(runGate{g}).Complete()
}

type agentPolicyGate struct{ client client.Client }

// effective lists + composes the namespace policies. ok=false ⇒ fail open
// (transient list error or no constraining policy).
func (g *agentPolicyGate) effective(ctx context.Context, ns string) (eff pure.EffectivePolicy, ok bool) {
	var list amv1.AgentPolicyList
	if err := g.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return pure.EffectivePolicy{}, false
	}
	pol := make([]pure.AgentPolicy, 0, len(list.Items))
	for i := range list.Items {
		pol = append(pol, pure.AgentPolicy{Name: list.Items[i].Name, Spec: list.Items[i].Spec})
	}
	eff = pure.ComposePolicies(pol)
	return eff, !eff.Empty
}

func (g *agentPolicyGate) checkAgent(ctx context.Context, a *amv1.Agent) error {
	eff, ok := g.effective(ctx, a.Namespace)
	if !ok {
		return nil
	}
	var errs field.ErrorList
	if a.Spec.Model.ProviderRef != "" && !eff.AllowsProvider(a.Spec.Model.ProviderRef) {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "model", "providerRef"),
			a.Spec.Model.ProviderRef+" is not in the AgentPolicy allow-list"))
	}
	for i, t := range a.Spec.Tools {
		if !eff.AllowsTool(t.Name) {
			errs = append(errs, field.Forbidden(field.NewPath("spec", "tools").Index(i),
				t.Name+" is not in the AgentPolicy allow-list"))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(agentGK, a.Name, errs)
}

func (g *agentPolicyGate) checkRun(ctx context.Context, run *amv1.AgentRun) error {
	eff, ok := g.effective(ctx, run.Namespace)
	if !ok || eff.Budget == nil {
		return nil
	}
	// Effective budget = budgetOverride ?? parent Agent's budget.
	want := pure.Budget{}
	if run.Spec.BudgetOverride != nil {
		want = *run.Spec.BudgetOverride
	} else {
		var agent amv1.Agent
		if err := g.client.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.AgentRef}, &agent); err != nil {
			return nil // can't resolve parent → the reconciler handles AgentMissing
		}
		want = agent.Spec.Budget
	}
	if okCap, axis := eff.CapBudget(want); !okCap {
		return apierrors.NewInvalid(agentRunGK, run.Name, field.ErrorList{
			field.Forbidden(field.NewPath("spec", "budgetOverride"),
				"exceeds the AgentPolicy maxBudget on axis "+axis),
		})
	}
	return nil
}

// agentGate is the typed validator for Agent.
type agentGate struct{ g *agentPolicyGate }

func (a agentGate) ValidateCreate(ctx context.Context, obj *amv1.Agent) (admission.Warnings, error) {
	return nil, a.g.checkAgent(ctx, obj)
}
func (a agentGate) ValidateUpdate(ctx context.Context, _, newObj *amv1.Agent) (admission.Warnings, error) {
	return nil, a.g.checkAgent(ctx, newObj)
}
func (a agentGate) ValidateDelete(context.Context, *amv1.Agent) (admission.Warnings, error) {
	return nil, nil
}

// runGate is the typed validator for AgentRun.
type runGate struct{ g *agentPolicyGate }

func (r runGate) ValidateCreate(ctx context.Context, obj *amv1.AgentRun) (admission.Warnings, error) {
	return nil, r.g.checkRun(ctx, obj)
}
func (r runGate) ValidateUpdate(ctx context.Context, _, newObj *amv1.AgentRun) (admission.Warnings, error) {
	return nil, r.g.checkRun(ctx, newObj)
}
func (r runGate) ValidateDelete(context.Context, *amv1.AgentRun) (admission.Warnings, error) {
	return nil, nil
}
