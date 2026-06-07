package agentjudge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// fakeLLM returns a canned verdict (or pass/fail decided by a func of the
// candidate, for calibration tests).
type fakeLLM struct {
	verdict func(candidate string) string
	tin     int64
	tout    int64
	noFinal bool
	err     error
}

func (f *fakeLLM) Chat(_ context.Context, req agentruntime.ChatRequest) (rt.LLMDecision, error) {
	if f.err != nil {
		return rt.LLMDecision{}, f.err
	}
	if f.noFinal {
		return rt.LLMDecision{TokensIn: f.tin, TokensOut: f.tout}, nil
	}
	// Extract the candidate from the input the judge sent.
	var in struct {
		Candidate string `json:"candidate"`
	}
	_ = json.Unmarshal(req.Input, &in)
	return rt.LLMDecision{
		FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(f.verdict(in.Candidate))},
		TokensIn:    f.tin, TokensOut: f.tout,
	}, nil
}

func const_(s string) func(string) string { return func(string) string { return s } }

func TestJudge_GradeParsesVerdictAndFoldsUsage(t *testing.T) {
	j := &Judge{LLM: &fakeLLM{verdict: const_(`{"score":88,"pass":true,"comment":"cites sources"}`), tin: 40, tout: 20}, Model: v1.ModelRef{Name: "judge-model"}}
	jd, err := j.Grade(context.Background(), JudgeSpec{Criteria: "cites a primary source"}, "the answer [1]")
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if !jd.Pass || jd.Score != 88 || jd.Feedback != "cites sources" {
		t.Fatalf("verdict parse wrong: %+v", jd)
	}
	if jd.Usage.Tokens != 60 || jd.Usage.ToolCalls != 0 {
		t.Fatalf("usage fold: want 60 tokens / 0 calls (obs-only), got %d/%d", jd.Usage.Tokens, jd.Usage.ToolCalls)
	}
	if jd.JudgeModel != "judge-model" {
		t.Fatalf("judge model not recorded: %q", jd.JudgeModel)
	}
}

func TestJudge_GradeClampsAndErrors(t *testing.T) {
	// Out-of-range score is clamped to [0,100].
	j := &Judge{LLM: &fakeLLM{verdict: const_(`{"score":150,"pass":true}`)}, Model: v1.ModelRef{Name: "m"}}
	jd, _ := j.Grade(context.Background(), JudgeSpec{Criteria: "c"}, "x")
	if jd.Score != 100 {
		t.Fatalf("score clamp: want 100, got %d", jd.Score)
	}
	// Missing criteria → error.
	if _, err := j.Grade(context.Background(), JudgeSpec{}, "x"); err == nil {
		t.Fatal("missing criteria must error")
	}
	// No final verdict → ErrNoVerdict.
	jn := &Judge{LLM: &fakeLLM{noFinal: true}, Model: v1.ModelRef{Name: "m"}}
	if _, err := jn.Grade(context.Background(), JudgeSpec{Criteria: "c"}, "x"); !errors.Is(err, ErrNoVerdict) {
		t.Fatalf("want ErrNoVerdict, got %v", err)
	}
	// Non-JSON verdict → error.
	jb := &Judge{LLM: &fakeLLM{verdict: const_("not json")}, Model: v1.ModelRef{Name: "m"}}
	if _, err := jb.Grade(context.Background(), JudgeSpec{Criteria: "c"}, "x"); err == nil {
		t.Fatal("non-JSON verdict must error")
	}
}

func TestJudge_Calibrate(t *testing.T) {
	// Judge passes a candidate iff it contains "good"; calibrate against goldens.
	llm := &fakeLLM{verdict: func(c string) string {
		pass := false
		for i := 0; i+4 <= len(c); i++ {
			if c[i:i+4] == "good" {
				pass = true
				break
			}
		}
		if pass {
			return `{"score":90,"pass":true}`
		}
		return `{"score":20,"pass":false}`
	}}
	j := &Judge{LLM: llm, Model: v1.ModelRef{Name: "m"}}
	cases := []CalibrationCase{
		{Candidate: "good answer", ExpectedPass: true},
		{Candidate: "bad answer", ExpectedPass: false},
		{Candidate: "good again", ExpectedPass: true},
		{Candidate: "good but expected fail", ExpectedPass: false}, // judge disagrees here
	}
	rep, err := j.Calibrate(context.Background(), JudgeSpec{Criteria: "is it good"}, cases)
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if rep.Total != 4 || rep.Agreements != 3 {
		t.Fatalf("calibrate: want 3/4 agreements, got %d/%d", rep.Agreements, rep.Total)
	}
	if !rep.Meets(0.75) || rep.Meets(0.8) {
		t.Fatalf("agreement-rate gate wrong: rate=%.2f", rep.AgreementRate)
	}
}
