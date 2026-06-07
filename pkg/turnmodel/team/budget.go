package team

import (
	"errors"
	"fmt"
	"time"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// ErrTeamBudgetExceeded is returned by BudgetGuard.AllowsMore when a gating axis
// of the team budget is reached.
var ErrTeamBudgetExceeded = errors.New("team: team budget exceeded")

// BudgetGuard enforces a team-wide Budget as a HARD ceiling on cumulative
// (field-wise) member usage — the team-scale analog of the per-run budget, and
// the backstop beneath the coordinator's convergence limits. Cost is NEVER a gate
// (milli-USD, observability only), matching the platform-wide invariant.
type BudgetGuard struct {
	Budget pure.Budget
}

// AllowsMore returns ErrTeamBudgetExceeded (naming the axis) if cumulative usage
// has reached any gating axis: steps, tokens, wall-clock, or — only when capped
// > 0 — tool calls. A team cannot out-spend its ceiling.
func (g *BudgetGuard) AllowsMore(used pure.Usage) error {
	if used.Steps >= g.Budget.MaxSteps {
		return fmt.Errorf("%w: steps (%d/%d)", ErrTeamBudgetExceeded, used.Steps, g.Budget.MaxSteps)
	}
	if used.Tokens >= g.Budget.MaxTokens {
		return fmt.Errorf("%w: tokens (%d/%d)", ErrTeamBudgetExceeded, used.Tokens, g.Budget.MaxTokens)
	}
	if used.WallClockUsed >= time.Duration(g.Budget.MaxWallClockSeconds)*time.Second {
		return fmt.Errorf("%w: wallclock", ErrTeamBudgetExceeded)
	}
	if g.Budget.MaxToolCalls > 0 && used.ToolCalls >= g.Budget.MaxToolCalls {
		return fmt.Errorf("%w: toolCalls (%d/%d)", ErrTeamBudgetExceeded, used.ToolCalls, g.Budget.MaxToolCalls)
	}
	return nil
}
