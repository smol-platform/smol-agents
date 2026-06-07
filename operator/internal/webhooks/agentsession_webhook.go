package webhooks

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	// A per-turn timeout longer than the turn-stream retention is nonsensical — the
	// turn could outlive the record of it (M2.22). Only checked when both are
	// explicitly set; the defaults (300s ≤ 3600s) are always valid.
	if s.Spec.TurnDeliveryTimeoutSeconds > 0 && s.Spec.TurnRetentionSeconds > 0 &&
		s.Spec.TurnDeliveryTimeoutSeconds > s.Spec.TurnRetentionSeconds {
		return fmt.Errorf("spec.turnDeliveryTimeoutSeconds (%d) must be <= spec.turnRetentionSeconds (%d)",
			s.Spec.TurnDeliveryTimeoutSeconds, s.Spec.TurnRetentionSeconds)
	}
	// Resource quantities must parse (M1.11) — reject a bad "500x" at apply rather
	// than silently dropping it on the worker pod build.
	if r := s.Spec.Resources; r != nil {
		for _, side := range []struct {
			path string
			m    map[string]string
		}{{"limits", r.Limits}, {"requests", r.Requests}} {
			for name, val := range side.m {
				if _, err := resource.ParseQuantity(val); err != nil {
					return fmt.Errorf("spec.resources.%s[%s]: %q is not a valid quantity: %w", side.path, name, val, err)
				}
			}
		}
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
