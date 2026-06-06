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

// M1.11: the hand-written DeepCopy of Resources must be independent — mutating the
// copy's maps must not bleed back into the original.
func TestAgentSessionSpec_ResourcesDeepCopy(t *testing.T) {
	orig := &AgentSessionSpec{Resources: &ResourceRequirements{
		Limits:   map[string]string{"memory": "512Mi"},
		Requests: map[string]string{"cpu": "100m"},
	}}
	cp := orig.DeepCopy()
	cp.Resources.Limits["memory"] = "1Gi"
	cp.Resources.Requests["cpu"] = "999m"
	if orig.Resources.Limits["memory"] != "512Mi" {
		t.Errorf("deep copy aliased Limits: original mutated to %s", orig.Resources.Limits["memory"])
	}
	if orig.Resources.Requests["cpu"] != "100m" {
		t.Errorf("deep copy aliased Requests: original mutated to %s", orig.Resources.Requests["cpu"])
	}
	// nil Resources copies cleanly.
	if (&AgentSessionSpec{}).DeepCopy().Resources != nil {
		t.Error("nil Resources must stay nil after DeepCopy")
	}
}
