package sessionqueue

import (
	"context"
	"encoding/json"

	"github.com/nats-io/nats.go"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// ProgressSink publishes each plan-act-observe Step to an ephemeral per-turn
// progress subject (agentsession.<key>.progress.<turnID>) so a gateway or attach
// client can tail live progress (the LangGraph updates/tasks stream). It
// structurally satisfies agentruntime.StepSink (Emit(ctx, v1.Step)).
//
// Best-effort + lossy BY DESIGN: a core-NATS publish (not JetStream) — a missed
// frame is fine because the durable, authoritative record is still the folded
// Result on the turn's .result subject. Steps must already be redacted by the
// executor (RedactPatterns) before they reach here.
type ProgressSink struct {
	nc      *nats.Conn
	subject string
}

// NewProgressSink builds a sink for one turn. The session worker sets it on the
// executor's StepSink for the duration of the turn.
func NewProgressSink(nc *nats.Conn, sessionKey, turnID string) *ProgressSink {
	return &ProgressSink{nc: nc, subject: progressSubject(sessionKey, turnID)}
}

func progressSubject(key, turnID string) string {
	return subjectPrefix + "." + key + ".progress." + turnID
}

// Emit publishes the step; failures are swallowed (progress is non-critical).
func (p *ProgressSink) Emit(_ context.Context, s v1.Step) {
	if p == nil || p.nc == nil {
		return
	}
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = p.nc.Publish(p.subject, data)
}
