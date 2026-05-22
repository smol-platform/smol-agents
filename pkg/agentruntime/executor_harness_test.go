package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// stubRunner is a fake HarnessRunner used to drive Mode=harness tests.
type stubRunner struct {
	output    []byte
	tokensIn  int64
	tokensOut int64
	err       error
}

func (s *stubRunner) RunHarness(_ context.Context, _ v1.HarnessSpec, _ string,
	_ json.RawMessage, _ string, _ map[string]string,
	_ v1.Budget, _ int64,
) ([]byte, int64, int64, int64, error) {
	return s.output, s.tokensIn, s.tokensOut, 0, s.err
}

func harnessAgent() v1.Agent {
	return v1.Agent{Spec: v1.AgentSpec{
		Mode:         v1.ModeHarness,
		Instructions: "be a code reviewer",
		Budget:       v1.Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 0},
		Harness:      &v1.HarnessSpec{Kind: v1.HarnessClaudeCode},
	}}
}

func TestExecutor_HarnessMode_Completed(t *testing.T) {
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Harness = &stubRunner{output: []byte(`{"answer":"ok"}`), tokensIn: 100, tokensOut: 50}

	res, err := e.Run(context.Background(), harnessAgent(), json.RawMessage(`{"prompt":"hi"}`), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Phase != v1.PhaseCompleted {
		t.Errorf("phase=%s, want Completed", res.Phase)
	}
	if string(res.Output) != `{"answer":"ok"}` {
		t.Errorf("output=%v", res.Output)
	}
	if res.Usage.Steps != 1 || res.Usage.Tokens != 150 {
		t.Errorf("usage=%+v", res.Usage)
	}
}

func TestExecutor_HarnessMode_BudgetExpiredOnTokens(t *testing.T) {
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Harness = &stubRunner{output: []byte(`{"answer":"ok"}`), tokensIn: 1000, tokensOut: 1000}
	a := harnessAgent()
	a.Spec.Budget.MaxTokens = 100

	res, _ := e.Run(context.Background(), a, json.RawMessage(`{"prompt":"hi"}`), 0)
	if res.Phase != v1.PhaseExpired {
		t.Errorf("phase=%s, want Expired", res.Phase)
	}
	if res.TerminationReason != "budget:tokens" {
		t.Errorf("reason=%q", res.TerminationReason)
	}
}

func TestExecutor_HarnessMode_HarnessError(t *testing.T) {
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Harness = &stubRunner{err: errors.New("boom")}

	res, _ := e.Run(context.Background(), harnessAgent(), json.RawMessage(`{"prompt":"hi"}`), 0)
	if res.Phase != v1.PhaseFailed {
		t.Errorf("phase=%s, want Failed", res.Phase)
	}
}

func TestExecutor_HarnessMode_RequiresRunner(t *testing.T) {
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	_, err := e.Run(context.Background(), harnessAgent(), json.RawMessage(`{}`), 0)
	if err == nil {
		t.Fatal("expected error when Harness nil")
	}
}
