package invokers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/teammailbox"
)

// TeamBusInvoker drives the kind=teambus loop tool (multi-agent orchestration
// P5): a member publishes events to a team topic, subscribes to topics, and
// drains received events. It is the emergent pub/sub channel (the bus pattern);
// the per-member bus credential confines it to the team's bus subtree. Wired only
// inside a team context (WireTeamBusInvoker) — fail-closed otherwise.
type TeamBusInvoker struct {
	Bus teammailbox.Bus
	// Self is this member's name (the From on published events).
	Self string
}

type teamBusArgs struct {
	// Op is publish | subscribe | receive.
	Op    string `json:"op"`
	Topic string `json:"topic,omitempty"`
	Body  string `json:"body,omitempty"`
	Max   int    `json:"max,omitempty"`
}

func (i *TeamBusInvoker) Invoke(ctx context.Context, tool v1.Tool, args json.RawMessage) (rt.Observation, error) {
	start := time.Now()
	if i.Bus == nil {
		return rt.Observation{}, fmt.Errorf("teambus tool %q: no team bus (agent is not a team member)", tool.Name)
	}
	var a teamBusArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return rt.Observation{}, fmt.Errorf("teambus tool %q: bad args: %w", tool.Name, err)
		}
	}

	var out any
	switch a.Op {
	case "publish":
		if a.Topic == "" {
			return rt.Observation{}, fmt.Errorf("teambus publish: 'topic' is required")
		}
		if err := i.Bus.Publish(ctx, teammailbox.BusEvent{Topic: a.Topic, From: i.Self, Body: a.Body}); err != nil {
			return rt.Observation{}, fmt.Errorf("teambus publish: %w", err)
		}
		out = map[string]any{"ok": true, "topic": a.Topic}
	case "subscribe":
		if a.Topic == "" {
			return rt.Observation{}, fmt.Errorf("teambus subscribe: 'topic' is required")
		}
		if err := i.Bus.Subscribe(ctx, a.Topic); err != nil {
			return rt.Observation{}, fmt.Errorf("teambus subscribe: %w", err)
		}
		out = map[string]any{"ok": true, "subscribed": a.Topic}
	case "receive", "":
		evs, err := i.Bus.Receive(ctx, a.Max)
		if err != nil {
			return rt.Observation{}, fmt.Errorf("teambus receive: %w", err)
		}
		if evs == nil {
			evs = []teammailbox.BusEvent{} // always a JSON array, never null
		}
		out = map[string]any{"events": evs}
	default:
		return rt.Observation{}, fmt.Errorf("teambus tool %q: unknown op %q (want publish|subscribe|receive)", tool.Name, a.Op)
	}

	body, err := json.Marshal(out)
	if err != nil {
		return rt.Observation{}, err
	}
	return rt.Observation{Output: body, DurationMs: time.Since(start).Milliseconds()}, nil
}
