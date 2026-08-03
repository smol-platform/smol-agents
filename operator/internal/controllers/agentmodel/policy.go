package agentmodel

import (
	"context"
	"regexp"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// effectivePolicyFor lists every AgentPolicy in the namespace and composes them
// into one EffectivePolicy. A list error returns the zero (Empty) policy and
// the error so callers can fail-open on a transient apiserver hiccup rather
// than wrongly deny.
func effectivePolicyFor(ctx context.Context, c client.Client, ns string) (pure.EffectivePolicy, error) {
	var list amv1.AgentPolicyList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return pure.EffectivePolicy{Empty: true}, err
	}
	pol := make([]pure.AgentPolicy, 0, len(list.Items))
	for i := range list.Items {
		pol = append(pol, pure.AgentPolicy{Name: list.Items[i].Name, Spec: list.Items[i].Spec})
	}
	return pure.ComposePolicies(pol), nil
}

// compileNamespaceRedaction returns the redaction patterns to apply for a
// namespace: pure.DefaultPatterns() (well-known credential shapes) ALWAYS
// apply, extended by any compiled patterns from the namespace's composed
// AgentPolicy. There is no opt-out; a namespace with no AgentPolicy still
// gets the defaults. A non-compiling policy pattern is skipped and logged (it
// was already rejected at admission by ValidateAgentPolicy; this is the
// belt-and-suspenders path) so one bad pattern never disables the rest or
// panics the fold. A transient list error still yields the defaults, failing
// toward masking rather than toward disclosure.
func compileNamespaceRedaction(ctx context.Context, c client.Client, ns string) []*regexp.Regexp {
	defaults := pure.DefaultPatterns()
	eff, err := effectivePolicyFor(ctx, c, ns)
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
