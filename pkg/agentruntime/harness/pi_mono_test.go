package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// M4.15: the pi-mono harness POSTs {prompt,system,model,seed} to the bridge and
// folds its {output,tokensIn,tokensOut,toolCalls} into a Response — the first
// CLI-family harness with honest tokens + tool-calls.
func TestPiMonoHarness_DrivesBridge(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"output":"42","tokensIn":5,"tokensOut":8,"toolCalls":[{"name":"bash"}]}`))
	}))
	defer srv.Close()

	h := &PiMonoHarness{Client: srv.Client()}
	req := Request{
		Spec:         v1.HarnessSpec{Kind: v1.HarnessPiMono, PiMono: &v1.HarnessPiMonoSpec{URL: srv.URL, Model: "pi-1"}},
		Instructions: "be brief",
		Input:        json.RawMessage(`{"prompt":"what is 6*7"}`),
		Seed:         11,
	}
	resp, err := h.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(resp.Output) != "42" || resp.TokensIn != 5 || resp.TokensOut != 8 {
		t.Errorf("resp = %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Tool != "bash" {
		t.Errorf("toolCalls = %+v", resp.ToolCalls)
	}
	// Request body carried prompt/system/model.
	if gotBody["prompt"] != "what is 6*7" || gotBody["system"] != "be brief" || gotBody["model"] != "pi-1" {
		t.Errorf("bridge request body = %+v", gotBody)
	}
}

// M4.15: a cancelled context aborts the run.
func TestPiMonoHarness_CancelAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never responds
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &PiMonoHarness{Client: srv.Client()}
	req := Request{Spec: v1.HarnessSpec{Kind: v1.HarnessPiMono, PiMono: &v1.HarnessPiMonoSpec{URL: srv.URL}}, Input: json.RawMessage(`{"prompt":"x"}`)}
	if _, err := h.Run(ctx, req); err == nil {
		t.Fatal("cancelled context must abort the run")
	}
}
