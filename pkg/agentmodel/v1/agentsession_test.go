package v1

import "testing"

// M2.17: the turn-scaling accessors default-preserve today's serial, unbounded
// behavior (0 → the default), and pass through set values.
func TestAgentSessionSpec_TurnAccessors(t *testing.T) {
	var zero AgentSessionSpec
	if zero.ConcurrentTurns() != 1 {
		t.Errorf("default ConcurrentTurns = %d, want 1 (proven serial path)", zero.ConcurrentTurns())
	}
	if zero.RetentionSeconds() != 3600 {
		t.Errorf("default RetentionSeconds = %d, want 3600", zero.RetentionSeconds())
	}
	if zero.InputBytesCap() != 1<<20 {
		t.Errorf("default InputBytesCap = %d, want 1MiB", zero.InputBytesCap())
	}
	if zero.HistoryLimit() != 0 {
		t.Errorf("default HistoryLimit = %d, want 0 (unbounded)", zero.HistoryLimit())
	}

	set := AgentSessionSpec{MaxConcurrentTurns: 4, TurnRetentionSeconds: 60, MaxTurnInputBytes: 2048, TurnHistoryLimit: 10}
	if set.ConcurrentTurns() != 4 || set.RetentionSeconds() != 60 || set.InputBytesCap() != 2048 || set.HistoryLimit() != 10 {
		t.Errorf("set values must pass through: %+v", set)
	}
}
