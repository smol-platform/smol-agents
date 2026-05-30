package agentruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

	// Seed declared input files into the workspace before execution, so a harness
	// or loop can work on "the files I gave you".
	if err := MaterializeInputs(ctx, agent.Spec.EffectiveWorkingDir(), run.Inputs, leaser); err != nil {
		return Result{}, err
	}
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

// MaterializeInputs writes the run's declared input files into workspace before
// the agent executes, so a harness or loop can read "the files to work on".
// Inline payloads travel in the run spec; secretRef payloads are leased from the
// broker (never inlined). Files are written 0600 under workspace with a
// traversal guard. A no-op when there are no inputs.
//
// workspace must be non-empty when inputs are present — an Agent with inputs
// needs a defined workspace (storage.agentfs or harness.cli.workingDir);
// otherwise the run fails loud rather than scattering files into an undefined
// cwd. HTTP harnesses (e.g. Hermes) don't read the workspace, so inputs only
// reach CLI harnesses / loop mode.
func MaterializeInputs(ctx context.Context, workspace string, inputs []v1.RunInputFile, leaser SecretLeaser) error {
	if len(inputs) == 0 {
		return nil
	}
	if strings.TrimSpace(workspace) == "" {
		return errors.New("agentruntime: run has input files but the agent has no workspace " +
			"(set spec.storage.agentfs or harness.cli.workingDir)")
	}
	for _, in := range inputs {
		data, err := inputBytes(ctx, in, leaser)
		if err != nil {
			return fmt.Errorf("agentruntime: input %q: %w", in.Path, err)
		}
		dest, err := safeWorkspacePath(workspace, in.Path)
		if err != nil {
			return fmt.Errorf("agentruntime: input %q: %w", in.Path, err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return fmt.Errorf("agentruntime: input %q: mkdir: %w", in.Path, err)
		}
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return fmt.Errorf("agentruntime: input %q: write: %w", in.Path, err)
		}
	}
	return nil
}

// inputBytes resolves a single input's content. Validation guarantees exactly
// one source is set; the order here is just a deterministic tiebreak.
func inputBytes(ctx context.Context, in v1.RunInputFile, leaser SecretLeaser) ([]byte, error) {
	switch {
	case in.SecretRef != nil && in.SecretRef.SecretName != "":
		if leaser == nil {
			return nil, errors.New("secretRef set but no secret broker is configured")
		}
		return leaser.LeaseSecret(ctx, in.SecretRef.SecretName, 0)
	case in.InlineBase64 != "":
		return base64.StdEncoding.DecodeString(in.InlineBase64)
	default:
		return []byte(in.Inline), nil
	}
}

// safeWorkspacePath joins rel under root, rejecting absolute paths and any
// ".." traversal (fail loud — defense-in-depth alongside ValidateAgentRun).
func safeWorkspacePath(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", errors.New("path must be relative")
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return "", errors.New(`path must not contain a ".." segment`)
		}
	}
	dest := filepath.Join(root, rel)
	cleanRoot := filepath.Clean(root)
	if dest != cleanRoot && !strings.HasPrefix(dest, cleanRoot+string(os.PathSeparator)) {
		return "", errors.New("path escapes the workspace")
	}
	return dest, nil
}
