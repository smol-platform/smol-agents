package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// HermesHarness drives NousResearch's Hermes Agent through its
// OpenAI-compatible gateway (`hermes gateway run`, POST /v1/chat/completions).
// Beyond a plain chat call it wires in the Hermes-specific features the gateway
// exposes:
//
//   - cross-run memory — when SessionPolicy=persistent and a HERMES_SESSION_ID
//     is present in the resolved env, it sends the X-Hermes-Session-Id (and, if
//     set, X-Hermes-Session-Key) headers so the agent's memory and skills carry
//     across Runs. Ephemeral sessions omit them (fresh context per Run).
//   - extra request fields — any env named BODY_<field> is merged into the
//     request body, JSON-typed when parseable (e.g. BODY_temperature=0.7,
//     BODY_reasoning_effort=high). Covers OpenAI knobs + Hermes extra_body.
//   - model selection — HERMES_MODEL env overrides the gateway default
//     ("hermes-agent").
//   - real token accounting — the OpenAI usage block is parsed back into
//     TokensIn/TokensOut so Budget enforcement + metrics see true counts
//     (most CLI harnesses can only report 0).
//
// Auth reuses the HEADER_<name> convention shared with the other HTTP
// harnesses: set HEADER_Authorization="Bearer <API_SERVER_KEY>" (via the
// broker) to authenticate to the gateway.
type HermesHarness struct {
	Client HTTPClient
}

func (h *HermesHarness) Kind() v1.HarnessKind { return v1.HarnessHermes }

// defaultHermesModel is what the Hermes gateway answers to when no specific
// model is requested; the gateway maps it to its configured provider/model.
const defaultHermesModel = "hermes-agent"

func (h *HermesHarness) Run(ctx context.Context, req Request) (Response, error) {
	spec := req.Spec.HTTP
	if spec == nil || strings.TrimSpace(spec.URL) == "" {
		return Response{}, errors.New("harness: hermes requires spec.http.url (the gateway /v1/chat/completions endpoint)")
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}

	// Effective env = spec literals (HarnessEnvVar.Value) overlaid by the
	// broker-resolved Request.Env (secrets win). The executor passes literal
	// env via Spec.Env and is expected to fill Request.Env with broker-leased
	// secretRef values; reading both lets a field be a literal in the CR or a
	// secretRef resolved at runtime (e.g. HEADER_Authorization as a leased
	// bearer token).
	env := effectiveEnv(req)

	// Build the OpenAI chat-completions body: instructions -> system message,
	// the Run input -> user message.
	messages := make([]map[string]string, 0, 2)
	if strings.TrimSpace(req.Instructions) != "" {
		messages = append(messages, map[string]string{"role": "system", "content": req.Instructions})
	}
	messages = append(messages, map[string]string{"role": "user", "content": promptFromInput(req.Input)})

	body := map[string]any{
		"model":    hermesModel(env),
		"messages": messages,
		"stream":   false,
	}
	if req.Budget.MaxTokens > 0 {
		body["max_tokens"] = req.Budget.MaxTokens
	}
	// BODY_<field> env entries become extra request fields (e.g. temperature,
	// top_p, reasoning_effort). Applied last so they can override the defaults.
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.URL, bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("harness: request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range spec.Headers {
		httpReq.Header.Set(k, v)
	}
	// HEADER_<name> env -> request headers (auth: HEADER_Authorization).
	for k, v := range env {
		if name, ok := strings.CutPrefix(k, "HEADER_"); ok && v != "" {
			httpReq.Header.Set(name, v)
		}
	}
	// Persistent sessions carry Hermes memory/skills across Runs via the
	// gateway's session headers; ephemeral sessions stay stateless.
	if req.Spec.SessionPolicy == v1.SessionPersistent {
		if sid := env["HERMES_SESSION_ID"]; sid != "" {
			httpReq.Header.Set("X-Hermes-Session-Id", sid)
		}
		if skey := env["HERMES_SESSION_KEY"]; skey != "" {
			httpReq.Header.Set("X-Hermes-Session-Key", skey)
		}
	}

	startedAt := time.Now()
	resp, err := client.Do(httpReq)
	dur := time.Since(startedAt).Milliseconds()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Response{DurationMs: dur}, ErrTimeout
		}
		return Response{DurationMs: dur}, fmt.Errorf("harness: http: %w", err)
	}
	defer resp.Body.Close()

	rb, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return Response{DurationMs: dur}, fmt.Errorf("harness: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return Response{DurationMs: dur}, fmt.Errorf("harness: http %d: %s", resp.StatusCode, string(rb))
	}

	respField := spec.ResponseField
	if respField == "" {
		respField = "choices.0.message.content"
	}
	in, out := parseUsage(rb)
	return Response{
		Output:     []byte(extractField(rb, respField)),
		TokensIn:   in,
		TokensOut:  out,
		DurationMs: dur,
	}, nil
}

// effectiveEnv merges the harness spec's literal env (HarnessEnvVar.Value) with
// the executor-resolved Request.Env, the latter winning. secretRef entries have
// an empty Value in the spec and only appear once the executor leases them into
// Request.Env, so a secret cleanly overrides any same-named literal.
func effectiveEnv(req Request) map[string]string {
	env := make(map[string]string, len(req.Spec.Env)+len(req.Env))
	for _, e := range req.Spec.Env {
		if e.Value != "" {
			env[e.Name] = e.Value
		}
	}
	for k, v := range req.Env {
		env[k] = v
	}
	return env
}

// hermesModel returns the model id to request: the HERMES_MODEL env override
// or the gateway's default agent model.
func hermesModel(env map[string]string) string {
	if m := env["HERMES_MODEL"]; m != "" {
		return m
	}
	return defaultHermesModel
}

// jsonOrString parses v as a JSON scalar/object/array, falling back to the raw
// string. Lets BODY_temperature=0.7 land as a number and BODY_model=foo as a
// string without the caller having to declare types.
func jsonOrString(v string) any {
	var out any
	if err := json.Unmarshal([]byte(v), &out); err == nil {
		return out
	}
	return v
}

// parseUsage extracts OpenAI usage.{prompt,completion}_tokens from a response
// body. Missing/garbled usage yields (0, 0).
func parseUsage(body []byte) (in, out int64) {
	var r struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Usage.PromptTokens, r.Usage.CompletionTokens
}
