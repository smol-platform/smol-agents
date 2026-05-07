// Package fakellm dials the cmd/fake-llm HTTP server. It implements
// agentruntime.LLM so the executor can run unmodified against a
// scripted, deterministic backend.
//
// Used by the L0 e2e ring (and any test that wants a real network
// hop instead of the in-process FakeLLM).
package fakellm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	rt "github.com/stigen/knative-agents/pkg/agentmodel/runtime"
	"github.com/stigen/knative-agents/pkg/agentruntime"
)

// Client is an LLM that posts ChatRequest to a cmd/fake-llm server.
// The server replies with an LLMDecision keyed by SHA-256 of the
// request body; missing keys fall back to a canned "I'm done" answer.
type Client struct {
	BaseURL string        // e.g. http://fake-llm:8080
	HTTP    *http.Client  // optional; defaults to a 10s-timeout client
	Timeout time.Duration // optional; defaults to 5s per Chat
}

// New returns a Client with sensible defaults.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		Timeout: 5 * time.Second,
	}
}

// Chat satisfies agentruntime.LLM by POSTing the request to /v1/chat.
func (c *Client) Chat(ctx context.Context, req agentruntime.ChatRequest) (rt.LLMDecision, error) {
	if c.BaseURL == "" {
		return rt.LLMDecision{}, fmt.Errorf("fakellm: BaseURL is required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return rt.LLMDecision{}, fmt.Errorf("fakellm: marshal: %w", err)
	}

	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.BaseURL+"/v1/chat", bytes.NewReader(body))
	if err != nil {
		return rt.LLMDecision{}, fmt.Errorf("fakellm: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return rt.LLMDecision{}, fmt.Errorf("fakellm: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return rt.LLMDecision{}, fmt.Errorf("fakellm: server status %d: %s", resp.StatusCode, raw)
	}
	var d rt.LLMDecision
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return rt.LLMDecision{}, fmt.Errorf("fakellm: decode: %w", err)
	}
	return d, nil
}

// Compile-time check.
var _ agentruntime.LLM = (*Client)(nil)
