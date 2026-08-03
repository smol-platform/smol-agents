package webhooks

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
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
// backstop. It fails open only on an empty effective policy (no constraining
// AgentPolicy in the namespace) — a transient AgentPolicy list error is fail
// CLOSED: the validator returns a retryable apierrors.NewInternalError so the
// apiserver denies the request instead of silently admitting past a namespace
// policy it couldn't read (knative-agents-2mi). defaultRunClass is the
// operator's --default-run-runtime-class, used to resolve an Agent that leaves
// spec.sandbox.runtimeClass empty for the claude-write warning.
func SetupAgentPolicyGateWebhook(mgr ctrl.Manager, defaultRunClass string) error {
	g := &agentPolicyGate{client: mgr.GetClient(), defaultRunClass: defaultRunClass}
	if err := ctrl.NewWebhookManagedBy(mgr, &amv1.Agent{}).WithValidator(agentGate{g}).Complete(); err != nil {
		return err
	}
	return ctrl.NewWebhookManagedBy(mgr, &amv1.AgentRun{}).WithValidator(runGate{g}).Complete()
}

type agentPolicyGate struct {
	client          client.Client
	defaultRunClass string
}

// effective lists + composes the namespace policies. ok=false ⇒ fail open
// because there is no constraining policy (an empty AgentPolicyList). A List
// error is NOT fail-open: it is returned separately so callers can deny the
// request instead of silently admitting past a policy they couldn't read.
func (g *agentPolicyGate) effective(ctx context.Context, ns string) (eff pure.EffectivePolicy, ok bool, err error) {
	var list amv1.AgentPolicyList
	if err := g.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return pure.EffectivePolicy{}, false, err
	}
	pol := make([]pure.AgentPolicy, 0, len(list.Items))
	for i := range list.Items {
		pol = append(pol, pure.AgentPolicy{Name: list.Items[i].Name, Spec: list.Items[i].Spec})
	}
	eff = pure.ComposePolicies(pol)
	return eff, !eff.Empty, nil
}

func (g *agentPolicyGate) checkAgent(ctx context.Context, a *amv1.Agent) error {
	eff, ok, err := g.effective(ctx, a.Namespace)
	if err != nil {
		// Fail closed (knative-agents-2mi): a List error must not silently admit
		// an Agent the namespace policy would have rejected. NewInternalError is
		// not an apierrors.IsInvalid — the caller (kubectl/controller) sees a
		// retryable 500, distinct from a genuine policy-violation Invalid.
		return apierrors.NewInternalError(fmt.Errorf("list AgentPolicy in namespace %q: %w", a.Namespace, err))
	}
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

// checkLoopToolKinds warns when a loop-mode Agent references a Tool whose kind
// has no production loop invoker (M2.16). The reconciler's SupportedLoopToolKinds
// gate (agent_controller.go) is the ENFORCEMENT: it flips the admitted Agent to
// Failed/ToolKindUnsupported, keeping the failure observable in status and robust
// to a Tool's kind changing after the Agent is admitted. Admission only surfaces
// it early as a warning and never rejects — a hard reject would pre-empt that
// observable reconcile state and couple admission to a cross-object Tool read
// (ordering-fragile). Harness mode and dangling refs are skipped.
func (g *agentPolicyGate) checkLoopToolKinds(ctx context.Context, a *amv1.Agent) admission.Warnings {
	if a.Spec.Mode == pure.ModeHarness {
		return nil
	}
	supported := pure.SupportedLoopToolKinds()
	var warns admission.Warnings
	for _, ref := range a.Spec.Tools {
		ns := ref.Namespace
		if ns == "" {
			ns = a.Namespace
		}
		var t amv1.Tool
		if err := g.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &t); err != nil {
			continue // dangling ref → the reconciler handles existence; kind unknowable here
		}
		if !supported[t.Spec.Kind] {
			warns = append(warns, "tool "+ref.Name+" has kind "+string(t.Spec.Kind)+
				" which has no loop-mode invoker; the Agent will be marked Failed/ToolKindUnsupported")
		}
	}
	return warns
}

// claudeWriteTools are the claude-code permission tool names that imply file
// writes. Headless claude-code only writes files under
// --dangerously-skip-permissions, which D3 gates to kata microVMs — so a
// claude Agent that allow-lists these on a shared-kernel runtime fails
// confusingly at runtime (proven live, see live_zai_5scenarios).
var claudeWriteTools = map[string]bool{
	"write": true, "edit": true, "multiedit": true, "notebookedit": true,
}

// checkClaudeWriteRuntime warns when a claude-code harness Agent allow-lists
// write-class tools (or sets approvalMode acceptEdits) but its resolved
// RuntimeClass is not a kata microVM. Warning-only, mirroring
// checkLoopToolKinds: the danger-flag enforcement (dangerFlagViolation, D3) is
// the reconcile-time gate; admission just surfaces the inevitable runtime
// failure early. Pure spec logic — no cluster lookups.
func (g *agentPolicyGate) checkClaudeWriteRuntime(a *amv1.Agent) admission.Warnings {
	h := a.Spec.Harness
	if h == nil || pure.CanonicalHarnessKind(h.Kind) != pure.HarnessClaudeCode || h.CLI == nil {
		return nil
	}
	wantsWrites := h.CLI.ApprovalMode == "acceptEdits"
	for _, t := range h.CLI.AllowedTools {
		if claudeWriteTools[strings.ToLower(t)] {
			wantsWrites = true
			break
		}
	}
	if !wantsWrites {
		return nil
	}
	class := a.Spec.Sandbox.RuntimeClass
	if class == "" {
		class = g.defaultRunClass
	}
	if class == "" || builders.RequiresKVM(class) {
		return nil // microVM (or unknowable default) — writes are gateable via danger flags
	}
	return admission.Warnings{
		"claude-code can only write files with --dangerously-skip-permissions, which D3 restricts to kata microVMs; " +
			"resolved runtimeClass " + class + " is shared-kernel, so this Agent's file writes will fail at runtime " +
			"(approvalMode/allowedTools are not sufficient headlessly)",
	}
}

func (g *agentPolicyGate) checkRun(ctx context.Context, run *amv1.AgentRun) error {
	eff, ok, err := g.effective(ctx, run.Namespace)
	if err != nil {
		// See checkAgent: fail closed on a List error, not open.
		return apierrors.NewInternalError(fmt.Errorf("list AgentPolicy in namespace %q: %w", run.Namespace, err))
	}
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
	if err := a.g.checkAgent(ctx, obj); err != nil {
		return nil, err
	}
	return append(a.g.checkLoopToolKinds(ctx, obj), a.g.checkClaudeWriteRuntime(obj)...), nil
}

func (a agentGate) ValidateUpdate(ctx context.Context, _, newObj *amv1.Agent) (admission.Warnings, error) {
	if err := a.g.checkAgent(ctx, newObj); err != nil {
		return nil, err
	}
	return append(a.g.checkLoopToolKinds(ctx, newObj), a.g.checkClaudeWriteRuntime(newObj)...), nil
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
