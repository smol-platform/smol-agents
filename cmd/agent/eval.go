package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/secrets"
)

// runEval is the `agent eval` subcommand: an offline regression runner over a
// suite of cases. Each case is a directory holding the exact pair RunOnce loads
// (agent.json + run.json) plus an optional expected.json. Every case runs LIVE
// via the genuine datapath (RunOnce) --samples times; the per-case pass
// distribution is reported. Replay/fixtures are post-GA (D6) — near-term
// reproducibility is the N-sample distribution, not bit-exact replay.
//
// Comparison: phase always; output exact when expected.output is present;
// opt-in expected.outputContains. It deliberately NEVER reads usage.toolCalls
// (structurally 0 on the harness path — see the cross-cutting invariant).
func runEval(args []string) int {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	suite := fs.String("suite", "", "directory of eval cases (each a dir with agent.json + run.json [+ expected.json])")
	samples := fs.Int("samples", 1, "live runs per case (the distribution substitutes for determinism)")
	format := fs.String("format", "text", "text|json")
	socket := fs.String("secret-socket", "/run/secret-broker/secret-broker.sock", "secret broker UDS")
	_ = fs.Parse(args)

	if *suite == "" {
		fmt.Fprintln(os.Stderr, "eval: --suite is required")
		return 2
	}
	if *samples < 1 {
		*samples = 1
	}

	ctx := context.Background()
	runner := func(ctx context.Context, dir string) (agentruntime.Result, error) {
		var leaser agentruntime.SecretLeaser
		if waitForBrokerSocket(*socket, 5*time.Second) {
			leaser = secretLeaser{c: secrets.NewClient(*socket)}
		}
		return agentruntime.RunOnce(ctx, dir, leaser, buildLoopLLM(ctx, dir, leaser, "")) // eval sample: one-shot, no durable session
	}

	report, failed := evalSuite(ctx, *suite, *samples, runner)
	if *format == "json" {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(b))
	} else {
		printEvalText(report)
	}
	if failed {
		return 1
	}
	return 0
}

// caseRunner executes one case directory and returns its Result. Injected so
// evalSuite is testable without a live backend.
type caseRunner func(ctx context.Context, caseDir string) (agentruntime.Result, error)

// expectation is the optional expected.json for a case.
type expectation struct {
	Phase          string          `json:"phase,omitempty"`
	Output         json.RawMessage `json:"output,omitempty"`
	OutputContains string          `json:"outputContains,omitempty"`
}

// CaseReport is the per-case eval outcome.
type CaseReport struct {
	Name           string `json:"name"`
	Samples        int    `json:"samples"`
	Passed         int    `json:"passed"`
	Classification string `json:"classification"` // PASS | FLAKY | FAIL
	FirstFailure   string `json:"firstFailure,omitempty"`
}

// SuiteReport is the whole-suite eval result.
type SuiteReport struct {
	Suite string       `json:"suite"`
	Cases []CaseReport `json:"cases"`
}

// evalSuite discovers cases under suiteDir, runs each `samples` times via runner,
// compares against the optional expected.json, and classifies. It returns the
// report and whether the suite failed (any case with zero passing samples).
func evalSuite(ctx context.Context, suiteDir string, samples int, runner caseRunner) (SuiteReport, bool) {
	report := SuiteReport{Suite: suiteDir}
	failed := false
	for _, dir := range discoverCases(suiteDir) {
		exp := loadExpectation(dir)
		cr := CaseReport{Name: filepath.Base(dir), Samples: samples}
		for i := 0; i < samples; i++ {
			res, err := runner(ctx, dir)
			pass, reason := false, ""
			if err != nil {
				reason = "run error: " + err.Error()
			} else {
				pass, reason = compareResult(res, exp)
			}
			if pass {
				cr.Passed++
			} else if cr.FirstFailure == "" {
				cr.FirstFailure = reason
			}
		}
		switch {
		case cr.Passed == samples:
			cr.Classification = "PASS"
		case cr.Passed == 0:
			cr.Classification = "FAIL"
			failed = true
		default:
			cr.Classification = "FLAKY"
		}
		report.Cases = append(report.Cases, cr)
	}
	return report, failed
}

// discoverCases returns the sorted case directories under suiteDir: any
// immediate subdir that holds an agent.json (the RunOnce spec).
func discoverCases(suiteDir string) []string {
	entries, err := os.ReadDir(suiteDir)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(suiteDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, agentruntime.AgentSpecFile)); err == nil {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// loadExpectation reads expected.json, returning nil when absent (a smoke case:
// it passes iff the run reaches Completed).
func loadExpectation(dir string) *expectation {
	b, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		return nil
	}
	var e expectation
	if json.Unmarshal(b, &e) != nil {
		return nil
	}
	return &e
}

// compareResult applies the expectation to a run Result. NEVER reads
// usage.toolCalls (it is structurally 0 on the harness path).
func compareResult(res agentruntime.Result, exp *expectation) (bool, string) {
	if exp == nil {
		if res.Phase != v1.PhaseCompleted {
			return false, fmt.Sprintf("phase=%s (no expected.json → want Completed)", res.Phase)
		}
		return true, ""
	}
	if exp.Phase != "" && string(res.Phase) != exp.Phase {
		return false, fmt.Sprintf("phase=%s, want %s", res.Phase, exp.Phase)
	}
	if len(exp.Output) > 0 && !jsonEqual(res.Output, exp.Output) {
		return false, "output mismatch"
	}
	if exp.OutputContains != "" && !strings.Contains(string(res.Output), exp.OutputContains) {
		return false, "outputContains miss: " + exp.OutputContains
	}
	return true, ""
}

// jsonEqual compares two JSON documents structurally (key order / whitespace
// insensitive). Non-JSON values compare as raw bytes.
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return strings.TrimSpace(string(a)) == strings.TrimSpace(string(b))
	}
	return reflect.DeepEqual(av, bv)
}

func printEvalText(r SuiteReport) {
	for _, c := range r.Cases {
		line := fmt.Sprintf("%-7s %s (%d/%d)", c.Classification, c.Name, c.Passed, c.Samples)
		if c.FirstFailure != "" {
			line += " — " + c.FirstFailure
		}
		fmt.Println(line)
	}
}
