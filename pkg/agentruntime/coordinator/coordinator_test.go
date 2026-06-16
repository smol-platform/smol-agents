package coordinator

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/agentjudge"
	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/turnmodel/team"
)

// fakeInvoker records every Invoke and returns a canned observation, so the
// A2ADispatcher's tool-building + observation-folding can be asserted without a
// real cluster.
type fakeInvoker struct {
	calls []struct {
		tool v1.Tool
		args json.RawMessage
	}
	obs rt.Observation
	err error
}

func (f *fakeInvoker) Invoke(_ context.Context, tool v1.Tool, args json.RawMessage) (rt.Observation, error) {
	f.calls = append(f.calls, struct {
		tool v1.Tool
		args json.RawMessage
	}{tool, args})
	return f.obs, f.err
}

// Dispatch builds a kind=agent tool for the member, threads objective+feedback+
// round into the child input, and folds the observation's tokens/toolCalls.
func TestA2ADispatcher_Dispatch(t *testing.T) {
	fi := &fakeInvoker{obs: rt.Observation{Output: json.RawMessage(`"the answer"`), Tokens: 12, ToolCalls: 3}}
	d := &A2ADispatcher{Invoker: fi, Member: "researcher", MaxTokens: 5000, TimeoutSeconds: 90}

	att, err := d.Dispatch(context.Background(), 2, "solve it", "try harder")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if att.Content != "the answer" {
		t.Errorf("content = %q, want unwrapped JSON string %q", att.Content, "the answer")
	}
	if att.Usage.Tokens != 12 || att.Usage.ToolCalls != 3 {
		t.Errorf("usage = %+v, want tokens=12 toolCalls=3 (field-wise from the observation)", att.Usage)
	}
	if len(fi.calls) != 1 {
		t.Fatalf("invoker calls = %d, want 1", len(fi.calls))
	}
	tool := fi.calls[0].tool
	if tool.Spec.Kind != v1.ToolAgent {
		t.Errorf("tool kind = %q, want agent", tool.Spec.Kind)
	}
	if tool.Spec.Agent == nil || tool.Spec.Agent.Ref.Name != "researcher" {
		t.Fatalf("tool.spec.agent = %+v, want ref.name=researcher", tool.Spec.Agent)
	}
	if tool.Spec.Agent.MaxTokens != 5000 || tool.Spec.Agent.TimeoutSeconds != 90 {
		t.Errorf("budget/timeout not threaded: %+v", tool.Spec.Agent)
	}
	var in memberInput
	if err := json.Unmarshal(fi.calls[0].args, &in); err != nil {
		t.Fatalf("args not valid memberInput JSON: %v", err)
	}
	if in.Objective != "solve it" || in.Feedback != "try harder" || in.Round != 2 {
		t.Errorf("member input = %+v, want {solve it, try harder, 2}", in)
	}
}

// A dispatch error propagates (fail-closed — no silent empty attempt).
func TestA2ADispatcher_InvokeErrorPropagates(t *testing.T) {
	fi := &fakeInvoker{err: context.DeadlineExceeded}
	d := &A2ADispatcher{Invoker: fi, Member: "m"}
	if _, err := d.Dispatch(context.Background(), 1, "obj", ""); err == nil {
		t.Error("want the invoker error to propagate, got nil")
	}
}

