package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	rt "github.com/stigen/knative-agents/pkg/agentmodel/runtime"
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
