package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
)

func TestGateway_PostThenFetchResult(t *testing.T) {
	q := sessionqueue.NewMemQueue()
	srv := httptest.NewServer((&Gateway{Queue: q}).Handler())
	defer srv.Close()

	// POST a turn → 202 queued + a turnId.
	resp, err := http.Post(srv.URL+"/v1/sessions/tenant-a/s1/turns", "application/json",
		strings.NewReader(`{"input":{"prompt":"hi"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", resp.StatusCode)
	}
	var posted struct {
		TurnID string `json:"turnId"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&posted)
	resp.Body.Close()
	if posted.TurnID == "" {
		t.Fatal("POST returned no turnId")
	}

	// A worker consumes the turn and publishes its result.
	key := sessionqueue.SessionKey("tenant-a", "s1")
	turns, _ := q.Consume(context.Background(), key, 10)
	if len(turns) != 1 || string(turns[0].Body) != `{"input":{"prompt":"hi"}}` {
		t.Fatalf("queue should hold the posted turn verbatim, got %+v", turns)
	}
	_ = q.PublishResult(context.Background(), key, posted.TurnID, []byte(`{"phase":"Completed","output":"done"}`))

	// GET the result → 200.
	gr, err := http.Get(srv.URL + "/v1/sessions/tenant-a/s1/turns/" + posted.TurnID + "?wait=1s")
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Body.Close()
	if gr.StatusCode != http.StatusOK {
		t.Fatalf("GET result status = %d, want 200", gr.StatusCode)
	}
	body, _ := io.ReadAll(gr.Body)
	if !strings.Contains(string(body), "Completed") {
		t.Errorf("result body missing the turn result: %s", body)
	}
}

func TestGateway_RejectsInvalidBody(t *testing.T) {
	srv := httptest.NewServer((&Gateway{Queue: sessionqueue.NewMemQueue()}).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/sessions/t/s/turns", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid body status = %d, want 400", resp.StatusCode)
	}
}

func TestGateway_ResultNotReady(t *testing.T) {
	srv := httptest.NewServer((&Gateway{Queue: sessionqueue.NewMemQueue()}).Handler())
	defer srv.Close()
	gr, err := http.Get(srv.URL + "/v1/sessions/t/s/turns/never?wait=10ms")
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Body.Close()
	if gr.StatusCode != http.StatusNotFound {
		t.Errorf("missing result status = %d, want 404", gr.StatusCode)
	}
}
