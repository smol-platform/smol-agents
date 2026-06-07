package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/agentruntime/openaillm"
)

func TestKeyFor_Stable(t *testing.T) {
	a := keyFor([]byte(`{"x":1}`))
	b := keyFor([]byte(`{"x":1}`))
	if a != b {
		t.Errorf("identical bodies → different keys: %s vs %s", a, b)
	}
	c := keyFor([]byte(`{"x":2}`))
	if a == c {
		t.Error("different bodies → same key")
	}
}

func TestServer_FallbackOnUnknownBody(t *testing.T) {
	s, err := newServer("")
	if err != nil {
		t.Fatal(err)
	}
	d := s.next("unknown")
	if d.FinalAnswer == nil {
		t.Error("expected fallback to terminate the loop")
	}
}

func TestServer_PlanFileSequence(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plans.json")
	body := []byte(`{"q":"hi"}`)
	key := keyFor(body)

	pf := PlanFile{
		Plans: map[string]ScriptedPlan{
			key: {Sequence: []rt.LLMDecision{
				{ToolCall: &rt.ToolCall{Tool: "search", Arguments: json.RawMessage(`{}`)}},
				{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"answer":"42"}`)}},
			}},
		},
	}
	raw, _ := json.Marshal(pf)
	if err := os.WriteFile(planFile, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := newServer(planFile)
	if err != nil {
		t.Fatal(err)
	}

	first := s.next(key)
	if first.ToolCall == nil || first.ToolCall.Tool != "search" {
		t.Errorf("first call: want tool=search, got %+v", first)
	}
	second := s.next(key)
	if second.FinalAnswer == nil {
		t.Errorf("second call: want FinalAnswer, got %+v", second)
	}
	// Past sequence end → fallback.
	third := s.next(key)
	if third.FinalAnswer == nil || string(third.FinalAnswer.Output) != `{"answer":"done"}` {
		t.Errorf("past-sequence fallthrough wrong: %+v", third)
	}
}

func TestServer_GlobalSequenceWinsOverPerKey(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plans.json")
	pf := PlanFile{
		GlobalSequence: []rt.LLMDecision{
			{ToolCall: &rt.ToolCall{Tool: "echo", Arguments: json.RawMessage(`{"x":"hi"}`)}},
			{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"echoed":"hi"}`)}},
		},
		Plans: map[string]ScriptedPlan{
			"some-key": {Plan: &rt.LLMDecision{
				FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"per-key":"won"}`)},
			}},
		},
	}
	raw, _ := json.Marshal(pf)
	_ = os.WriteFile(planFile, raw, 0o644)

	s, err := newServer(planFile)
	if err != nil {
		t.Fatal(err)
	}

	// Both calls should hit the global sequence regardless of key.
	first := s.next("some-key")
	if first.ToolCall == nil || first.ToolCall.Tool != "echo" {
		t.Errorf("first global step lost: %+v", first)
	}
	second := s.next("a-different-key")
	if second.FinalAnswer == nil || string(second.FinalAnswer.Output) != `{"echoed":"hi"}` {
		t.Errorf("second global step lost: %+v", second)
	}
	// Sequence exhausted → fallback.
	third := s.next("some-key")
	if third.FinalAnswer == nil || string(third.FinalAnswer.Output) != `{"answer":"done"}` {
		t.Errorf("post-sequence wasn't fallback: %+v", third)
	}
}

func TestServer_HTTPChat(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plans.json")
	body := []byte(`{"q":"hello"}`)
	key := keyFor(body)
	pf := PlanFile{
		Plans: map[string]ScriptedPlan{
			key: {Plan: &rt.LLMDecision{
				FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"answer":"world"}`)},
			}},
		},
	}
	raw, _ := json.Marshal(pf)
	_ = os.WriteFile(planFile, raw, 0o644)

	s, err := newServer(planFile)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat", bytes.NewReader(body))
	s.chat(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var got rt.LLMDecision
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.FinalAnswer == nil || string(got.FinalAnswer.Output) != `{"answer":"world"}` {
		t.Errorf("response wrong: %+v", got)
	}
}

// TestChatCompletions_OpenAIWire drives the REAL production loop-mode client
// (pkg/agentruntime/openaillm) against the OpenAI endpoint, proving the
// LLMDecision→OpenAI translation round-trips: a scripted tool call surfaces as
// a ToolCall decision, the next call surfaces as a FinalAnswer. This is the
// seam that lets an operator-scheduled loop pod run against the mock.
func TestChatCompletions_OpenAIWire(t *testing.T) {
	s := &server{
		plans:   map[string]ScriptedPlan{},
		cursors: map[string]int{},
		fallback: rt.LLMDecision{
			FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"answer":"done"}`)},
		},
		globalSequence: []rt.LLMDecision{
			{ToolCall: &rt.ToolCall{Tool: "search", Arguments: json.RawMessage(`{"q":"x"}`)}},
			{FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"answer":"final"}`)}},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := openaillm.New(srv.URL, "")

	d1, err := c.Chat(context.Background(), agentruntime.ChatRequest{Instructions: "agent A"})
	if err != nil {
		t.Fatalf("chat 1: %v", err)
	}
	if d1.ToolCall == nil || d1.ToolCall.Tool != "search" {
		t.Fatalf("call 1: want ToolCall search, got %+v", d1)
	}
	if string(d1.ToolCall.Arguments) != `{"q":"x"}` {
		t.Errorf("call 1 args = %q, want {\"q\":\"x\"}", d1.ToolCall.Arguments)
	}

	d2, err := c.Chat(context.Background(), agentruntime.ChatRequest{Instructions: "agent A"})
	if err != nil {
		t.Fatalf("chat 2: %v", err)
	}
	if d2.FinalAnswer == nil {
		t.Fatalf("call 2: want FinalAnswer, got %+v", d2)
	}
}
