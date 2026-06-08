package webhooks

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// SetupToolWebhook registers a validating webhook that rejects a self-
// inconsistent Tool at apply via pure.ValidateTool: a missing kind-specific
// field (mcp.url / http.url / agent.ref.name / function.name / fanout.ref.name),
// a malformed input/output JSON schema, a cross-namespace fanout/agent ref, or a
// bad fanout reduce/maxParallel. The check is self-contained (no cluster
// lookups), so rejecting at admission introduces no ordering fragility.
func SetupToolWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &amv1.Tool{}).
		WithValidator(&toolWebhook{}).
		Complete()
}

type toolWebhook struct{}

func (w *toolWebhook) validate(t *amv1.Tool) error {
	return pure.ValidateTool(pure.Tool{Name: t.Name, Spec: t.Spec})
}

func (w *toolWebhook) ValidateCreate(_ context.Context, obj *amv1.Tool) (admission.Warnings, error) {
	return nil, w.validate(obj)
}

func (w *toolWebhook) ValidateUpdate(_ context.Context, _, newObj *amv1.Tool) (admission.Warnings, error) {
	return nil, w.validate(newObj)
}

func (w *toolWebhook) ValidateDelete(context.Context, *amv1.Tool) (admission.Warnings, error) {
	return nil, nil
}
