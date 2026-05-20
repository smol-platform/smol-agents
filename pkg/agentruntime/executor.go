package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// HarnessRunner is the abstract harness driver. The agentruntime/harness
// package supplies the registry that satisfies this interface; the
// executor consumes the abstraction so unit tests can swap a fake.
type HarnessRunner interface {
	RunHarness(ctx context.Context, spec v1.HarnessSpec, instructions string,
		input json.RawMessage, workingDir string, env map[string]string,
		budget v1.Budget, seed int64) ([]byte, int64, int64, int64, error)
}

// Executor is the in-process implementation of the plan-act-observe loop.
// One Executor instance corresponds to one AgentRun.
type Executor struct {
	LLM      LLM
	Tools    map[string]v1.Tool          // by name; the catalog visible to this Run
	Invokers map[v1.ToolKind]ToolInvoker // one per kind
	Clock    Clock

	// Harness drives Mode==harness Agents. Optional when every Agent
	// the executor sees is Mode==loop.
	Harness HarnessRunner
}

// New returns a default Executor. Caller must set LLM (for Mode=loop)
// or Harness (for Mode=harness), plus Tools/Invokers when relevant.
func New() *Executor {
	return &Executor{
		Clock:    SystemClock(),
		Tools:    map[string]v1.Tool{},
		Invokers: map[v1.ToolKind]ToolInvoker{},
	}
}

// Result is the outcome of a single Run.
type Result struct {
	Phase             v1.Phase
	Steps             []v1.Step
	Usage             v1.Usage
	TerminationReason string
	Output            json.RawMessage
}

