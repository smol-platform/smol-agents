package v1

import "testing"

func pol(name string, spec AgentPolicySpec) AgentPolicy {
	return AgentPolicy{Name: name, Spec: spec}
}

func TestComposePolicies_Empty(t *testing.T) {
	eff := ComposePolicies(nil)
	if !eff.Empty {
		t.Fatalf("no policies should yield Empty=true")
	}
	if !eff.AllowsProvider("anything") || !eff.AllowsTool("anything") {
		t.Fatalf("empty policy must allow all providers/tools")
	}
	if ok, _ := eff.CapBudget(Budget{MaxSteps: 1 << 20, MaxTokens: 1 << 40, MaxWallClockSeconds: 1 << 20, MaxToolCalls: 1 << 20}); !ok {
		t.Fatalf("empty policy must impose no budget cap")
	}
}

func TestComposePolicies_EmptySliceContributesNothing(t *testing.T) {
	// A policy with empty allow-lists must NOT deny-all.
	eff := ComposePolicies([]AgentPolicy{pol("p", AgentPolicySpec{AllowedProviders: []string{}, AllowedTools: []string{}})})
	if !eff.Empty {
		t.Fatalf("policy with only empty lists should be Empty")
	}
	if !eff.AllowsProvider("openai") {
		t.Fatalf("empty allow-list must not deny providers")
	}
}

func TestComposePolicies_Union(t *testing.T) {
	eff := ComposePolicies([]AgentPolicy{
		pol("a", AgentPolicySpec{AllowedProviders: []string{"openai"}, AllowedTools: []string{"search"}}),
		pol("b", AgentPolicySpec{AllowedProviders: []string{"anthropic"}, AllowedTools: []string{"calc"}}),
	})
	if eff.Empty {
		t.Fatalf("two non-empty policies must not be Empty")
	}
	for _, p := range []string{"openai", "anthropic"} {
		if !eff.AllowsProvider(p) {
			t.Errorf("union should allow provider %q", p)
		}
	}
	if eff.AllowsProvider("cohere") {
		t.Errorf("provider outside the union must be denied")
	}
	for _, tool := range []string{"search", "calc"} {
		if !eff.AllowsTool(tool) {
			t.Errorf("union should allow tool %q", tool)
		}
	}
	if eff.AllowsTool("rm-rf") {
		t.Errorf("tool outside the union must be denied")
	}
}

func TestComposePolicies_BudgetMinOverSetAxes(t *testing.T) {
	eff := ComposePolicies([]AgentPolicy{
		pol("a", AgentPolicySpec{MaxBudget: &Budget{MaxSteps: 10, MaxTokens: 0, MaxWallClockSeconds: 600, MaxToolCalls: 0}}),
		pol("b", AgentPolicySpec{MaxBudget: &Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 0, MaxToolCalls: 7}}),
	})
	if eff.Budget == nil {
		t.Fatalf("budget should be composed")
	}
	// per-axis min over axes that are set (>0); 0 = unset, never lowers a cap.
	if eff.Budget.MaxSteps != 5 {
		t.Errorf("MaxSteps min: got %d want 5", eff.Budget.MaxSteps)
	}
	if eff.Budget.MaxTokens != 1000 {
		t.Errorf("MaxTokens (only b set): got %d want 1000", eff.Budget.MaxTokens)
	}
	if eff.Budget.MaxWallClockSeconds != 600 {
		t.Errorf("MaxWallClock (only a set): got %d want 600", eff.Budget.MaxWallClockSeconds)
	}
	if eff.Budget.MaxToolCalls != 7 {
		t.Errorf("MaxToolCalls (only b set, 0 is unset): got %d want 7", eff.Budget.MaxToolCalls)
	}
}

func TestCapBudget_OffendingAxisAndBoundary(t *testing.T) {
	eff := ComposePolicies([]AgentPolicy{
		pol("a", AgentPolicySpec{MaxBudget: &Budget{MaxSteps: 10, MaxTokens: 1000, MaxWallClockSeconds: 600, MaxToolCalls: 5}}),
	})
	// equal-to-cap is allowed on every axis.
	if ok, axis := eff.CapBudget(Budget{MaxSteps: 10, MaxTokens: 1000, MaxWallClockSeconds: 600, MaxToolCalls: 5}); !ok {
		t.Fatalf("equal-to-cap must be allowed, got violation on %q", axis)
	}
	// over on tokens → reports the tokens axis.
	if ok, axis := eff.CapBudget(Budget{MaxSteps: 10, MaxTokens: 1001, MaxWallClockSeconds: 600, MaxToolCalls: 5}); ok || axis != "tokens" {
		t.Fatalf("over-tokens: got ok=%v axis=%q want false/tokens", ok, axis)
	}
	// over on steps → reports the steps axis.
	if _, axis := eff.CapBudget(Budget{MaxSteps: 11, MaxTokens: 1000, MaxWallClockSeconds: 600, MaxToolCalls: 5}); axis != "steps" {
		t.Fatalf("over-steps axis: got %q want steps", axis)
	}
}
