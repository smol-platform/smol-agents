package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	if zero.BatchSize() != 1 || zero.PollIntervalMs() != 500 || zero.DeliveryTimeoutSeconds() != 300 {
		t.Errorf("defaults: batch=%d poll=%d delivery=%d, want 1/500/300",
			zero.BatchSize(), zero.PollIntervalMs(), zero.DeliveryTimeoutSeconds())
	}

	set := AgentSessionSpec{
		MaxConcurrentTurns: 4, TurnRetentionSeconds: 60, MaxTurnInputBytes: 2048, TurnHistoryLimit: 10,
		TurnBatchSize: 8, TurnPollIntervalMs: 250, TurnDeliveryTimeoutSeconds: 120,
	}
	if set.ConcurrentTurns() != 4 || set.RetentionSeconds() != 60 || set.InputBytesCap() != 2048 || set.HistoryLimit() != 10 {
		t.Errorf("set values must pass through: %+v", set)
	}
	if set.BatchSize() != 8 || set.PollIntervalMs() != 250 || set.DeliveryTimeoutSeconds() != 120 {
		t.Errorf("set batch/poll/delivery must pass through: %+v", set)
	}
}

// M2.17: the status carries cumulative usage + turn counters + last-turn time; the
// hand-written DeepCopy must not alias the LastTurnTime pointer.
func TestAgentSessionStatus_DeepCopy(t *testing.T) {
	ts := metav1.Unix(1000, 0)
	orig := &AgentSessionStatus{
		Usage:        Usage{Tokens: 1234, Steps: 5},
		Turns:        8,
		FailedTurns:  1,
		LastTurnTime: &ts,
	}
	cp := orig.DeepCopy()
	if cp.Usage.Tokens != 1234 || cp.Turns != 8 || cp.FailedTurns != 1 {
		t.Errorf("scalar status fields not copied: %+v", cp)
	}
	*cp.LastTurnTime = metav1.Unix(9999, 0)
	if orig.LastTurnTime.Time.Equal(cp.LastTurnTime.Time) {
		t.Error("LastTurnTime pointer aliased — mutating the copy changed the original")
	}
	if (&AgentSessionStatus{}).DeepCopy().LastTurnTime != nil {
		t.Error("nil LastTurnTime must stay nil")
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
