package webhooks

import (
	"errors"
	"fmt"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/operator/pkg/features"
	pkgsandbox "github.com/smol-platform/smol-agents/pkg/sandbox"
)

// AllowInsecureAnnotation is the annotation a tenant must set to opt
// into mode=insecure. Mirrors the platform's
// `SMOL_AGENTS_ALLOW_INSECURE` runtime guard.
const AllowInsecureAnnotation = "smol-agents.smol-agents.ai/allow-insecure"

// ValidateAgent runs every R-OP-WH-1 admission rule against cr,
// optionally consulting platform's featurePolicy when supplied.
// Returns a joined error or nil.
func ValidateAgent(cr *v1.SmolAgent, platform *v1.SmolAgentPlatform) error {
	var errs []error

	if cr.Spec.TrustDomain == "" {
		errs = append(errs, errors.New("spec.trustDomain is required"))
	}

	mode := cr.Spec.Mode
	if mode == "" {
		mode = cr.Spec.Features.Identity.Mode
	}
	if mode == "insecure" && cr.Annotations[AllowInsecureAnnotation] != "true" {
		errs = append(errs, fmt.Errorf("mode=insecure requires annotation %s=true", AllowInsecureAnnotation))
	}

	rc := cr.Spec.Features.Sandbox.RuntimeClass
	kind := pkgsandbox.ParseKind(rc)
	if kind == pkgsandbox.KindRunc && !cr.Spec.Features.Sandbox.AllowHostEscape {
		errs = append(errs, errors.New("sandbox.runtimeClass=runc requires sandbox.allowHostEscape=true (R-SBX-1)"))
	}
	// Unknown runtimeClass that ParseKind couldn't resolve also triggers
	// the runc fallback above, so the same error fires.

	if platform != nil {
		errs = append(errs, validatePolicy(cr, platform)...)
	}
	return errors.Join(errs...)
}

func validatePolicy(cr *v1.SmolAgent, platform *v1.SmolAgentPlatform) []error {
	var errs []error
	policy := platform.Spec.FeaturePolicy
	enabledByName := map[string]bool{
		string(features.Identity):         cr.Spec.Features.Identity.Enabled,
		string(features.TransportPrivate): cr.Spec.Features.Transport.Private.Enabled,
		string(features.TransportPublic):  cr.Spec.Features.Transport.Public.Enabled,
		string(features.Secrets):          cr.Spec.Features.Secrets.Enabled,
		string(features.Sandbox):          cr.Spec.Features.Sandbox.Enabled,
		string(features.EBPF):             cr.Spec.Features.EBPF.Enabled,
		string(features.Knative):          cr.Spec.Features.Knative.Enabled,
		string(features.Observability):    cr.Spec.Features.Observability.Enabled,
	}
	for _, row := range policy {
		if row.Allowed {
			continue
		}
		if enabledByName[row.Feature] {
			errs = append(errs, fmt.Errorf("feature %q is forbidden by SmolAgentPlatform.spec.featurePolicy", row.Feature))
		}
	}
	return errs
}

// DefaultAgent fills unset fields from the platform CR's Defaults and
// applies compiled-in defaults. Mutates cr in place.
//
// Implements R-OP-WH-2.
func DefaultAgent(cr *v1.SmolAgent, platform *v1.SmolAgentPlatform) {
	if cr.Spec.TrustDomain == "" && platform != nil {
		cr.Spec.TrustDomain = platform.Spec.DefaultTrustDomain
	}
	if cr.Spec.DeploymentKind == "" {
		cr.Spec.DeploymentKind = "knative"
	}
	if cr.Spec.Replicas == 0 {
		cr.Spec.Replicas = 1
	}
	if cr.Spec.Features.Sandbox.RuntimeClass == "" {
		cr.Spec.Features.Sandbox.RuntimeClass = string(pkgsandbox.KindKataFC)
	}
	if cr.Spec.Features.Identity.Mode == "" {
		cr.Spec.Features.Identity.Mode = "strict"
	}
	if cr.Spec.Features.Identity.WorkloadAPI == "" {
		cr.Spec.Features.Identity.WorkloadAPI = "unix:///run/spire/agent-sockets/api.sock"
	}
}

// ValidatePlatformSingleton enforces R-OP-API-2: only one Platform CR
// per cluster, named exactly `singletonName`.
func ValidatePlatformSingleton(p *v1.SmolAgentPlatform, singletonName string) error {
	if p.Name != singletonName {
		return fmt.Errorf("SmolAgentPlatform name=%q is not %q (R-OP-API-2)", p.Name, singletonName)
	}
	if p.Spec.DefaultTrustDomain == "" {
		return errors.New("spec.defaultTrustDomain is required")
	}
	for _, row := range p.Spec.FeaturePolicy {
		if !features.Valid(features.Feature(row.Feature)) {
			return fmt.Errorf("featurePolicy[%q] is not a known feature", row.Feature)
		}
	}
	return nil
}
