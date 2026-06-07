package webhooks

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// SetupAgentTeamWebhook registers a validating webhook that rejects a
// self-inconsistent AgentTeam at apply: missing lead, no members, duplicate
// member names, cross-namespace lead/member refs, an unknown pattern, or an
// invalid team budget. The check is self-contained (ValidateAgentTeam does no
// cluster lookups), so rejecting at admission introduces no ordering fragility;
// the AgentTeam reconciler runs the same ValidateAgentTeam as a fail-closed
// backstop (→ Failed/InvalidSpec).
func SetupAgentTeamWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &amv1.AgentTeam{}).
		WithValidator(&agentTeamWebhook{}).
		Complete()
}

type agentTeamWebhook struct{}

func (w *agentTeamWebhook) validate(t *amv1.AgentTeam) error {
	return pure.ValidateAgentTeam(pure.AgentTeam{Name: t.Name, Spec: t.Spec})
}

func (w *agentTeamWebhook) ValidateCreate(_ context.Context, obj *amv1.AgentTeam) (admission.Warnings, error) {
	return nil, w.validate(obj)
}

func (w *agentTeamWebhook) ValidateUpdate(_ context.Context, _, newObj *amv1.AgentTeam) (admission.Warnings, error) {
	return nil, w.validate(newObj)
}

func (w *agentTeamWebhook) ValidateDelete(context.Context, *amv1.AgentTeam) (admission.Warnings, error) {
	return nil, nil
}
