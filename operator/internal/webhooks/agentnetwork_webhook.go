package webhooks

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	amv1 "github.com/stigen/knative-agents/operator/api/agentmodel/v1"
	pure "github.com/stigen/knative-agents/pkg/agentmodel/v1"
)

// SetupAgentNetworkWebhook wires a validating webhook for
// AgentNetwork CRs. The reconciler already runs the same validation
// (ValidateAgentNetwork) at Reconcile time and reports
// Status.Phase=Failed when it rejects — this webhook surfaces the
// same errors at admission time so apply pipelines fail fast.
//
// Implements R-AN-API-1 at the apiserver edge.
func SetupAgentNetworkWebhook(mgr ctrl.Manager) error {
	w := &agentNetworkWebhook{}
	return ctrl.NewWebhookManagedBy(mgr, &amv1.AgentNetwork{}).
		WithValidator(w).
		Complete()
}

type agentNetworkWebhook struct{}

func (w *agentNetworkWebhook) ValidateCreate(_ context.Context, an *amv1.AgentNetwork) (admission.Warnings, error) {
	return nil, pure.ValidateAgentNetwork(an.Spec)
}

func (w *agentNetworkWebhook) ValidateUpdate(_ context.Context, _, newObj *amv1.AgentNetwork) (admission.Warnings, error) {
	return nil, pure.ValidateAgentNetwork(newObj.Spec)
}

func (w *agentNetworkWebhook) ValidateDelete(_ context.Context, _ *amv1.AgentNetwork) (admission.Warnings, error) {
	return nil, nil
}
