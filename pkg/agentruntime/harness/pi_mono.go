package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// defaultPiBridgeURL is the in-pod pi-bridge endpoint (cmd/pi-bridge, M4.16).
const defaultPiBridgeURL = "http://127.0.0.1:8848/run"

// PiMonoHarness drives the pi coding agent over HTTP via the in-pod pi-bridge
// (M4.15). It POSTs {prompt,system,model,seed} and parses the bridge's
// {output,tokensIn,tokensOut,toolCalls} — the first CLI-family harness to report
// honest tokens + tool-calls (the bridge parses them from pi's --mode json).
// doWithRetry rides out bridge startup; ctx cancellation aborts the request.
type PiMonoHarness struct {
	Client HTTPClient
}

func (h *PiMonoHarness) Kind() v1.HarnessKind { return v1.HarnessPiMono }

type piBridgeRequest struct {
	Prompt string `json:"prompt"`
	System string `json:"system,omitempty"`
	Model  string `json:"model,omitempty"`
	Seed   int64  `json:"seed,omitempty"`
}

type piBridgeToolEvent struct {
	Name string `json:"name"`
}

type piBridgeResponse struct {
	Output    string              `json:"output"`
	TokensIn  int64               `json:"tokensIn"`
	TokensOut int64               `json:"tokensOut"`
	ToolCalls []piBridgeToolEvent `json:"toolCalls"`
}

func (h *PiMonoHarness) Run(ctx context.Context, req Request) (Response, error) {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	url := defaultPiBridgeURL
	model := ""
	if pm := req.Spec.PiMono; pm != nil {
		if pm.URL != "" {
			url = pm.URL
		}
		model = pm.Model
	}

	body, err := json.Marshal(piBridgeRequest{
		Prompt: promptFromInput(req.Input),
		System: req.Instructions,
		Model:  model,
		Seed:   req.Seed,
	})
	if err != nil {
		return Response{}, fmt.Errorf("harness: pi-mono marshal: %w", err)
	}

	ctx, cancel := budgetTimeout(ctx, req.Budget)
	defer cancel()

	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		return r, nil
	}
	// Bridge-startup retry: the first turn may race the bridge's /healthz.
	retry := &v1.RetrySpec{MaxAttempts: 5, BackoffBaseMs: 200, MaxBackoffMs: 2000}
	res, err := doWithRetry(ctx, client, newReq, retry)
	if err != nil {
		return Response{DurationMs: res.DurationMs}, err
	}

	var br piBridgeResponse
	if err := json.Unmarshal(res.Body, &br); err != nil {
		// Degrade to raw output if the bridge response isn't the expected shape.
		return Response{Output: res.Body, DurationMs: res.DurationMs}, nil
	}
	resp := Response{
		Output:     []byte(br.Output),
		TokensIn:   br.TokensIn,
		TokensOut:  br.TokensOut,
		DurationMs: res.DurationMs,
	}
	for _, tc := range br.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, v1.ToolCallRecord{Tool: tc.Name})
	}
	return resp, nil
}
