package webhooks

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// SetupAgentPolicySelfWebhook wires a validating webhook on AgentPolicy ITSELF so
// a malformed policy is rejected at apply rather than silently skipped later. The
// gate webhook + reconcilers compose every namespace policy and trust each stored
// one is well-formed; without this a redaction pattern that does not compile (or
// a bad budget) would be admitted and then skipped+logged on the fold path —
// silently weakening redaction. This keeps "a stored policy is valid" true at the
// apiserver edge. Same validator the reconciler runs (ValidateAgentPolicy). M1.7.
func SetupAgentPolicySelfWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &amv1.AgentPolicy{}).
		WithValidator(agentPolicySelf{}).
		Complete()
}

type agentPolicySelf struct{}

// validate adapts the operator's typed AgentPolicy (ObjectMeta name + pure spec)
// to the pure validator.
func (agentPolicySelf) validate(ap *amv1.AgentPolicy) error {
	return pure.ValidateAgentPolicy(pure.AgentPolicy{Name: ap.Name, Spec: ap.Spec})
}

func (v agentPolicySelf) ValidateCreate(_ context.Context, ap *amv1.AgentPolicy) (admission.Warnings, error) {
	return nil, v.validate(ap)
}

func (v agentPolicySelf) ValidateUpdate(_ context.Context, _, newObj *amv1.AgentPolicy) (admission.Warnings, error) {
	return nil, v.validate(newObj)
}

func (agentPolicySelf) ValidateDelete(context.Context, *amv1.AgentPolicy) (admission.Warnings, error) {
	return nil, nil
}
