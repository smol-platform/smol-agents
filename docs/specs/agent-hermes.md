# Spec: Full support for NousResearch Hermes Agent

> **Status: DESIGN / SPEC — 2026-06-03 (v0.2.0 source).** Implementation-grade plan for taking the `hermes` harness from "chat-only, single-shot, fire-and-forget" to **full Hermes Agent gateway support**: structured tool-call visibility via `/v1/responses`, SSE streaming, async `/v1/runs` with stop-on-cancel, AgentSession-driven stable session ids, and a corrected admission rule for gateway-side memory. Every code claim is cited `file:line` against the tree; every external-API claim is cited to the Hermes docs. Proposals are marked **PROPOSED**; nothing here is implemented yet unless explicitly stated.
>
> **Extends, does not duplicate:** [harness-authoring.md](../design/harness-authoring.md) (the `HarnessKind` authoring contract + Response richness contract) and [framework-enhancements.md](../design/framework-enhancements.md) §2A (H1/H2/H4/H5), §2C/2D (A2/A4). This spec is the concrete, ready-to-implement version of those sketches for the Hermes-specific surface. Read those first for the rationale; this file is the build sheet.
>
> **Companion specs (this run):** [response-richness](response-richness.md) (the cross-harness `Steps`/`ToolCalls` wire — a hard dependency of §4.1), [agentsession-scaling-impl](agentsession-scaling-impl.md) (the `AgentSession` reconciler this spec drives in §4.4), [determinism-and-replay](determinism-and-replay.md) (`Seed` semantics), [human-in-the-loop](human-in-the-loop.md) (the `RequiresAction` gate async runs could feed). Background: [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md) §2 (harness scorecard), [agent-model.md](../features/agent-model.md) (the `Mode=harness` path).

---

## 1. Summary

