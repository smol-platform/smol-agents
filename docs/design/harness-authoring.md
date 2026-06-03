# Authoring a Harness Kind + the Response Richness Contract

> Status as of 2026-06-02 (v0.2.0 source). Scope: how to add a new `HarnessKind` to the smol-agents runtime, and the exact behavioral contract a harness must honor — what `Response` fields are populated, how sessions and multimodal input are handled, and which validation duties keep authors honest. Every claim is grounded against the tree; callouts flag dead fields and false friends so future kinds stay consistent and callers stop assuming fields that nobody fills.
>
> Companion reads: the harness layer is the `Mode=harness` execution path described in [agent-model.md](../features/agent-model.md); the v0.2.0 scorecard's harness row is in [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md); the broader wiring roadmap (Steps, tool calls, files) is [framework-enhancements.md](framework-enhancements.md).

---

## 1. What a harness is

A **harness** is a single bounded call the executor treats as an opaque oracle: it takes a `Request` (spec + instructions + input + resolved env + budget) and returns a `Response` (final output + best-effort accounting). The interface is deliberately tiny (`pkg/agentruntime/harness/iface.go:64-74`):

```go
type Harness interface {
	Kind() v1.HarnessKind
	Run(ctx context.Context, req Request) (Response, error)
}
```

`Run` is called **exactly once** per `AgentRun` — the plan-act-observe loop, tool execution, and any multi-step agency live *inside* the harness (a CLI subprocess or a remote gateway), invisible to us. `ctx` cancellation MUST terminate the run (kill the subprocess, abort the HTTP request); the executor enforces hard timeouts independently via the same `ctx`.

There are **8 registered kinds**, all wired in `Default()` (`iface.go:83-94`):

| Kind | Transport | Driver file | Implementation |
|---|---|---|---|
| `claude-code` | CLI subprocess | `cli.go` | `ClaudeCodeHarness` → `claude --print <prompt>` |
| `codex` | CLI subprocess | `cli.go` | `CodexHarness` → `codex exec <prompt>` |
| `aider` | CLI subprocess | `cli.go` | `AiderHarness` → `aider --message <prompt> --no-pretty --yes` |
| `goose` | CLI subprocess | `cli.go` | `GooseHarness` → `goose run --instructions <prompt>` |
| `generic-cli` | CLI subprocess | `cli.go` | `GenericCLIHarness` → arbitrary `spec.command` |
| `generic-http` | HTTP | `http.go` | `GenericHTTPHarness` → POST to any URL, configurable fields |
| `hermes` | HTTP | `hermes.go` | `HermesHarness` → OpenAI-compatible `/v1/chat/completions` |
| `pi` | HTTP | `http.go` | `PiHarness` → Inflection AI's hosted Pi (**false friend — see §7**) |

---

## 2. Anatomy of adding a harness kind

Adding a kind is small but spans three packages (pure model → runtime → operator) plus an image and a test. The type system keeps callers honest: an unknown kind is rejected at admission, never silently no-op'd.

1. **Add the kind constant + `Valid()` arm.** In `pkg/agentmodel/v1/harness.go`, add a `HarnessKind` const (`harness.go:38-49`) and a case in `HarnessKind.Valid()` (`harness.go:53-60`). `Valid()` is the admission gate — a typo'd kind fails fast rather than falling through to a no-op.

2. **Implement `Kind()` + `Run()`** in `pkg/agentruntime/harness`. For a subprocess kind, build your `args` and delegate to the shared `runCLI` driver (`cli.go:27-79`) — it handles `spec.command` override, working dir, env merge, bounded stdout/stderr capture, budget timeout, and `ctx` cancellation, so a new CLI is usually ~6 lines (see `GooseHarness`, `cli.go:194-204`). For an HTTP kind, delegate to `doHTTP` (`http.go:77-125`) for a plain prompt-field/response-field call, or hand-roll the body like `HermesHarness.Run` (`hermes.go:61-190`) when you need messages, usage parsing, or session headers; reuse `doWithRetry` (`retry.go:41-95`) so transient failures (network, 429, 5xx) retry with capped backoff inside the budget.

