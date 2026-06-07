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

// compileNamespaceRedaction returns the compiled redaction patterns from the
// namespace's composed policy. A non-compiling pattern is skipped and logged
// (it was already rejected at admission by ValidateAgentPolicy; this is the
// belt-and-suspenders path) so one bad pattern never disables the rest or
// panics the fold. Returns nil when there is nothing to redact.
func compileNamespaceRedaction(ctx context.Context, c client.Client, ns string) []*regexp.Regexp {
	eff, err := effectivePolicyFor(ctx, c, ns)
	if err != nil || len(eff.Patterns) == 0 {
		return nil
	}
	res, errs := pure.CompilePatterns(eff.Patterns)
	for _, e := range errs {
		log.FromContext(ctx).Error(e, "skipping non-compiling redaction pattern")
	}
	return res
}
