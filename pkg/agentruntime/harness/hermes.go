package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
//     across Runs. Ephemeral runs (the default) instead send a fresh unique id
//     per run — omitting the header is NOT stateless (see Run for why).
//   - extra request fields — any env named BODY_<field> is merged into the
//     request body, JSON-typed when parseable (e.g. BODY_temperature=0.7,
//     BODY_top_p=0.9), reaching the gateway as OpenAI request fields / Hermes
//     extra_body. Knobs the stock gateway treats as server-side config (e.g.
//     reasoning_effort, tool/skill enablement) live in the gateway's own
//     config.yaml and may be ignored when sent as request fields.
//   - multimodal input — an "images" array in the Run input is forwarded as
//     OpenAI image_url content parts: a self-contained data: URI (from a b64
//     entry or a data: url), or an http(s) url ONLY when http.imagePolicy
//     permits it. http(s) URLs are screened because the GATEWAY (not the
//     sandboxed agent) fetches them — an SSRF surface; default-denied, internal
//     targets always blocked (see imagesFromInput + screenImages).
//   - seed — the Run's seed is forwarded as the OpenAI `seed` field when set;
//     determinism is best-effort (a hint the backend may ignore).
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
	// The Responses API (M3.10) is a distinct request/response shape; handle it
	// in its own path so the chat-completions path below stays byte-identical.
	if spec.API == "responses" {
		return h.runResponses(ctx, req, spec)
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
	// Screen any image references against the agent's policy BEFORE building the
	// request: an http(s) image URL is fetched by the gateway (an SSRF surface),
	// so it is denied unless explicitly opted in. A violation fails the run.
	images, err := screenImages(imagesFromInput(req.Input), spec.ImagePolicy)
	if err != nil {
		return Response{}, err
	}
	messages := make([]map[string]any, 0, 2)
	if strings.TrimSpace(req.Instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.Instructions})
	}
	messages = append(messages, map[string]any{
		"role":    "user",
		"content": userContent(promptFromInput(req.Input), images),
	})

	body := map[string]any{
		"model":    hermesModel(env),
		"messages": messages,
		"stream":   false,
	}
	if req.Budget.MaxTokens > 0 {
		body["max_tokens"] = req.Budget.MaxTokens
	}
	// Forward the run's seed when set so a backend that honors it can be
	// deterministic — a hint, not a guarantee (many gateways ignore it). A
	// BODY_seed env (below) overrides this.
	if req.Seed != 0 {
		body["seed"] = req.Seed
	}
	// BODY_<field> env entries become extra request fields (e.g. temperature,
	// top_p). Applied last so they can override the defaults.
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

	// Resolve the request headers ONCE, before the retry loop. In particular the
	// ephemeral session id is minted once here (not per attempt): a fresh id per
	// retry would defeat session continuity and, for a retry the gateway DID
	// process, fork the conversation.
	reqHeaders := map[string]string{"Content-Type": "application/json"}
	for k, v := range spec.Headers {
		reqHeaders[k] = v
	}
	// HEADER_<name> env -> request headers (auth: HEADER_Authorization).
	for k, v := range env {
		if name, ok := strings.CutPrefix(k, "HEADER_"); ok && v != "" {
			reqHeaders[name] = v
		}
	}
	// Hermes's /v1/chat/completions is NOT stateless. When no X-Hermes-Session-Id
	// header is sent, the gateway derives one from sha256(system prompt + first
	// user message) and reuses that session across requests — so repeated runs of
	// the same prompt pile into one ever-growing conversation until it overflows
	// the model's context window and the gateway returns empty output (cleared
	// only by restarting the gateway). We therefore always set the header
	// explicitly, keyed by SessionPolicy, instead of relying on that default.
	switch req.Spec.SessionPolicy {
	case v1.SessionPersistent:
		// Carry Hermes memory/skills across runs via a caller-stable id.
		if sid := env["HERMES_SESSION_ID"]; sid != "" {
			reqHeaders["X-Hermes-Session-Id"] = sid
		}
		if skey := env["HERMES_SESSION_KEY"]; skey != "" {
			reqHeaders["X-Hermes-Session-Key"] = skey
		}
	default:
		// Ephemeral (the default): a unique id per run gives each run its own
		// fresh, empty session — making "ephemeral" actually ephemeral.
		reqHeaders["X-Hermes-Session-Id"] = newEphemeralSessionID()
	}

	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.URL, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		for k, v := range reqHeaders {
			r.Header.Set(k, v)
		}
		return r, nil
	}

	res, err := doWithRetry(ctx, client, newReq, spec.Retry)
	if err != nil {
		return Response{DurationMs: res.DurationMs}, err
	}

	respField := spec.ResponseField
	if respField == "" {
		respField = "choices.0.message.content"
	}
	in, out, costMilli := parseUsage(res.Body)
	return Response{
		Output:       []byte(extractField(res.Body, respField)),
		TokensIn:     in,
		TokensOut:    out,
		CostUSDMilli: costMilli,
		DurationMs:   res.DurationMs,
	}, nil
}

