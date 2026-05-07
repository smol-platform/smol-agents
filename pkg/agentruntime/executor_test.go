package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	rt "github.com/stigen/knative-agents/pkg/agentmodel/runtime"
	v1 "github.com/stigen/knative-agents/pkg/agentmodel/v1"
)

func sampleAgent(budget v1.Budget, tools ...string) v1.Agent {
	a := v1.Agent{Spec: v1.AgentSpec{
		Model:        v1.ModelRef{ProviderRef: "openai", Name: "gpt-4"},
		Instructions: "be helpful",
		Budget:       budget,
	}}
	for _, t := range tools {
		a.Spec.Tools = append(a.Spec.Tools, v1.ToolRef{Name: t})
	}
	return a
}

func sampleTool(name string) v1.Tool {
	return v1.Tool{
		Name: name,
		Spec: v1.ToolSpec{
			Kind:         v1.ToolFunction,
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
			Function:     &v1.FunctionSpec{Name: name},
		},
	}
}

func TestExecutor_HappyPath_FinalAnswerImmediately(t *testing.T) {
	llm := &FakeLLM{Script: []rt.LLMDecision{{
		FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"a":1}`)},
		TokensIn:    10, TokensOut: 5,
	}}}
	e := New()
	e.LLM = llm
	e.Clock = &FakeClock{T: time.Unix(0, 0)}

	res, err := e.Run(context.Background(),
		sampleAgent(v1.Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 0}),
		json.RawMessage(`{"q":"hi"}`),
		42)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Phase != v1.PhaseCompleted {
		t.Errorf("phase=%s, want Completed", res.Phase)
	}
	if string(res.Output) != `{"a":1}` {
		t.Errorf("output=%v", res.Output)
	}
}

func TestExecutor_BudgetExpired_OnSteps(t *testing.T) {
	// Script demands more steps than budget allows.
	script := []rt.LLMDecision{
		{ToolCall: &rt.ToolCall{Tool: "noop", Arguments: json.RawMessage(`{}`)}, TokensIn: 1, TokensOut: 1},
		{ToolCall: &rt.ToolCall{Tool: "noop", Arguments: json.RawMessage(`{}`)}, TokensIn: 1, TokensOut: 1},
		{ToolCall: &rt.ToolCall{Tool: "noop", Arguments: json.RawMessage(`{}`)}, TokensIn: 1, TokensOut: 1},
	}
	e := New()
	e.LLM = &FakeLLM{Script: script}
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Tools = map[string]v1.Tool{"noop": sampleTool("noop")}
	e.Invokers = map[v1.ToolKind]ToolInvoker{
		v1.ToolFunction: &InProcessInvoker{Handlers: map[string]func(json.RawMessage) (json.RawMessage, error){
			"noop": func(_ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{"ok":true}`), nil },
		}},
	}
	res, err := e.Run(context.Background(),
		sampleAgent(v1.Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 5}, "noop"),
		json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != v1.PhaseExpired {
		t.Errorf("phase=%s, want Expired", res.Phase)
	}
	if !strings.HasPrefix(res.TerminationReason, "budget:") {
		t.Errorf("reason=%q", res.TerminationReason)
	}
}

