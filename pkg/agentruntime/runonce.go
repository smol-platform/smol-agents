package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime/harness"
)

// Mounted per-run payload filenames. The operator writes these into the
// AgentRun pod (a projected ConfigMap); the run entrypoint reads them.
const (
	AgentSpecFile = "agent.json" // marshalled pure v1.Agent
	RunSpecFile   = "run.json"   // marshalled pure v1.AgentRunSpec
)

// RunResult is the compact JSON the AgentRun pod emits — to its termination
// message (the controller's primary signal) and stdout (logs). It is the
// runtime → controller contract for folding into AgentRun.Status.
type RunResult struct {
	Phase  v1.Phase        `json:"phase"`
	Output json.RawMessage `json:"output,omitempty"`
	// Steps is the per-step execution trace folded into AgentRun.Status.Steps:
	// the loop's plan-act-observe iterations, or the single Final step a harness
	// run produces (carrying the harness's tool-call log when it surfaces one).
	// Bounded for the termination message by cmd/agent (see clampForTerminationMessage).
	Steps             []v1.Step `json:"steps,omitempty"`
	Usage             v1.Usage  `json:"usage"`
	TerminationReason string    `json:"terminationReason,omitempty"`
	Error             string    `json:"error,omitempty"`
}

// RunOnce loads the Agent + AgentRunSpec from dir and executes one bounded run.
// The harness registry is always wired; leaser (optional) resolves harness env
// secretRef via the broker; llm (optional) backs Mode=loop agents (nil is fine
// for Mode=harness — the Hermes path).
func RunOnce(ctx context.Context, dir string, leaser SecretLeaser, llm LLM) (Result, error) {
	var agent v1.Agent
	if err := readJSONFile(filepath.Join(dir, AgentSpecFile), &agent); err != nil {
		return Result{}, fmt.Errorf("load agent spec: %w", err)
	}
	var run v1.AgentRunSpec
	if err := readJSONFile(filepath.Join(dir, RunSpecFile), &run); err != nil {
		return Result{}, fmt.Errorf("load run spec: %w", err)
	}
	if run.BudgetOverride != nil {
		agent.Spec.Budget = *run.BudgetOverride
	}

	exec := New()
	exec.Harness = NewRegistryRunner(harness.Default())
	exec.Secrets = leaser
	exec.LLM = llm
	return exec.Run(ctx, agent, run.Input, run.Seed)
}

// ResultToWire converts an executor Result (+ any run error) into the compact
// RunResult the controller consumes.
func ResultToWire(res Result, runErr error) RunResult {
	w := RunResult{
		Phase:             res.Phase,
		Output:            res.Output,
		Steps:             res.Steps,
		Usage:             res.Usage,
		TerminationReason: res.TerminationReason,
	}
	if runErr != nil {
		w.Error = runErr.Error()
		if w.Phase == "" {
			w.Phase = v1.PhaseFailed
		}
	}
	return w
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
