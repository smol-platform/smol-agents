package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// runAsync drives the Hermes async /v1/runs API (M3.11): submit a run, poll until
// terminal, and on ANY ctx cancellation (run cancelled / pod SIGTERM / budget
// wall-clock) fire POST /v1/runs/{id}/stop on a FRESH short ctx so the gateway
// does not keep executing an orphaned job after the pod exits. The terminal run
// object is responses-shaped (output[] + usage), so it reuses the responses
// parsers. status:failed → error.
//
// NOTE: M3.11's "post-hoc tool-call cap" is intentionally NOT implemented — the
// project invariant is that usage.toolCalls is observability-only and never gates
// a run (gating it here would violate that). The gateway's own loop bounds itself.
func (h *HermesHarness) runAsync(ctx context.Context, req Request, spec *v1.HarnessHTTPSpec) (Response, error) {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	env := effectiveEnv(req)

	body := map[string]any{
		"model": hermesModel(env),
		"input": promptFromInput(req.Input),
	}
	if strings.TrimSpace(req.Instructions) != "" {
		body["instructions"] = req.Instructions
	}
	if req.Budget.MaxTokens > 0 {
		body["max_output_tokens"] = req.Budget.MaxTokens
	}
	if req.Seed != 0 {
		body["seed"] = req.Seed
	}
	if spec.Stream {
		body["stream"] = true
	}
	for k, v := range env {
		if field, ok := strings.CutPrefix(k, "BODY_"); ok && field != "" {
			body[field] = jsonOrString(v)
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("harness: marshal: %w", err)
	}

	ctx, cancel := budgetTimeout(ctx, req.Budget)
	defer cancel()

	headers := asyncHeaders(req, spec, env)
	start := time.Now()

	submit, err := h.asyncDo(ctx, client, http.MethodPost, spec.URL, raw, headers)
	if err != nil {
		return Response{DurationMs: time.Since(start).Milliseconds()}, err
	}
	id, _ := submit["id"].(string)
	if id == "" {
		return Response{DurationMs: time.Since(start).Milliseconds()}, fmt.Errorf("harness: hermes /v1/runs submit returned no run id")
	}
	pollURL := strings.TrimRight(spec.URL, "/") + "/" + id

	pollEvery := time.Duration(spec.PollIntervalMs) * time.Millisecond
	if pollEvery <= 0 {
		pollEvery = time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			h.stopAsync(client, pollURL, headers) // orphan fix: stop on a fresh ctx
			return Response{DurationMs: time.Since(start).Milliseconds()}, ctx.Err()
		case <-timer.C:
		}
		obj, err := h.asyncDo(ctx, client, http.MethodGet, pollURL, nil, headers)
		if err != nil {
			if ctx.Err() != nil {
				h.stopAsync(client, pollURL, headers)
				return Response{DurationMs: time.Since(start).Milliseconds()}, ctx.Err()
			}
			timer.Reset(pollEvery) // transient poll error → keep polling
			continue
		}
		switch status, _ := obj["status"].(string); status {
		case "completed", "succeeded", "success":
			objRaw, _ := json.Marshal(obj)
			output, toolCalls := parseResponsesOutput(objRaw)
			in, out, costMilli := parseUsage(objRaw)
			return Response{
				Output: output, ToolCalls: toolCalls,
				TokensIn: in, TokensOut: out, CostUSDMilli: costMilli,
				DurationMs: time.Since(start).Milliseconds(),
			}, nil
		case "failed", "cancelled", "canceled", "error", "expired":
			msg, _ := obj["error"].(string)
			return Response{DurationMs: time.Since(start).Milliseconds()},
				fmt.Errorf("harness: hermes run %s ended %s: %s", id, status, msg)
		}
		timer.Reset(pollEvery)
	}
}

// asyncHeaders builds the auth + session headers for the runs API (mirrors the
// responses path).
func asyncHeaders(req Request, spec *v1.HarnessHTTPSpec, env map[string]string) map[string]string {
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range spec.Headers {
		h[k] = v
	}
	for k, v := range env {
		if name, ok := strings.CutPrefix(k, "HEADER_"); ok && v != "" {
			h[name] = v
		}
	}
	if req.Spec.SessionPolicy == v1.SessionPersistent {
		if sid := env["HERMES_SESSION_ID"]; sid != "" {
			h["X-Hermes-Session-Id"] = sid
		}
	} else {
		h["X-Hermes-Session-Id"] = newEphemeralSessionID()
	}
	return h
}

// asyncDo performs one request and decodes the JSON object body.
func (h *HermesHarness) asyncDo(ctx context.Context, client HTTPClient, method, url string, body []byte, headers map[string]string) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	r, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, err := client.Do(r)
	if err != nil {
		return nil, fmt.Errorf("harness: hermes /v1/runs %s: %w", method, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("harness: hermes /v1/runs %s: http %d: %s", method, resp.StatusCode, errSnippet(b))
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("harness: hermes /v1/runs decode: %w", err)
	}
	return obj, nil
}

// stopAsync best-effort cancels a gateway-side run on a FRESH short ctx (the run
// ctx is already done when this is called).
func (h *HermesHarness) stopAsync(client HTTPClient, pollURL string, headers map[string]string) {
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(sctx, http.MethodPost, pollURL+"/stop", nil)
	if err != nil {
		return
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	if resp, err := client.Do(r); err == nil {
		_ = resp.Body.Close()
	}
}
