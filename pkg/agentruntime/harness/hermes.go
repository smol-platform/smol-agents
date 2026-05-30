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
	in, out := parseUsage(res.Body)
	return Response{
		Output:     []byte(extractField(res.Body, respField)),
		TokensIn:   in,
		TokensOut:  out,
		DurationMs: res.DurationMs,
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
