package agentruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// scriptedVerifier rejects the first N calls (with feedback + usage), then accepts.
type scriptedVerifier struct {
	rejectFirst int
	calls       int
}

func (s *scriptedVerifier) Verify(_ context.Context, _ json.RawMessage, _ string) (VerifyResult, error) {
	s.calls++
	if s.calls <= s.rejectFirst {
		return VerifyResult{Accepted: false, Score: 20, Feedback: "add a citation", Usage: v1.Usage{Tokens: 3}}, nil
	}
	return VerifyResult{Accepted: true, Score: 95, Usage: v1.Usage{Tokens: 3}}, nil
}

func TestExecutor_Verifier_RepairsThenAccepts(t *testing.T) {
	// Two terminal answers in a row; the verifier rejects the first → the loop
	// injects feedback and continues → the second is accepted.
	script := []rt.LLMDecision{
		{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"a":"draft"}`)}, TokensIn: 5, TokensOut: 5},
		{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"a":"final [1]"}`)}, TokensIn: 5, TokensOut: 5},
	}
	v := &scriptedVerifier{rejectFirst: 1}
	e := New()
	e.LLM = &FakeLLM{Script: script}
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Verifier = v
	e.VerifyCriteria = "has a citation"
	e.MaxRepairRounds = 2

	res, err := e.Run(context.Background(),
		sampleAgent(v1.Budget{MaxSteps: 10, MaxTokens: 100000, MaxWallClockSeconds: 60, MaxToolCalls: 0}),
		json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Phase != v1.PhaseCompleted {
		t.Fatalf("phase: want Completed, got %s (%s)", res.Phase, res.TerminationReason)
	}
	if string(res.Output) != `{"a":"final [1]"}` {
		t.Fatalf("want the revised answer, got %s", res.Output)
	}
	if v.calls != 2 {
		t.Fatalf("verifier should run twice (reject+accept), ran %d", v.calls)
	}
}

func TestExecutor_Verifier_UnconvergedReturnsBestOrFails(t *testing.T) {
	// Verifier always rejects; with MaxRepairRounds=1 the loop gives up.
	mkScript := func() []rt.LLMDecision {
		d := rt.LLMDecision{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"a":1}`)}, TokensIn: 1, TokensOut: 1}
		return []rt.LLMDecision{d, d, d, d}
	}
	// return-best (default): Completed + verify:unconverged.
	e := New()
	e.LLM = &FakeLLM{Script: mkScript()}
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Verifier = &scriptedVerifier{rejectFirst: 99}
	e.MaxRepairRounds = 1
	res, _ := e.Run(context.Background(),
		sampleAgent(v1.Budget{MaxSteps: 10, MaxTokens: 100000, MaxWallClockSeconds: 60, MaxToolCalls: 0}),
		json.RawMessage(`{}`), 0)
	if res.Phase != v1.PhaseCompleted || res.TerminationReason != "verify:unconverged" {
		t.Fatalf("return-best: want Completed/verify:unconverged, got %s/%s", res.Phase, res.TerminationReason)
	}

	// fail-closed: Failed + verify:rejected.
	e2 := New()
	e2.LLM = &FakeLLM{Script: mkScript()}
	e2.Clock = &FakeClock{T: time.Unix(0, 0)}
	e2.Verifier = &scriptedVerifier{rejectFirst: 99}
	e2.MaxRepairRounds = 1
	e2.VerifyFailOnReject = true
	res2, _ := e2.Run(context.Background(),
		sampleAgent(v1.Budget{MaxSteps: 10, MaxTokens: 100000, MaxWallClockSeconds: 60, MaxToolCalls: 0}),
		json.RawMessage(`{}`), 0)
	if res2.Phase != v1.PhaseFailed || res2.TerminationReason != "verify:rejected" {
		t.Fatalf("fail-closed: want Failed/verify:rejected, got %s/%s", res2.Phase, res2.TerminationReason)
	}
}
