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

// envCapturingRunner records the env the executor resolves for the harness.
type envCapturingRunner struct{ gotEnv map[string]string }

func (s *envCapturingRunner) RunHarness(_ context.Context, _ v1.HarnessSpec, _ string,
	_ json.RawMessage, _ string, env map[string]string, _ v1.Budget, _ int64,
) ([]byte, int64, int64, int64, error) {
	s.gotEnv = env
	return []byte(`{"ok":true}`), 1, 1, 0, nil
}

type fakeLeaser struct {
	vals map[string]string
	err  error
}

func (f fakeLeaser) LeaseSecret(_ context.Context, name string, _ time.Duration) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.vals[name]
	if !ok {
		return nil, errors.New("secret not found: " + name)
	}
	return []byte(v), nil
}

func TestExecutor_HarnessMode_ResolvesSecretRefEnv(t *testing.T) {
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	runner := &envCapturingRunner{}
	e.Harness = runner
	e.Secrets = fakeLeaser{vals: map[string]string{"gw-token": "Bearer s3cr3t"}}

	a := harnessAgent()
	a.Spec.Harness.Env = []v1.HarnessEnvVar{
		{Name: "HEADER_Authorization", SecretRef: &v1.AuthRef{SecretName: "gw-token"}},
		{Name: "HERMES_MODEL", Value: "hermes-agent"}, // literal — must NOT enter the broker env map
	}
	if _, err := e.Run(context.Background(), a, json.RawMessage(`{"prompt":"hi"}`), 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.gotEnv["HEADER_Authorization"] != "Bearer s3cr3t" {
		t.Errorf("resolved env = %v, want HEADER_Authorization leased", runner.gotEnv)
	}
	if _, ok := runner.gotEnv["HERMES_MODEL"]; ok {
		t.Error("literal env leaked into the broker-resolved env map (harness reads literals itself)")
	}
}

func TestExecutor_HarnessMode_SecretRefWithoutBrokerErrors(t *testing.T) {
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Harness = &stubRunner{output: []byte(`{}`)}
	// e.Secrets is nil — a declared secretRef must fail loudly.
	a := harnessAgent()
	a.Spec.Harness.Env = []v1.HarnessEnvVar{{Name: "HEADER_Authorization", SecretRef: &v1.AuthRef{SecretName: "x"}}}
	if _, err := e.Run(context.Background(), a, json.RawMessage(`{}`), 0); err == nil {
		t.Fatal("expected error: secretRef declared with no broker configured")
	}
}