// observationContent unwraps a JSON string, passes structured JSON through, and
// maps empty to empty.
func TestObservationContent(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"hello"`, "hello"},                   // JSON string → unwrapped
		{`{"k":"v"}`, `{"k":"v"}`},             // object → verbatim
		{`[1,2,3]`, `[1,2,3]`},                 // array → verbatim
		{``, ""},                               // empty → empty
		{`"with \"quotes\""`, `with "quotes"`}, // escaped string unwrapped
	}
	for _, c := range cases {
		if got := observationContent(json.RawMessage(c.in)); got != c.want {
			t.Errorf("observationContent(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// End-to-end generator seam: the dispatcher drives team.Coordinate via
// GeneratorOverDispatch, and a rejecting-then-accepting verifier proves the
// rolling feedback reaches the member each round.
func TestA2ADispatcher_DrivesConvergenceLoop(t *testing.T) {
	fi := &fakeInvoker{obs: rt.Observation{Output: json.RawMessage(`"draft"`), Tokens: 4}}
	d := &A2ADispatcher{Invoker: fi, Member: "writer"}
	gen := team.GeneratorOverDispatch(d, "write the post")

	// Accept on round 2; round 1 rejects with feedback the dispatcher must relay.
	calls := 0
	verify := func(_ context.Context, a team.Attempt, criteria string) (team.Verdict, error) {
		calls++
		if calls >= 2 {
			return team.Verdict{Accepted: true, Score: 100}, nil
		}
		return team.Verdict{Accepted: false, Score: 1, Feedback: "add a conclusion"}, nil
	}

	cfg := team.CoordinatorConfig{Spec: v1.ConvergenceSpec{MaxIterations: 5, Criteria: "complete"}}
	res, err := team.Coordinate(context.Background(), cfg, gen, verify)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Accepted || res.Convergence.Rounds != 2 {
		t.Fatalf("want accepted in 2 rounds, got accepted=%v rounds=%d", res.Accepted, res.Convergence.Rounds)
	}
	if len(fi.calls) != 2 {
		t.Fatalf("dispatcher invoked %d times, want 2", len(fi.calls))
	}
	var r2 memberInput
	_ = json.Unmarshal(fi.calls[1].args, &r2)
	if r2.Round != 2 || r2.Feedback != "add a conclusion" {
		t.Errorf("round-2 member input = %+v, want round 2 with the verifier's feedback", r2)
	}
	if res.Convergence.Usage.Tokens != 8 {
		t.Errorf("usage tokens = %d, want 8 (2 rounds * 4 member tokens; verifier tokens 0)", res.Convergence.Usage.Tokens)
	}
}

// The judge verifier built from the loop LLM grades a member attempt: a passing
// scripted verdict accepts the turn on round 1 and the judge's tokens fold in.
func TestNewJudgeVerifier_GradesViaLoopLLM(t *testing.T) {
	llm := &agentruntime.FakeLLM{
		Script: []rt.LLMDecision{{
			FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"score":92,"pass":true,"comment":"lgtm"}`)},
			TokensIn:    3,
			TokensOut:   2,
		}},
	}
	verify := NewJudgeVerifier(llm, v1.ModelRef{ProviderRef: "prov", Name: "judge-model"}, agentjudge.JudgeSpec{})

	fi := &fakeInvoker{obs: rt.Observation{Output: json.RawMessage(`"a solid answer"`), Tokens: 7, ToolCalls: 1}}
	d := &A2ADispatcher{Invoker: fi, Member: "solver"}
	gen := team.GeneratorOverDispatch(d, "solve it")

	cfg := team.CoordinatorConfig{Spec: v1.ConvergenceSpec{MaxIterations: 3, Criteria: "be correct"}}
	res, err := team.Coordinate(context.Background(), cfg, gen, verify)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Accepted || res.Convergence.Rounds != 1 {
		t.Fatalf("want accepted on round 1, got accepted=%v rounds=%d stop=%s", res.Accepted, res.Convergence.Rounds, res.StopReason)
	}
	if res.Convergence.Verdict.Score != 92 || res.Convergence.Verdict.Feedback != "lgtm" {
		t.Errorf("verdict = %+v, want score 92 / lgtm from the scripted judge", res.Convergence.Verdict)
	}
	// Usage folds field-wise: member 7 tokens + judge 5 tokens (in+out).
	if res.Convergence.Usage.Tokens != 12 {
		t.Errorf("usage tokens = %d, want 12 (member 7 + judge 5)", res.Convergence.Usage.Tokens)
	}
}