3. **Register it in `Default()`** (`iface.go:83-94`). Without this line `Registry.For` returns `harness: no implementation for kind %q` (`iface.go:105-111`) at runtime.

4. **Add `ValidateHarness` rules** (`harness.go:289-324`). Put your kind in the right `switch` arm: HTTP kinds require `spec.http.url`; CLI kinds get the optional-CLI-block arm (only sanity-checking `MaxOutputBytes`). `generic-cli` additionally requires `spec.command` — but note that check lives in the *harness* (`cli.go:215-217`), not `ValidateHarness` (see §6).

5. **For a CLI kind, publish a bundle image** at `deploy/docker/harness-<kind>.Dockerfile` and wire it into `HarnessImage()`. The four known coding CLIs each ship a bundle (CLI + git + shell + `/agent` driver) so an Agent needs no custom `harness.image`. The Dockerfile is a two-stage build — compile `/agent` from `golang:1.26`, then install the CLI on a runtime base, set `ENV HOME=/tmp`, run as uid `65532`, `ENTRYPOINT ["/agent"]` (`deploy/docker/harness-claude-code.Dockerfile:11-32`; `harness-codex.Dockerfile` is identical bar the npm package). Then add a `perKindHarnessImage` entry (`operator/internal/builders/harness_image.go:20-25`). HTTP kinds skip this — they reuse the base agent image (which makes its calls over HTTP).

6. **Add a unit test.** CLI harnesses inject a fake via the `Cmd commandFunc` seam (`cli.go:17-22`); HTTP harnesses inject a fake transport via the `HTTPClient` interface (`http.go:15-18`). Assert the exact argv/flag (or request body + headers) and the `Response` field population per the contract in §4.

> The model-layer comment already promises this: *"Adding a new one is a matter of registering an implementation in `pkg/agentruntime/harness` — the type system keeps callers honest."* (`harness.go:33-35`). Steps 1–4 are that promise; 5–6 are the image + test that make it land.

---

## 3. Transport decision table

The single biggest authoring decision is **CLI vs HTTP**, because it determines what the harness can ever report. A subprocess is an opaque text oracle; an HTTP backend can return structured JSON the harness parses.

| Property | CLI (subprocess) | HTTP |
|---|---|---|
| Shared driver | `runCLI` (`cli.go:27`) | `doHTTP` (`http.go:77`) / hand-rolled |
| Output capture | bounded stdout (`MaxOutputBytes`, default 1 MiB; `cli.go:49-52`) | dotted-path field extract (`extractField`, `http.go:129`) |
| `TokensIn/TokensOut` | **always 0** — `runCLI` never parses tokens | **0 unless the harness parses a usage block** (only Hermes does) |
| `ToolCalls` | empty (no structured log) | empty today (no harness fills it) |
| Multimodal `images` | **silently dropped** | unpacked (only HTTP kinds call `imagesFromInput`) |
| Secrets / auth | env vars via broker (`mergeEnv`, `cli.go:108`) | `HEADER_<name>` env → request headers |
| Retry on transient failure | no (one `c.Run()`) | yes via `doWithRetry` (`retry.go`) |
| Stderr diagnostics | captured (8 KiB cap) into the error (`cli.go:58,72-76`) | error carries a 512-byte body snippet (`retry.go:175`) |
| Kinds | `claude-code`, `codex`, `aider`, `goose`, `generic-cli` | `generic-http`, `hermes`, `pi` |

**Rule of thumb:** if the backend can return token usage or a structured tool-call log and you want the executor to see it, it must be HTTP — a CLI kind structurally cannot surface those (§4). If the tool is a coding CLI whose whole value is editing files in the workspace, CLI is correct and the accounting loss is accepted.

---

## 4. The Response richness contract (authoritative)

This is the behavioral contract every harness MUST satisfy and every caller MUST assume. It is the source of truth — do not infer richer behavior from the struct fields existing. (`Response` is defined at `iface.go:44-62`.)

