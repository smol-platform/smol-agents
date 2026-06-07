package invokers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/teamtask"
)

// TaskInvoker drives the kind=task loop tool (multi-agent orchestration P1): a
// team member lists / claims / completes work on the team's shared task list
// (teamtask.Store, a per-team NATS KV bucket). A claim is atomic, so two members
// never win the same task; a task is claimable only once its dependencies are
// complete. The invoker is wired ONLY when the pod carries a team context
// (WireTaskInvoker) — fail-closed: outside a team, kind=task has no invoker and
// the executor rejects the call.
type TaskInvoker struct {
	Store teamtask.Store
	// Owner is the member's name within the team (the claim owner).
	Owner string
}

type taskArgs struct {
	// Op is list | claim | complete | create.
	Op string `json:"op"`
	// ID is the task id (claim, complete).
	ID string `json:"id,omitempty"`
	// Title + Deps create a new task.
	Title string   `json:"title,omitempty"`
	Deps  []string `json:"deps,omitempty"`
	// Result is the outcome reported on complete.
	Result string `json:"result,omitempty"`
}

func (i *TaskInvoker) Invoke(ctx context.Context, tool v1.Tool, args json.RawMessage) (rt.Observation, error) {
	start := time.Now()
	if i.Store == nil {
		return rt.Observation{}, fmt.Errorf("task tool %q: no team task store (agent is not a team member)", tool.Name)
	}
	var a taskArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return rt.Observation{}, fmt.Errorf("task tool %q: bad args: %w", tool.Name, err)
		}
	}

	var out any
	switch a.Op {
	case "list", "":
		tasks, err := i.Store.List(ctx)
		if err != nil {
			return rt.Observation{}, fmt.Errorf("task list: %w", err)
		}
		out = map[string]any{"tasks": tasks}
	case "create":
		id, err := i.Store.Create(ctx, teamtask.Task{Title: a.Title, Deps: a.Deps})
		if err != nil {
			return rt.Observation{}, fmt.Errorf("task create: %w", err)
		}
		out = map[string]any{"ok": true, "id": id}
	case "claim":
		task, err := i.Store.Claim(ctx, a.ID, i.Owner)
		switch {
		case errors.Is(err, teamtask.ErrNotFound):
			out = map[string]any{"ok": false, "reason": "no such task"}
		case errors.Is(err, teamtask.ErrNotClaimable):
			out = map[string]any{"ok": false, "reason": "not claimable: already claimed or a dependency is incomplete"}
		case errors.Is(err, teamtask.ErrConflict):
			out = map[string]any{"ok": false, "reason": "claim lost a concurrency race; retry"}
		case err != nil:
			return rt.Observation{}, fmt.Errorf("task claim: %w", err)
		default:
			out = map[string]any{"ok": true, "task": task}
		}
	case "complete":
		err := i.Store.Complete(ctx, a.ID, i.Owner, a.Result)
		switch {
		case errors.Is(err, teamtask.ErrNotFound):
			out = map[string]any{"ok": false, "reason": "no such task"}
		case errors.Is(err, teamtask.ErrNotOwner):
			out = map[string]any{"ok": false, "reason": "you do not own this task"}
		case errors.Is(err, teamtask.ErrNotClaimable):
			out = map[string]any{"ok": false, "reason": "task is not in progress"}
		case errors.Is(err, teamtask.ErrConflict):
			out = map[string]any{"ok": false, "reason": "complete lost a concurrency race; retry"}
		case err != nil:
			return rt.Observation{}, fmt.Errorf("task complete: %w", err)
		default:
			out = map[string]any{"ok": true, "id": a.ID}
		}
	default:
		return rt.Observation{}, fmt.Errorf("task tool %q: unknown op %q (want list|claim|complete|create)", tool.Name, a.Op)
	}

	body, err := json.Marshal(out)
	if err != nil {
		return rt.Observation{}, err
	}
	// Task ops consume no LLM tokens/tool-calls — leave Tokens/ToolCalls zero
	// (never inflate the team usage roll-up with bookkeeping calls).
	return rt.Observation{Output: body, DurationMs: time.Since(start).Milliseconds()}, nil
}
