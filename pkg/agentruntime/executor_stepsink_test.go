package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

type captureSink struct {
	mu    sync.Mutex
	steps []v1.Step
}

func (c *captureSink) Emit(_ context.Context, s v1.Step) {
	c.mu.Lock()
	c.steps = append(c.steps, s)
	c.mu.Unlock()
}

func TestExecutor_StepSink_EmitsEachStep(t *testing.T) {
	script := []rt.LLMDecision{
		{ToolCall: &rt.ToolCall{Tool: "noop", Arguments: json.RawMessage(`{}`)}, TokensIn: 1, TokensOut: 1},
		{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"a":1}`)}, TokensIn: 2, TokensOut: 2},
	}
	sink := &captureSink{}
	e := New()
	e.LLM = &FakeLLM{Script: script}
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Tools = map[string]v1.Tool{"noop": sampleTool("noop")}
	e.Invokers = map[v1.ToolKind]ToolInvoker{
		v1.ToolFunction: &InProcessInvoker{Handlers: map[string]func(json.RawMessage) (json.RawMessage, error){
			"noop": func(_ json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{"ok":true}`), nil },
		}},
	}
	e.StepSink = sink

	res, err := e.Run(context.Background(),
		sampleAgent(v1.Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 5}, "noop"),
		json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The sink saw exactly the steps the Result folded — live progress == final.
	if len(sink.steps) == 0 || len(sink.steps) != len(res.Steps) {
		t.Fatalf("sink saw %d steps, result has %d", len(sink.steps), len(res.Steps))
	}
}

func TestExecutor_NilStepSink_NoOp(t *testing.T) {
	e := New()
	e.LLM = &FakeLLM{Script: []rt.LLMDecision{{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{}`)}, TokensIn: 1, TokensOut: 1}}}
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	res, err := e.Run(context.Background(),
		sampleAgent(v1.Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 0}),
		json.RawMessage(`{}`), 0)
	if err != nil || res.Phase != v1.PhaseCompleted {
		t.Fatalf("nil sink must be a no-op: err=%v phase=%s", err, res.Phase)
	}
}

func TestExecutor_StepSink_RedactsBeforeEmit(t *testing.T) {
	sink := &captureSink{}
	pats, errs := v1.CompilePatterns([]string{"sk-secret[0-9]+"})
	if len(errs) != 0 {
		t.Fatalf("compile: %v", errs)
	}
	e := &Executor{StepSink: sink, RedactPatterns: pats}
	e.record(context.Background(), nil, v1.Step{Index: 0, Kind: v1.StepObservationRejected, Error: "leaked sk-secret42 here"})
	if len(sink.steps) != 1 {
		t.Fatalf("want 1 emitted step, got %d", len(sink.steps))
	}
	if strings.Contains(sink.steps[0].Error, "sk-secret42") {
		t.Fatalf("secret reached the progress stream unredacted: %q", sink.steps[0].Error)
	}
}