> **RESPONSE RICHNESS CONTRACT**
>
> - **`Output` is ALWAYS set.** Every harness returns the final answer as raw bytes (even on a failing CLI, `runCLI` returns the captured partial output alongside the error — `cli.go:67,74,76`).
> - **`TokensIn` / `TokensOut` are best-effort and `0` for all CLI kinds.** `runCLI` never parses tokens, so `claude-code`/`codex`/`aider`/`goose`/`generic-cli` always report `0`. **Only Hermes** parses an OpenAI-style `usage` block back into these fields (`parseUsage`, `hermes.go:265-276`; `pi`/`generic-http` do not).
> - **`ToolCalls` is populated by NO harness today.** It is a forward-compat field (`iface.go:56-58`) — subprocess harnesses leave it empty and the HTTP harnesses never fill it. Treat a non-empty `ToolCalls` as "future kind", never assume it is present.
> - **`DurationMs` is computed by the executor's clock** for every kind (measured around the call in `runCLI`/`doHTTP`).
>
> **Therefore: callers needing token or tool-call accounting MUST use Hermes (tokens) or `Mode=loop` (the native plan-act-observe executor). A CLI harness's budget/observability are wall-clock only.**

This contract is *why* the v0.2.0 scorecard flags the harness layer's central limitation: "CLI harnesses are opaque (no tokens, no tool calls), so budget/observability only work for Hermes/HTTP" ([agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md), §2). When you author a new kind, decide deliberately which fields it can honestly populate — and leave the rest at their zero values rather than fabricating them.

---

## 5. Sessions: `SessionPolicy` → behavior

`HarnessSpec.SessionPolicy` is `ephemeral` (default) or `persistent` (`harness.go:62-68`). It controls whether a *single-run* harness reuses state across runs. It does **not** mean a long-lived multi-turn worker — that is the separate `AgentSession` CRD. **The two "session" concepts are distinct; do not conflate them.** See [durable-session-architecture.md](durable-session-architecture.md) for the full CRD-vs-`SessionPolicy` distinction (and [agent-session-scaling.md](agent-session-scaling.md) for the worker scaling model).

`SessionPolicy` maps to behavior per transport:

| Transport | `ephemeral` (default) | `persistent` |
|---|---|---|
| CLI | fresh process; CWD is `/tmp` unless durable storage binds it | reuse the AgentFS-mounted workspace (`EffectiveWorkingDir`, `harness.go:272-285`) so file state survives across runs |
| HTTP (Hermes) | mint a **fresh** `X-Hermes-Session-Id` per run | forward a **stable** provider session id (`HERMES_SESSION_ID` → `X-Hermes-Session-Id`, plus optional `HERMES_SESSION_KEY`) so the gateway's memory/skills carry across runs |

**The Hermes session detail is the single subtlest correctness point in the layer** (`hermes.go:141-161`). Hermes's `/v1/chat/completions` is **not stateless**: with no `X-Hermes-Session-Id`, the gateway derives one from `sha256(system prompt + first user message)` and reuses it across requests — so repeated runs of the same prompt pile into one ever-growing conversation until it overflows the context window and returns empty output. The harness therefore **always sets the header explicitly**: ephemeral mints a fresh random id per run (`newEphemeralSessionID`, `hermes.go:244-252`), persistent forwards the caller-stable id. A new HTTP kind with server-side session state should follow the same pattern — never rely on the backend's implicit session fallback.

> **Authoring rule:** an ephemeral HTTP harness against a stateful backend must actively isolate each run (a fresh id/conversation), not merely omit the session header. Omitting it is *not* statelessness.

---

## 6. Multimodal input + SSRF screening

Only **HTTP kinds** unpack the optional `images` array from the Run input — CLI kinds silently drop it (they never call `imagesFromInput`). An entry is either `{"url":"…"}` or `{"b64":"…","mime":"…"}`; b64 is assembled into a self-contained `data:` URI (`imagesFromInput`, `images.go:22-50`).

The security model hinges on **who fetches an `http(s)` image URL**: the **gateway (a separate Service)**, not the sandboxed agent pod. That makes an `http(s)` URL an SSRF/exfil surface AgentNet cannot see. So `ImagePolicy` (`harness.go:189-199`) **default-denies** `http(s)` URLs (`screenImages` / `screenImageURL`, `images.go:63-102`):

