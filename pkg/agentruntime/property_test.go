package agentruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"pgregory.net/rapid"
)

// arbitraryDecision generates a random LLMDecision: either a final answer
// or a tool call, with random token costs.
func arbitraryDecision(t *rapid.T, allowedTools []string) rt.LLMDecision {
	tokensIn := int64(rapid.IntRange(0, 50).Draw(t, "tokensIn"))
	tokensOut := int64(rapid.IntRange(0, 50).Draw(t, "tokensOut"))
	if rapid.Bool().Draw(t, "isFinal") {
		return rt.LLMDecision{
			FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"answer":"x"}`)},
			TokensIn:    tokensIn, TokensOut: tokensOut,
		}
	}
	var name string
	if len(allowedTools) > 0 && rapid.Bool().Draw(t, "useAllowed") {
		name = allowedTools[rapid.IntRange(0, len(allowedTools)-1).Draw(t, "idx")]
	} else {
		name = rapid.StringMatching("[a-z]{1,6}").Draw(t, "toolName")
	}
	return rt.LLMDecision{
		ToolCall: &rt.ToolCall{Tool: name, Arguments: json.RawMessage(`{"x":1}`)},
		TokensIn: tokensIn, TokensOut: tokensOut,
	}
}

func arbitraryBudget(t *rapid.T) v1.Budget {
	return v1.Budget{
		MaxSteps:            int32(rapid.IntRange(1, 8).Draw(t, "maxSteps")),
		MaxTokens:           int64(rapid.IntRange(50, 5000).Draw(t, "maxTokens")),
		MaxWallClockSeconds: int32(rapid.IntRange(1, 30).Draw(t, "maxWallClockSeconds")),
		MaxToolCalls:        int32(rapid.IntRange(0, 5).Draw(t, "maxToolCalls")),
	}
}

func mkExecutor(script []rt.LLMDecision, allowed []string) *Executor {
	tools := make(map[string]v1.Tool, len(allowed))
	handlers := make(map[string]func(json.RawMessage) (json.RawMessage, error), len(allowed))
	for _, n := range allowed {
		tools[n] = sampleTool(n)
		nm := n
		handlers[nm] = func(_ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		}
	}
	e := New()
	e.LLM = &FakeLLM{Script: script}
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Tools = tools
	e.Invokers = map[v1.ToolKind]ToolInvoker{v1.ToolFunction: &InProcessInvoker{Handlers: handlers}}
	return e
}

// Property: Usage at termination NEVER exceeds Budget on any axis.
// R-AM-VRF-2 — `BudgetNeverExceeded`.
func TestProperty_BudgetNeverExceeded(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		budget := arbitraryBudget(t)
		nAllowed := rapid.IntRange(1, 4).Draw(t, "nAllowed")
		allowed := make([]string, nAllowed)
		for i := range allowed {
			allowed[i] = rapid.StringMatching("[a-z]{2,5}").Draw(t, "name")
		}
		nDecisions := rapid.IntRange(1, 30).Draw(t, "nDecisions")
		script := make([]rt.LLMDecision, nDecisions)
		for i := range script {
			script[i] = arbitraryDecision(t, allowed)
		}
		e := mkExecutor(script, allowed)
		agent := sampleAgent(budget, allowed...)
		res, err := e.Run(context.Background(), agent, json.RawMessage(`{}`), int64(rapid.Int().Draw(t, "seed")))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Usage.Steps > budget.MaxSteps {
			t.Fatalf("budget violated: steps %d > maxSteps %d", res.Usage.Steps, budget.MaxSteps)
		}
		if res.Usage.Tokens > budget.MaxTokens {
			t.Fatalf("budget violated: tokens %d > maxTokens %d", res.Usage.Tokens, budget.MaxTokens)
		}
		if res.Usage.ToolCalls > budget.MaxToolCalls {
			t.Fatalf("budget violated: toolCalls %d > maxToolCalls %d", res.Usage.ToolCalls, budget.MaxToolCalls)
		}
		// Phase must be a terminal one.
		if !res.Phase.Terminal() {
			t.Fatalf("non-terminal phase %s", res.Phase)
		}
	})
}

// Property: every Observation step references a tool name in the
// agent's allow-list. R-AM-TOOL-1 + Quint OnlyAllowedToolsCalled.
func TestProperty_OnlyAllowedToolsCalled(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nAllowed := rapid.IntRange(1, 3).Draw(t, "nAllowed")
		allowed := make([]string, nAllowed)
		seen := map[string]bool{}
		for i := range allowed {
			for {
				n := rapid.StringMatching("[a-z]{2,5}").Draw(t, "allowed")
				if !seen[n] {
					seen[n] = true
					allowed[i] = n
					break
				}
			}
		}
		// Mix in some bogus tools.
		nDecisions := rapid.IntRange(1, 20).Draw(t, "nDecisions")
		script := make([]rt.LLMDecision, nDecisions)
		for i := range script {
			script[i] = arbitraryDecision(t, allowed)
		}
		e := mkExecutor(script, allowed)
		agent := sampleAgent(v1.Budget{
			MaxSteps: 30, MaxTokens: 100000, MaxWallClockSeconds: 60, MaxToolCalls: 30,
		}, allowed...)
		res, err := e.Run(context.Background(), agent, json.RawMessage(`{}`), 0)
		if err != nil {
			t.Fatal(err)
		}
		allowedSet := map[string]bool{}
		for _, n := range allowed {
			allowedSet[n] = true
		}
		for _, s := range res.Steps {
			if s.Kind == v1.StepObservation || s.Kind == v1.StepToolCall {
				for _, tc := range s.ToolCalls {
					if !allowedSet[tc.Tool] {
						t.Fatalf("invariant violated: invoked %q (not in allow-list %v)", tc.Tool, allowed)
					}
				}
			}
		}
	})
}

// Property: same seed + same script → identical step kind sequence.
func TestProperty_Determinism(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nAllowed := rapid.IntRange(1, 3).Draw(t, "nAllowed")
		allowed := make([]string, nAllowed)
		for i := range allowed {
			allowed[i] = rapid.StringMatching("[a-z]{2,5}").Draw(t, "n")
		}
		nDecisions := rapid.IntRange(1, 12).Draw(t, "nDecisions")
		script := make([]rt.LLMDecision, nDecisions)
		for i := range script {
			script[i] = arbitraryDecision(t, allowed)
		}
		seed := int64(rapid.Int().Draw(t, "seed"))
		mk := func() *Executor { return mkExecutor(script, allowed) }
		agent := sampleAgent(v1.Budget{
			MaxSteps: 30, MaxTokens: 100000, MaxWallClockSeconds: 60, MaxToolCalls: 30,
		}, allowed...)
		r1, _ := mk().Run(context.Background(), agent, json.RawMessage(`{}`), seed)
		r2, _ := mk().Run(context.Background(), agent, json.RawMessage(`{}`), seed)
		if len(r1.Steps) != len(r2.Steps) {
			t.Fatalf("step count differs: %d vs %d", len(r1.Steps), len(r2.Steps))
		}
		for i := range r1.Steps {
			if r1.Steps[i].Kind != r2.Steps[i].Kind {
				t.Fatalf("step %d kind differs", i)
			}
		}
		if r1.Phase != r2.Phase {
			t.Fatalf("phase differs: %s vs %s", r1.Phase, r2.Phase)
		}
	})
}

// Property: terminal phase is one of {Completed, Failed, Cancelled, Expired}.
func TestProperty_LifecycleProgresses(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nAllowed := rapid.IntRange(0, 2).Draw(t, "nAllowed")
		allowed := make([]string, nAllowed)
		for i := range allowed {
			allowed[i] = rapid.StringMatching("[a-z]{2,5}").Draw(t, "n")
		}
		nDecisions := rapid.IntRange(0, 10).Draw(t, "nDecisions")
		script := make([]rt.LLMDecision, nDecisions)
		for i := range script {
			script[i] = arbitraryDecision(t, allowed)
		}
		e := mkExecutor(script, allowed)
		agent := sampleAgent(arbitraryBudget(t), allowed...)
		res, _ := e.Run(context.Background(), agent, json.RawMessage(`{}`), 0)
		if !res.Phase.Terminal() {
			t.Fatalf("non-terminal phase %s", res.Phase)
		}
	})
}
