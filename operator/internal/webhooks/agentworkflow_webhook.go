package webhooks

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// SetupAgentWorkflowWebhook registers a validating webhook that rejects a
// self-inconsistent AgentWorkflow at apply: no nodes, duplicate/reserved node
// names, cross-namespace agentRefs, dangling edge endpoints, a missing START
// edge, a non-compiling routing predicate, or a cycle (the graph must be a DAG).
// ValidateAgentWorkflow is self-contained (no cluster lookups), so admission
// rejection is safe; the reconciler runs the same check fail-closed.
func SetupAgentWorkflowWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &amv1.AgentWorkflow{}).
		WithValidator(&agentWorkflowWebhook{}).
		Complete()
}

type agentWorkflowWebhook struct{}

func (w *agentWorkflowWebhook) validate(o *amv1.AgentWorkflow) error {
	return pure.ValidateAgentWorkflow(pure.AgentWorkflow{Name: o.Name, Spec: o.Spec})
}

func (w *agentWorkflowWebhook) ValidateCreate(_ context.Context, obj *amv1.AgentWorkflow) (admission.Warnings, error) {
	return nil, w.validate(obj)
}

func (w *agentWorkflowWebhook) ValidateUpdate(_ context.Context, _, newObj *amv1.AgentWorkflow) (admission.Warnings, error) {
	return nil, w.validate(newObj)
}

func (w *agentWorkflowWebhook) ValidateDelete(context.Context, *amv1.AgentWorkflow) (admission.Warnings, error) {
	return nil, nil
}
