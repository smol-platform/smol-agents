package v1

import (
	"errors"
	"fmt"
	"time"
)

// Budget is a non-optional per-Agent resource cap. Implements R-AM-BUD-1.
//
// The runtime evaluates a candidate (steps_so_far+1, tokens_so_far+est,
// elapsed, toolCalls_so_far) against this struct *before* every step and
// transitions to Expired on the first violation. See
// `Budget.AllowsStep` and the Quint model in
// `spec/quint/agent_execution.qnt`.
type Budget struct {
	// MaxSteps is the maximum number of plan-act-observe iterations.
	// +kubebuilder:validation:Minimum=1
	MaxSteps int32 `json:"maxSteps"`

	// MaxTokens is the cumulative token cap across all steps.
	// +kubebuilder:validation:Minimum=1
	MaxTokens int64 `json:"maxTokens"`

	// MaxWallClockSeconds is the hard timeout from Run start.
	// +kubebuilder:validation:Minimum=1
	MaxWallClockSeconds int32 `json:"maxWallClockSeconds"`

	// MaxToolCalls is the maximum number of tool invocations.
	// +kubebuilder:validation:Minimum=0
	MaxToolCalls int32 `json:"maxToolCalls"`
}

// Usage tracks consumption so far. The runtime increments after each
// step and asks Budget.AllowsStep(usage, expectedDelta) before the next.
type Usage struct {
	Steps         int32         `json:"steps"`
	Tokens        int64         `json:"tokens"`
	ToolCalls     int32         `json:"toolCalls"`
	WallClockUsed time.Duration `json:"wallClockUsed"`
	// CostUSDMilli is the backend-reported cost in integer milli-USD,
	// observability only — never read by AllowsStep or any gate.
	CostUSDMilli int64 `json:"costUSDMilli,omitempty"`
}

// Validate ensures all four budget axes are positive (or zero, for
// MaxToolCalls) — R-AM-BUD-1.
func (b Budget) Validate() error {
	var errs []error
	if b.MaxSteps <= 0 {
		errs = append(errs, errors.New("budget.maxSteps must be > 0"))
	}
	if b.MaxTokens <= 0 {
		errs = append(errs, errors.New("budget.maxTokens must be > 0"))
	}
	if b.MaxWallClockSeconds <= 0 {
		errs = append(errs, errors.New("budget.maxWallClockSeconds must be > 0"))
	}
	if b.MaxToolCalls < 0 {
		errs = append(errs, errors.New("budget.maxToolCalls must be ≥ 0"))
	}
	return errors.Join(errs...)
}

// AllowsStep returns nil iff a step that would consume the given delta
// is permitted given the current usage. Returns a typed error pointing
// at the offending axis on rejection.
//
// Implements R-AM-BUD-2: the pre-check executed before every step.
func (b Budget) AllowsStep(used Usage, deltaTokens int64, deltaToolCalls int32) error {
	if used.Steps+1 > b.MaxSteps {
		return BudgetExceededError{Axis: "steps"}
	}
	if used.Tokens+deltaTokens > b.MaxTokens {
		return BudgetExceededError{Axis: "tokens"}
	}
	if used.WallClockUsed >= time.Duration(b.MaxWallClockSeconds)*time.Second {
		return BudgetExceededError{Axis: "wallclock"}
	}
	if used.ToolCalls+deltaToolCalls > b.MaxToolCalls {
		return BudgetExceededError{Axis: "toolCalls"}
	}
	return nil
}

// Add returns u + delta as a new Usage; pure for tests.
func (u Usage) Add(deltaTokens int64, deltaToolCalls int32, deltaWall time.Duration) Usage {
	return Usage{
		Steps:         u.Steps + 1,
		Tokens:        u.Tokens + deltaTokens,
		ToolCalls:     u.ToolCalls + deltaToolCalls,
		WallClockUsed: u.WallClockUsed + deltaWall,
	}
}

// BudgetExceededError is the typed error returned by AllowsStep.
type BudgetExceededError struct {
	Axis string
}

func (e BudgetExceededError) Error() string {
	return fmt.Sprintf("agentmodel: budget exceeded on axis %q", e.Axis)
}

// IsBudgetExceeded helps callers check the typed error without unwrapping.
func IsBudgetExceeded(err error) (axis string, ok bool) {
	var be BudgetExceededError
	if errors.As(err, &be) {
		return be.Axis, true
	}
	return "", false
}
