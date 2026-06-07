package invokers

import (
	"context"
	"encoding/json"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/teamtask"
)

func taskTool() v1.Tool { return v1.Tool{Name: "tasks", Spec: v1.ToolSpec{Kind: v1.ToolTask}} }

func invokeTask(t *testing.T, inv *TaskInvoker, args string) map[string]any {
	t.Helper()
	obs, err := inv.Invoke(context.Background(), taskTool(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("invoke %s: %v", args, err)
	}
	var out map[string]any
	if err := json.Unmarshal(obs.Output, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	return out
}

func TestTaskInvoker_Lifecycle(t *testing.T) {
	store := teamtask.NewMemStore()
	inv := &TaskInvoker{Store: store, Owner: "alice"}

	created := invokeTask(t, inv, `{"op":"create","title":"research"}`)
	if created["ok"] != true || created["id"] == nil {
		t.Fatalf("create: %+v", created)
	}
	id := created["id"].(string)

	list := invokeTask(t, inv, `{"op":"list"}`)
	if tasks, ok := list["tasks"].([]any); !ok || len(tasks) != 1 {
		t.Fatalf("list: %+v", list)
	}

	claim := invokeTask(t, inv, `{"op":"claim","id":"`+id+`"}`)
	if claim["ok"] != true {
		t.Fatalf("claim: %+v", claim)
	}

	done := invokeTask(t, inv, `{"op":"complete","id":"`+id+`","result":"found it"}`)
	if done["ok"] != true {
		t.Fatalf("complete: %+v", done)
	}
}

func TestTaskInvoker_ClaimOutcomes(t *testing.T) {
	store := teamtask.NewMemStore()
	id, _ := store.Create(context.Background(), teamtask.Task{Title: "t"})
	alice := &TaskInvoker{Store: store, Owner: "alice"}
	bob := &TaskInvoker{Store: store, Owner: "bob"}

	if out := invokeTask(t, alice, `{"op":"claim","id":"`+id+`"}`); out["ok"] != true {
		t.Fatalf("alice claim: %+v", out)
	}
	// Bob loses the (already-claimed) task — a structured outcome, not a hard error.
	if out := invokeTask(t, bob, `{"op":"claim","id":"`+id+`"}`); out["ok"] != false {
		t.Fatalf("bob claim must report ok=false: %+v", out)
	}
	// Bob cannot complete a task he does not own.
	if out := invokeTask(t, bob, `{"op":"complete","id":"`+id+`"}`); out["ok"] != false {
		t.Fatalf("bob complete must report ok=false: %+v", out)
	}
	// Claiming a missing task is a structured outcome too.
	if out := invokeTask(t, alice, `{"op":"claim","id":"ghost"}`); out["ok"] != false {
		t.Fatalf("claim missing: %+v", out)
	}
}

func TestTaskInvoker_Errors(t *testing.T) {
	// No store → fail-closed (agent is not a team member).
	if _, err := (&TaskInvoker{}).Invoke(context.Background(), taskTool(), json.RawMessage(`{"op":"list"}`)); err == nil {
		t.Fatalf("no store must error")
	}
	inv := &TaskInvoker{Store: teamtask.NewMemStore(), Owner: "m"}
	// Unknown op → error.
	if _, err := inv.Invoke(context.Background(), taskTool(), json.RawMessage(`{"op":"frobnicate"}`)); err == nil {
		t.Fatalf("unknown op must error")
	}
	// Malformed args → error.
	if _, err := inv.Invoke(context.Background(), taskTool(), json.RawMessage(`{`)); err == nil {
		t.Fatalf("bad args must error")
	}
}