// userContent builds the OpenAI "content" for the user message. With no images
// it returns the plain prompt string (the common path — content stays a string,
// byte-identical to before). With images it returns OpenAI multimodal content
// parts: one text part plus an image_url part per image, the standard shape the
// Hermes gateway accepts.
func userContent(text string, images []string) any {
	if len(images) == 0 {
		return text
	}
	parts := make([]map[string]any, 0, 1+len(images))
	parts = append(parts, map[string]any{"type": "text", "text": text})
	for _, url := range images {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": url},
		})
	}
	return parts
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

// newEphemeralSessionID returns a per-run-unique Hermes session id. Sending a
// fresh id makes the gateway start a new, empty session instead of falling back
// to its hash-of-(system+first-user-message) session reuse, which otherwise
// accumulates an unbounded conversation across runs (see Run). Each AgentRun
// executes in its own `agent run` process that calls Run exactly once, so a
// value unique per process is unique per run.
func newEphemeralSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand effectively never fails; if it somehow does, fall back to
		// a still-unique value rather than reintroduce a shared session.
		return fmt.Sprintf("api-eph-%d", time.Now().UnixNano())
	}
	return "api-eph-" + hex.EncodeToString(b[:])
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

// runResponses drives the OpenAI Responses API (/v1/responses, M3.10):
// instructions → "instructions", the prompt → "input", and the output[] array
// is parsed for the final text + the gateway's internal tool calls. Usage is
// read via the dual-shape parseUsage (M2.7), which handles the Responses
// input/output_tokens. Isolated from the chat path so that stays byte-identical.
func (h *HermesHarness) runResponses(ctx context.Context, req Request, spec *v1.HarnessHTTPSpec) (Response, error) {
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

	reqHeaders := map[string]string{"Content-Type": "application/json"}
	for k, v := range spec.Headers {
		reqHeaders[k] = v
	}
	for k, v := range env {
		if name, ok := strings.CutPrefix(k, "HEADER_"); ok && v != "" {
			reqHeaders[name] = v
		}
	}
	switch req.Spec.SessionPolicy {
	case v1.SessionPersistent:
		if sid := env["HERMES_SESSION_ID"]; sid != "" {
			reqHeaders["X-Hermes-Session-Id"] = sid
		}
	default:
		reqHeaders["X-Hermes-Session-Id"] = newEphemeralSessionID()
	}

	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.URL, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		for k, v := range reqHeaders {
			r.Header.Set(k, v)
		}
		return r, nil
	}
	res, err := doWithRetry(ctx, client, newReq, spec.Retry)
	if err != nil {
		return Response{DurationMs: res.DurationMs}, err
	}
	output, toolCalls := parseResponsesOutput(res.Body)
	in, out, costMilli := parseUsage(res.Body)
	return Response{
		Output:       output,
		ToolCalls:    toolCalls,
		TokensIn:     in,
		TokensOut:    out,
		CostUSDMilli: costMilli,
		DurationMs:   res.DurationMs,
	}, nil
}

// parseResponsesOutput walks the /v1/responses output[] array: it concatenates
// message output_text into the answer and pairs function_call /
// function_call_output (by call_id) into ToolCallRecords (the gateway's INTERNAL
// tool log — audit, not schema-validated StepToolCalls). A malformed body
// degrades to empty output.
func parseResponsesOutput(body []byte) ([]byte, []v1.ToolCallRecord) {
	var r struct {
		Output []struct {
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Output    json.RawMessage `json:"output"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, nil
	}
	var sb strings.Builder
	calls := map[string]*v1.ToolCallRecord{}
	var order []string
	for _, o := range r.Output {
		switch o.Type {
		case "message":
			for _, c := range o.Content {
				if c.Type == "output_text" {
					sb.WriteString(c.Text)
				}
			}
		case "function_call":
			calls[o.CallID] = &v1.ToolCallRecord{Tool: o.Name, Arguments: o.Arguments}
			order = append(order, o.CallID)
		case "function_call_output":
			if tc, ok := calls[o.CallID]; ok {
				tc.Result = o.Output
			}
		}
	}
	recs := make([]v1.ToolCallRecord, 0, len(order))
	for _, id := range order {
		recs = append(recs, *calls[id])
	}
	return []byte(sb.String()), recs
}

// parseUsage extracts token usage from a response body, accepting BOTH the chat
// shape (usage.prompt_tokens/completion_tokens) and the Responses shape
// (usage.input_tokens/output_tokens). The two never cross-zero: a non-zero
// value in either field wins, so a body carrying only one shape — or a 0 in one
// and a real count in the other — still reports the real counts (the top
// correctness hazard: a mis-parse that silently zeroes the token budget). Also
// returns best-effort cost in integer milli-USD from total_cost_usd when the
// gateway reports it. Missing/garbled usage yields (0, 0, 0).
func parseUsage(body []byte) (in, out, costMilli int64) {
	var r struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
		} `json:"usage"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	_ = json.Unmarshal(body, &r)
	in = firstNonZeroI64(r.Usage.PromptTokens, r.Usage.InputTokens)
	out = firstNonZeroI64(r.Usage.CompletionTokens, r.Usage.OutputTokens)
	if r.TotalCostUSD > 0 {
		costMilli = int64(r.TotalCostUSD*1000 + 0.5)
	}
	return in, out, costMilli
}

func firstNonZeroI64(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}