// Run executes the agent against `input` and returns the Result. The
// executor is deterministic: given the same agent, the same input, and
// the same seed, it produces the same Result.
//
// When AgentSpec.Mode==harness the executor delegates to the configured
// HarnessRunner — a single bounded subprocess/HTTP call with no
// plan-act-observe loop. Budget enforcement still applies via the
// executor's wallclock + token caps.
func (e *Executor) Run(ctx context.Context, agent v1.Agent, input json.RawMessage, seed int64) (Result, error) {
	if e.Clock == nil {
		e.Clock = SystemClock()
	}
	if agent.Spec.Mode == v1.ModeHarness {
		return e.runHarness(ctx, agent, input, seed)
	}
	if e.LLM == nil {
		return Result{}, errors.New("agentruntime: LLM is required (mode=loop)")
	}

	budget := agent.Spec.Budget
	allowed := allowedNames(agent.Spec.Tools)

	startedAt := e.Clock.Now()
	usage := v1.Usage{}
	steps := []v1.Step{}
	phase := v1.PhasePending
	var terminationReason string
	var output json.RawMessage

	// Pending → Running
	phase = v1.PhaseRunning

	for {
		if ctx.Err() != nil {
			phase = v1.PhaseCancelled
			terminationReason = "ctx:" + ctx.Err().Error()
			break
		}

		// Refresh wallclock usage and pre-check budget BEFORE every step.
		usage.WallClockUsed = e.Clock.Since(startedAt)
		if err := budget.AllowsStep(usage, 0, 0); err != nil {
			phase = v1.PhaseExpired
			if axis, ok := v1.IsBudgetExceeded(err); ok {
				terminationReason = "budget:" + axis
			} else {
				terminationReason = err.Error()
			}
			break
		}

		// Plan: ask the LLM what to do next.
		planStart := e.Clock.Now()
		decision, err := e.LLM.Chat(ctx, ChatRequest{
			Model:        agent.Spec.Model,
			Instructions: agent.Spec.Instructions,
			Tools:        e.toolsFor(allowed),
			History:      steps,
			Input:        input,
			Seed:         seed,
		})
		planEnd := e.Clock.Now()
		usage.WallClockUsed = e.Clock.Since(startedAt)

		if err != nil {
			steps = append(steps, v1.Step{
				Index: int32(len(steps)), Kind: v1.StepPlan,
				StartedAt: metav1.NewTime(planStart), EndedAt: metav1.NewTime(planEnd),
				Error: err.Error(),
			})
			// If the LLM error is a cancellation (the cancel raced
			// LLM.Chat after the loop's ctx.Err check at the top), we
			// classify the run as Cancelled rather than Failed — the
			// outer caller asked for it.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				phase = v1.PhaseCancelled
				terminationReason = "ctx:" + err.Error()
			} else {
				phase = v1.PhaseFailed
				terminationReason = "llm:" + err.Error()
			}
			break
		}

		// Account tokens consumed by this Plan, but ONLY if doing so keeps
		// us within the token budget. If accepting these tokens would
		// exceed maxTokens, terminate as Expired BEFORE recording so the
		// safety invariant `Usage.Tokens ≤ MaxTokens` holds tightly.
		// The Plan step is still recorded with the observed token counts
		// for audit, but those counts do not flow into Usage.
		newTokens := decision.TokensIn + decision.TokensOut
		if usage.Tokens+newTokens > budget.MaxTokens {
			steps = append(steps, v1.Step{
				Index: int32(len(steps)), Kind: v1.StepPlan,
				StartedAt: metav1.NewTime(planStart), EndedAt: metav1.NewTime(planEnd),
				TokensIn: decision.TokensIn, TokensOut: decision.TokensOut,
				Error: "budget:tokens",
			})
			phase = v1.PhaseExpired
			terminationReason = "budget:tokens"
			break
		}
		usage = usage.Add(newTokens, 0, 0)

		steps = append(steps, v1.Step{
			Index: int32(len(steps)), Kind: v1.StepPlan,
			StartedAt: metav1.NewTime(planStart), EndedAt: metav1.NewTime(planEnd),
			TokensIn: decision.TokensIn, TokensOut: decision.TokensOut,
		})

		// Re-check the other axes (steps, wallclock) after the Plan.
		if err := budget.AllowsStep(usage, 0, 0); err != nil {
			phase = v1.PhaseExpired
			if axis, ok := v1.IsBudgetExceeded(err); ok {
				terminationReason = "budget:" + axis
			} else {
				terminationReason = err.Error()
			}
			break
		}

		if decision.IsTerminal() {
			output = decision.FinalAnswer.Output
			steps = append(steps, v1.Step{
				Index: int32(len(steps)), Kind: v1.StepFinal,
				StartedAt: metav1.NewTime(planEnd), EndedAt: metav1.NewTime(e.Clock.Now()),
			})
			phase = v1.PhaseCompleted
			break
		}

		if decision.ToolCall == nil {
			phase = v1.PhaseFailed
			terminationReason = "decision:neither_terminal_nor_tool"
			break
		}

		tc := decision.ToolCall

		// Allow-list check (R-AM-TOOL-1 + safety invariant OnlyAllowedToolsCalled).
		if !allowed[tc.Tool] {
			steps = append(steps, v1.Step{
				Index: int32(len(steps)), Kind: v1.StepToolCallRejected,
				StartedAt: metav1.NewTime(e.Clock.Now()), EndedAt: metav1.NewTime(e.Clock.Now()),
				ToolCalls: []v1.ToolCallRecord{{Tool: tc.Tool, Arguments: tc.Arguments,
					Error: "tool not in allow-list"}},
				Error: ErrToolNotInAllowList.Error(),
			})
			continue
		}

		tool, ok := e.Tools[tc.Tool]
		if !ok {
			steps = append(steps, v1.Step{
				Index: int32(len(steps)), Kind: v1.StepToolCallRejected,
				ToolCalls: []v1.ToolCallRecord{{Tool: tc.Tool, Error: ErrToolNotFound.Error()}},
				StartedAt: metav1.NewTime(e.Clock.Now()), EndedAt: metav1.NewTime(e.Clock.Now()),
				Error: ErrToolNotFound.Error(),
			})
			continue
		}

		// Validate args vs input schema.
		if err := v1.MatchesSchema(tool.Spec.InputSchema, tc.Arguments); err != nil {
			steps = append(steps, v1.Step{
				Index: int32(len(steps)), Kind: v1.StepToolCallRejected,
				ToolCalls: []v1.ToolCallRecord{{Tool: tc.Tool, Arguments: tc.Arguments,
					Error: err.Error()}},
				StartedAt: metav1.NewTime(e.Clock.Now()), EndedAt: metav1.NewTime(e.Clock.Now()),
				Error: ErrInvalidArgs.Error(),
			})
			continue
		}

		// Pre-check budget for the tool call.
		if err := budget.AllowsStep(usage, 0, 1); err != nil {
			phase = v1.PhaseExpired
			if axis, ok := v1.IsBudgetExceeded(err); ok {
				terminationReason = "budget:" + axis
			} else {
				terminationReason = err.Error()
			}
			break
		}

		// Invoke.
		invoker, ok := e.Invokers[tool.Spec.Kind]
		if !ok {
			steps = append(steps, v1.Step{
				Index: int32(len(steps)), Kind: v1.StepToolCallRejected,
				ToolCalls: []v1.ToolCallRecord{{Tool: tc.Tool,
					Error: fmt.Sprintf("no invoker for kind %q", tool.Spec.Kind)}},
				StartedAt: metav1.NewTime(e.Clock.Now()), EndedAt: metav1.NewTime(e.Clock.Now()),
				Error: ErrToolNotFound.Error(),
			})
			continue
		}

		callStart := e.Clock.Now()
		obs, err := invoker.Invoke(ctx, tool, tc.Arguments)
		callEnd := e.Clock.Now()

		if err != nil {
			steps = append(steps, v1.Step{
				Index: int32(len(steps)), Kind: v1.StepToolCall,
				StartedAt: metav1.NewTime(callStart), EndedAt: metav1.NewTime(callEnd),
				ToolCalls: []v1.ToolCallRecord{{Tool: tc.Tool, Arguments: tc.Arguments,
					Error: err.Error(), DurationMs: callEnd.Sub(callStart).Milliseconds()}},
				Error: err.Error(),
			})
			usage.ToolCalls++
			usage.WallClockUsed = e.Clock.Since(startedAt)
			continue
		}

		// Validate observation vs output schema.
		if err := v1.MatchesSchema(tool.Spec.OutputSchema, obs.Output); err != nil {
			steps = append(steps, v1.Step{
				Index: int32(len(steps)), Kind: v1.StepObservationRejected,
				StartedAt: metav1.NewTime(callStart), EndedAt: metav1.NewTime(callEnd),
				ToolCalls: []v1.ToolCallRecord{{Tool: tc.Tool, Arguments: tc.Arguments,
					Result: obs.Output, Error: err.Error(),
					DurationMs: callEnd.Sub(callStart).Milliseconds()}},
				Error: ErrInvalidObservation.Error(),
			})
			usage.ToolCalls++
			usage.WallClockUsed = e.Clock.Since(startedAt)
			continue
		}

		steps = append(steps, v1.Step{
			Index: int32(len(steps)), Kind: v1.StepObservation,
			StartedAt: metav1.NewTime(callStart), EndedAt: metav1.NewTime(callEnd),
			ToolCalls: []v1.ToolCallRecord{{Tool: tc.Tool, Arguments: tc.Arguments,
				Result: obs.Output, DurationMs: callEnd.Sub(callStart).Milliseconds()}},
		})
		usage.ToolCalls++
		usage.WallClockUsed = e.Clock.Since(startedAt)
	}

	// Sanity check: phase MUST be a valid Phase.
	if !phase.Valid() {
		phase = v1.PhaseFailed
		terminationReason = "internal:invalid_phase"
	}

	return Result{
		Phase:             phase,
		Steps:             steps,
		Usage:             usage,
		TerminationReason: terminationReason,
		Output:            output,
	}, nil
}

