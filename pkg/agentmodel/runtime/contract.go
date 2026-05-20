// Package runtime defines the wire contract between the controller and
// the in-Pod agent executor. Importing this package gives a third party
// everything they need to build a compatible runtime.
package runtime

import (
	"encoding/json"
	"time"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// RunRef identifies an AgentRun unambiguously across reconciles.
type RunRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// LLMDecision is the structured output the runtime expects back from a Plan
// step. Either FinalAnswer is set (terminal) or ToolCall is set (continue).
type LLMDecision struct {
	FinalAnswer *FinalAnswer `json:"finalAnswer,omitempty"`
	ToolCall    *ToolCall    `json:"toolCall,omitempty"`
	Reasoning   string       `json:"reasoning,omitempty"`
	TokensIn    int64        `json:"tokensIn"`
	TokensOut   int64        `json:"tokensOut"`
}

// IsTerminal returns true if this decision ends the loop.
func (d LLMDecision) IsTerminal() bool { return d.FinalAnswer != nil }

// ToolCall is the runtime's request to invoke a Tool.
type ToolCall struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// Observation is the result of a successful ToolCall.
type Observation struct {
	Output     json.RawMessage `json:"output"`
	DurationMs int64           `json:"durationMs"`
}

// FinalAnswer is the terminal output of a Run.
type FinalAnswer struct {
	Output json.RawMessage `json:"output"`
}

// StepRequest is what the controller sends to the runtime to take one
// step. The controller pre-computes BudgetLeft so the runtime does not
// need access to the cluster.
type StepRequest struct {
	Run        RunRef       `json:"run"`
	AgentSpec  v1.AgentSpec `json:"agentSpec"`
	History    []v1.Step    `json:"history"`
	Now        time.Time    `json:"now"`
	BudgetLeft v1.Usage     `json:"budgetLeft"`
	Cancel     bool         `json:"cancel"`
}

// StepResponse is the runtime's reply. Exactly one of NextStep or
// Terminal is set.
type StepResponse struct {
	NextStep *v1.Step  `json:"nextStep,omitempty"`
	Terminal *Terminal `json:"terminal,omitempty"`
	Audit    StepAudit `json:"audit"`
}

// Terminal carries the final outcome.
type Terminal struct {
	Phase             v1.Phase         `json:"phase"`
	TerminationReason string           `json:"terminationReason,omitempty"`
	Output            *json.RawMessage `json:"output,omitempty"`
}

// StepAudit records non-functional facts about a step (timing, tokens).
type StepAudit struct {
	RequestID string        `json:"requestID"` // dedup key for at-least-once retries
	Duration  time.Duration `json:"duration"`
	TokensIn  int64         `json:"tokensIn"`
	TokensOut int64         `json:"tokensOut"`
}
