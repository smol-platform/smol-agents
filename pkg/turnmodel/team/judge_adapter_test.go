package team

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/agentjudge"
	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// passOnContains is a fake LLM judge that passes a candidate iff it contains the
// substring "answer:".
type passOnContains struct{}

func (passOnContains) Chat(_ context.Context, req agentruntime.ChatRequest) (rt.LLMDecision, error) {
	var in struct {
		Candidate string `json:"candidate"`
	}
	_ = json.Unmarshal(req.Input, &in)
	pass := false
	for i := 0; i+7 <= len(in.Candidate); i++ {
		if in.Candidate[i:i+7] == "answer:" {
			pass = true
			break
		}
	}
	v := `{"score":30,"pass":false,"comment":"no final answer yet"}`
	if pass {
		v = `{"score":95,"pass":true,"comment":"ok"}`
	}
	return rt.LLMDecision{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(v)}, TokensOut: 5}, nil
}

func TestJudgeVerifier_DrivesConvergenceLoop(t *testing.T) {
	judge := &agentjudge.Judge{LLM: passOnContains{}, Model: v1.ModelRef{Name: "judge"}}
	verify := JudgeVerifier(judge, agentjudge.JudgeSpec{})

	// Generator emits a bare draft on round 1, then a final "answer:" on round 2.
	gen := func(_ context.Context, round int, _ string) (Attempt, error) {
		if round == 1 {
			return Attempt{Content: "draft", Usage: v1.Usage{Tokens: 10}}, nil
		}
		return Attempt{Content: "answer: 42", Usage: v1.Usage{Tokens: 10}}, nil
	}
	res, err := RunGeneratorVerifier(context.Background(),
		v1.ConvergenceSpec{MaxIterations: 5, Criteria: "has a final answer"}, gen, verify, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Accepted || res.Rounds != 2 {
		t.Fatalf("judge-driven loop should accept on round 2: %+v", res)
	}
	// The judge's tokens (5/round) fold field-wise alongside the generator's.
	if res.Usage.Tokens != 30 { // 2 gens × 10 + 2 verifies × 5
		t.Fatalf("usage roll-up incl. judge tokens: want 30, got %d", res.Usage.Tokens)
	}
}
