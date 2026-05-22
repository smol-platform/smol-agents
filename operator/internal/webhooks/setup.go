package webhooks

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

// SetupAgentWebhook wires the validating + defaulting webhooks for
// SmolAgent into a controller-runtime Manager.
//
// The pure rule logic lives in ValidateAgent / DefaultAgent; this file
// is just the glue that fetches the singleton Platform CR and adapts
// the rules into webhook.CustomValidator + webhook.CustomDefaulter.
func SetupAgentWebhook(mgr ctrl.Manager, platformName string) error {
	if platformName == "" {
		platformName = "default"
	}
	w := &agentWebhook{
		client:       mgr.GetClient(),
		platformName: platformName,
	}
	return ctrl.NewWebhookManagedBy(mgr, &v1.SmolAgent{}).
		WithValidator(w).
		WithDefaulter(w).
		Complete()
}

type agentWebhook struct {
	client       client.Client
	platformName string
}

// fetchPlatform reads the singleton Platform CR. Returns nil if absent
// — defaulting falls back to compiled-in defaults; validation skips
// the policy gate.
func (w *agentWebhook) fetchPlatform(ctx context.Context) (*v1.SmolAgentPlatform, error) {
	p := &v1.SmolAgentPlatform{}
	err := w.client.Get(ctx, client.ObjectKey{Name: w.platformName}, p)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

// Default implements webhook.CustomDefaulter. R-OP-WH-2.
func (w *agentWebhook) Default(ctx context.Context, cr *v1.SmolAgent) error {
	platform, err := w.fetchPlatform(ctx)
	if err != nil {
		return err
	}
	DefaultAgent(cr, platform)
	return nil
}

// ValidateCreate implements webhook.CustomValidator. R-OP-WH-1.
func (w *agentWebhook) ValidateCreate(ctx context.Context, cr *v1.SmolAgent) (admission.Warnings, error) {
	return w.validate(ctx, cr)
}

// ValidateUpdate implements webhook.CustomValidator.
func (w *agentWebhook) ValidateUpdate(ctx context.Context, _, newObj *v1.SmolAgent) (admission.Warnings, error) {
	return w.validate(ctx, newObj)
}

// ValidateDelete implements webhook.CustomValidator.
func (w *agentWebhook) ValidateDelete(_ context.Context, _ *v1.SmolAgent) (admission.Warnings, error) {
	return nil, nil
}

func (w *agentWebhook) validate(ctx context.Context, cr *v1.SmolAgent) (admission.Warnings, error) {
	platform, err := w.fetchPlatform(ctx)
	if err != nil {
		return nil, err
	}
	if err := ValidateAgent(cr, platform); err != nil {
		return nil, err
	}
	return nil, nil
}

// SetupPlatformWebhook wires a singleton-enforcing webhook for the
// SmolAgentPlatform CR.
func SetupPlatformWebhook(mgr ctrl.Manager, singletonName string) error {
	if singletonName == "" {
		singletonName = "default"
	}
	w := &platformWebhook{singletonName: singletonName}
	return ctrl.NewWebhookManagedBy(mgr, &v1.SmolAgentPlatform{}).
		WithValidator(w).
		Complete()
}

type platformWebhook struct {
	singletonName string
}

func (w *platformWebhook) ValidateCreate(_ context.Context, p *v1.SmolAgentPlatform) (admission.Warnings, error) {
	return nil, ValidatePlatformSingleton(p, w.singletonName)
}

func (w *platformWebhook) ValidateUpdate(_ context.Context, _, newObj *v1.SmolAgentPlatform) (admission.Warnings, error) {
	return nil, ValidatePlatformSingleton(newObj, w.singletonName)
}

func (w *platformWebhook) ValidateDelete(_ context.Context, _ *v1.SmolAgentPlatform) (admission.Warnings, error) {
	return nil, nil
}

// Interface conformance is checked by the call sites in
// SetupAgentWebhook / SetupPlatformWebhook above; explicit checks
// would require the generic admission interfaces which moved between
// controller-runtime versions.
