package agentmodel

import (
	"context"
	"regexp"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/smol-platform/smol-agents/operator/internal/policy"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// effectivePolicyFor composes the namespace's AgentPolicies, falling back to
// the platform baseline (knative-agents-7dm) when the namespace constrains
// nothing — see policy.Effective for the exact semantics. baseline.Name == ""
// disables the fallback (pre-7dm behavior). A read error (namespace list or
// baseline get) returns the zero (Empty) policy and the error so callers can
// fail CLOSED (knative-agents-2mi) rather than wrongly admit.
func effectivePolicyFor(ctx context.Context, c client.Client, ns string, baseline types.NamespacedName) (pure.EffectivePolicy, error) {
	eff, err := policy.Effective(ctx, c, ns, baseline)
	if err != nil {
		return pure.EffectivePolicy{Empty: true}, err
	}
	return eff, nil
}

// compileNamespaceRedaction returns the redaction patterns to apply for a
// namespace: pure.DefaultPatterns() (well-known credential shapes) ALWAYS
// apply, extended by any compiled patterns from the namespace's composed
// AgentPolicy — which is baseline-aware, so a policy-less namespace also
// inherits the platform baseline's patterns (knative-agents-7dm, see
// effectivePolicyFor). There is no opt-out; a namespace with no AgentPolicy
// still gets the defaults. A non-compiling policy pattern is skipped and
// logged (it was already rejected at admission by ValidateAgentPolicy; this
// is the belt-and-suspenders path) so one bad pattern never disables the rest
// or panics the fold. A transient read error still yields the defaults,
// failing toward masking rather than toward disclosure — redaction is a
// disclosure control, not containment, so unlike the fail-closed admission
// gate it stays best-effort rather than blocking the fold.
func compileNamespaceRedaction(ctx context.Context, c client.Client, ns string, baseline types.NamespacedName) []*regexp.Regexp {
	defaults := pure.DefaultPatterns()
	eff, err := effectivePolicyFor(ctx, c, ns, baseline)
	if err != nil || len(eff.Patterns) == 0 {
		return defaults
	}
	res, errs := pure.CompilePatterns(eff.Patterns)
	for _, e := range errs {
		log.FromContext(ctx).Error(e, "skipping non-compiling redaction pattern")
	}
	// Copy: defaults is a memoized shared slice (pure.DefaultPatterns), so it
	// must never be appended to in place — that could race or corrupt it
	// across concurrent reconciles.
	pats := make([]*regexp.Regexp, 0, len(defaults)+len(res))
	pats = append(pats, defaults...)
	pats = append(pats, res...)
	return pats
}
