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

// TeammateInvoker drives the kind=teammate loop tool (multi-agent orchestration
// P2): a member messages another member BY NAME (teammate.send) and drains its
// own inbox (teammate.receive). Delivery is the per-team mailbox
// (teammailbox.Mailbox); the per-member NATS credential makes "read only your own
// inbox" the enforced boundary. Wired ONLY inside a team context
// (WireTeammateInvoker) — fail-closed otherwise.
type TeammateInvoker struct {
	Mailbox teammailbox.Mailbox
	// Self is this member's name (the From on sent messages, the inbox drained).
	Self string
}

type teammateArgs struct {
	// Op is send | receive.
	Op string `json:"op"`
	// To + Message send a note.
	To      string `json:"to,omitempty"`
	Message string `json:"message,omitempty"`
	// Max bounds a receive batch (0 → all buffered).
	Max int `json:"max,omitempty"`
}

func (i *TeammateInvoker) Invoke(ctx context.Context, tool v1.Tool, args json.RawMessage) (rt.Observation, error) {
	start := time.Now()
	if i.Mailbox == nil {
		return rt.Observation{}, fmt.Errorf("teammate tool %q: no team mailbox (agent is not a team member)", tool.Name)
	}
	var a teammateArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return rt.Observation{}, fmt.Errorf("teammate tool %q: bad args: %w", tool.Name, err)
		}
	}

	var out any
	switch a.Op {
	case "send":
		if a.To == "" {
			return rt.Observation{}, fmt.Errorf("teammate send: 'to' is required")
		}
		if err := i.Mailbox.Send(ctx, teammailbox.Message{From: i.Self, To: a.To, Body: a.Message}); err != nil {
			return rt.Observation{}, fmt.Errorf("teammate send: %w", err)
		}
		out = map[string]any{"ok": true, "to": a.To}
	case "receive", "":
		msgs, err := i.Mailbox.Receive(ctx, i.Self, a.Max)
		if err != nil {
			return rt.Observation{}, fmt.Errorf("teammate receive: %w", err)
		}
		if msgs == nil {
			msgs = []teammailbox.Message{} // always a JSON array, never null
		}
		out = map[string]any{"messages": msgs}
	default:
		return rt.Observation{}, fmt.Errorf("teammate tool %q: unknown op %q (want send|receive)", tool.Name, a.Op)
	}

	body, err := json.Marshal(out)
	if err != nil {
		return rt.Observation{}, err
	}
	// Messaging consumes no LLM tokens/tool-calls — never inflate the team roll-up.
	return rt.Observation{Output: body, DurationMs: time.Since(start).Milliseconds()}, nil
}
