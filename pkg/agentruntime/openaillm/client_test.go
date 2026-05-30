package openaillm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

func TestClient_FinalAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"the answer"}}],` +
			`"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, HTTP: srv.Client()}
	dec, err := c.Chat(context.Background(), agentruntime.ChatRequest{
		Model: v1.ModelRef{Name: "gpt-4"}, Instructions: "be terse", Input: json.RawMessage(`{"prompt":"yo"}`),
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if dec.FinalAnswer == nil || string(dec.FinalAnswer.Output) != `"the answer"` {
		t.Errorf("final = %+v", dec.FinalAnswer)
	}
	if dec.ToolCall != nil {
		t.Error("unexpected tool call")
	}
	if dec.TokensIn != 7 || dec.TokensOut != 3 {
		t.Errorf("usage = %d/%d, want 7/3", dec.TokensIn, dec.TokensOut)
	}
}

func TestClient_ToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":` +
			`[{"id":"x","type":"function","function":{"name":"search","arguments":"{\"q\":\"cats\"}"}}]}}]}`))
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, HTTP: srv.Client()}
	dec, err := c.Chat(context.Background(), agentruntime.ChatRequest{Model: v1.ModelRef{Name: "gpt-4"}, Input: json.RawMessage(`"go"`)})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if dec.ToolCall == nil || dec.ToolCall.Tool != "search" {
		t.Fatalf("toolCall = %+v", dec.ToolCall)
	}
	if string(dec.ToolCall.Arguments) != `{"q":"cats"}` {
		t.Errorf("args = %s", dec.ToolCall.Arguments)
	}
}

func TestClient_RequestShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-x" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, APIKey: "sk-x", HTTP: srv.Client()}
	temp := 0.3
	_, err := c.Chat(context.Background(), agentruntime.ChatRequest{
		Model:        v1.ModelRef{Name: "m1", Temperature: &temp},
		Instructions: "sys",
		Input:        json.RawMessage(`{"question":"q?"}`),
		Seed:         42,
		Tools:        []v1.Tool{{Name: "search", Spec: v1.ToolSpec{Description: "web", InputSchema: json.RawMessage(`{"type":"object"}`)}}},
		History: []v1.Step{{Index: 0, ToolCalls: []v1.ToolCallRecord{
			{Tool: "search", Arguments: json.RawMessage(`{"q":"x"}`), Result: json.RawMessage(`{"r":1}`)},
		}}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got["model"] != "m1" || got["temperature"] != 0.3 {
		t.Errorf("model/temp = %v/%v", got["model"], got["temperature"])
	}
	if got["seed"] != float64(42) {
		t.Errorf("seed = %v, want 42", got["seed"])
	}
	// system, user, assistant(tool_call), tool(result).
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4: %v", len(msgs), msgs)
	}
	if m, _ := msgs[0].(map[string]any); m["role"] != "system" || m["content"] != "sys" {
		t.Errorf("system msg = %v", msgs[0])
	}
	if m, _ := msgs[1].(map[string]any); m["role"] != "user" || m["content"] != "q?" {
		t.Errorf("user msg = %v", msgs[1])
	}
	if m, _ := msgs[2].(map[string]any); m["role"] != "assistant" {
		t.Errorf("assistant(tool_call) msg = %v", msgs[2])
	}
	if m, _ := msgs[3].(map[string]any); m["role"] != "tool" {
		t.Errorf("tool(result) msg = %v", msgs[3])
	}
	if tools, _ := got["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %v", got["tools"])
	}
}
