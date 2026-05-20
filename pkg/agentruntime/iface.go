package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	rt "github.com/stigen/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// LLM is the abstract chat model the executor talks to. Implementations
// (OpenAILLM, AnthropicLLM, FakeLLM, …) must be deterministic when given
// the same seed plus the same prompt; tests rely on this.
type LLM interface {
	// Chat returns a structured decision and reports tokens used.
	Chat(ctx context.Context, req ChatRequest) (rt.LLMDecision, error)
}

// ChatRequest is what we send to LLM.Chat.
type ChatRequest struct {
	Model        v1.ModelRef
	Instructions string
	Tools        []v1.Tool
	History      []v1.Step
	Input        json.RawMessage
	Seed         int64
}

// ToolInvoker is the abstract tool transport. The runtime resolves the
// concrete implementation per Tool.Spec.Kind.
type ToolInvoker interface {
	Invoke(ctx context.Context, tool v1.Tool, args json.RawMessage) (rt.Observation, error)
}

// Clock lets tests advance time deterministically.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
}

// realClock backs the production Executor.
type realClock struct{}

func (realClock) Now() time.Time                  { return time.Now() }
func (realClock) Since(t time.Time) time.Duration { return time.Since(t) }

// SystemClock returns a real-time Clock.
func SystemClock() Clock { return realClock{} }

// Errors returned by the executor that callers can branch on.
var (
	ErrCancelled          = errors.New("agentruntime: cancelled")
	ErrToolNotInAllowList = errors.New("agentruntime: tool not in allow-list")
	ErrToolNotFound       = errors.New("agentruntime: tool not found in catalog")
	ErrInvalidArgs        = errors.New("agentruntime: tool args failed input schema")
	ErrInvalidObservation = errors.New("agentruntime: tool result failed output schema")
)
