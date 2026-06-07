package invokers

import (
	"context"
	"encoding/json"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/teammailbox"
)

func teamBusTool() v1.Tool {
	return v1.Tool{Name: "bus", Spec: v1.ToolSpec{Kind: v1.ToolTeamBus}}
}

func invokeBus(t *testing.T, inv *TeamBusInvoker, args string) map[string]any {
	t.Helper()
	obs, err := inv.Invoke(context.Background(), teamBusTool(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("invoke %s: %v", args, err)
	}
	var out map[string]any
	if err := json.Unmarshal(obs.Output, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestTeamBusInvoker_PubSubReceive(t *testing.T) {
	bus := teammailbox.NewMemBus()
	inv := &TeamBusInvoker{Bus: bus, Self: "alice"}

	if out := invokeBus(t, inv, `{"op":"subscribe","topic":"findings"}`); out["ok"] != true {
		t.Fatalf("subscribe: %+v", out)
	}
	if out := invokeBus(t, inv, `{"op":"publish","topic":"findings","body":"found A"}`); out["ok"] != true {
		t.Fatalf("publish: %+v", out)
	}
	out := invokeBus(t, inv, `{"op":"receive"}`)
	evs, ok := out["events"].([]any)
	if !ok || len(evs) != 1 {
		t.Fatalf("receive: %+v", out)
	}
	ev := evs[0].(map[string]any)
	if ev["from"] != "alice" || ev["body"] != "found A" || ev["topic"] != "findings" {
		t.Fatalf("event wrong: %+v", ev)
	}
}

func TestTeamBusInvoker_Errors(t *testing.T) {
	if _, err := (&TeamBusInvoker{Self: "m"}).Invoke(context.Background(), teamBusTool(), json.RawMessage(`{"op":"receive"}`)); err == nil {
		t.Fatalf("no bus must error")
	}
	inv := &TeamBusInvoker{Bus: teammailbox.NewMemBus(), Self: "m"}
	if _, err := inv.Invoke(context.Background(), teamBusTool(), json.RawMessage(`{"op":"publish"}`)); err == nil {
		t.Fatalf("publish without topic must error")
	}
	if _, err := inv.Invoke(context.Background(), teamBusTool(), json.RawMessage(`{"op":"nope"}`)); err == nil {
		t.Fatalf("unknown op must error")
	}
}
