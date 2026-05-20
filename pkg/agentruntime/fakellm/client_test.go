package fakellm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rt "github.com/stigen/smol-agents/pkg/agentmodel/runtime"
	"github.com/stigen/smol-agents/pkg/agentruntime"
)

func TestClient_Chat_HappyPath(t *testing.T) {
	want := rt.LLMDecision{
		FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"answer":"42"}`)},
		TokensIn:    10, TokensOut: 5,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"Instructions"`) {
			t.Errorf("body missing ChatRequest fields: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := New(srv.URL)
	got, err := c.Chat(context.Background(), agentruntime.ChatRequest{
		Instructions: "be helpful",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.FinalAnswer == nil || string(got.FinalAnswer.Output) != `{"answer":"42"}` {
		t.Errorf("decision lost in round-trip: %+v", got)
	}
	if got.TokensIn != 10 || got.TokensOut != 5 {
		t.Errorf("tokens lost: in=%d out=%d", got.TokensIn, got.TokensOut)
	}
}

func TestClient_Chat_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.Chat(context.Background(), agentruntime.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 error, got %v", err)
	}
}

func TestClient_Chat_RespectsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: &http.Client{}, Timeout: 50 * time.Millisecond}
	_, err := c.Chat(context.Background(), agentruntime.ChatRequest{})
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestClient_RejectsMissingBaseURL(t *testing.T) {
	c := &Client{}
	_, err := c.Chat(context.Background(), agentruntime.ChatRequest{})
	if err == nil {
		t.Error("expected error for empty BaseURL")
	}
}
