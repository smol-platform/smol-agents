// Package eventsink emits platform results as CloudEvents to a configured sink
// (wbb): a finished AgentRun / team coordinator / workflow POSTs a CloudEvent so
// agent outputs can drive downstream Knative Triggers (composable event
// pipelines). It is the reverse of the agentgateway's event INTAKE — the platform
// as an event SOURCE. The CloudEvent id is stable (the object UID) so a redeliver
// or a controller re-emit is idempotent for consumers (dedupe on ce-id).
package eventsink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// SpecVersion is the CloudEvents spec version this emitter speaks (binary mode).
const SpecVersion = "1.0"

// Event is a CloudEvent to emit. Data is the JSON event payload (the agent's
// result); the rest are CloudEvents context attributes.
type Event struct {
	// ID is the CloudEvent id — use a STABLE value (the object UID) so a re-emit
	// is idempotent for consumers.
	ID string
	// Type is the CloudEvent type, e.g. "com.smol-agents.run.completed".
	Type string
	// Source identifies the emitter, e.g. "/namespaces/<ns>/agentruns/<name>".
	Source string
	// Subject is an optional finer-grained subject within the source.
	Subject string
	// Data is the JSON payload (the result). Nil/empty sends an empty body.
	Data json.RawMessage
}

// Emit POSTs ev to sinkURL as an HTTP binary-mode CloudEvent (ce-* headers, the
// body = data). It is best-effort and bounded by ctx: the caller (a controller)
// must not block on a slow sink, and treats a returned error as "retry next
// reconcile" (at-least-once; consumers dedupe on ce-id). A nil client uses
// http.DefaultClient. A non-2xx response is an error.
func Emit(ctx context.Context, client *http.Client, sinkURL string, ev Event) error {
	if sinkURL == "" {
		return fmt.Errorf("eventsink: empty sink URL")
	}
	if ev.ID == "" {
		return fmt.Errorf("eventsink: event id is required (stable id => idempotent)")
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sinkURL, bytes.NewReader(ev.Data))
	if err != nil {
		return fmt.Errorf("eventsink: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Ce-Id", ev.ID)
	req.Header.Set("Ce-Specversion", SpecVersion)
	if ev.Type != "" {
		req.Header.Set("Ce-Type", ev.Type)
	}
	if ev.Source != "" {
		req.Header.Set("Ce-Source", ev.Source)
	}
	if ev.Subject != "" {
		req.Header.Set("Ce-Subject", ev.Subject)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("eventsink: POST %s: %w", sinkURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("eventsink: sink %s returned %d", sinkURL, resp.StatusCode)
	}
	return nil
}
