package agentbench

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// TestLoadPlan_DecodesAndValidates loads the testdata plan, confirms it decodes
// across all three case-file shapes (inline, list-wrapper, single-doc), and
// asserts every oracle.kind in it is registered.
func TestLoadPlan_DecodesAndValidates(t *testing.T) {
	plan, err := LoadPlan("testdata")
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if plan.Metadata.Name != "testdata-core" {
		t.Fatalf("plan name = %q, want testdata-core", plan.Metadata.Name)
	}
	// 2 inline + 2 from tools.bench.yaml + 1 from secrets.bench.yaml = 5.
	if len(plan.Cases) != 5 {
		names := make([]string, len(plan.Cases))
		for i, c := range plan.Cases {
			names[i] = c.Metadata.Name
		}
		t.Fatalf("got %d cases %v, want 5", len(plan.Cases), names)
	}

	// Every oracle kind referenced must be registered (the coverage gate).
	for _, c := range plan.Cases {
		if !IsRegistered(c.Oracle.Kind) {
			t.Errorf("case %q references unregistered oracle kind %q", c.Metadata.Name, c.Oracle.Kind)
		}
	}

	// The digest must be stable + well-formed.
	d := plan.Digest()
	if !strings.HasPrefix(d, "sha256:") || len(d) != len("sha256:")+64 {
		t.Errorf("digest %q is malformed", d)
	}
	if d2 := plan.Digest(); d2 != d {
		t.Errorf("digest not stable: %q != %q", d, d2)
	}
}

// TestCasesForTier_FuturePark verifies that a future/blocked case is excluded
// without --allow-blocked and included with it.
func TestCasesForTier_FuturePark(t *testing.T) {
	plan, err := LoadPlan("testdata")
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	without := plan.CasesForTier("", false)
	with := plan.CasesForTier("", true)
	if len(with) <= len(without) {
		t.Fatalf("expected --allow-blocked to add the future case: with=%d without=%d", len(with), len(without))
	}
	for _, c := range without {
		if c.Tier == TierFuture || c.Blocked != nil {
			t.Errorf("future/blocked case %q leaked without --allow-blocked", c.Metadata.Name)
		}
	}
}

