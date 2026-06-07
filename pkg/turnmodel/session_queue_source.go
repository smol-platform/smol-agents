package turnmodel

import (
	"context"
	"encoding/json"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
)

// QueueSource is a TurnSource backed by a sessionqueue.Queue (NATS JetStream in
// production): it pulls pending turns for one session, decodes each body as an
// AgentRunSpec, and wires Ack to the queue's at-least-once ack.
type QueueSource struct {
	Queue      sessionqueue.Queue
	SessionKey string
	Max        int
}

func (s *QueueSource) Poll(ctx context.Context) ([]InboundTurn, error) {
	msgs, err := s.Queue.Consume(ctx, s.SessionKey, s.Max)
	if err != nil {
		return nil, err
	}
	out := make([]InboundTurn, 0, len(msgs))
	for _, m := range msgs {
		var spec v1.AgentRunSpec
		if err := json.Unmarshal(m.Body, &spec); err != nil {
			if m.Ack != nil {
				_ = m.Ack() // drop a malformed turn so it can't wedge the queue
			}
			continue
		}
		out = append(out, InboundTurn{ID: m.ID, Spec: spec, Ack: m.Ack})
	}
	return out, nil
}

// QueueSink publishes folded turn results back to the queue so the gateway's
// synchronous callers can fetch them by turn id.
type QueueSink struct {
	Queue      sessionqueue.Queue
	SessionKey string
}

func (s *QueueSink) Publish(ctx context.Context, turnID string, st SessionTurn) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.Queue.PublishResult(ctx, s.SessionKey, turnID, b)
}
