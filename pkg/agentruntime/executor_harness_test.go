package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime/harness"
)

// stubRunner is a fake HarnessRunner used to drive Mode=harness tests.
type stubRunner struct {
	output       []byte
	tokensIn     int64
	tokensOut    int64
	toolCalls    []v1.ToolCallRecord
	costUSDMilli int64
	err          error

	gotWorkingDir string // captured for the WorkingDir-binding test
	gotSession    string // captured resume session id (M3.19)
	sessionID     string // returned as Response.SessionID (M3.19)
}

func (s *stubRunner) RunHarness(_ context.Context, _ v1.HarnessSpec, _ string,
	_ json.RawMessage, workingDir string, _ map[string]string,
	_ v1.Budget, _ int64, sessionID string,
) (harness.Response, error) {
	s.gotWorkingDir = workingDir
	s.gotSession = sessionID
	return harness.Response{
		Output:       s.output,
		TokensIn:     s.tokensIn,
		TokensOut:    s.tokensOut,
		ToolCalls:    s.toolCalls,
		CostUSDMilli: s.costUSDMilli,
		SessionID:    s.sessionID,
	}, s.err
}

// M3.19: the executor threads ResumeSessionID into the harness and captures the
// harness's returned SessionID into the Result (folded to status.HarnessSessionID).
func TestExecutor_HarnessSessionResume(t *testing.T) {
	r := &stubRunner{output: []byte(`{"ok":true}`), sessionID: "sess-new"}
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Harness = r
	e.ResumeSessionID = "sess-prior"

	res, err := e.Run(context.Background(), harnessAgent(), json.RawMessage(`{"prompt":"hi"}`), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.gotSession != "sess-prior" {
		t.Errorf("ResumeSessionID not threaded to harness: got %q, want sess-prior", r.gotSession)
	}
	if res.SessionID != "sess-new" {
		t.Errorf("Result.SessionID = %q, want sess-new (captured from Response)", res.SessionID)
	}
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

// O1: the harness's Final step — and any tool-call log it surfaces — must reach
// the wire RunResult (and thus AgentRun.Status.Steps), not be dropped. Today
// chat harnesses surface no tool calls; this proves the plumbing carries them
// when they do (e.g. Hermes via the Responses API).
func TestExecutor_HarnessMode_StepsAndToolCallsSurfaced(t *testing.T) {
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Harness = &stubRunner{
		output:    []byte(`{"answer":"ok"}`),
		tokensIn:  10,
		tokensOut: 5,
		toolCalls: []v1.ToolCallRecord{{
			Tool:      "search",
			Arguments: json.RawMessage(`{"q":"x"}`),
			Result:    json.RawMessage(`"hit"`),
		}},
	}

	res, err := e.Run(context.Background(), harnessAgent(), json.RawMessage(`{"prompt":"hi"}`), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wire := ResultToWire(res, nil)
	if len(wire.Steps) != 1 {
		t.Fatalf("want 1 step on the wire, got %d", len(wire.Steps))
	}
	if wire.Steps[0].Kind != v1.StepFinal {
		t.Errorf("step kind = %s, want Final", wire.Steps[0].Kind)
	}
	if len(wire.Steps[0].ToolCalls) != 1 || wire.Steps[0].ToolCalls[0].Tool != "search" {
		t.Errorf("harness tool calls not surfaced into the Step: %+v", wire.Steps[0].ToolCalls)
	}
	if res.Usage.ToolCalls != 1 {
		t.Errorf("usage.ToolCalls = %d, want 1", res.Usage.ToolCalls)
	}
}

// M2.8: a harness-reported cost folds into Usage.CostUSDMilli (observability
// only) and never affects the budget verdict.
func TestExecutor_HarnessMode_CostFolds(t *testing.T) {
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	e.Harness = &stubRunner{output: []byte(`{"answer":"ok"}`), tokensIn: 10, tokensOut: 5, costUSDMilli: 1234}

	res, err := e.Run(context.Background(), harnessAgent(), json.RawMessage(`{"prompt":"hi"}`), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Phase != v1.PhaseCompleted {
		t.Errorf("phase=%s, want Completed (cost must never gate)", res.Phase)
	}
	if res.Usage.CostUSDMilli != 1234 {
		t.Errorf("usage.CostUSDMilli = %d, want 1234", res.Usage.CostUSDMilli)
	}
}

// F1: when an Agent has durable AgentFS storage but no explicit CLI working dir,
// the harness must run in the AgentFS mount (so its writes hit the backed-up
// volume), not /tmp. Regression test for the WorkingDir that was never bound.
func TestExecutor_HarnessMode_WorkingDirBindsToAgentFS(t *testing.T) {
	e := New()
	e.Clock = &FakeClock{T: time.Unix(0, 0)}
	runner := &stubRunner{output: []byte(`{"ok":true}`)}
	e.Harness = runner

	a := harnessAgent()
	a.Spec.Storage = &v1.StorageSpec{Kind: v1.StorageAgentFS, AgentFS: &v1.AgentFSSpec{SizeGiB: 1}}
	if _, err := e.Run(context.Background(), a, json.RawMessage(`{"prompt":"hi"}`), 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.gotWorkingDir != v1.DefaultAgentFSMountPath {
		t.Errorf("harness workingDir = %q, want the AgentFS mount %q", runner.gotWorkingDir, v1.DefaultAgentFSMountPath)
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
	// Non-JSON text is encoded as a JSON string (AgentRun.status.output accepts
	// any JSON value), round-tripping to the original text verbatim.
	var s string
	if err := json.Unmarshal(res.Output, &s); err != nil {
		t.Fatalf("Output should decode as a JSON string: %v (got %q)", err, res.Output)
	}
	if s != prose {
		t.Errorf("round-tripped output = %q, want %q", s, prose)
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
	_ json.RawMessage, _ string, env map[string]string, _ v1.Budget, _ int64, _ string,
) (harness.Response, error) {
	s.gotEnv = env
	return harness.Response{Output: []byte(`{"ok":true}`), TokensIn: 1, TokensOut: 1}, nil
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