func TestExecutor_RejectsToolNotInAllowList(t *testing.T) {
	script := []rt.LLMDecision{
		{ToolCall: &rt.ToolCall{Tool: "evil", Arguments: json.RawMessage(`{}`)}, TokensIn: 1, TokensOut: 1},
		{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"done":true}`)}},
	}
	e := New()
	e.LLM = &FakeLLM{Script: script}
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Tools = map[string]v1.Tool{"safe": sampleTool("safe")}
	e.Invokers = map[v1.ToolKind]ToolInvoker{v1.ToolFunction: &InProcessInvoker{Handlers: map[string]func(json.RawMessage) (json.RawMessage, error){}}}

	res, _ := e.Run(context.Background(),
		sampleAgent(v1.Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 5}, "safe"),
		json.RawMessage(`{}`), 0)

	// Should have a ToolCallRejected step somewhere.
	found := false
	for _, s := range res.Steps {
		if s.Kind == v1.StepToolCallRejected {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a StepToolCallRejected step; got steps: %+v", res.Steps)
	}
	if res.Phase != v1.PhaseCompleted {
		t.Errorf("phase=%s, want Completed (LLM eventually finalises)", res.Phase)
	}
}

func TestExecutor_InvalidArgs_RejectedNotCounted(t *testing.T) {
	script := []rt.LLMDecision{
		{ToolCall: &rt.ToolCall{Tool: "f", Arguments: json.RawMessage(`not json`)}, TokensIn: 1, TokensOut: 1},
		{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"end":true}`)}},
	}
	e := New()
	e.LLM = &FakeLLM{Script: script}
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Tools = map[string]v1.Tool{"f": sampleTool("f")}
	e.Invokers = map[v1.ToolKind]ToolInvoker{v1.ToolFunction: &InProcessInvoker{Handlers: map[string]func(json.RawMessage) (json.RawMessage, error){
		"f": func(_ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	}}}
	res, _ := e.Run(context.Background(),
		sampleAgent(v1.Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 5}, "f"),
		json.RawMessage(`{}`), 0)

	if res.Usage.ToolCalls != 0 {
		t.Errorf("invalid args should not increment ToolCalls: got %d", res.Usage.ToolCalls)
	}
}

func TestExecutor_Determinism_SameSeedSameLog(t *testing.T) {
	mkExec := func() *Executor {
		e := New()
		e.LLM = &FakeLLM{Script: []rt.LLMDecision{
			{ToolCall: &rt.ToolCall{Tool: "f", Arguments: json.RawMessage(`{"x":1}`)}, TokensIn: 1, TokensOut: 1},
			{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"x":2}`)}, TokensIn: 1, TokensOut: 1},
		}}
		e.Clock = &FakeClock{T: time.Unix(0, 0)}
		e.Tools = map[string]v1.Tool{"f": sampleTool("f")}
		e.Invokers = map[v1.ToolKind]ToolInvoker{v1.ToolFunction: &InProcessInvoker{Handlers: map[string]func(json.RawMessage) (json.RawMessage, error){
			"f": func(_ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{"r":1}`), nil },
		}}}
		return e
	}
	agent := sampleAgent(v1.Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 5}, "f")
	r1, _ := mkExec().Run(context.Background(), agent, json.RawMessage(`{"q":"hi"}`), 42)
	r2, _ := mkExec().Run(context.Background(), agent, json.RawMessage(`{"q":"hi"}`), 42)
	if len(r1.Steps) != len(r2.Steps) {
		t.Fatalf("step count differs: %d vs %d", len(r1.Steps), len(r2.Steps))
	}
	for i := range r1.Steps {
		if r1.Steps[i].Kind != r2.Steps[i].Kind {
			t.Errorf("step %d kind differs: %s vs %s", i, r1.Steps[i].Kind, r2.Steps[i].Kind)
		}
	}
}

func TestExecutor_Cancellation(t *testing.T) {
	llm := &FakeLLM{Script: []rt.LLMDecision{
		{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"ok":true}`)}},
	}}
	e := New()
	e.LLM = llm
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, _ := e.Run(ctx,
		sampleAgent(v1.Budget{MaxSteps: 5, MaxTokens: 100, MaxWallClockSeconds: 30, MaxToolCalls: 0}),
		json.RawMessage(`{}`), 0)
	if res.Phase != v1.PhaseCancelled {
		t.Errorf("phase=%s want Cancelled", res.Phase)
	}
}

func TestExecutor_RequiresLLM(t *testing.T) {
	e := New()
	_, err := e.Run(context.Background(), sampleAgent(v1.Budget{MaxSteps: 1, MaxTokens: 1, MaxWallClockSeconds: 1, MaxToolCalls: 0}), json.RawMessage(`{}`), 0)
	if err == nil {
		t.Fatal("expected error when LLM nil")
	}
}
