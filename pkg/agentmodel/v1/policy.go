package v1

// EffectivePolicy is the composition of zero or more AgentPolicySpecs that
// apply to a single Agent (all AgentPolicies in its namespace). It is a pure
// value computed by ComposePolicies and consumed by the admission webhook and
// the reconcile-time gate.
//
// An empty allow-set means "no constraint on this axis" (allow all), NOT
// "deny all" — a namespace with no policies, or policies that leave an axis
// empty, imposes nothing on that axis. This keeps the default open and makes
// adding a policy a deliberate tightening.
type EffectivePolicy struct {
	// Providers is the union of every policy's AllowedProviders. Empty ⇒ any
	// provider is allowed (no policy constrained providers).
	Providers map[string]struct{}
	// Tools is the union of every policy's AllowedTools. Empty ⇒ any tool.
	Tools map[string]struct{}
	// Budget is the per-axis minimum across every policy's MaxBudget. An axis
	// set to 0 in a source budget is treated as "unset" and does not lower the
	// cap. nil ⇒ no policy capped the budget.
	Budget *Budget
	// Patterns is the de-duplicated union of every policy's redaction patterns
	// (raw, uncompiled — compile with CompilePatterns).
	Patterns []string
	// Empty is true iff no policy contributed any constraint (no provider,
	// tool, budget axis, or pattern). A fully-empty EffectivePolicy allows
	// everything and the caller may skip enforcement entirely.
	Empty bool
}

// ComposePolicies folds a set of AgentPolicies into one EffectivePolicy.
// Allow-lists union (each policy *adds* permitted values; an empty list
// contributes nothing), budgets compose per-axis by minimum over set axes,
// and redaction patterns de-duplicate. The result is order-independent.
func ComposePolicies(policies []AgentPolicy) EffectivePolicy {
	eff := EffectivePolicy{
		Providers: map[string]struct{}{},
		Tools:     map[string]struct{}{},
	}
	seenPat := map[string]struct{}{}
	for _, p := range policies {
		for _, prov := range p.Spec.AllowedProviders {
			if prov != "" {
				eff.Providers[prov] = struct{}{}
			}
		}
		for _, t := range p.Spec.AllowedTools {
			if t != "" {
				eff.Tools[t] = struct{}{}
			}
		}
		if p.Spec.MaxBudget != nil {
			eff.Budget = minBudget(eff.Budget, p.Spec.MaxBudget)
		}
		if p.Spec.Redaction != nil {
			for _, pat := range p.Spec.Redaction.Patterns {
				if pat == "" {
					continue
				}
				if _, ok := seenPat[pat]; !ok {
					seenPat[pat] = struct{}{}
					eff.Patterns = append(eff.Patterns, pat)
				}
			}
		}
	}
	eff.Empty = len(eff.Providers) == 0 && len(eff.Tools) == 0 &&
		eff.Budget == nil && len(eff.Patterns) == 0
	return eff
}

// AllowsProvider reports whether providerRef is permitted. An empty provider
// allow-set (no policy constrained providers) permits any provider.
func (e EffectivePolicy) AllowsProvider(providerRef string) bool {
	if len(e.Providers) == 0 {
		return true
	}
	_, ok := e.Providers[providerRef]
	return ok
}

// AllowsTool reports whether a tool name is permitted. An empty tool allow-set
// permits any tool.
func (e EffectivePolicy) AllowsTool(tool string) bool {
	if len(e.Tools) == 0 {
		return true
	}
	_, ok := e.Tools[tool]
	return ok
}

// CapBudget reports whether want stays within the effective per-axis caps. On
// violation it returns ok=false and the name of the first offending axis. An
// unset effective cap (nil Budget, or a 0 axis) imposes no limit on that axis;
// equal-to-cap is allowed.
func (e EffectivePolicy) CapBudget(want Budget) (ok bool, axis string) {
	if e.Budget == nil {
		return true, ""
	}
	b := e.Budget
	if b.MaxSteps > 0 && want.MaxSteps > b.MaxSteps {
		return false, "steps"
	}
	if b.MaxTokens > 0 && want.MaxTokens > b.MaxTokens {
		return false, "tokens"
	}
	if b.MaxWallClockSeconds > 0 && want.MaxWallClockSeconds > b.MaxWallClockSeconds {
		return false, "wallclock"
	}
	if b.MaxToolCalls > 0 && want.MaxToolCalls > b.MaxToolCalls {
		return false, "toolCalls"
	}
	return true, ""
}

// minBudget returns a Budget whose every axis is the minimum of a and b over
// the axes each *sets* (a 0 means "unset" and never lowers the cap), so a
// policy that omits an axis never pins it — including MaxToolCalls, where 0
// means "no cap", not "zero tool calls".
func minBudget(a, b *Budget) *Budget {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := Budget{
		MaxSteps:            minSetInt32(a.MaxSteps, b.MaxSteps),
		MaxTokens:           minSetInt64(a.MaxTokens, b.MaxTokens),
		MaxWallClockSeconds: minSetInt32(a.MaxWallClockSeconds, b.MaxWallClockSeconds),
		MaxToolCalls:        minSetInt32(a.MaxToolCalls, b.MaxToolCalls),
	}
	return &out
}

func minSetInt32(a, b int32) int32 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func minSetInt64(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}