// TestValidate_RejectsBadPlans covers each validation rule with a minimal
// failing plan decoded from YAML bytes.
func TestValidate_RejectsBadPlans(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "unregistered oracle kind",
			yaml: `
apiVersion: agentbench/v1
kind: BenchPlan
metadata: { name: bad }
cases:
  - metadata: { name: c1 }
    tier: correctness
    agentRef: a
    oracle: { kind: does_not_exist }
`,
			wantSub: `oracle.kind="does_not_exist" is not registered`,
		},
		{
			name: "numeric gate but too few samples",
			yaml: `
apiVersion: agentbench/v1
kind: BenchPlan
metadata: { name: bad }
cases:
  - metadata: { name: c1 }
    tier: perf
    agentRef: a
    samples: 2
    oracle: { kind: output_match, want: x }
    gates:
      - { metric: latency.p95.ms, op: lte, want: 100 }
`,
			wantSub: "requires samples>=3",
		},
		{
			name: "blocked case with a positive oracle",
			yaml: `
apiVersion: agentbench/v1
kind: BenchPlan
metadata: { name: bad }
cases:
  - metadata: { name: c1 }
    tier: future
    agentRef: a
    oracle: { kind: output_match, want: x }
    blocked: { reason: nope }
`,
			wantSub: "must carry a negative oracle",
		},
		{
			name: "bad tier",
			yaml: `
apiVersion: agentbench/v1
kind: BenchPlan
metadata: { name: bad }
cases:
  - metadata: { name: c1 }
    tier: bogus
    agentRef: a
    oracle: { kind: output_match, want: x }
`,
			wantSub: "not in the enum",
		},
		{
			name: "wrong apiVersion",
			yaml: `
apiVersion: agentbench/v2
kind: BenchPlan
metadata: { name: bad }
cases:
  - metadata: { name: c1 }
    tier: correctness
    agentRef: a
    oracle: { kind: output_match, want: x }
`,
			wantSub: "apiVersion",
		},
		{
			name: "missing agentRef",
			yaml: `
apiVersion: agentbench/v1
kind: BenchPlan
metadata: { name: bad }
cases:
  - metadata: { name: c1 }
    tier: correctness
    oracle: { kind: output_match, want: x }
`,
			wantSub: "agentRef is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := decodeYAMLPlan(t, tc.yaml)
			err := plan.Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestToolRejectedOracle_Tripwire is the anti-staleness assertion: the negative
// oracle returns Blocked while the rejection is present, and FAILs (forcing a
// flip-to-positive) when the rejection disappears.
func TestToolRejectedOracle_Tripwire(t *testing.T) {
	o, ok := LookupOracle("tool_rejected")
	if !ok {
		t.Fatal("tool_rejected not registered")
	}
	rejected := pure.RunStatus{
		State: pure.PhaseFailed,
		Steps: []pure.Step{{
			Index: 0, Kind: pure.StepToolCallRejected,
			Error: `no invoker for kind "http"`,
		}},
	}
	if v := o.Check(rejected, CollectCtx{}); v.Kind != VerdictBlocked {
		t.Errorf("rejection present: got %q, want blocked (%s)", v.Kind, v.Evidence)
	}
	// Gap got fixed: no rejection step → must FAIL so the stale plan is caught.
	wired := pure.RunStatus{
		State: pure.PhaseCompleted,
		Steps: []pure.Step{{Index: 0, Kind: pure.StepObservation}},
	}
	v := o.Check(wired, CollectCtx{})
	if v.Kind != VerdictFail {
		t.Errorf("rejection absent: got %q, want fail (anti-staleness tripwire)", v.Kind)
	}
	if !strings.Contains(v.Evidence, "STALE") {
		t.Errorf("expected STALE hint in evidence, got %q", v.Evidence)
	}
}

// TestSecretAbsentOracle_KindAware verifies the kind-aware inversion: on the
// Hermes path the secret must be ABSENT (leak = fail); on a CLI path the secret
// is EXPECTED present (not-blind contract).
func TestSecretAbsentOracle_KindAware(t *testing.T) {
	o, _ := LookupOracle("secret_absent")
	secret := "s3cr3t-value"
	withSecret := pure.RunStatus{Output: json.RawMessage(`"SEEN=` + secret + `"`)}
	clean := pure.RunStatus{Output: json.RawMessage(`"done"`)}
	vals := map[string]string{"token": secret}

	// Hermes (HTTP, blind): leak → fail.
	if v := o.Check(withSecret, CollectCtx{HarnessKind: "hermes", SecretValues: vals}); v.Kind != VerdictFail {
		t.Errorf("hermes leak: got %q want fail (%s)", v.Kind, v.Evidence)
	}
	// Hermes clean → pass.
	if v := o.Check(clean, CollectCtx{HarnessKind: "hermes", SecretValues: vals}); v.Kind != VerdictPass {
		t.Errorf("hermes clean: got %q want pass (%s)", v.Kind, v.Evidence)
	}
	// CLI (not blind): secret present → pass (inverted).
	if v := o.Check(withSecret, CollectCtx{HarnessKind: "claude-code", SecretValues: vals}); v.Kind != VerdictPass {
		t.Errorf("cli present: got %q want pass (%s)", v.Kind, v.Evidence)
	}
	// CLI but secret absent → fail (not-blind contract violated).
	if v := o.Check(clean, CollectCtx{HarnessKind: "claude-code", SecretValues: vals}); v.Kind != VerdictFail {
		t.Errorf("cli absent: got %q want fail (%s)", v.Kind, v.Evidence)
	}
	// Fail-closed: no secret values supplied.
	if v := o.Check(clean, CollectCtx{HarnessKind: "hermes"}); v.Kind != VerdictFail {
		t.Errorf("no secret values: got %q want fail (fail-closed)", v.Kind)
	}
}

// TestBudgetTerminatedOracle exercises the budget: prefix + exact-reason paths.
func TestBudgetTerminatedOracle(t *testing.T) {
	o, _ := LookupOracle("budget_terminated")
	expired := pure.RunStatus{State: pure.PhaseExpired, TerminationReason: "budget:tokens"}
	exact := BenchCase{Oracle: Oracle{Kind: "budget_terminated", Want: "budget:tokens"}}
	if v := o.Check(expired, CollectCtx{Case: exact}); v.Kind != VerdictPass {
		t.Errorf("exact reason: got %q want pass (%s)", v.Kind, v.Evidence)
	}
	wrong := BenchCase{Oracle: Oracle{Kind: "budget_terminated", Want: "budget:wallclock"}}
	if v := o.Check(expired, CollectCtx{Case: wrong}); v.Kind != VerdictFail {
		t.Errorf("wrong reason: got %q want fail", v.Kind)
	}
	natural := pure.RunStatus{State: pure.PhaseCompleted}
	if v := o.Check(natural, CollectCtx{Case: BenchCase{Oracle: Oracle{Kind: "budget_terminated"}}}); v.Kind != VerdictFail {
		t.Errorf("natural completion: got %q want fail", v.Kind)
	}
}

// TestIsolationKernelOracle_SkipsWithoutKata asserts the SKIP-not-fail downgrade
// when no kata-fc RuntimeClass is present.
func TestIsolationKernelOracle_SkipsWithoutKata(t *testing.T) {
	o, _ := LookupOracle("isolation_kernel")
	status := pure.RunStatus{Output: json.RawMessage(`"6.1.0-generic"`)}
	if v := o.Check(status, CollectCtx{KataAvailable: false, NodeKernel: "6.1.0-generic"}); v.Kind != VerdictSkip {
		t.Errorf("no kata: got %q want skip (%s)", v.Kind, v.Evidence)
	}
	// With kata + matching kernel → FAIL (silent runc fallback).
	if v := o.Check(status, CollectCtx{KataAvailable: true, NodeKernel: "6.1.0-generic"}); v.Kind != VerdictFail {
		t.Errorf("kata matching kernel: got %q want fail", v.Kind)
	}
	// With kata + different kernel → PASS.
	microvm := pure.RunStatus{Output: json.RawMessage(`"5.10.0-microvm"`)}
	if v := o.Check(microvm, CollectCtx{KataAvailable: true, NodeKernel: "6.1.0-generic"}); v.Kind != VerdictPass {
		t.Errorf("kata distinct kernel: got %q want pass (%s)", v.Kind, v.Evidence)
	}
}

// TestOutputJSONPathOracle checks a jsonpath assertion over Output.
func TestOutputJSONPathOracle(t *testing.T) {
	o, _ := LookupOracle("output_jsonpath")
	status := pure.RunStatus{Output: json.RawMessage(`{"fib12":144,"prime":false}`)}
	c := BenchCase{Oracle: Oracle{Kind: "output_jsonpath", Path: ".fib12", Want: "144"}}
	if v := o.Check(status, CollectCtx{Case: c}); v.Kind != VerdictPass {
		t.Errorf("jsonpath match: got %q want pass (%s)", v.Kind, v.Evidence)
	}
	cBad := BenchCase{Oracle: Oracle{Kind: "output_jsonpath", Path: ".fib12", Want: "143"}}
	if v := o.Check(status, CollectCtx{Case: cBad}); v.Kind != VerdictFail {
		t.Errorf("jsonpath mismatch: got %q want fail", v.Kind)
	}
}

// TestMetrics_Percentiles validates the percentile + token aggregation, plus
// the tokensReal contract (CLI kinds aggregate to a contractual 0).
func TestMetrics_Percentiles(t *testing.T) {
	samples := []Sample{
		mkSample(100, 10, true, pure.PhaseCompleted),
		mkSample(200, 20, true, pure.PhaseCompleted),
		mkSample(300, 30, true, pure.PhaseCompleted),
		mkSample(400, 40, true, pure.PhaseFailed),
	}
	agg := AggregateSamples(samples, time.Minute)
	if agg.LatencyMs.P50 != 200 {
		t.Errorf("p50 = %d, want 200", agg.LatencyMs.P50)
	}
	if agg.LatencyMs.Max != 400 || agg.LatencyMs.Min != 100 {
		t.Errorf("min/max = %d/%d, want 100/400", agg.LatencyMs.Min, agg.LatencyMs.Max)
	}
	if agg.Tokens.Total != 100 || !agg.Tokens.Real {
		t.Errorf("tokens total=%d real=%v, want 100/true", agg.Tokens.Total, agg.Tokens.Real)
	}
	if agg.ErrorRatePct != 25 {
		t.Errorf("errorRate = %v, want 25", agg.ErrorRatePct)
	}
	if agg.ThroughputRunsPerMin != 4 {
		t.Errorf("throughput = %v, want 4", agg.ThroughputRunsPerMin)
	}
}

// TestEvalGate_TokenGateNAonCLI confirms a token gate resolves to N/A (not a
// failure) on a CLI kind whose tokens are a contractual 0.
func TestEvalGate_TokenGateNAonCLI(t *testing.T) {
	cliAgg := AggregateSamples([]Sample{mkSample(100, 0, false, pure.PhaseCompleted)}, 0)
	g := Gate{Metric: "tokens.total", Op: GateLTE, Want: json.RawMessage("800")}
	res := EvalGate(g, cliAgg, VerdictPass)
	if !res.NA || !res.Pass {
		t.Errorf("CLI token gate: NA=%v pass=%v, want NA + pass", res.NA, res.Pass)
	}
	// On a Hermes (real) aggregate the same gate is enforced.
	hermesAgg := AggregateSamples([]Sample{mkSample(100, 900, true, pure.PhaseCompleted)}, 0)
	res2 := EvalGate(g, hermesAgg, VerdictPass)
	if res2.NA || res2.Pass {
		t.Errorf("hermes token gate over budget: NA=%v pass=%v, want enforced + fail", res2.NA, res2.Pass)
	}
}

// TestComputeCasePass_AntiStaleness verifies a blocked case fails when its
// negative oracle stops holding (Fail verdict), and passes while it holds
// (Blocked verdict).
func TestComputeCasePass_AntiStaleness(t *testing.T) {
	if !ComputeCasePass(VerdictBlocked, nil, false) {
		t.Error("blocked verdict should pass (negative oracle held)")
	}
	if ComputeCasePass(VerdictFail, nil, false) {
		t.Error("fail verdict (stale tripwire) must not pass")
	}
	if ComputeCasePass(VerdictPass, nil, true) {
		t.Error("skipped case is neither pass nor fail")
	}
	// A failing non-NA gate fails the case even with a passing oracle.
	gates := []GateResult{{Name: "latency.p95.ms", Pass: false}}
	if ComputeCasePass(VerdictPass, gates, false) {
		t.Error("failing metric gate must fail the case")
	}
}

// TestFoldSessionTurn verifies the gateway result body (a worker SessionTurn)
// normalizes into the same pure.RunStatus the run driver returns, with latency
// derived from startedAt/endedAt.
func TestFoldSessionTurn(t *testing.T) {
	body := []byte(`{
		"output": "PRODUCT=49096740249106611",
		"phase": "Completed",
		"usage": { "steps": 1, "tokens": 359 },
		"startedAt": "2026-06-03T18:12:04Z",
		"endedAt":   "2026-06-03T18:12:06Z"
	}`)
	status, err := foldSessionTurn(body)
	if err != nil {
		t.Fatalf("foldSessionTurn: %v", err)
	}
	if status.State != pure.PhaseCompleted {
		t.Errorf("state = %q, want Completed", status.State)
	}
	if status.Usage.Tokens != 359 {
		t.Errorf("tokens = %d, want 359", status.Usage.Tokens)
	}
	if status.StartedAt == nil || status.EndedAt == nil {
		t.Fatal("startedAt/endedAt should be set")
	}
	if got := status.EndedAt.Sub(status.StartedAt.Time); got != 2*time.Second {
		t.Errorf("latency = %v, want 2s", got)
	}
	s := SampleFromStatus(status, true)
	if s.LatencyMs != 2000 {
		t.Errorf("sample latencyMs = %d, want 2000", s.LatencyMs)
	}
}

// TestResults_FinalizeAndRender exercises the summary tally (incl. a stale
// blocked case counting as a failure) and report.md rendering round-trip.
func TestResults_FinalizeAndRender(t *testing.T) {
	res := &Results{
		RunID:      "test-run",
		PlanDigest: "sha256:" + strings.Repeat("0", 64),
		Plan:       "demo",
		Cluster:    ClusterInfo{Runtime: "runc", Caps: []string{"gateway", "hermes"}},
		StartedAt:  time.Now().UTC(),
		FinishedAt: time.Now().UTC(),
		Cases: []CaseResult{
			{ID: "ok", Tier: TierCorrectness, Oracle: OracleResult{Kind: "output_match", Verdict: VerdictPass}, Pass: true},
			{ID: "blocked-held", Tier: TierFuture, Oracle: OracleResult{Kind: "tool_rejected", Verdict: VerdictBlocked},
				Blocked: &BlockedSpec{Reason: "unwired"}, Pass: true},
			{ID: "blocked-stale", Tier: TierFuture, Oracle: OracleResult{Kind: "tool_rejected", Verdict: VerdictFail},
				Blocked: &BlockedSpec{Reason: "unwired"}, Pass: false},
			{ID: "skipped", Tier: TierIsolation, Oracle: OracleResult{Kind: "isolation_kernel", Verdict: VerdictSkip},
				Skipped: true, SkipReason: "no kata-fc"},
		},
	}
	res.Finalize()
	if res.Summary.Total != 4 || res.Summary.Passed != 2 || res.Summary.Failed != 1 || res.Summary.Skipped != 1 || res.Summary.Blocked != 1 {
		t.Fatalf("summary = %+v, want total4 passed2 failed1 skipped1 blocked1", res.Summary)
	}
	if res.Verdict != "FAIL" {
		t.Errorf("verdict = %q, want FAIL (a stale blocked case is a failure)", res.Verdict)
	}

	dir := t.TempDir()
	if _, err := res.WriteJSON(dir); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	mdPath, err := res.WriteMarkdown(dir)
	if err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	for _, want := range []string{"## BLOCKED", "## SKIPPED", "STALE", "no kata-fc"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("report.md missing %q", want)
		}
	}
}

// ---- helpers --------------------------------------------------------------

func mkSample(latencyMs, tokens int64, real bool, phase pure.Phase) Sample {
	return Sample{LatencyMs: latencyMs, Tokens: tokens, TokensReal: real, Terminal: phase}
}

// decodeYAMLPlan decodes a plan from inline YAML, mirroring LoadPlan's decode
// path but without the filesystem.
func decodeYAMLPlan(t *testing.T, y string) *BenchPlan {
	t.Helper()
	var plan BenchPlan
	if err := yaml.Unmarshal([]byte(y), &plan); err != nil {
		t.Fatalf("decode test yaml: %v", err)
	}
	return &plan
}
