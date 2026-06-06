package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// Request is what the executor passes to a Harness.
type Request struct {
	// Spec is the full HarnessSpec block from the Agent CR.
	Spec v1.HarnessSpec

	// Instructions is the agent's system prompt (forwarded as system
	// message / appended to the prompt depending on the harness).
	Instructions string

	// Input is the user-provided prompt for this Run, as JSON.
	Input json.RawMessage

	// WorkingDir is where the harness runs, resolved by
	// AgentSpec.EffectiveWorkingDir: an explicit CLI working dir wins, else the
	// AgentFS mount when the Agent has durable storage, else empty (the harness
	// falls back to /tmp).
	WorkingDir string

	// Env is the resolved environment (literal Env values + secrets
	// fetched from the broker).
	Env map[string]string

	// Budget caps that apply to this single invocation. The harness
	// SHOULD respect them (wall clock especially); the executor
	// enforces hard timeouts independently via ctx.
	Budget v1.Budget

	// Seed is forwarded where the harness exposes one, as a best-effort
	// determinism hint — bit-exact reproduction is NOT guaranteed (providers may
	// ignore it under load; temperature/model drift and gateway-side loops defeat
	// it). For exact reproduction use record/replay. See
	// docs/specs/determinism-and-replay.md.
	Seed int64

	// SessionID is a prior harness session/thread id to RESUME (M3.19/M3.23): the
	// session worker threads it from AgentSessionStatus.HarnessSessionID across
	// turns of a persistent session. Empty = a fresh session. Consumed by
	// resumable harnesses (claude --resume, codex exec resume); ignored otherwise.
	SessionID string
}

// Response is what a Harness returns. The executor folds it into
// AgentRun.Status.
//
// RESPONSE RICHNESS CONTRACT (authoritative — see docs/design/harness-authoring.md):
//   - Output is ALWAYS set.
//   - TokensIn/TokensOut/CostUSDMilli/ToolCalls are BEST-EFFORT: a harness
//     populates them when it can parse its backend's usage/event stream
//     (Hermes; claude-code/codex with --output-format json; pi-mono) and leaves
//     them zero/empty otherwise. They are observability only — cost is never a
//     budget axis, and no gate/oracle reads ToolCalls (structurally 0 for kinds
//     that emit no tool stream).
//   - DurationMs is measured by the executor's clock.
type Response struct {
	// Output is the harness's final answer (raw bytes). Always set.
	Output []byte

	// TokensIn / TokensOut: best-effort, set when the harness parses a usage
	// block (see contract above).
	TokensIn  int64
	TokensOut int64

	// CostUSDMilli is the backend-reported cost in integer milli-USD,
	// observability only — never a budget axis. Best-effort.
	CostUSDMilli int64

	// ToolCalls: best-effort tool-call trace parsed from the backend (see
	// contract above).
	ToolCalls []v1.ToolCallRecord

	// DurationMs measured by the executor's clock.
	DurationMs int64
}

// Harness is the abstract harness backend. Implementations live in
// this package; selection happens in Registry.For.
type Harness interface {
	// Kind returns the Kind this implementation handles.
	Kind() v1.HarnessKind

	// Run invokes the harness once and returns its Response. ctx
	// cancellation MUST terminate the run (kill subprocess, abort
	// HTTP request).
	Run(ctx context.Context, req Request) (Response, error)
}

// Registry maps Kind to implementation. Defaults are wired in
// Default(); callers can swap implementations for testing.
type Registry struct {
	impls map[v1.HarnessKind]Harness
}

// Default returns a registry with all built-in harnesses registered.
func Default() *Registry {
	r := &Registry{impls: map[v1.HarnessKind]Harness{}}
	r.Register(&ClaudeCodeHarness{})
	r.Register(&CodexHarness{})
	r.Register(&AiderHarness{})
	r.Register(&GooseHarness{})
	r.Register(&GenericCLIHarness{})
	r.Register(&PiHarness{})
	r.Register(&GenericHTTPHarness{})
	r.Register(&HermesHarness{})
	return r
}

// Register adds (or replaces) an implementation. Used by tests.
func (r *Registry) Register(h Harness) {
	if r.impls == nil {
		r.impls = map[v1.HarnessKind]Harness{}
	}
	r.impls[h.Kind()] = h
}

// For returns the Harness for kind, resolving deprecated aliases first (e.g.
// "pi" → "inflection-pi") so an alias still finds its implementation.
func (r *Registry) For(kind v1.HarnessKind) (Harness, error) {
	h, ok := r.impls[v1.CanonicalHarnessKind(kind)]
	if !ok {
		return nil, fmt.Errorf("harness: no implementation for kind %q", kind)
	}
	return h, nil
}

// Common errors visible to callers.
var (
	ErrUnsupportedKind = errors.New("harness: unsupported kind")
	ErrCancelled       = errors.New("harness: cancelled")
	ErrTimeout         = errors.New("harness: budget wallclock exceeded")
)

// budgetTimeout returns a context with the wallclock cap from the
// request's budget applied. A zero or negative budget disables the cap.
func budgetTimeout(parent context.Context, b v1.Budget) (context.Context, context.CancelFunc) {
	if b.MaxWallClockSeconds <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(b.MaxWallClockSeconds)*time.Second)
}
