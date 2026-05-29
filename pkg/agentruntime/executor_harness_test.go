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

// A harness that returns non-JSON text (the common case — a reasoning model
// explaining its steps in `content`, not a bare JSON object) must still
// produce a Result whose Output is valid JSON, so the whole RunResult
// marshals. Regression test for the silent-empty-output bug: non-JSON bytes in
// a json.RawMessage made json.Marshal fail, and the run entrypoint swallowed
// the error, emitting empty output.
func TestExecutor_HarnessMode_NonJSONOutputWrapped(t *testing.T) {
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	prose := "Step 1: F(12) = 144\nFinal line: {\"fib12\": 144}"
	e.Harness = &stubRunner{output: []byte(prose), tokensIn: 10, tokensOut: 5}

	res, err := e.Run(context.Background(), harnessAgent(), json.RawMessage(`{"prompt":"hi"}`), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Phase != v1.PhaseCompleted {
		t.Errorf("phase=%s, want Completed", res.Phase)
	}
	if !json.Valid(res.Output) {
		t.Fatalf("Output must be valid JSON, got %q", res.Output)
	}
	// The whole wire result must marshal (this is what silently failed before).
	if _, err := json.Marshal(ResultToWire(res, nil)); err != nil {
		t.Fatalf("RunResult must marshal: %v", err)
	}
	// Output must be a JSON object — AgentRun's status.output is object-typed,
	// so a bare JSON string would be rejected by the CRD. Non-object text is
	// wrapped as {"output": "..."} holding the original text verbatim.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(res.Output, &m); err != nil {
		t.Fatalf("Output must decode as a JSON object: %v (got %q)", err, res.Output)
	}
	var s string
	if err := json.Unmarshal(m["output"], &s); err != nil || s != prose {
		t.Errorf("output.output = %q (err %v), want %q", s, err, prose)
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