The `hermes` harness (`pkg/agentruntime/harness/hermes.go`) drives [NousResearch's Hermes Agent](https://hermes-agent.nousresearch.com/docs/) — a full self-hosted agent (memory + skills + ~64 built-in tools) — through its OpenAI-compatible gateway. Today it makes exactly one synchronous `POST /v1/chat/completions`, parses the OpenAI `usage` block into real token counts, manages `X-Hermes-Session-Id` correctly (the single subtlest correctness point in the layer), screens multimodal image URLs for SSRF, and retries transient failures. That path is **e2e-green** (Hermes + z.ai GLM-4.6 on the cftest Hetzner cluster — see `.claude/hermes-e2e.yaml`).

**"Full support"** means closing the gap between what the Hermes gateway exposes and what a smol-agents `Agent` can use. The gateway has a far richer surface than `/v1/chat/completions`: a Responses API that emits the agent's own `function_call`/`function_call_output` items, SSE streaming, an async Runs API with stop-on-cancel, and stable per-channel memory keying. This spec wires those in, ordered by leverage:

| Increment | What it unlocks | Depends on |
|---|---|---|
| **A. `/v1/responses` adoption** | Hermes's internal tool calls become `Response.ToolCalls` → `Status.steps[]` (today they collapse to one opaque string) | [response-richness](response-richness.md) (the size-budget); the Steps wire itself is **already done** (§2) |
| **B. SSE streaming** | live token/tool progress + faster cancellation on long runs; foundation for AgentSession turn streaming | A (Responses event stream) |
| **C. `/v1/runs` async submit+poll+stop** | non-blocking runs; **fixes orphaned gateway runs** when the smol-agents run pod is killed (today the gateway keeps working after we walk away) | — |
| **D. AgentSession-driven stable session id** | a Hermes conversation that *remembers across runs* without the author hand-setting a shared id (today's sample shares one transcript across every run — the wrong default) | [agentsession-scaling-impl](agentsession-scaling-impl.md) |
| **E. Admission fix: `persistent` ⇏ `storage` for Hermes** | persistent Hermes agents stop being rejected for lacking AgentFS they don't need (memory is gateway-side) | — |
| **F. Operator-managed gateway (decision)** | optional first-class gateway lifecycle vs URL-only | — (see §10) |

The outcome: a Hermes agent that surfaces its tool-call trace to `kubectl`, streams progress, cleans up after itself on cancel, and carries memory across runs of a session — all while keeping the green chat path the default and unchanged.

---

## 2. Current state

### What exists (verified)

`HermesHarness.Run` (`hermes.go:61-190`) is the most fully-wired harness:

- **Chat-completions only.** It POSTs `{model, messages, stream:false, ...}` to the single configured `spec.http.url` (`hermes.go:97-101`). There is no notion of an "API surface" choice — the URL *is* the endpoint and it is hard-assumed to be `/v1/chat/completions` (the default `ResponseField` is `choices.0.message.content`, `hermes.go:179-182`).
- **Real token accounting.** `parseUsage` (`hermes.go:265-276`) reads `usage.prompt_tokens`/`completion_tokens`. This is the **only** harness that populates `TokensIn/TokensOut` (Response richness contract, [harness-authoring.md](../design/harness-authoring.md) §4).
- **Correct session management.** `X-Hermes-Session-Id` is set explicitly per `SessionPolicy` (`hermes.go:148-161`): persistent forwards `HERMES_SESSION_ID`/`HERMES_SESSION_KEY` env; ephemeral mints a fresh random id (`newEphemeralSessionID`, `hermes.go:244-252`) — because the gateway is **not** stateless (it derives a session from `sha256(system+first-user-message)` when the header is absent, accumulating an unbounded transcript).
- **Multimodal + SSRF screen.** `imagesFromInput` + `screenImages` (`hermes.go:84`, `images.go`) unpack an `images` array and default-deny `http(s)` URLs (the gateway, not the sandbox, fetches them — an SSRF surface AgentNet can't see).
- **Retry/backoff.** `doWithRetry` (`retry.go:41-95`) retries network/429/5xx with capped backoff honoring `Retry-After`, inside the budget ctx; `classifyHTTP` (`retry.go:113-126`) yields stable reason tokens (`auth`/`rate_limited`/`overloaded`/`bad_request`). **H4 from framework-enhancements is DONE.**
- **`BODY_`/`HEADER_` env conventions** (`hermes.go:113-117,136-140`), `HERMES_MODEL` override (`hermes.go:231-236`), `seed` forwarding (`hermes.go:108-110`).

### What is already wired downstream (corrects a stale belief)

The framework-enhancements H1 sketch describes "wire surgery (the real work)" to carry `Steps`/`ToolCalls` to the cluster. **That surgery is already landed:**

- `HarnessRunner.RunHarness` returns `harness.Response` **whole** (`harness_runner.go:30-51`) — it does *not* drop `ToolCalls`.
- `runHarness` folds `resp.ToolCalls` into the single `Step` and into `Usage.ToolCalls` (`executor.go:397-408`).
- `RunResult` **has** a `Steps []v1.Step` field; `ResultToWire` copies it (`runonce.go:24-38, ~84`).
- The controller sets `run.Status.Steps = rr.Steps` (`agentrun_controller.go:404`).
- `cmd/agent/run.go` already size-budgets the termination message: `clampForTerminationMessage` (`run.go:102-115`) sheds output → tool-call arg/result bodies (`elideStepPayloads`) → the whole step trace, under a `terminationMessageBudget = 3072` cap (`run.go:94`), full detail in pod logs.

> **So the ONLY thing standing between Hermes and tool-call visibility is the harness populating `Response.ToolCalls`** — which `/v1/chat/completions` structurally cannot do (it returns one assistant string). This is what `/v1/responses` (§4.1) fixes. The cross-harness Steps plumbing is the subject of [response-richness](response-richness.md); this spec is its first real *producer*.

### What is stubbed / missing (the gap "full support" closes)

| Gap | Evidence | Increment |
|---|---|---|
| No `/v1/responses` → `Response.ToolCalls` always empty for Hermes | `hermes.go:184-189` builds Response with no `ToolCalls`; only `choices.0.message.content` is read | A (§4.1) |
| No SSE streaming — always `"stream":false` | `hermes.go:101` | B (§4.2) |
| No async `/v1/runs`; a killed run pod **orphans** the gateway-side run | `hermes.go` is a single blocking `doWithRetry`; ctx-cancel aborts our HTTP read but never tells the gateway to stop | C (§4.3) |
| `AgentSession`-driven session id unwired; `AgentRunSpec.SessionRef` read by nobody | no reconciler ([framework-enhancements.md](../design/framework-enhancements.md) §2A H2, scaffolding table) | D (§4.4) |
| Admission over-strict: `persistent` requires `spec.storage` even for Hermes (memory is gateway-side) | `validation.go:47-51` | E (§4.5) |
| No `/v1/capabilities` probe to make endpoint choice version-safe | (none) | A (gated, §4.1) |
| Sample hard-codes one shared session id at the **agent** level | `operator/config/samples/agent_hermes.yaml:46-47` (`HERMES_SESSION_ID: tenant-a-hermes`) — every run piles into one transcript | D (§4.4) |
| `reasoning_effort` mis-documented as a working `BODY_` field | `hermes.go:31-33` comment | H5 honesty fix (§4.6) |

---

## 3. External interface research — the Hermes Agent gateway API

> Sources, confirmed 2026-06-03 (training cutoff is Jan 2026; this API moves fast):
> [API Server reference](https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server) ·
> [api-server.md (GitHub source)](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/api-server.md) ·
> [Programmatic Integration](https://hermes-agent.nousresearch.com/docs/developer-guide/programmatic-integration) ·
> [Configuration](https://hermes-agent.nousresearch.com/docs/user-guide/configuration) ·
> [hermes-agent repo](https://github.com/NousResearch/hermes-agent).

### 3.1 Endpoint surface (the part that matters for this spec)

| Endpoint | Method | Use here |
|---|---|---|
| `/v1/chat/completions` | POST | **Current** path. OpenAI Chat Completions; `usage.{prompt,completion}_tokens`. SSE when `"stream":true`. |
| `/v1/responses` | POST | **Increment A.** OpenAI Responses API; server-side state; `output[]` array emits `function_call`/`function_call_output`/`message` items; `usage.{input,output}_tokens` (**different field names**). `previous_response_id` chaining; `store` flag. |
| `GET /v1/responses/{id}` · `DELETE /v1/responses/{id}` | GET/DELETE | Retrieve / delete a stored response (chaining management). |
| `/v1/runs` | POST | **Increment C.** Async submit → `{"run_id","status":"started"}`. |
| `GET /v1/runs/{run_id}` | GET | Poll: `status` ∈ `started`/`completed`/`failed`/`cancelled`; includes `output`, `usage`, `session_id`, `model`. |
| `GET /v1/runs/{run_id}/events` | GET (SSE) | **Increment B.** Live tool-call progress, token deltas, lifecycle. |
| `POST /v1/runs/{run_id}/stop` | POST | **Increment C — the orphan fix.** Returns `{"status":"stopping"}`; agent stops at next safe interruption point. |
| `/v1/capabilities` | GET | **Increment A gate.** `{"features":{"responses_api":true,"run_submission":true,"run_events_sse":true,"run_stop":true,...}}` + `{"auth":{"type":"bearer","required":true}}`. |
| `/v1/models` | GET | List models (model is server-side anyway). |
| `/v1/health` · `/health` | GET | `{"status":"ok"}`. Useful for gateway readiness (§10). |

### 3.2 Request/response shapes (verbatim, trimmed)

**`/v1/responses` request:**
```json
{ "model": "hermes-agent", "input": "What files are in my project?",
  "instructions": "You are a helpful coding assistant.", "store": false }
```
Multi-turn chaining: `{ "input": "...", "previous_response_id": "resp_abc123" }`.

**`/v1/responses` response — the structured `output[]` we need:**
```json
{ "id": "resp_abc123", "object": "response", "status": "completed", "model": "hermes-agent",
  "output": [
    { "type": "function_call", "name": "terminal", "arguments": "{\"command\": \"ls\"}", "call_id": "call_1" },
    { "type": "function_call_output", "call_id": "call_1", "output": "README.md src/ tests/" },
    { "type": "message", "role": "assistant", "content": [ { "type": "output_text", "text": "Your project has..." } ] }
  ],
  "usage": { "input_tokens": 50, "output_tokens": 200, "total_tokens": 250 } }
```

**`/v1/runs`:** `POST` → `{ "run_id": "run_abc123", "status": "started" }`; `GET /v1/runs/{id}` →
```json
{ "object": "hermes.run", "run_id": "run_abc123", "status": "completed",
  "session_id": "...", "model": "hermes-agent", "output": "Done.",
  "usage": { "input_tokens": 50, "output_tokens": 200, "total_tokens": 250 } }
```
`POST /v1/runs/{id}/stop` → `{ "status": "stopping" }`.

### 3.3 SSE event names

- **Chat-completions stream:** `event: chat.completion.chunk` (OpenAI token chunks) + Hermes-custom `event: hermes.tool.progress` (tool-start, kept out of persisted text).
- **Responses stream (spec-native OpenAI):** `response.created`, `response.output_text.delta`, `response.output_item.added`, `response.output_item.done`, `response.completed` — with `function_call`/`function_call_output` output items.
- **Runs stream (`GET /v1/runs/{id}/events`):** tool-call progress, token deltas, lifecycle events.

### 3.4 Session headers (confirmed — matches current code)

- **`X-Hermes-Session-Id`** — transcript-scoped; accepted by `/v1/chat/completions`, `/v1/responses`, `/v1/runs`. (Matches `hermes.go:152,160`.)
- **`X-Hermes-Session-Key`** — stable per-channel memory key, ≤256 chars, rejects control chars; accepted by the same three endpoints; echoed back in JSON + SSE. (Matches `hermes.go:154`.)

### 3.5 Auth, usage, and limits

- **Auth:** `Authorization: Bearer <API_SERVER_KEY>` required on all three inference endpoints **and** `/v1/capabilities` ([api-server.md](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/api-server.md)). Matches our `HEADER_Authorization` convention (`hermes.go:136-140`).
- **Usage field-name split (load-bearing):** chat uses `prompt_tokens`/`completion_tokens`; responses + runs use `input_tokens`/`output_tokens`. **A parser must read both shapes** or it silently zeroes the token budget (a safety invariant). See §4.1.
- **Model is server-side.** The `model` field is accepted but the upstream model is configured in the gateway's `config.yaml` ([api-server.md](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/api-server.md)). Our e2e proved this exactly: z.ai needed `config.yaml: {model: {provider: zai, model: glm-4.6}}` (`.claude/hermes-e2e.yaml:31-34`), and `HERMES_MODEL` only helps gateways that honor the request field.
- **Response storage:** stored responses persist in SQLite, survive restarts, **max 100, LRU-evicted** — a constraint on `previous_response_id` chaining depth (§4.4 risk).
- **Tool *declaration* is NOT a request parameter.** The docs document tools only in *responses* (the `output[]` items); there is no request-side `tools`/function-definition array — the gateway owns its toolset server-side. So **the harness consumes structured tool calls; it does not push tool definitions.** This is the opposite of loop mode and must be doc-commented to avoid conflation with [loop-mode-tools-and-invokers](loop-mode-tools-and-invokers.md).

---

## 4. Design

### 4.0 The unifying CRD seam: `HarnessHTTPSpec.API`

All of A/B/C hang off one new discriminator so the green chat path stays the default:

```go
// HarnessHTTPSpec (pkg/agentmodel/v1/harness.go)
// API selects the Hermes gateway surface. Default "chat" = the current
// /v1/chat/completions path (unchanged). "responses" adopts /v1/responses to
// surface the agent's function_call/function_call_output items into
// Response.ToolCalls. "runs" submits async to /v1/runs and polls, enabling
// stop-on-cancel. Ignored for kind!=hermes.
// +kubebuilder:validation:Enum=chat;responses;runs
// +optional
API string `json:"api,omitempty"`
```

`spec.http.url` is reinterpreted as the **gateway base** when `API != ""` (the harness appends the path), but **stays backward-compatible**: if `URL` already ends in a known path (`/v1/chat/completions`, `/v1/responses`, `/v1/runs`), the harness uses it verbatim. This preserves every existing manifest (sample + e2e both set the full `/v1/chat/completions` URL).

```text
HarnessHTTPSpec.API ──┬─ "" | "chat"  → POST {base}/v1/chat/completions   (current; default)
                      ├─ "responses"  → POST {base}/v1/responses          (Increment A)
                      └─ "runs"        → POST {base}/v1/runs → poll → stop (Increment C)
```

### 4.1 Increment A — `/v1/responses` for tool-call structure

```text
AgentRun(input) → agent run → HermesHarness.Run(API=responses)
   → [optional once] GET /v1/capabilities  (gate: features.responses_api?)
   → POST /v1/responses {model, instructions, input, store:false, X-Hermes-Session-Id}
   → parseResponsesOutput(body):
        output[] →  message items      → concatenated into Response.Output
                    function_call      ─┐ paired by call_id
                    function_call_output┘→ v1.ToolCallRecord{Tool,Arguments,Result}
        usage.{input,output}_tokens → Response.TokensIn/Out
   → runHarness folds ToolCalls into Step + Usage.ToolCalls (executor.go:397-408, EXISTING)
   → RunResult.Steps → Status.steps[]  (EXISTING wire, §2)
```

- New `parseResponsesOutput(body []byte) (output []byte, calls []v1.ToolCallRecord)` in `hermes.go`. Walk `output[]`; concatenate every `message.content[].output_text.text` into `Output`; pair each `function_call` with its `function_call_output` by `call_id` into one `ToolCallRecord{Tool:name, Arguments:json.RawMessage(arguments), Result:json.RawMessage(output)}`. An unmatched `function_call` (no output yet) still records the call with empty `Result`.
- **Generalize `parseUsage`** to read both shapes with explicit precedence: prefer non-zero `prompt_tokens`/`completion_tokens`, else `input_tokens`/`output_tokens`. A zero in one shape must never zero the other. **Direct unit test required** for the `input_tokens`/`output_tokens` path (mis-parse silently disables the token budget — a safety invariant).
- **Version-safety gate.** When `API=responses`, do a **single** `GET /v1/capabilities` (cached for the process — irrelevant for single-shot, valuable under AgentSession reuse). If `features.responses_api != true`, **fail loud** (`harness:bad_request: gateway lacks responses_api`), never silently fall back to chat (a silent empty result is worse than an honest failure). Skip the probe entirely for `API=""`/`chat` (the 100%-of-today path) so no wasted round-trip — this is the salvageable, deferred-until-now slice of framework-enhancements H5.

> **`Response.ToolCalls` semantics doc-comment (mandatory):** these are the *gateway's internal* tool calls, NOT smol-agents `v1.Tool`s — they bypass the loop-mode `OutputSchema` check and there is no per-step token attribution (the gateway reports aggregate `usage`). Record them as audit, not as schema-validated `StepToolCall`s. This keeps the `StepKind` taxonomy honest ([harness-authoring.md](../design/harness-authoring.md) §4).

### 4.2 Increment B — SSE streaming

Streaming buys two things: faster cancellation (we see the stream stop) and live progress for AgentSession turn streaming ([agentsession-scaling-impl](agentsession-scaling-impl.md)). It does **not** change the folded `Response` — we still emit one final `Output` + `ToolCalls`.

- New `streamResponses(ctx, client, req) (Response, error)` reading `text/event-stream`: accumulate `response.output_text.delta` into `Output`; build `ToolCalls` from `response.output_item.done` items of type `function_call`/`function_call_output`; finalize on `response.completed`. For chat-API streaming, accumulate `chat.completion.chunk` deltas and ignore `hermes.tool.progress` (UI-only).
- Gated by a new `HarnessHTTPSpec.Stream bool` (default false → current buffered behavior, keeps tests byte-identical). Only meaningful with `API ∈ {chat,responses}`; `runs` has its own event stream (§4.3).
- `doWithRetry` (`retry.go`) is request/response-buffered and cannot retry mid-stream. Streaming therefore **retries only the initial connect** (pre-first-byte); once bytes flow, a mid-stream error is terminal for that attempt. Document this; do not pretend mid-stream resumability.

> **Honest scope:** for the dominant single-shot batch run, streaming is marginal (we wait for the answer either way). Its real payoff is C's responsiveness and AgentSession. Ship B **after** A and **with** C/D, or skip to C if streaming-for-its-own-sake isn't worth the parser.

### 4.3 Increment C — `/v1/runs` async, and the orphan fix

**The problem it fixes is correctness, not just latency.** Today, when the smol-agents run pod is killed (budget timeout, eviction, `kubectl delete`, gateway/agentgateway crash), ctx-cancel aborts our *local* HTTP read — but the Hermes gateway **keeps running the turn**: it has no idea we left. That run consumes the gateway's upstream tokens and a session slot indefinitely. The Runs API gives us an explicit `stop`.

```text
HermesHarness.Run(API=runs):
  POST /v1/runs {input, instructions, session_id, previous_response_id?} → run_id
  loop:
     select {
       <-ctx.Done():  POST /v1/runs/{run_id}/stop   ← THE ORPHAN FIX
                      then return ErrCancelled/ErrTimeout
       <-tick:        GET /v1/runs/{run_id}
                      status==completed → parse output+usage → Response, return
                      status∈{failed,cancelled} → classified error
     }
  (poll interval = backoff-capped; whole loop inside budget ctx)
```

- New `runAsync(ctx, client, base, body, headers) (Response, error)` in a new `hermes_runs.go`. On **any** ctx-done (cancel *or* deadline), it fires `POST /v1/runs/{run_id}/stop` with a short, **separate** timeout (the parent ctx is already dead — use `context.WithTimeout(context.Background(), 3s)` so the stop actually sends), then returns the sentinel. Best-effort: log if stop fails; never block teardown.
- Prefer `GET /v1/runs/{id}/events` (SSE) over polling when `features.run_events_sse` and `Stream=true`; fall back to `GET /v1/runs/{id}` polling otherwise. Both paths must honor the same stop-on-cancel.
- **`maxToolCalls` ConfigMap → `--max-tool-calls`?** No — the gateway runs its own loop; our `Budget.MaxToolCalls` can't bound it mid-flight. We enforce it *after the fact* by counting `len(resp.ToolCalls)` against budget in `runHarness` (extend the existing token-cap check, `executor.go:419-427`). Document that for harness mode tool-call budget is a post-hoc verdict, not a live cap.

### 4.4 Increment D — AgentSession-driven stable session id

**Goal:** a Hermes conversation that remembers across runs *without* the author hand-setting a shared id (and *without* the current footgun where the sample shares ONE id at the agent level so every run interleaves — `agent_hermes.yaml:46-47`).

**Design (controller-side injection — the harness already reads the env):**

```text
AgentRun{spec.sessionRef: "chat-42"}  +  Agent{mode:harness, harness.kind:hermes}
   └─ AgentRun reconciler (BuildRunSpecConfigMap path):
        deep-copy agent.Spec
        set Harness.SessionPolicy = persistent
        append Harness.Env += {HERMES_SESSION_ID: "sess-" + AgentSession.UID}   ← UID, not Name
        (optional) {HERMES_SESSION_KEY: MemoryScope}
        marshal the COPY into agent.json
   └─ HermesHarness reads HERMES_SESSION_ID (EXISTING, hermes.go:151) → X-Hermes-Session-Id
   └─ AgentSession reconciler recomputes Status.Usage by listing child runs (see below)
```

- **Inject into the per-run *copy* of the Agent spec**, not the AgentRun — there is no per-run env or per-run `SessionPolicy` seam (`runHarness` reads both exclusively from `agent.Spec.Harness`, `executor.go:386-394`). This corrects the framework-enhancements H2 "inject into harness env, zero harness change" phrasing, which was wrong about *where* the env lives.
- **Derive the id from the AgentSession's namespaced UID, never its Name** — immutable across recreate, and prevents cross-tenant memory bleed if a name is reused.
- The `AgentSession` reconciler itself (status roll-up, `+kubebuilder:subresource:status`, field-wise `Usage` sum — **not** `Usage.Add`, which is a per-step incrementer at `budget.go`) is specified in [agentsession-scaling-impl](agentsession-scaling-impl.md). This spec only owns the **Hermes session-id injection** that the reconciler enables. `AgentSessionSpec.MemoryScope` (optional override of the derived key, decoupling ephemeral transcript from persistent `X-Hermes-Session-Key` memory) is a Hermes-specific addition proposed here.
- Update `agent_hermes.yaml` to **remove** the agent-level `HERMES_SESSION_ID` and instead show the `AgentRun.spec.sessionRef` → AgentSession pattern.

### 4.5 Increment E — admission fix for gateway-side memory

`ValidateAgent` rejects `SessionPolicy=persistent` unless `spec.storage` is set (`validation.go:47-51`). For CLI kinds that's correct (persistent = "reuse the AgentFS workspace"). **For Hermes, memory lives gateway-side** — AgentFS is irrelevant. The current rule forces every persistent Hermes agent to declare storage it never uses.

**Fix (relax for `HarnessHermes` specifically):**
```go
// validation.go — replace the blanket check:
if a.Spec.Mode == ModeHarness && a.Spec.Harness != nil &&
   a.Spec.Harness.SessionPolicy == SessionPersistent &&
   a.Spec.Harness.Kind != HarnessHermes &&        // ← Hermes memory is gateway-side
   a.Spec.Storage == nil {
   errs = append(errs, errors.New("harness.sessionPolicy=persistent requires spec.storage"))
}
```
Do **not** silently force the field or drop the rule globally — CLI persistence genuinely needs storage. Doc-comment that Hermes persistence is gateway-scoped and that deleting an AgentSession does **not** purge gateway-side memory (a tenant retention note).

### 4.6 Increment H5 (cheap honesty fix, fold into A)

Rewrite the `hermes.go:31-33` comment: `reasoning_effort` is a **server-side `config.yaml` knob the stock gateway may ignore as a request field**, not a working `BODY_` example. Mirror in `agent_hermes.yaml`. No CRD/interface churn. (This is the deferred remnant of framework-enhancements H5, now landing alongside the `/v1/capabilities` probe that earns its keep in A.)

---

## 5. Concrete changes

### 5.1 CRD / Go type additions (`pkg/agentmodel/v1/harness.go` + mirror `operator/api/.../types.go`)

| Field | Type | Default | Validation | Increment |
|---|---|---|---|---|
| `HarnessHTTPSpec.API` | `string` | `""` (=chat) | `+kubebuilder:validation:Enum=chat;responses;runs`; in `ValidateHarness`, accepted only for `kind=hermes` | A/C |
| `HarnessHTTPSpec.Stream` | `bool` | `false` | only meaningful for `kind=hermes`, `API ∈ {chat,responses}` (warn-noop elsewhere) | B |
| `HarnessHTTPSpec.PollIntervalMs` | `int32` | `1000` | clamp `[250, MaxBackoffMs]`; used only when `API=runs` w/o SSE | C |
| `AgentSessionSpec.MemoryScope` | `string` | `""` | ≤256 chars, no control chars (Hermes `X-Hermes-Session-Key` rule) | D |

`ValidateHarness` (`harness.go:308-343`) gains an arm: if `HTTP.API != ""` and `Kind != HarnessHermes` → error `harness.http.api is only valid for kind=hermes`.

**CRD YAML is hand-edited** (CRD-generation drift is a known hazard — see project memory; do **not** blindly `make manifests`). Edit `operator/config/crd/...agents.yaml` (`HarnessHTTPSpec` props) and `...agentsessions.yaml` (`MemoryScope`) directly; `status.steps[].toolCalls[]` is already `preserve-unknown-fields`, so **no schema change** for the tool-call payload.

### 5.2 Harness implementation (`pkg/agentruntime/harness/`)

- `hermes.go`:
  - Split `Run` into a dispatcher on `spec.HTTP.API`: `runChat` (existing body, refactored out), `runResponses` (A), `runAsync` (delegates to `hermes_runs.go`, C).
  - `parseResponsesOutput(body) ([]byte, []v1.ToolCallRecord)` (A).
  - Generalize `parseUsage` to both field-name shapes (A) — `hermes.go:265-276`.
  - `resolveEndpoint(base, api) string` (path append + backward-compat passthrough, §4.0).
  - `probeCapabilities(ctx, client, base, headers) (caps, error)` with per-process cache (A gate).
  - Fix the `reasoning_effort` comment (H5).
- `hermes_stream.go` (new): `streamResponses` / `streamChat` SSE readers (B).
- `hermes_runs.go` (new): `runAsync` submit/poll/stop with the ctx-done→`/stop` orphan fix (C).
- `retry.go`: unchanged for buffered calls; streaming uses a thin connect-only retry wrapper (B) — keep the transport-only scope discipline from framework-enhancements H4.

### 5.3 Executor / wire (mostly already done — verify, don't rebuild)

- `executor.go:397-427` already folds `resp.ToolCalls` into `Step`+`Usage` and caps tokens. **Extend** the post-hoc cap to also reject when `Usage.ToolCalls > Budget.MaxToolCalls` (C, §4.3) — a few lines mirroring the token check.
- `harness_runner.go`, `runonce.go` (`RunResult.Steps`, `ResultToWire`), `agentrun_controller.go:404` (`Status.Steps = rr.Steps`), `cmd/agent/run.go` clamp — **all already present** (§2). No change needed for the wire itself; this spec is the first real *producer* of non-empty `ToolCalls`.

### 5.4 Controller (Increment D)

- `operator/internal/controllers/agentmodel/agentrun_controller.go` (the `BuildRunSpecConfigMap` / `runspec.go` path): when `run.Spec.SessionRef != "" && agent.Spec.Mode == ModeHarness && agent.Spec.Harness.Kind == HarnessHermes`, deep-copy `agent.Spec`, set `Harness.SessionPolicy = SessionPersistent`, append `HERMES_SESSION_ID = "sess-"+sessionUID` (look up the AgentSession to get its UID), and optional `HERMES_SESSION_KEY = MemoryScope`. Marshal the copy.
- The AgentSession reconciler + scheme registration + `subresource:status` live in [agentsession-scaling-impl](agentsession-scaling-impl.md).

### 5.5 Samples

- `operator/config/samples/agent_hermes.yaml`: remove agent-level `HERMES_SESSION_ID`; add `API: responses` example (commented, with the "fail-loud if gateway lacks it" note); show `AgentRun.spec.sessionRef` for cross-run memory.
- (Optional) a `deploy/` example mirroring `.claude/hermes-e2e.yaml` with `API: responses` once verified on cftest.

### 5.6 New files / binaries / invokers

| New | Purpose |
|---|---|
| `pkg/agentruntime/harness/hermes_runs.go` | async Runs API + stop-on-cancel (C) |
| `pkg/agentruntime/harness/hermes_stream.go` | SSE readers (B) |
| `pkg/agentruntime/harness/hermes_responses_test.go` | `output[]` + dual-usage parsing (A) |
| `pkg/agentruntime/harness/hermes_runs_test.go` | poll/stop lifecycle with a fake transport (C) |

No new binary, no new invoker (Hermes consumes tool calls, it doesn't dispatch smol-agents tools). The base `agent` image already carries the harness (HTTP kinds reuse it — [harness-authoring.md](../design/harness-authoring.md) §2 step 5).

---

## 6. Data / control flow (end-to-end, `API=responses`)

```text
kubectl apply AgentRun{agentRef:hermes, sessionRef:chat-42, input:{prompt:"refactor x"}}
   │
AgentRun reconciler (agentrun_controller.go)
   ├─ resolve broker secretRefs (HEADER_Authorization → leased bearer)
   ├─ [D] deep-copy Agent.Spec; SessionPolicy=persistent; HERMES_SESSION_ID=sess-<UID>
   ├─ BuildRunSpecConfigMap → agent.json + run.json
   └─ create run pod (kata-fc RuntimeClass, default-deny egress NetworkPolicy)   ← unchanged sandbox
        │
   agent run (cmd/agent/run.go → RunOnce → RunTurn → Executor.runHarness)
        ├─ resolveHarnessEnv → broker lease (SO_PEERCRED/SPIFFE)
        ├─ HermesHarness.Run(API=responses):
        │    ├─ [gate] GET /v1/capabilities (Bearer) → features.responses_api? else FAIL LOUD
        │    ├─ X-Hermes-Session-Id = sess-<UID>   (persistent, from env)
        │    ├─ POST {base}/v1/responses {model,instructions,input,store:false}
        │    │      (retry network/429/5xx, capped backoff, inside budget ctx)
        │    └─ parseResponsesOutput → Output + []ToolCallRecord; parseUsage(dual) → tokens
        ├─ runHarness folds → Step{Final, ToolCalls}, Usage{Tokens,ToolCalls:N}
        ├─ post-hoc budget caps (tokens, [C] tool-calls)
        └─ RunResult{Phase, Output, Steps, Usage} → clampForTerminationMessage → /dev/termination-log
                                                  └─ full detail → stdout (pod logs)
   foldRunResult (agentrun_controller.go:398-410): Status.{Phase,Output,Usage,Steps,TerminationReason}
   AgentSession reconciler: recompute Status.Usage from child runs   ← [agentsession-scaling-impl]
```

Cancellation path (`API=runs`, Increment C): a run-pod kill → ctx cancel → `runAsync` fires `POST /v1/runs/{id}/stop` on a fresh 3s ctx → gateway stops the turn → no orphan.

---

## 7. Security model

How each increment composes with the existing hardened datapath (kata-fc microVM RuntimeClass fail-closed + default-deny egress NetworkPolicy + non-root/drop-ALL/seccomp on the run pod — verified in [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md) §1):

| Surface | Composition / new risk | Mitigation |
|---|---|---|
| **Gateway egress** | The Hermes gateway is a **separate Service**, not the sandboxed pod. Every endpoint the harness hits (`/v1/responses`, `/v1/runs`, capabilities) is the gateway fetching/executing on our behalf — including **terminal commands** ([api-server.md](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/api-server.md): "full access to hermes-agent's toolset, including terminal commands"). AgentNet cannot see inside the gateway. | The run pod's default-deny egress must allow **only** the gateway Service. **The gateway itself should run in its own hardened namespace with its own egress policy** (it is the real blast radius). Treat the gateway as a privileged dependency, not part of the sandbox. |
| **`Authorization` bearer** | `API_SERVER_KEY` grants full agent control. Same `HEADER_Authorization` broker path as today (`hermes.go:136-140`) — never inlined (`ValidateHarness` forbids `value`+`secretRef`, `harness.go:338-340`). | Lease via broker (SO_PEERCRED `LocalPeerAttestor` fallback / SPIFFE). Rotate `API_SERVER_KEY`; one key per tenant gateway. |
| **`/v1/capabilities` probe** | An unauth or spoofed gateway could lie ("responses_api:true") to coax a different request shape. | Probe is **post-auth** (Bearer required). It only *gates*, never *escalates* — worst case is a failed/empty `/v1/responses` we already fail-loud on. |
| **Image SSRF (existing)** | Unchanged: the gateway fetches `http(s)` image URLs; `data:`-only default + `screenImages` block internal targets (`images.go`; [harness-authoring.md](../design/harness-authoring.md) §6). `/v1/responses` multimodal uses `input_image` parts — reuse the **same** `screenImages`. | Keep default-deny; never hand-roll URL gating for the responses shape. |
| **`previous_response_id` / session-key memory (D)** | A leaked/guessable session id or `MemoryScope` lets one run read another's gateway-side memory; the gateway also stores responses in SQLite (max 100, LRU). | Derive id from **namespaced UID** (unguessable, tenant-scoped). `MemoryScope` validated ≤256 chars, no control chars. Document that AgentSession delete ≠ gateway memory purge. |
| **Async run leakage (C)** | A `run_id` is a handle to a live, tool-executing run; if the pod dies without stopping, it orphans. | The stop-on-cancel IS the mitigation. Use a **fresh** ctx for `/stop` so a dead parent ctx doesn't swallow it. |
| **SSE (B)** | A malicious/huge stream could exhaust memory. | Reuse `maxResponseBytes` (16 MiB, `retry.go:18`) as a stream byte cap; finalize/abort past it. |

No increment weakens the sandbox; the net-new surface is entirely **gateway-side**, which is why §10's "should the operator manage the gateway?" decision matters for security posture.

---

## 8. Phasing & effort

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **E** — admission relax (`persistent` ⇏ storage for Hermes) | `validation.go` one-clause + test | **S** | — |
| **H5** — `reasoning_effort` comment honesty | `hermes.go` + sample | **XS** | — (fold into A) |
| **A** — `/v1/responses` + `ToolCalls` + dual-usage + caps gate | `harness.go` (API field), `hermes.go`, new test; wire already done (§2) | **L** | [response-richness](response-richness.md) (size-budget — already landed; confirm) |
| **C** — `/v1/runs` async + stop-on-cancel | `hermes_runs.go`, executor tool-call cap | **M–L** | A (shared dispatcher + capabilities gate) |
| **D** — AgentSession session-id injection | `agentrun_controller.go` copy-mutate, `MemoryScope` field, sample rewrite | **M** | [agentsession-scaling-impl](agentsession-scaling-impl.md) (the reconciler) |
| **B** — SSE streaming | `hermes_stream.go`, `Stream` field | **M** | A (responses event stream); pairs with C |
| **F** — operator-managed gateway | (decision-gated, see §10) | **XL** | a maintainer decision |

**Recommended sequence:** E + H5 (trivial, unblock authoring) → **A** (the keystone: unlocks tool-call visibility, the headline gap) → **C** (the correctness fix for orphans) → **D** (multi-turn memory) → **B** (streaming, only if AgentSession turn-streaming or long-run responsiveness justifies the parser). **F** is out-of-band.

Cross-spec dependencies: A is the first real producer for [response-richness](response-richness.md)'s wire; D consumes [agentsession-scaling-impl](agentsession-scaling-impl.md); C's async model is the substrate [human-in-the-loop](human-in-the-loop.md)'s `RequiresAction` gate could later use (a paused run is an un-polled `run_id`).

---

## 9. Test plan

**Unit (fake `HTTPClient` transport — `http.go:15-18` seam):**
- A: `parseResponsesOutput` over the §3.2 `output[]` fixture → asserts concatenated `Output` + one `ToolCallRecord{Tool:"terminal",Arguments,Result}` paired by `call_id`; unmatched `function_call` records empty `Result`.
- A: **dual-usage parse** — `{usage:{input_tokens,output_tokens}}` populates tokens; a chat fixture still parses `prompt/completion_tokens`; a zero in one shape doesn't zero the other. (Safety-critical — explicit table test.)
- A: capabilities gate — `responses_api:false` → fail-loud error, no request to `/v1/responses`; `chat`/`""` API → **no** probe issued (assert zero capability calls).
- A: backward-compat — `URL` ending in `/v1/chat/completions` with `API=""` builds the exact current request (assert body byte-identical to `TestHermesHarness` so the green path is provably unchanged).
- C: `runAsync` happy path (submit→poll started→completed→parse); ctx-cancel mid-poll fires `POST /stop` on a **non-dead** ctx (assert the stop request was sent); `status:failed` → classified error.
- C: executor tool-call budget — `len(ToolCalls) > MaxToolCalls` → `PhaseExpired`, `budget:toolcalls` (mirror the token-cap test).
- B: SSE reader assembles deltas + `output_item.done` tool items; over-cap stream aborts; connect-only retry (no mid-stream retry).
- D: controller copy-mutate — `sessionRef` set → agent.json copy has `SessionPolicy=persistent` + `HERMES_SESSION_ID=sess-<UID>`; original Agent spec untouched (deep-copy proven); no `sessionRef` → no mutation.
- E: `ValidateAgent` — Hermes+persistent+no-storage **passes**; claude-code+persistent+no-storage still **fails**.

**E2E (the cftest single-node k0s box exists for live verification — `.claude/hermes-e2e.yaml` is the green baseline):**
- Re-run the existing Hermes+z.ai green path with `API=""` → confirm no regression (the gate of "don't break green").
- `API=responses` against the live gateway (z.ai GLM-4.6 backend) → `kubectl get agentrun -o yaml` shows non-empty `status.steps[0].toolCalls[]` (the headline deliverable made visible). **Note:** `/v1/responses` is externally gated and unverifiable from the laptop — cftest is the only place to prove it; if the deployed `nousresearch/hermes-agent` tag lacks it, the capabilities gate must fail loud (verify that too).
- C: kill a long-running AgentRun pod mid-flight; assert the gateway's run goes `cancelled` (poll `GET /v1/runs/{id}` from a debug pod) — proves the orphan fix.
- D: two AgentRuns sharing one `sessionRef`; assert the second sees memory from the first (gateway-side); a third with a different `sessionRef` does not.

---

## 10. Risks & open decisions

**Open decisions for the maintainer:**

1. **Should the operator ever MANAGE a Hermes gateway Deployment, or stay URL-only?** (Increment F.) Today the gateway is a hand-deployed workload (`.claude/hermes-e2e.yaml`) and the Agent points at its Service URL — clean separation, but the tenant owns gateway lifecycle, security posture, and the `API_SERVER_KEY`. **Options:** (a) **URL-only forever** (status quo; simplest; gateway is "someone else's privileged service"). (b) A `HermesGateway` CRD the operator reconciles (Deployment+Service+NetworkPolicy+config.yaml+key), so the gateway gets the *same* hardened namespace/egress treatment as run pods (§7 wants this) and the Agent references it by name. **Recommendation: defer (a), but write F as a follow-up** the moment a second tenant needs a gateway — the security argument (the gateway is the real blast radius, with terminal access) is strong, but it's a large surface and orthogonal to A/C/D. **Decision needed before F.**
2. **`API` default and migration.** Default MUST stay `chat`. Do we ever auto-upgrade to `responses` when `/v1/capabilities` advertises it? **Recommend: no** — explicit opt-in; auto-upgrade silently changes request shape + token-field parsing for every existing agent. (Stated as a firm default, but flag for confirmation.)
3. **Streaming worth it for batch?** (Increment B.) For single-shot runs the payoff is marginal. **Decide:** build B only if/when AgentSession turn-streaming ([agentsession-scaling-impl](agentsession-scaling-impl.md)) needs it, else skip the SSE parser.

**Honest unknowns / risks:**

- **`/v1/responses` + `/v1/runs` are unverifiable from the laptop** — only cftest can prove them, and the deployed gateway image tag must actually expose them. The capabilities gate (fail-loud) is the safety net; do not ship A claiming responses support without a cftest run.
- **Usage field-name split is a silent-zeroing trap.** A mis-parse disables the token budget (a safety invariant). The dual-usage unit test is non-negotiable.
- **Per-step token attribution is fictional** — the gateway reports aggregate `usage`; do not claim per-tool-call token fidelity in `Status.steps[]`.
- **`previous_response_id` chaining is capped at 100 stored responses (LRU)** — long sessions silently lose deep history gateway-side. Document; consider `conversation` named-chaining (auto-chains to latest) as an alternative if depth matters.
- **Tool budget is post-hoc, not live** (§4.3): we count `ToolCalls` after the gateway's loop finishes; we cannot interrupt the gateway mid-loop except via `/v1/runs/{id}/stop` (which is coarse — it stops the whole turn, not "after N tools").
- **Deleting an AgentSession does not purge gateway-side memory** (D) — a tenant data-retention surprise; must be documented.
- **CRD-generation drift** (project memory): the `API`/`Stream`/`MemoryScope` fields must be **hand-added** to the CRD YAML; `make manifests` is not reproducible here.
- **The `pi` false-friend** ([harness-authoring.md](../design/harness-authoring.md) §7) is adjacent but out of scope — do not "fix" it while in this file.
