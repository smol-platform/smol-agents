// Package invokers holds the production ToolInvoker implementations the
// loop-mode executor drives, one per Tool.Spec.Kind. They are wired into the
// executor's Invokers map by cmd/agent (the executor depends only on the
// abstract ToolInvoker interface, so this package imports the runtime, never
// the reverse).
package invokers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// SecretLeaser leases a named secret from the broker (e.g. a tool's bearer
// token). Satisfied by pkg/secrets.Client / the executor's leaser — declared
// locally so this package stays decoupled from the runtime.
type SecretLeaser interface {
	LeaseSecret(ctx context.Context, name string, ttl time.Duration) ([]byte, error)
}

// maxToolResponseBytes caps how much of a tool response we read into the
// observation, so a hostile/buggy tool can't blow up the run's memory or the
// status record.
const maxToolResponseBytes = 256 << 10

// HTTPInvoker drives a kind=http tool: POST the args JSON to the tool's URL,
// apply headers + an optional broker-leased bearer token, and return the raw
// JSON body as the Observation. It deliberately does NOT validate the body
// against the tool's output schema — that is the executor's job (so a schema
// rejection is recorded as ObservationRejected, distinct from a transport
// error).
type HTTPInvoker struct {
	Client *http.Client
	Leaser SecretLeaser // required only when a tool sets Auth
}

func (h *HTTPInvoker) Invoke(ctx context.Context, tool v1.Tool, args json.RawMessage) (rt.Observation, error) {
	spec := tool.Spec.HTTP
	if spec == nil || spec.URL == "" {
		return rt.Observation{}, fmt.Errorf("http tool %q: missing spec.http.url", tool.Name)
	}
	method := spec.Method
	if method == "" {
		method = http.MethodPost
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, spec.URL, bytes.NewReader(args))
	if err != nil {
		return rt.Observation{}, fmt.Errorf("http tool %q: %w", tool.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	// Broker-leased bearer token, unless the tenant already supplied an
	// Authorization header.
	if spec.Auth != nil && spec.Auth.SecretName != "" && req.Header.Get("Authorization") == "" {
		if h.Leaser == nil {
			return rt.Observation{}, fmt.Errorf("http tool %q: auth set but no secret leaser configured", tool.Name)
		}
		tok, err := h.Leaser.LeaseSecret(ctx, spec.Auth.SecretName, 0)
		if err != nil {
			return rt.Observation{}, fmt.Errorf("http tool %q: lease auth: %w", tool.Name, err)
		}
		req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(tok)))
	}

	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return rt.Observation{}, fmt.Errorf("http tool %q: %w", tool.Name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxToolResponseBytes+1))
	if err != nil {
		return rt.Observation{}, fmt.Errorf("http tool %q: read body: %w", tool.Name, err)
	}
	if int64(len(body)) > maxToolResponseBytes {
		return rt.Observation{}, fmt.Errorf("http tool %q: response exceeds %d bytes", tool.Name, maxToolResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return rt.Observation{}, fmt.Errorf("http tool %q: status %d", tool.Name, resp.StatusCode)
	}
	if !json.Valid(body) {
		return rt.Observation{}, fmt.Errorf("http tool %q: response is not JSON", tool.Name)
	}
	return rt.Observation{Output: body, DurationMs: time.Since(start).Milliseconds()}, nil
}
