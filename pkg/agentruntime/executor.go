package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime/harness"
)

// HarnessRunner is the abstract harness driver. The agentruntime/harness
// package supplies the registry that satisfies this interface; the
// executor consumes the abstraction so unit tests can swap a fake.
type HarnessRunner interface {
	RunHarness(ctx context.Context, spec v1.HarnessSpec, instructions string,
		input json.RawMessage, workingDir string, env map[string]string,
		budget v1.Budget, seed int64) (harness.Response, error)
}

// SecretLeaser leases a named secret from the broker so the executor can
// resolve a harness env entry's secretRef into a value the harness sees.
// Implemented by pkg/secrets.Client (adapter wired in cmd/agent); nil when the
// run has no broker, in which case a declared secretRef is a hard error rather
// than a silently-missing credential.
type SecretLeaser interface {
	LeaseSecret(ctx context.Context, name string, ttl time.Duration) ([]byte, error)
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

	// Secrets resolves harness env secretRef entries via the broker. Optional;
	// required only when an Agent declares a harness env with a secretRef.
	Secrets SecretLeaser
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

// resolveHarnessEnv leases each harness env entry that carries a secretRef and
// returns a name→value map for the harness. Entries with a literal Value are
// left to the harness (it reads HarnessSpec.Env directly). A secretRef with no
// configured broker is a hard error — a missing credential must fail loudly,
// not silently produce an unauthenticated call.
func (e *Executor) resolveHarnessEnv(ctx context.Context, vars []v1.HarnessEnvVar) (map[string]string, error) {
	var out map[string]string
	for _, v := range vars {
		if v.SecretRef == nil || v.SecretRef.SecretName == "" {
			continue
		}
		if e.Secrets == nil {
			return nil, fmt.Errorf("agentruntime: harness env %q has a secretRef but no secret broker is configured", v.Name)
		}
		val, err := e.Secrets.LeaseSecret(ctx, v.SecretRef.SecretName, 0)
		if err != nil {
			return nil, fmt.Errorf("agentruntime: lease secret %q for env %q: %w", v.SecretRef.SecretName, v.Name, err)
		}
		if out == nil {
			out = make(map[string]string, len(vars))
		}
		out[v.Name] = string(val)
	}
	return out, nil
}

// runHarness drives a single Mode==harness execution. The harness gets
// one shot at the input; we record one Step capturing the entire call.
func (e *Executor) runHarness(ctx context.Context, agent v1.Agent, input json.RawMessage, seed int64) (Result, error) {
	if e.Harness == nil {
		return Result{}, errors.New("agentruntime: Harness is required (mode=harness)")
	}
	if agent.Spec.Harness == nil {
		return Result{}, errors.New("agentruntime: spec.harness is required (mode=harness)")
	}
	// Resolve harness env secretRef entries to values via the broker. Literal
	// env travels in the HarnessSpec and is read by the harness itself; this
	// map carries only the broker-leased secrets (e.g. a gateway bearer token).
	env, err := e.resolveHarnessEnv(ctx, agent.Spec.Harness.Env)
	if err != nil {
		return Result{}, err
	}

	startedAt := e.Clock.Now()
	resp, err := e.Harness.RunHarness(ctx, *agent.Spec.Harness, agent.Spec.Instructions,
		input, agent.Spec.EffectiveWorkingDir(), env, agent.Spec.Budget, seed)
	endedAt := e.Clock.Now()

	usage := v1.Usage{Steps: 1, Tokens: resp.TokensIn + resp.TokensOut,
		ToolCalls:     int32(len(resp.ToolCalls)),
		CostUSDMilli:  resp.CostUSDMilli, // observability only — not read by AllowsStep
		WallClockUsed: e.Clock.Since(startedAt)}

	// One Step captures the whole bounded call. ToolCalls carries the harness's
	// own tool log when it surfaces one (e.g. Hermes via the Responses API);
	// chat/subprocess harnesses leave it empty and the Step is just the Final.
	step := v1.Step{
		Index: 0, Kind: v1.StepFinal,
		StartedAt: metav1.NewTime(startedAt), EndedAt: metav1.NewTime(endedAt),
		TokensIn: resp.TokensIn, TokensOut: resp.TokensOut,
		ToolCalls: resp.ToolCalls,
	}

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
		Output: harnessOutputJSON(resp.Output),
	}, nil
}

// harnessOutputJSON normalizes a harness's raw stdout into a valid
// json.RawMessage. Harness output is frequently plain text — an LLM's prose
// answer, e.g. a reasoning model that explains its steps before the final
// line — not a JSON document. Storing non-JSON bytes in a json.RawMessage makes
// every later json.Marshal of the Result/RunResult fail, which (because the
// AgentRun pod marshals best-effort) surfaces as silently-empty output. So:
// pass valid JSON through unchanged (a harness that emits structured JSON keeps
// its shape), otherwise encode the text as a JSON string. AgentRun.status.output
// accepts any JSON value, so both forms are stored as the run's answer as-is.
func harnessOutputJSON(out []byte) json.RawMessage {
	t := bytes.TrimSpace(out)
	if len(t) == 0 {
		return nil
	}
	if json.Valid(t) {
		return json.RawMessage(t)
	}
	b, _ := json.Marshal(string(out)) // marshaling a string cannot fail
	return json.RawMessage(b)
}
