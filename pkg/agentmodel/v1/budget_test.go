package v1

import (
	"errors"
	"testing"
	"time"
)

func TestBudgetValidate(t *testing.T) {
	good := Budget{MaxSteps: 10, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 5}
	if err := good.Validate(); err != nil {
		t.Errorf("good budget rejected: %v", err)
	}
	bad := []Budget{
		{MaxSteps: 0, MaxTokens: 1, MaxWallClockSeconds: 1, MaxToolCalls: 1},
		{MaxSteps: 1, MaxTokens: 0, MaxWallClockSeconds: 1, MaxToolCalls: 1},
		{MaxSteps: 1, MaxTokens: 1, MaxWallClockSeconds: 0, MaxToolCalls: 1},
		{MaxSteps: 1, MaxTokens: 1, MaxWallClockSeconds: 1, MaxToolCalls: -1},
	}
	for _, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("bad budget accepted: %+v", b)
		}
	}
}

func TestAllowsStep_HappyPath(t *testing.T) {
	b := Budget{MaxSteps: 3, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 10}
	used := Usage{Steps: 1, Tokens: 200, WallClockUsed: 2 * time.Second, ToolCalls: 1}
	if err := b.AllowsStep(used, 100, 1); err != nil {
		t.Errorf("expected allowed: %v", err)
	}
}

func TestAllowsStep_StepCap(t *testing.T) {
	b := Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 10}
	used := Usage{Steps: 1}
	err := b.AllowsStep(used, 0, 0)
	axis, ok := IsBudgetExceeded(err)
	if !ok || axis != "steps" {
		t.Errorf("expected axis=steps, got %v (%v)", axis, err)
	}
}

func TestAllowsStep_TokenCap(t *testing.T) {
	b := Budget{MaxSteps: 100, MaxTokens: 100, MaxWallClockSeconds: 60, MaxToolCalls: 10}
	used := Usage{Tokens: 99}
	err := b.AllowsStep(used, 2, 0)
	axis, _ := IsBudgetExceeded(err)
	if axis != "tokens" {
		t.Errorf("expected axis=tokens, got %v (%v)", axis, err)
	}
}

func TestAllowsStep_WallClockCap(t *testing.T) {
	b := Budget{MaxSteps: 100, MaxTokens: 1000, MaxWallClockSeconds: 5, MaxToolCalls: 10}
	used := Usage{WallClockUsed: 5 * time.Second}
	err := b.AllowsStep(used, 0, 0)
	axis, _ := IsBudgetExceeded(err)
	if axis != "wallclock" {
		t.Errorf("expected axis=wallclock, got %v (%v)", axis, err)
	}
}

func TestAllowsStep_ToolCallCap(t *testing.T) {
	b := Budget{MaxSteps: 100, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 1}
	used := Usage{ToolCalls: 1}
	err := b.AllowsStep(used, 0, 1)
	axis, _ := IsBudgetExceeded(err)
	if axis != "toolCalls" {
		t.Errorf("expected axis=toolCalls, got %v (%v)", axis, err)
	}
}

func TestUsage_Add(t *testing.T) {
	u := Usage{}
	u = u.Add(10, 1, 100*time.Millisecond)
	if u.Steps != 1 || u.Tokens != 10 || u.ToolCalls != 1 || u.WallClockUsed != 100*time.Millisecond {
		t.Errorf("add wrong: %+v", u)
	}
	u = u.Add(5, 0, 50*time.Millisecond)
	if u.Steps != 2 || u.Tokens != 15 || u.ToolCalls != 1 || u.WallClockUsed != 150*time.Millisecond {
		t.Errorf("add wrong: %+v", u)
	}
}

func TestIsBudgetExceeded(t *testing.T) {
	if axis, ok := IsBudgetExceeded(errors.New("wat")); ok || axis != "" {
		t.Error("non-budget error misclassified")
	}
}
