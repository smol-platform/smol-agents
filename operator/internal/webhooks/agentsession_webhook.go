package webhooks

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

// SetupAgentSessionWebhook registers a validating webhook that rejects an
// AgentSession whose spec.agentRef does not resolve to an Agent in the same
// namespace — failing fast at apply instead of leaving the session in a 15s
// Pending limbo until the reconciler notices (M2.22). The reconciler keeps its
// NotFound→Pending handling as belt-and-suspenders.
func SetupAgentSessionWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &amv1.AgentSession{}).
		WithValidator(&agentSessionWebhook{client: mgr.GetClient()}).
		Complete()
}

type agentSessionWebhook struct{ client client.Client }

func (w *agentSessionWebhook) validate(ctx context.Context, s *amv1.AgentSession) error {
	if s.Spec.AgentRef == "" {
		return fmt.Errorf("spec.agentRef is required")
	}
	var agent amv1.Agent
	err := w.client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Spec.AgentRef}, &agent)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("spec.agentRef %q: no such Agent in namespace %q", s.Spec.AgentRef, s.Namespace)
	}
	return err // nil on success; a transient get error surfaces (fail-closed at the edge)
}

func (w *agentSessionWebhook) ValidateCreate(ctx context.Context, obj *amv1.AgentSession) (admission.Warnings, error) {
	return nil, w.validate(ctx, obj)
}
func (w *agentSessionWebhook) ValidateUpdate(ctx context.Context, _, newObj *amv1.AgentSession) (admission.Warnings, error) {
	return nil, w.validate(ctx, newObj)
}
func (w *agentSessionWebhook) ValidateDelete(context.Context, *amv1.AgentSession) (admission.Warnings, error) {
	return nil, nil
}