// allowedNames returns a set lookup keyed by tool name.
func allowedNames(refs []v1.ToolRef) map[string]bool {
	out := make(map[string]bool, len(refs))
	for _, r := range refs {
		out[r.Name] = true
	}
	return out
}

// toolsFor returns the catalog entries the LLM should see.
func (e *Executor) toolsFor(allowed map[string]bool) []v1.Tool {
	out := make([]v1.Tool, 0, len(allowed))
	for name := range allowed {
		if t, ok := e.Tools[name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// helper — keep an import alive
var _ = time.Second

// runHarness drives a single Mode==harness execution. The harness gets
// one shot at the input; we record one Step capturing the entire call.
func (e *Executor) runHarness(ctx context.Context, agent v1.Agent, input json.RawMessage, seed int64) (Result, error) {
	if e.Harness == nil {
		return Result{}, errors.New("agentruntime: Harness is required (mode=harness)")
	}
	if agent.Spec.Harness == nil {
		return Result{}, errors.New("agentruntime: spec.harness is required (mode=harness)")
	}
	startedAt := e.Clock.Now()
	out, tIn, tOut, durMs, err := e.Harness.RunHarness(ctx, *agent.Spec.Harness, agent.Spec.Instructions,
		input, agent.Spec.Harness.WorkingDirOrEmpty(), nil, agent.Spec.Budget, seed)
	endedAt := e.Clock.Now()

	usage := v1.Usage{Steps: 1, Tokens: tIn + tOut, ToolCalls: 0,
		WallClockUsed: e.Clock.Since(startedAt)}

	step := v1.Step{
		Index: 0, Kind: v1.StepFinal,
		StartedAt: metav1.NewTime(startedAt), EndedAt: metav1.NewTime(endedAt),
		TokensIn: tIn, TokensOut: tOut,
	}
	_ = durMs // surfaced via WallClockUsed; kept for symmetry with harness Response

	if err != nil {
		step.Error = err.Error()
		return Result{
			Phase:             v1.PhaseFailed,
			Steps:             []v1.Step{step},
			Usage:             usage,
			TerminationReason: "harness:" + err.Error(),
		}, nil
	}
	// Cap tokens against budget — same treatment as Mode=loop.
	if agent.Spec.Budget.MaxTokens > 0 && usage.Tokens > agent.Spec.Budget.MaxTokens {
		return Result{
			Phase:             v1.PhaseExpired,
			Steps:             []v1.Step{step},
			Usage:             usage,
			TerminationReason: "budget:tokens",
		}, nil
	}
	return Result{
		Phase:  v1.PhaseCompleted,
		Steps:  []v1.Step{step},
		Usage:  usage,
		Output: json.RawMessage(out),
	}, nil
}
