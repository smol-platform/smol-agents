package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// writeCase materializes a case dir (agent.json + run.json so discoverCases
// finds it, + optional expected.json).
func writeCase(t *testing.T, suite, name, expected string) {
	t.Helper()
	dir := filepath.Join(suite, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{agentruntime.AgentSpecFile, agentruntime.RunSpecFile} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if expected != "" {
		if err := os.WriteFile(filepath.Join(dir, "expected.json"), []byte(expected), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// M2.28: a suite with a passing case and a CHANGED case classifies each and the
// suite fails (exit 1). The fake runner returns per-case canned Results.
func TestEvalSuite_PassAndChanged(t *testing.T) {
	suite := t.TempDir()
	writeCase(t, suite, "a-pass", `{"phase":"Completed","output":"42"}`)
	writeCase(t, suite, "b-changed", `{"phase":"Completed","output":"expected"}`)

	runner := func(_ context.Context, dir string) (agentruntime.Result, error) {
		switch filepath.Base(dir) {
		case "a-pass":
			return agentruntime.Result{Phase: v1.PhaseCompleted, Output: json.RawMessage(`"42"`)}, nil
		default:
			return agentruntime.Result{Phase: v1.PhaseCompleted, Output: json.RawMessage(`"actual"`)}, nil
		}
	}

	report, failed := evalSuite(context.Background(), suite, 1, runner)
	if !failed {
		t.Error("a CHANGED case must fail the suite (exit 1)")
	}
	if len(report.Cases) != 2 {
		t.Fatalf("want 2 cases, got %d", len(report.Cases))
	}
	got := map[string]string{}
	for _, c := range report.Cases {
		got[c.Name] = c.Classification
	}
	if got["a-pass"] != "PASS" || got["b-changed"] != "FAIL" {
		t.Errorf("classification = %v, want a-pass=PASS b-changed=FAIL", got)
	}
}

// --samples N aggregates a distribution: a runner that passes 2 of 3 times is
// FLAKY (not a hard suite failure).
func TestEvalSuite_SampleDistribution(t *testing.T) {
	suite := t.TempDir()
	writeCase(t, suite, "flaky", `{"phase":"Completed","outputContains":"ok"}`)

	var n int
	runner := func(_ context.Context, _ string) (agentruntime.Result, error) {
		n++
		if n == 2 { // the middle sample misses
			return agentruntime.Result{Phase: v1.PhaseCompleted, Output: json.RawMessage(`"nope"`)}, nil
		}
		return agentruntime.Result{Phase: v1.PhaseCompleted, Output: json.RawMessage(`"ok!"`)}, nil
	}

	report, failed := evalSuite(context.Background(), suite, 3, runner)
	if failed {
		t.Error("a partially-passing case must NOT hard-fail the suite (near-term: don't gate)")
	}
	c := report.Cases[0]
	if c.Classification != "FLAKY" || c.Passed != 2 || c.Samples != 3 {
		t.Errorf("got %+v, want FLAKY 2/3", c)
	}
}

// compareResult applies phase/output/outputContains — and must IGNORE
// usage.toolCalls entirely (it is structurally 0 on the harness path).
func TestCompareResult(t *testing.T) {
	res := func(p v1.Phase, out string, toolCalls int32) agentruntime.Result {
		return agentruntime.Result{Phase: p, Output: json.RawMessage(out), Usage: v1.Usage{ToolCalls: toolCalls}}
	}
	// No expected.json → smoke: Completed passes, anything else fails.
	if ok, _ := compareResult(res(v1.PhaseCompleted, `"x"`, 0), nil); !ok {
		t.Error("nil expectation + Completed must pass")
	}
	if ok, _ := compareResult(res(v1.PhaseFailed, `"x"`, 0), nil); ok {
		t.Error("nil expectation + Failed must fail")
	}
	// Phase mismatch.
	if ok, _ := compareResult(res(v1.PhaseFailed, `"x"`, 0), &expectation{Phase: "Completed"}); ok {
		t.Error("phase mismatch must fail")
	}
	// Output exact, key-order/whitespace insensitive.
	if ok, _ := compareResult(res(v1.PhaseCompleted, `{"a":1,"b":2}`, 0), &expectation{Output: json.RawMessage(`{"b":2,"a":1}`)}); !ok {
		t.Error("structurally-equal JSON output must pass")
	}
	// toolCalls must NOT affect the verdict: same output, wildly different
	// toolCalls → still PASS.
	exp := &expectation{Phase: "Completed", Output: json.RawMessage(`"answer"`)}
	if ok, _ := compareResult(res(v1.PhaseCompleted, `"answer"`, 99), exp); !ok {
		t.Error("comparator must ignore usage.toolCalls")
	}
	// outputContains.
	if ok, _ := compareResult(res(v1.PhaseCompleted, `"the answer is 42"`, 0), &expectation{OutputContains: "42"}); !ok {
		t.Error("outputContains hit must pass")
	}
	if ok, _ := compareResult(res(v1.PhaseCompleted, `"nope"`, 0), &expectation{OutputContains: "42"}); ok {
		t.Error("outputContains miss must fail")
	}
}