- `data:` URIs are **always** allowed (self-contained, no fetch).
- An `http(s)` URL is allowed only when `imagePolicy.allowURLs=true`, **never** to a private/loopback/link-local/metadata target (always blocked, e.g. `169.254.169.254`; `isInternalHost`, `images.go:109-119`), and — when `allowedURLHosts` is set — only to a listed host.
- A disallowed image **fails the run loudly** rather than being dropped silently (dropping would quietly change the request the caller asked for).

This is harness-side **best-effort**: the gateway is the real fetcher, so the harness can't stop DNS rebinding (a public host resolving to an internal IP). **The default (data: only) is the actual protection.** A new multimodal HTTP kind MUST reuse `screenImages` with the spec's `ImagePolicy` before building the request — do not hand-roll URL handling. (How egress controls relate to image fetch is covered in [agentnetwork-agentpolicy-interaction.md](agentnetwork-agentpolicy-interaction.md).)

---

## 7. Validation duties + env conventions

`ValidateHarness` (`harness.go:289-324`) is the admission gate. The duties that matter most for a new kind:

- **HTTP kinds REQUIRE `spec.http.url`.** `generic-http`, `pi`, and `hermes` all fail admission without it (`harness.go:298-301`).
- **`pi` is a false friend — and the URL requirement is load-bearing.** `PiHarness` defaults to `https://api.inflection.ai/external/api/inference` (Inflection AI's **hosted Pi**) when no URL is set (`http.go:34-36`). It is **NOT** Mario Zechner's `pi-mono` coding CLI. Because `ValidateHarness` requires `http.url` for `kind=pi`, an author who actually wants Inflection's Pi must set it explicitly rather than silently hitting the hosted default; anyone wanting the `pi-mono` *CLI* must use `generic-cli` with a custom image. Do not "fix" pi by removing the URL requirement — that would re-arm the silent-default footgun.
- **`generic-cli` REQUIRES `spec.command`.** This check lives in the harness (`cli.go:215-217`), returning `harness: generic-cli requires spec.command` — **not** in `ValidateHarness`. That is an inconsistency worth knowing: a `generic-cli` with no command passes admission and fails at run time. A cleanup would move the check into `ValidateHarness`.
- **Env is name-required and value/secretRef are mutually exclusive** (`harness.go:315-322`). Secrets MUST come through the broker via `secretRef` — never inline a secret as a literal `value` (`harness.go:92-95`). See [secrets-broker-credential-backends.md](secrets-broker-credential-backends.md) for how the broker leases land in `Request.Env`.

**Env conventions by transport:**

- **HTTP (`hermes`, `generic-http`):** `HEADER_<name>` env entries become request headers — auth is `HEADER_Authorization="Bearer …"` (`http.go:98-103`, `hermes.go:136-140`). Hermes additionally reads `BODY_<field>` env into OpenAI request fields (e.g. `BODY_temperature=0.7`, JSON-typed when parseable; `hermes.go:113-117`) and `HERMES_MODEL` / `HERMES_SESSION_ID` / `HERMES_SESSION_KEY`.
- **CLI (`claude-code`, `codex`, …):** provider keys flow as ordinary env (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, …) injected by the broker into `Request.Env`. `mergeEnv` inherits `os.Environ()` (the image's `HOME`/`PATH` — without which `claude` crashes on `uv_os_homedir`) then overlays `Request.Env` + `spec.Env` literals, **last wins** (`cli.go:108-119`).

> **DEAD FIELD — `HarnessCLISpec.PassthroughEnv`.** The type exists (`harness.go:143-146`) but **nothing reads it**: `mergeEnv` (`cli.go:108`) already inherits the full `os.Environ()`, so per-var passthrough is moot. It should be **removed** (or repurposed to *restrict* inherited env, the opposite of its name). Do not wire a new kind to depend on it. (Also flagged dead in [framework-enhancements.md](framework-enhancements.md), §1.)

---

## 8. Per-kind permission / flag passthrough (DESIGN — not implemented)

> **Status: DESIGN. None of this section is in the v0.2.0 tree.** It is the proposed answer to the scorecard gap "no per-harness permission flags" (harness row, [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md)).

The CLI harnesses hard-code their argv (`cli.go:147-231`): `claude --print`, `codex exec`, `aider … --yes`, `goose run`. Coding CLIs gate destructive actions behind interactive approval prompts that a non-interactive `agent run` can't answer, and each CLI has a different "trust me" flag — `claude --dangerously-skip-permissions`, `codex --ask-for-approval never`, etc. Today the only escape hatch is overriding the whole entrypoint via `spec.command` (`cli.go:31-36`), which discards the kind's curated defaults.

**Proposal.** Add a structured, per-kind flag seam to `HarnessCLISpec` rather than forcing a full `command` override:

```go
type HarnessCLISpec struct {
	// ... existing fields ...

	// ExtraFlags are appended verbatim to the kind's default argv (after the
	// prompt flag), letting a tenant pass CLI-specific options without
	// discarding the curated defaults via a full command override.
	// +optional
	ExtraFlags []string `json:"extraFlags,omitempty"`

	// ApprovalMode maps a portable intent to each CLI's approval flag, so the
	// spec doesn't hard-code one tool's flag spelling. e.g. "never" →
	// claude `--dangerously-skip-permissions`, codex `--ask-for-approval never`.
	// Default "" leaves the CLI's own default (interactive/safe) in place.
	// +optional
	ApprovalMode string `json:"approvalMode,omitempty"`
}
```

`runCLI` would append `ExtraFlags` after the prompt args, and each CLI harness would translate `ApprovalMode` to its own flag in `Run`. `ValidateHarness` would constrain `ApprovalMode` to a small enum (`""`/`safe`/`never`).

**Security trade-off (state it loudly).** `approvalMode=never` removes the CLI's last interactive guardrail — the harness can then edit/delete files and run arbitrary commands unattended. This is *acceptable only because* the run pod is already a hardened sandbox: kata-fc microVM RuntimeClass (fail-closed) + default-deny egress NetworkPolicy + non-root/drop-ALL/seccomp (verified in [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md), §1, citing `run_sandbox.go`/`sandbox.go`). The flag must be **opt-in per Agent**, never a default, and the default (`""`) must preserve each CLI's own safe behavior. Pairing it with this section's cleanup, **`PassthroughEnv` should be removed in the same change** (it is dead and its name is misleading; §7).

---

## 9. Authoring checklist

- [ ] **Kind constant + `Valid()` arm** added in `pkg/agentmodel/v1/harness.go` (`harness.go:38-60`).
- [ ] **`Kind()` + `Run()`** implemented in `pkg/agentruntime/harness`; `ctx` cancellation kills the subprocess / aborts the request.
- [ ] **Registered in `Default()`** (`iface.go:83-94`).
- [ ] **`ValidateHarness` rule** in the correct `switch` arm (`harness.go:297-314`); HTTP ⇒ require `http.url`.
- [ ] **Transport chosen deliberately** — CLI cannot report tokens/tool-calls (§3/§4); use HTTP if the backend can.
- [ ] **`Response` honest to the contract** — `Output` always set; only populate `TokensIn/Out`/`ToolCalls` if you actually parse them; leave the rest at zero (§4).
- [ ] **Sessions** — ephemeral actively isolates each run (fresh id/conversation), persistent forwards a stable provider session id (§5); don't rely on a backend's implicit session fallback.
- [ ] **Multimodal (HTTP only)** — reuse `screenImages` with `spec.ImagePolicy`; never hand-roll URL gating; default-deny `http(s)` (§6).
- [ ] **Secrets via broker `secretRef`**, never inline literals; env conventions honored (`HEADER_`/`BODY_` for HTTP; `ANTHROPIC_*`/`OPENAI_*` for CLI) (§7).
- [ ] **CLI kind: bundle image** at `deploy/docker/harness-<kind>.Dockerfile` + `perKindHarnessImage` entry (`harness_image.go:20-25`); `HarnessSpec.Version` pins the tag.
- [ ] **Unit test** with the `Cmd`/`HTTPClient` seam asserting argv (or body + headers) and contract-correct `Response` field population.
