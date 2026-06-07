package invokers

import (
	"context"
	"encoding/json"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/teammailbox"
)

func teammateTool() v1.Tool {
	return v1.Tool{Name: "teammate", Spec: v1.ToolSpec{Kind: v1.ToolTeammate}}
}

func invokeTeammate(t *testing.T, inv *TeammateInvoker, args string) map[string]any {
	t.Helper()
	obs, err := inv.Invoke(context.Background(), teammateTool(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("invoke %s: %v", args, err)
	}
	var out map[string]any
	if err := json.Unmarshal(obs.Output, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestTeammateInvoker_SendReceive(t *testing.T) {
	mbx := teammailbox.NewMemMailbox()
	alice := &TeammateInvoker{Mailbox: mbx, Self: "alice"}
	bob := &TeammateInvoker{Mailbox: mbx, Self: "bob"}

	if out := invokeTeammate(t, alice, `{"op":"send","to":"bob","message":"start task 3"}`); out["ok"] != true {
		t.Fatalf("send: %+v", out)
	}
	// Bob receives it; the From is alice.
	out := invokeTeammate(t, bob, `{"op":"receive"}`)
	msgs, ok := out["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("bob receive: %+v", out)
	}
	m := msgs[0].(map[string]any)
	if m["from"] != "alice" || m["body"] != "start task 3" {
		t.Fatalf("message wrong: %+v", m)
	}
	// Alice's own inbox is empty (and the field is an empty array, never null).
	aliceOut := invokeTeammate(t, alice, `{"op":"receive"}`)
	if msgs, _ := aliceOut["messages"].([]any); len(msgs) != 0 {
		t.Fatalf("alice inbox must be empty: %+v", aliceOut)
	}
}

func TestTeammateInvoker_Errors(t *testing.T) {
	// No mailbox → fail-closed.
	if _, err := (&TeammateInvoker{Self: "m"}).Invoke(context.Background(), teammateTool(), json.RawMessage(`{"op":"receive"}`)); err == nil {
		t.Fatalf("no mailbox must error")
	}
	inv := &TeammateInvoker{Mailbox: teammailbox.NewMemMailbox(), Self: "m"}
	// send without 'to' → error.
	if _, err := inv.Invoke(context.Background(), teammateTool(), json.RawMessage(`{"op":"send","message":"x"}`)); err == nil {
		t.Fatalf("send without 'to' must error")
	}
	// unknown op → error.
	if _, err := inv.Invoke(context.Background(), teammateTool(), json.RawMessage(`{"op":"broadcast"}`)); err == nil {
		t.Fatalf("unknown op must error")
	}
}
