package eventsink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Emit sends a well-formed binary-mode CloudEvent: ce-* headers + the data body.
func TestEmit_BinaryModeHeadersAndBody(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	ev := Event{
		ID:      "run-uid-123",
		Type:    "com.smol-agents.run.completed",
		Source:  "/namespaces/tenant-a/agentruns/r1",
		Subject: "r1",
		Data:    json.RawMessage(`{"answer":42}`),
	}
	if err := Emit(context.Background(), srv.Client(), srv.URL, ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got.Header.Get("Ce-Id") != "run-uid-123" {
		t.Errorf("ce-id = %q", got.Header.Get("Ce-Id"))
	}
	if got.Header.Get("Ce-Specversion") != "1.0" {
		t.Errorf("ce-specversion = %q, want 1.0", got.Header.Get("Ce-Specversion"))
	}
	if got.Header.Get("Ce-Type") != "com.smol-agents.run.completed" {
		t.Errorf("ce-type = %q", got.Header.Get("Ce-Type"))
	}
	if got.Header.Get("Ce-Source") != "/namespaces/tenant-a/agentruns/r1" {
		t.Errorf("ce-source = %q", got.Header.Get("Ce-Source"))
	}
	if got.Header.Get("Ce-Subject") != "r1" {
		t.Errorf("ce-subject = %q", got.Header.Get("Ce-Subject"))
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content-type = %q", got.Header.Get("Content-Type"))
	}
	if string(body) != `{"answer":42}` {
		t.Errorf("body = %s, want the event data", body)
	}
}

// A non-2xx sink response is an error (the controller retries next reconcile).
func TestEmit_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := Emit(context.Background(), srv.Client(), srv.URL, Event{ID: "x", Data: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("a 500 from the sink must be an error")
	}
}

// Guard rails: empty sink URL and empty id are rejected before any request.
func TestEmit_Validation(t *testing.T) {
	if err := Emit(context.Background(), nil, "", Event{ID: "x"}); err == nil {
		t.Error("empty sink URL must error")
	}
	if err := Emit(context.Background(), nil, "http://example.invalid", Event{}); err == nil {
		t.Error("empty event id must error")
	}
}
