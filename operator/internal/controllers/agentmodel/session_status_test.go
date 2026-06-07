package agentmodel

import (
	"testing"
	"time"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/turnmodel"
)

// M2.19: the worker's SessionSummary folds field-wise into AgentSessionStatus
// (Usage verbatim — never Usage.Add), including the LastTurnTime conversion.
func TestApplySummaryToStatus(t *testing.T) {
	lt := time.Unix(5000, 0).UTC()
	sum := turnmodel.SessionSummary{
		Usage:        pure.Usage{Tokens: 999, Steps: 4},
		Turns:        12,
		FailedTurns:  2,
		LastTurnTime: &lt,
	}
	var st pure.AgentSessionStatus
	applySummaryToStatus(&st, sum)
	if st.Usage.Tokens != 999 || st.Usage.Steps != 4 || st.Turns != 12 || st.FailedTurns != 2 {
		t.Errorf("mirror = %+v", st)
	}
	if st.LastTurnTime == nil || !st.LastTurnTime.Time.Equal(lt) {
		t.Errorf("LastTurnTime = %v, want %v", st.LastTurnTime, lt)
	}

	var st2 pure.AgentSessionStatus
	applySummaryToStatus(&st2, turnmodel.SessionSummary{Turns: 1})
	if st2.LastTurnTime != nil {
		t.Error("a summary with no LastTurnTime must leave status.lastTurnTime nil")
	}
}
