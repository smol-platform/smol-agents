# Spec: Full support for Anthropic Claude Code (`harness.kind=claude-code`)

> **✅ Decisions resolved 2026-06-03 — see [decisions.md](../design/decisions.md).** D3: permission flags (`--dangerously-skip-permissions`/`--permission-mode`) are opt-in-only, microVM-gated, never default — `HarnessCLISpec.ExtraFlags` shipped; D6: resumable/resident claude is post-GA, batch `--print` now; D4: `spec.session` field. Where this doc still says OPEN/PROPOSED and conflicts, the decision log wins.

> **Status: DESIGN / SPEC — 2026-06-03 (against v0.2.0 source).** Implementation-grade specification for taking the `claude-code` harness from a one-shot `claude --print <prompt>` text oracle to a first-class, richly-instrumented, resumable, MCP-capable agent on the smol-agents platform. Nothing in the **Design** / **Concrete changes** sections is in the tree yet unless explicitly marked "BUILT"; every "BUILT" claim is cited to `file:line`.
>
> **Extends, does not duplicate:** [harness-authoring.md](../design/harness-authoring.md) (the authoring contract + §8 "per-kind permission / flag passthrough" DESIGN block this spec makes concrete for Claude Code). Read that first — this spec is the Claude-Code-specific deepening of it.
>
> **Companion reads:** [response-richness](response-richness.md) (the cross-kind Response contract this populates for `claude-code`), [loop-mode-tools-and-invokers](loop-mode-tools-and-invokers.md) (why Claude Code's *in-harness* tool loop is orthogonal to loop-mode invokers), [terminal-exposure-http-ssh-tmux](terminal-exposure-http-ssh-tmux.md) (interactive access, §6e), [agentsession-scaling-impl](agentsession-scaling-impl.md) (durable multi-turn worker that §4d's `--resume` plugs into), [dynamic-credential-backends](dynamic-credential-backends.md) (`apiKeyHelper`, §4e), [agentnetwork-datapath-enforcement](agentnetwork-datapath-enforcement.md) (egress for `api.anthropic.com` + MCP servers), [run-governance](run-governance.md) (cost/turn budgets the JSON envelope now feeds). Scorecard context: [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md).

---

## 1. Summary

"Full support for Claude Code" means the platform drives Anthropic's `claude` CLI ([code.claude.com/docs/en/headless](https://code.claude.com/docs/en/headless)) the way a production operator should, not the way a smoke test does. Today the `ClaudeCodeHarness` runs `claude --print "<prompt>"` and captures stdout as opaque text (`pkg/agentruntime/harness/cli.go:147-165`) — tokens are `0`, cost is invisible, tool calls are unrecorded, MCP servers can't be configured, the CLI's interactive permission prompts are unhandled, and every `AgentRun` is a cold start with no conversational memory. This spec specifies the full surface: (a) **structured output** via `--output-format json`/`stream-json` so a run yields real `TokensIn/Out`, `total_cost_usd`, `session_id`, `num_turns`, and a tool-call trace; (b) **MCP server passthrough** via `--mcp-config`; (c) **permission modes** (the §8-of-harness-authoring `ExtraFlags`/`approvalMode` seam, made concrete with Claude's real flags `--permission-mode`, `--allowedTools`, `--dangerously-skip-permissions`); (d) **resumable multi-turn sessions** via `--resume`/`--continue` + the JSON `session_id`, so an `AgentSession` carries *conversation* context, not just a durable workspace; (e) **short-lived credentials** via Claude's `apiKeyHelper` + the broker; (f) **interactive terminal** access (delegated to the terminal-exposure spec). The outcome: a `claude-code` Agent that is observable (budgeted by cost and tokens, not just wall-clock), governable (permission posture is explicit and opt-in), composable (MCP tools), and stateful (true conversational sessions) — all inside the existing kata-fc sandbox + default-deny egress envelope.

---

## 2. Current state

### 2.1 What is BUILT

| Capability | Where | Notes |
|---|---|---|
| `claude --print <prompt>` driver | `pkg/agentruntime/harness/cli.go:147-165` (`ClaudeCodeHarness`) | One `exec` via the shared `runCLI` (`cli.go:27-79`). Prepends `req.Instructions` to the prompt (`cli.go:160-162`). |
| Kind constant + admission | `pkg/agentmodel/v1/harness.go:39-40`, `:64-71` (`Valid()`), `:327-332` (`ValidateHarness` CLI arm) | CLI block optional; only `MaxOutputBytes ≥ 0` checked. |
| Bundle image | `deploy/docker/harness-claude-code.Dockerfile` | `node:22-slim` + `npm i -g @anthropic-ai/claude-code` + `/agent`; `HOME=/tmp`; uid `65532`; `ENTRYPOINT ["/agent"]`. |
| Per-kind image resolution | `operator/internal/builders/harness_image.go:20-25,33-45` | `claude-code` → `harness-claude-code` bundle unless `harness.image` set; `harness.version` pins the tag. |
| Env / secret injection | `cli.go:108-119` (`mergeEnv`) + executor `resolveHarnessEnv` (`pkg/agentruntime/executor.go:349-373`) | Inherits `os.Environ()` (HOME/PATH — `claude` crashes on `uv_os_homedir` without it) then overlays `Request.Env` (broker leases) + `spec.Env` literals, last wins. |
| AgentFS-bound CWD | `pkg/agentmodel/v1/harness.go:291-304` (`EffectiveWorkingDir`) + `cli.go:42-46` | When the Agent has durable AgentFS, the CLI runs in the mount so file edits land on the backed-up volume. |
| Sandbox + egress envelope | operator (`--default-run-runtime-class=kata-fc`, `resolveSandbox`); `operator/internal/builders/run_sandbox.go` | kata-fc microVM (fail-closed) + static default-deny NetworkPolicy (DNS + in-cluster RFC1918 + public 80/443; metadata blocked). Applies to all run pods incl. `claude-code`. |
| Result fold (incl. Steps) | `pkg/agentruntime/runonce.go:84` (`ResultToWire`) → `agentrun_controller.go:404`; executor harness path `executor.go:375-419` | The single `Final` step is folded; `resp.ToolCalls`/`resp.TokensIn/Out` are threaded — they're just always zero/empty for this kind today. |

The plumbing for richness already exists end-to-end (`harness.Response` → `executor.go:397-407` `Usage{Tokens,ToolCalls}` → `Step` → `RunResult` → controller). **The only gap is that `ClaudeCodeHarness` never produces those values**, because it parses nothing.

### 2.2 What is STUBBED / MISSING (the gap this spec closes)

- **No structured output.** `ClaudeCodeHarness.Run` passes only `--print <prompt>` (`cli.go:163-164`); it never sets `--output-format`, so it gets prose and `runCLI` returns `Response{Output: <stdout>}` with `TokensIn/Out=0`, `ToolCalls=nil` (the contract, `iface.go:56-69`; `harness-authoring.md` §4). Cost and `session_id` are thrown away.
- **No MCP config.** `harness.kind=claude-code` has no field to declare MCP servers; `--mcp-config` is never emitted. Claude Code's headless MCP capability is unreachable.
- **No permission posture.** argv is hard-coded; Claude's interactive approval prompts can hang a non-interactive run, and the only escape today is overriding the *entire* entrypoint via `spec.command` (`cli.go:31-36`), which discards the curated defaults. `HarnessCLISpec.ExtraFlags`/`ApprovalMode` are a **DESIGN proposal only** in [harness-authoring.md §8](../design/harness-authoring.md) — not in the tree.
- **No conversational resume.** `SessionPolicy=persistent` for a CLI kind reuses only the **AgentFS workspace** (`harness.go:291-304`; `harness-authoring.md` §5) — *files*, not *conversation*. The JSON `session_id` is discarded, so `--resume`/`--continue` are never used; a durable `AgentSession` gives Claude a warm disk but a cold memory.
- **No short-lived creds.** Creds flow as static env (`ANTHROPIC_API_KEY`, sample `operator/config/samples/agent_claude_code.yaml:27-30`); the broker lease is read once at process start. Claude's `apiKeyHelper` (re-invoked on TTL/401) is unused, so a run longer than the lease TTL can fail mid-flight.
- **No terminal access.** `claude-code` only ever runs head-less one-shot; there is no interactive TTY path. (Owned by [terminal-exposure-http-ssh-tmux](terminal-exposure-http-ssh-tmux.md).)
- **`PassthroughEnv` is DEAD** (`harness.go:160-165`) — no reader; `mergeEnv` already inherits everything. Slated for removal alongside the §8 change (`harness-authoring.md` §7/§8).
- **Termination-message cap.** Even once we fold richer Steps, the kubelet ~4 KiB cap (`cmd/agent/run.go:90-118`, budget `3072`) elides large traces. Tool-call argument/result bodies are the first thing shed (`elideStepPayloads`); a verbose Claude run still loses its trace to the controller (full trace survives in pod logs). Cross-cutting — owned by [response-richness](response-richness.md).

---

## 3. External interface research (Claude Code headless, confirmed 2026-06)

> Confirmed against Anthropic's live docs ([code.claude.com/docs/en/headless](https://code.claude.com/docs/en/headless), [/cli-reference](https://code.claude.com/docs/en/cli-reference), [/permission-modes](https://code.claude.com/docs/en/permission-modes), [/authentication](https://code.claude.com/docs/en/authentication), [/agent-sdk/python](https://code.claude.com/docs/en/agent-sdk/python)) and context7 `/websites/code_claude`. Training cutoff is Jan 2026 and this surface moves fast — these are the **2026-06** shapes. The CLI is now branded the **Claude Agent SDK** (`claude -p` is "the Agent SDK via the CLI").

### 3.1 Invocation & output formats

`claude -p "<prompt>"` (`-p` ≡ `--print`) runs non-interactively; it also **reads stdin** (pipe the prompt or context in, capped at 10 MB as of v2.1.128). `--output-format` selects the shape:

| Value | Shape | Use |
|---|---|---|
| `text` (default) | plain prose on stdout | human; machine-hostile (what we get today) |
| `json` | **single JSON envelope** at end | scripted single-answer + metadata — **what this spec adopts for one-shot runs** |
| `stream-json` | newline-delimited JSON events (with `--verbose`, optionally `--include-partial-messages`) | real-time event stream — for live progress / terminal streaming |

### 3.2 The `--output-format json` result envelope (authoritative)

The final object (type `result`) carries (confirmed via the SDK `ResultMessage` dataclass, context7 `/websites/code_claude`, `/agent-sdk/python`):

| Field | Type | Meaning (mapping target) |
|---|---|---|
| `type` | `"result"` | envelope discriminator |
| `subtype` | string | e.g. `success`, `error_max_turns`, `error_during_execution` → **`TerminationReason`** |
| `is_error` | bool | run-level error → drives `Phase` |
| `result` | string | final assistant text → **`Output`** |
| `session_id` | string (UUID) | conversation id → **resume key** (§4d) |
| `total_cost_usd` | float | client-side cost → **new `Usage.CostUSDMilli`** (§4a) |
| `usage` | object | `input_tokens`, `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens` → **`TokensIn/TokensOut`** (+ cache fields, §4a) |
| `num_turns` | int | turns executed → budget/observability |
| `duration_ms`, `duration_api_ms` | int | timings (we keep our own clock; `duration_api_ms` is informational) |
| `permission_denials` | array | tools Claude wanted but was denied → surfaced as a diagnostic |
| `model_usage` | object | per-model `inputTokens`/`outputTokens`/`cacheReadInputTokens`/`costUSD`/`contextWindow`/… |
| `structured_output` | any | present only with `--json-schema` |

> **Session-id constraint:** session ids **must be UUIDs**; `--session-id "my-string"` fails ([headless docs](https://code.claude.com/docs/en/headless)). Resume keys we mint/forward must be valid UUIDv4.

### 3.3 `stream-json` events

Newline-delimited; event `type`s observed: `system` (subtype `init` — first event, reports model/tools/MCP servers/plugins; subtype `api_retry` — emitted before a retryable-error retry with `attempt`/`max_retries`/`retry_delay_ms`/`error`; subtype `plugin_install`), `assistant` / `user` (message objects), `stream_event` (token deltas with `--include-partial-messages`; `.event.delta.type == "text_delta"`), and the terminal `result` object (same fields as §3.2). Requires `--verbose`.

### 3.4 Sessions: `--resume` / `--continue`

- `--continue` resumes the **most recent** conversation (per working dir) — no id needed.
- `--resume <session_id>` resumes a **specific** conversation by UUID.
- Canonical pattern ([headless docs](https://code.claude.com/docs/en/headless)): `sid=$(claude -p "…" --output-format json | jq -r '.session_id'); claude -p "…" --resume "$sid"`.
- Resume state lives under Claude's home (`~/.claude` / project history) — i.e. **on disk**, which is why pairing resume with **AgentFS-persisted HOME** is the correct durable design (§4d).

### 3.5 MCP, permissions, prompts, model

- **MCP:** `--mcp-config <file-or-json>` loads MCP servers (stdio/SSE/HTTP). Combined with `--allowedTools` to pre-approve `mcp__<server>__<tool>` names.
- **Permission modes** ([/permission-modes](https://code.claude.com/docs/en/permission-modes)): `--permission-mode <mode>`, observed values incl. `default`, `acceptEdits` (auto-approve file writes + common fs cmds `mkdir/touch/mv/cp`), `plan`, `dontAsk` (deny anything not in `permissions.allow` or the read-only set — "locked-down CI"), `bypassPermissions`. `--dangerously-skip-permissions` bypasses **all** prompts (the blunt instrument). `--allowedTools "Bash,Read,Edit"` / `--disallowedTools` allow/deny by [permission rule syntax](https://code.claude.com/docs/en/settings) (e.g. `Bash(git diff *)` with prefix match).
- **System prompt:** `--append-system-prompt` (keep defaults) or `--system-prompt` (replace); file variants `--append-system-prompt-file`. We currently fold instructions into the *user* prompt (`cli.go:160-162`); `--append-system-prompt` is the correct channel (§4a).
- **Model:** `--model <name>`; or `ANTHROPIC_MODEL` env.
- **`--bare`:** skips auto-discovery (hooks/skills/plugins/MCP/CLAUDE.md/auto-memory) for reproducible CI; in bare mode auth must come from `ANTHROPIC_API_KEY` or an `apiKeyHelper` in `--settings`, and only explicit flags take effect. **Recommended for scripted/SDK calls; will become the `-p` default.** Strongly aligned with our sandbox goals (§7).
- **`--add-dir <path>`:** grant Claude access to extra directories.
- **`--settings <file-or-json>`:** inline/file settings (incl. `apiKeyHelper`, `permissions`).

### 3.6 Authentication & `apiKeyHelper`

Credential priority (highest→lowest, [/authentication](https://code.claude.com/docs/en/authentication)): cloud-provider creds (Bedrock/Vertex/Foundry) → `ANTHROPIC_AUTH_TOKEN` → `ANTHROPIC_API_KEY` → **`apiKeyHelper` script stdout** → OAuth subscription. `apiKeyHelper` is a path to an executable whose stdout is the credential; re-invoked after **5 min or on HTTP 401**, tunable via `CLAUDE_CODE_API_KEY_HELPER_TTL_MS`. (A known caching bug exists — anthropics/claude-code#11639 — note it.) `ANTHROPIC_BASE_URL` redirects to a gateway/proxy. This is the hook for short-lived broker leases (§4e). Bare mode skips OAuth/keychain — perfect for our headless pods.

Sources: [Run Claude Code programmatically](https://code.claude.com/docs/en/headless) · [CLI reference](https://code.claude.com/docs/en/cli-reference) · [Permission modes](https://code.claude.com/docs/en/permission-modes) · [Authentication](https://code.claude.com/docs/en/authentication) · [Agent SDK (Python) ResultMessage](https://code.claude.com/docs/en/agent-sdk/python) · context7 `/websites/code_claude`.

---

## 4. Design

The design keeps the existing one-`exec`-per-`AgentRun` shape (the harness contract: `Run` called exactly once, `harness-authoring.md` §1) and the shared `runCLI` driver, but makes `ClaudeCodeHarness` (a) **build a richer argv**, (b) **parse the JSON envelope into `Response`**, and (c) **thread a session id** for resume. New CRD fields are added to `HarnessCLISpec` (kind-agnostic where sensible) plus a small Claude-specific block. Everything composes with the unchanged sandbox/egress/broker/SPIFFE envelope.

```
                          AgentRun (claude-code)
                                  │
           operator/internal/builders/runspec.go  (NEW: render mcp.json, settings.json,
                                  │                 set ANTHROPIC_* env, mount HOME on AgentFS)
                                  ▼
                         run pod (kata-fc, default-deny egress)
                                  │  /agent run
                                  ▼
                 RunOnce → RunTurn → Executor.runHarness (executor.go:377)
                                  │
                  ClaudeCodeHarness.Run  (cli.go — REWORKED)
                  ├─ build argv: --print --output-format json [--bare]
                  │     [--append-system-prompt <instr>] [--mcp-config …]
                  │     [--permission-mode …|--allowedTools …|--dangerously-skip-permissions]
                  │     [--resume <sid>|--continue] [--model …] [ExtraFlags…]
                  ├─ runCLI (unchanged driver: env merge, budget, cancel, bounded capture)
                  └─ parseClaudeJSON(stdout) → Response{Output, TokensIn/Out, Cost, ToolCalls?, sessionID}
                                  │
                   executor.go:397 Usage{Tokens, Cost, ToolCalls} + Final Step
                                  ▼
                    RunResult → controller fold → AgentRun.Status
                    (sessionID persisted for next turn; §4d)
```

### 4a. Structured output → Response richness

`ClaudeCodeHarness.Run` defaults `--output-format json` (override-able). After `runCLI` returns stdout, a new `parseClaudeResultJSON([]byte) (claudeResult, error)` unmarshals the envelope (§3.2) and maps:

- `result` → `Response.Output` (raw bytes of the *text*, preserving today's "Output is the answer" semantics — **not** the whole JSON envelope, so downstream `output` stays a clean answer).
- `usage.input_tokens`/`output_tokens` → `Response.TokensIn`/`TokensOut`. Cache fields (`cache_creation_input_tokens`, `cache_read_input_tokens`) → **new `Response.CacheCreationTokens`/`CacheReadTokens`** (additive; `harness-authoring.md` §4 contract gains these for kinds that report them).
- `total_cost_usd` → **new `Response.CostUSDMilli int64`** (store as integer micro/milli-USD to avoid float in CRD status; proposal: **milli-USD**, i.e. `round(total_cost_usd*1000)`).
- `session_id` → **new `Response.SessionID string`** (used for resume + status surfacing).
- `num_turns` → folded into `Usage.Steps`? No — `Steps` is our plan-act-observe count; instead expose as **`Response.NumTurns int32`** → status. (Harness `num_turns` ≠ our `Step` trace; keep distinct.)
- `permission_denials` (non-empty) → appended to `Step.Error`/a diagnostic note (loud, since a silently-denied tool changes behavior).
- `is_error: true` / `subtype` starting `error_` → return a non-nil error so the executor records `Phase=Failed` with `TerminationReason=subtype` (`executor.go:409-417`).

**Instructions channel fix:** stop prepending `req.Instructions` to the user prompt; pass it via `--append-system-prompt <instr>` instead (correct semantics; keeps Claude's default system behavior). The user prompt is then the clean `promptFromInput(req.Input)` and may be piped on stdin (avoids argv length limits for big prompts).

> **stream-json (optional, phase 3):** when the spec opts into streaming (for terminal/live progress), the harness reads NDJSON, accumulates `assistant`/`stream_event` text into `Output`, takes `usage`/`session_id`/`total_cost_usd` from the terminal `result` event, and may surface `assistant` tool_use blocks as `ToolCallRecord`s. Default stays `json` (simpler, one parse).

### 4b. MCP server passthrough

New `HarnessCLISpec.MCP` (Claude-relevant; generic enough to reuse) → the operator renders an MCP config file into the run ConfigMap and the harness passes `--mcp-config <path>`. Two layers:

1. **CRD:** a typed list of MCP servers (name + transport + command/url + env), OR an escape hatch `MCPConfigInline json.RawMessage` for power users. (See `MCPServerSpec` in §5.)
2. **Operator:** `runspec.go` marshals it to `/etc/agent/mcp.json` (mounted) and sets `cli.MCPConfigPath`. Secrets for MCP servers (e.g. a GitHub PAT for a remote MCP) flow via the broker into the rendered config's `env` — **never** inlined in the CR.
3. **Harness:** if `cli.MCPConfigPath != ""`, append `--mcp-config <path>`; auto-allow declared `mcp__<server>__*` tools unless permission posture says otherwise.

> **Egress coupling (loud):** every MCP server with a remote URL is a new egress destination the **default-deny NetworkPolicy will block** (it only opens DNS + in-cluster RFC1918 + public 80/443). Remote MCP over 443 works *today by coincidence* of the public-443 allowance; anything else (custom port, or once AgentNetwork datapath enforcement lands and tightens egress per [agentnetwork-datapath-enforcement](agentnetwork-datapath-enforcement.md)) must be added to the Agent's allow-list. **stdio MCP servers run in-pod** (subprocess) — no egress, but they execute code inside the sandbox, so they inherit the sandbox's protections and must be trusted. This is the single biggest new attack surface (§7).

### 4c. Permission modes (the §8 seam, made concrete for Claude)

Adopt `harness-authoring.md` §8's proposed `HarnessCLISpec.ExtraFlags` + `ApprovalMode` and bind Claude's real flags:

| `approvalMode` | Claude flags emitted | Posture |
|---|---|---|
| `""` (default) | *(none)* — Claude's own default | safest; may prompt → can hang headless. **We additionally default `--permission-mode dontAsk`** for headless safety unless overridden (see note). |
| `safe` | `--permission-mode dontAsk` + honor `cli.allowedTools` | denies anything not explicitly allowed; locked-down. |
| `acceptEdits` | `--permission-mode acceptEdits` | auto-approve file writes + common fs cmds; other shell/network still need `allowedTools`. |
| `auto` / `never` | `--dangerously-skip-permissions` | **no guardrails** — Claude can run arbitrary commands/edits unattended. Opt-in only. |

Plus `cli.allowedTools []string` → `--allowedTools "<csv>"` and `cli.disallowedTools []string` → `--disallowedTools`. `ExtraFlags []string` appended verbatim after the curated args for anything not modeled.

> **Headless default decision (OPEN, §10):** a bare `--print` with Claude's *default* permission mode can block on an approval prompt that no one answers, hanging until the wall-clock budget kills it. Options: (i) default to `--permission-mode dontAsk` (safe but Claude can do little without `allowedTools`); (ii) require the Agent author to set `approvalMode` explicitly and fail admission otherwise; (iii) default to `--dangerously-skip-permissions` (matches "it's a sandbox anyway" but normalizes the dangerous flag). **Recommendation: (i)** — safe-by-default, force opt-in for power. The kata-fc + default-deny envelope is what makes even `never` *acceptable* when chosen, but defaults must stay safe (`harness-authoring.md` §8 security trade-off).

### 4d. Resumable multi-turn sessions

This is the headline feature: make a durable `AgentSession` give Claude **conversation** memory, not just files.

Mechanism (CLI-kind, follows the `harness-authoring.md` §5 "actively isolate vs forward stable id" rule, here for resume rather than a header):

1. **Persist HOME on AgentFS.** Claude's resume history lives under its home dir; today `HOME=/tmp` (Dockerfile:30, ephemeral). For resumable sessions the operator sets `HOME` to an AgentFS-backed path (e.g. `<mount>/.claude-home`) so `~/.claude` survives across turns/pods. (New: `runspec.go` overrides `HOME` env when `sessionPolicy=persistent` + AgentFS present.)
2. **Capture `session_id`.** After turn N, the harness returns `Response.SessionID` (§4a). The session worker / controller stores it (see §5: `AgentSessionStatus.HarnessSessionID`, or threaded via the turn queue).
3. **Resume on turn N+1.** When a stored session id exists, the harness appends `--resume <sid>`; when only "latest in this workspace" is desired, `--continue`. Selection by `sessionPolicy`:
   - `ephemeral` (default): no resume — fresh conversation each run (must NOT rely on `--continue` picking up a stale conversation; explicitly omit, mirroring the Hermes "actively isolate" rule).
   - `persistent`: resume the stored id (`--resume`), or `--continue` if no id captured yet.

```
AgentSession (durable worker, see agentsession-scaling-impl.md)
  turn 1: claude --print --output-format json "<q1>"        → session_id=S, HOME on AgentFS
  turn 2: claude --print --output-format json --resume S "<q2>"   (S read from status)
  turn 3: claude --print --output-format json --resume S "<q3>"
            └── conversation + workspace both carried across turns
```

> This upgrades `SessionPolicy=persistent` for `claude-code` from "reuse workspace" (today, `harness.go:291-304`) to "reuse workspace **and** conversation". The durable worker that issues turns is specified in [agentsession-scaling-impl](agentsession-scaling-impl.md); this spec defines the resume mechanics it drives. **Dependency, not duplication.**

### 4e. Short-lived credentials via `apiKeyHelper`

Instead of a single static `ANTHROPIC_API_KEY` (good only until the broker lease expires), render an `apiKeyHelper` script that re-fetches a fresh lease from the broker on demand:

1. Operator writes a tiny helper (e.g. `/etc/agent/anthropic-key.sh`) that calls the in-pod broker socket and prints the current token. (Broker access is already the model for run-pod secrets — [dynamic-credential-backends](dynamic-credential-backends.md).)
2. Operator renders `--settings '{"apiKeyHelper":"/etc/agent/anthropic-key.sh"}'` (or a mounted `settings.json`).
3. Claude re-invokes it on 5-min TTL / 401 (tunable via `CLAUDE_CODE_API_KEY_HELPER_TTL_MS`), so a 30-min run survives a 10-min lease.

This is **opt-in** (`cli.apiKeyHelper: true` or implied when the Anthropic secretRef is a short-TTL backend). Static `ANTHROPIC_API_KEY` remains the simple default. Note the known TTL-cache bug (#11639) — verify on the live box (§9).

### 4f. Terminal / interactive access

Out of scope for the mechanics here — **owned by [terminal-exposure-http-ssh-tmux](terminal-exposure-http-ssh-tmux.md)**. What this spec contributes: the `claude-code` bundle image already carries the interactive `claude` TUI; an interactive session would run `claude` (no `--print`) under a PTY inside the same sandboxed, AgentFS-backed pod, with `--resume` to attach to the same conversation a headless turn created. The terminal spec defines the PTY/SSH/tmux exposure; this spec guarantees the image + resume semantics it builds on.

---

## 5. Concrete changes

### 5.1 CRD / Go type additions (`pkg/agentmodel/v1/harness.go`)

Extend `HarnessCLISpec` (currently `harness.go:145-166`). **All additive; `+optional`.**

```go
type HarnessCLISpec struct {
    // ... existing: PromptFlag, WorkingDir, MaxOutputBytes, (PassthroughEnv — REMOVE) ...

    // OutputFormat selects the harness's structured-output mode where the CLI
    // supports it. For claude-code: "json" (default), "stream-json", or "text".
    // Empty defaults to "json" for claude-code so tokens/cost/session are captured.
    // +kubebuilder:validation:Enum=text;json;stream-json
    // +optional
    OutputFormat string `json:"outputFormat,omitempty"`

    // ApprovalMode maps a portable intent to each CLI's approval flags.
    // "" (default) = safe headless posture; "safe"; "acceptEdits"; "never".
    // +kubebuilder:validation:Enum="";safe;acceptEdits;never
    // +optional
    ApprovalMode string `json:"approvalMode,omitempty"`

    // AllowedTools / DisallowedTools map to --allowedTools / --disallowedTools
    // (permission-rule syntax, e.g. "Bash(git diff *)").
    // +optional
    AllowedTools []string `json:"allowedTools,omitempty"`
    // +optional
    DisallowedTools []string `json:"disallowedTools,omitempty"`

    // ExtraFlags are appended verbatim after the curated args. Escape hatch for
    // flags not modeled above; does NOT discard the curated defaults (unlike
    // spec.command). (harness-authoring.md §8)
    // +optional
    ExtraFlags []string `json:"extraFlags,omitempty"`

    // MCP declares MCP servers passed via --mcp-config. Mutually exclusive with
    // MCPConfigInline.
    // +optional
    MCP []MCPServerSpec `json:"mcp,omitempty"`
    // MCPConfigInline is a raw Claude MCP-config JSON document (power users).
    // +optional
    MCPConfigInline json.RawMessage `json:"mcpConfigInline,omitempty"`

    // APIKeyHelper, when true, renders a broker-backed apiKeyHelper so the CLI
    // re-fetches short-lived creds on TTL/401 instead of using a static key.
    // +optional
    APIKeyHelper bool `json:"apiKeyHelper,omitempty"`

    // Bare passes --bare (skip auto-discovery) for reproducible runs. Default
    // false today; track Anthropic making it the -p default.
    // +optional
    Bare bool `json:"bare,omitempty"`

    // Model overrides the model (--model). Otherwise ANTHROPIC_MODEL env wins.
    // +optional
    Model string `json:"model,omitempty"`
}

// MCPServerSpec is one MCP server. Transport: "stdio" (command, in-pod) or
// "http"/"sse" (url, egress). Secrets via SecretRef → rendered env, never inline.
type MCPServerSpec struct {
    Name      string            `json:"name"`
    Transport string            `json:"transport"` // stdio | http | sse
    Command   []string          `json:"command,omitempty"` // stdio
    URL       string            `json:"url,omitempty"`      // http/sse
    Env       []HarnessEnvVar   `json:"env,omitempty"`      // reuse broker secretRef
}
```

`Response` (`pkg/agentruntime/harness/iface.go:56-69`) gains:

```go
SessionID            string  // claude session_id (resume key)
CostUSDMilli         int64   // round(total_cost_usd * 1000)
CacheCreationTokens  int64   // usage.cache_creation_input_tokens
CacheReadTokens      int64   // usage.cache_read_input_tokens
NumTurns             int32   // result.num_turns
```

`v1.Usage` (`pkg/agentmodel/v1/types.go`, near `Step`/`RunStatus`) gains `CostUSDMilli int64` and optionally cache-token fields; folded at `executor.go:397-407`. `RunStatus` (`types.go:317-326`) / `AgentRun.Status` gains `HarnessSessionID string` so the next turn can resume. **Coordinate the Usage/Response field additions with [response-richness](response-richness.md)** (it owns the cross-kind shape; this spec is the first concrete producer for `claude-code`).

`AgentSessionStatus` (currently skeletal — `agentRef`+`idleTimeoutSeconds`, and `.Runs` is dead per the verified facts) gains `HarnessSessionID string` to persist the resume id across turns (or thread it on the turn queue — decide with [agentsession-scaling-impl](agentsession-scaling-impl.md)).

### 5.2 Harness implementation (`pkg/agentruntime/harness/cli.go`)

Rework `ClaudeCodeHarness.Run` (`cli.go:154-165`). New helpers in a `claude.go` (keep `cli.go` generic):

- `buildClaudeArgs(req Request, resumeID string) []string` — assembles `--print` (or `cli.PromptFlag`), `--output-format <fmt>`, `[--bare]`, `[--append-system-prompt <instr>]`, `[--model …]`, `[--mcp-config <path>]`, permission flags from `ApprovalMode`/`AllowedTools`/`DisallowedTools`, `[--resume <id>|--continue]`, then `ExtraFlags`, then the prompt (or pipe via stdin).
- `parseClaudeResultJSON([]byte) (claudeResult, error)` — unmarshal the §3.2 envelope (mirrors `parseUsage`, `hermes.go:265-276`, but for Claude's shape). For `stream-json`, `parseClaudeStream(io.Reader)` accumulates events.
- `Run` calls `runCLI`, then `parseClaudeResultJSON`, populates the new `Response` fields, returns an error when `is_error`/`subtype` indicates failure.
- The resume id is read from `req` (new `Request.ResumeSessionID string`, set by `RunTurn`/session worker; `harness_runner.go:30-46` passes it through, like `Seed`).

`runCLI` (`cli.go:27-79`) gets one generic change: append `req.Spec.CLI.ExtraFlags` after the prompt args (the §8 driver change), and optionally write the prompt to **stdin** instead of argv when a new `cli.PromptViaStdin` is set (avoids argv limits; Claude reads stdin). Keep the bounded capture, budget timeout, cancel, env merge unchanged.

### 5.3 Operator wiring (`operator/internal/builders/`)

- `runspec.go`: when `harness.kind=claude-code`,
  - render MCP config → `/etc/agent/mcp.json` (ConfigMap), set `cli.MCPConfigPath`; inject MCP-server secretRefs via broker;
  - render `apiKeyHelper` script + `--settings` when `cli.apiKeyHelper`;
  - set `HOME` to `<agentfsMount>/.claude-home` when `sessionPolicy=persistent` + AgentFS (so `~/.claude` is durable);
  - keep mapping the Anthropic secretRef to `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN` and `cli.model`→`ANTHROPIC_MODEL`/`--model`, `ANTHROPIC_BASE_URL` when a gateway is configured.
- `harness_image.go:20-25`: unchanged (bundle already maps).
- `ValidateHarness` (`harness.go:327-332`): add the `claude-code` arm validations — `OutputFormat`/`ApprovalMode` enums, MCP vs MCPConfigInline mutual exclusion, MCPServerSpec transport enum + (stdio⇒command, http/sse⇒url).

### 5.4 Bundle image (`deploy/docker/harness-claude-code.Dockerfile`)

- Pin `CLAUDE_CODE_VERSION` (today `latest`, line 22) to a tested tag for reproducibility (publish a matrix of pins).
- Add `jq` (optional, for debugging) — not required since we parse in Go.
- Ensure a writable `~/.claude` works when `HOME` is overridden to AgentFS (the operator creates the dir; the image must not hard-require `/tmp`). `ENV HOME=/tmp` stays as the *default*; the operator overrides per §5.3.

### 5.5 New files

- `pkg/agentruntime/harness/claude.go` — `buildClaudeArgs`, `parseClaudeResultJSON`, `parseClaudeStream`, `claudeResult` struct.
- `pkg/agentruntime/harness/claude_test.go` — argv + parse tests.
- `operator/internal/builders/mcp_config.go` — render `MCPServerSpec` → Claude MCP JSON.
- `operator/config/samples/agent_claude_code_full.yaml` — MCP + approvalMode + persistent-session sample.

---

## 6. Data / control flow (end-to-end, one resumable session)

1. **Apply** an `Agent{mode:harness, harness:{kind:claude-code, sessionPolicy:persistent, cli:{approvalMode:safe, allowedTools:["Read","Edit","Bash(git *)"], mcp:[{name:github, transport:http, url:…, env:[{name:TOKEN, secretRef:…}]}], apiKeyHelper:true}}, storage:{kind:agentfs,…}}` + an `AgentSession{agentRef:…}`.
2. **Operator** (`runspec.go`) renders mcp.json + apiKeyHelper script into the run ConfigMap, sets `HOME=<mount>/.claude-home`, maps Anthropic secretRef, builds the run pod with kata-fc + default-deny egress (unchanged `run_sandbox.go`).
3. **Turn 1:** session worker issues a turn → `RunTurn` (`runonce.go:51`) → `Executor.runHarness` (`executor.go:377`) → `ClaudeCodeHarness.Run` builds `claude --print --output-format json --append-system-prompt <instr> --permission-mode dontAsk --allowedTools "Read,Edit,Bash(git *)" --mcp-config /etc/agent/mcp.json "<q1>"`.
4. `runCLI` (`cli.go:27`) execs in `<mount>` with merged env; Claude (re)reads creds via `apiKeyHelper`; runs its **own** plan-act-observe loop + MCP tools *inside* the process.
5. Claude prints the JSON envelope; `parseClaudeResultJSON` → `Response{Output:result, TokensIn/Out, CostUSDMilli, SessionID:S, NumTurns}`.
6. `executor.go:397-407` folds `Usage{Tokens, Cost, ToolCalls}` + a `Final` `Step`; `RunResult` → controller fold (`agentrun_controller.go:404`); `HarnessSessionID=S` persisted to session status; termination-message clamp (`cmd/agent/run.go:102`) keeps it under 3 KiB (Output already a clean answer string).
7. **Turn 2:** worker reads `S`, issues `<q2>`; harness appends `--resume S`; Claude resumes the conversation (history under the AgentFS-durable `~/.claude`) **and** sees the same workspace files. Cost/tokens accumulate per turn in status.

For `ephemeral` (default), step 7 omits `--resume` and each run is a fresh conversation (and, without AgentFS, a fresh workspace).

---

## 7. Security model

Composes on the **unchanged** v0.2.0 run-pod envelope (kata-fc microVM fail-closed RuntimeClass + static default-deny NetworkPolicy + non-root/drop-ALL/seccomp; `run_sandbox.go`, `sandbox.go`, verified in [agent-runtime-fit-analysis-v0.2.0.md §1](../research/agent-runtime-fit-analysis-v0.2.0.md)). New surface introduced by this spec, with mitigations:

| New surface | Risk | Mitigation |
|---|---|---|
| **`approvalMode=never` / `--dangerously-skip-permissions`** | Claude edits files + runs arbitrary commands unattended | Acceptable **only** because the pod is a kata-fc sandbox with default-deny egress (`harness-authoring.md` §8). Opt-in per Agent; default (`""`/`dontAsk`) keeps safe posture. No cluster RBAC/SA is granted to the run pod (verified fact #4), so even arbitrary code can't reach the apiserver. |
| **Remote MCP servers** (`http`/`sse`) | New egress destination; SSRF/exfil to a malicious or attacker-controlled MCP endpoint | Each remote MCP URL must pass the egress allow-list. Default-deny blocks all but DNS/RFC1918/public-80-443 today; once [agentnetwork-datapath-enforcement](agentnetwork-datapath-enforcement.md) lands, MCP hosts must be explicitly allowed. Validate URL is not metadata/loopback/link-local (reuse the `images.go:109-119` `isInternalHost` pattern at admission). |
| **stdio MCP servers** (`command`) | Arbitrary subprocess executes inside the sandbox; supply-chain risk of the MCP package | Runs under the same non-root/drop-ALL/seccomp + kata-fc isolation; no egress unless the command itself opens one (blocked by default-deny). Treat the MCP command as trusted code the tenant chose; document that it inherits sandbox protections, not bypasses them. |
| **`apiKeyHelper` script** | Script runs in-pod, prints a live credential to Claude; a compromised Claude could read it | The credential is *already* available to Claude (it's the Anthropic key); apiKeyHelper just refreshes it. Helper talks only to the in-pod broker socket (SO_PEERCRED/SPIFFE-gated per [dynamic-credential-backends](dynamic-credential-backends.md)) — no network. Short TTL limits blast radius vs a static long-lived key (a *net security gain*). |
| **`session_id` reuse across tenants** | Resuming the wrong conversation could leak context | Session id is namespaced to the Agent/AgentSession and lives on that Agent's AgentFS HOME (per-Agent volume). Never share `HarnessSessionID` across Agents. Ids are UUIDs (unguessable). |
| **Durable `~/.claude` on AgentFS** | Conversation history + cached creds persisted to a backed-up volume (S3) | Same protection as any AgentFS data: SSE-KMS at rest (sample `agent_claude_code.yaml:41-42`), per-Agent prefix. Don't persist raw creds — `apiKeyHelper` re-fetches, so the cache TTL is short; document scrubbing if Claude caches tokens under HOME. |
| **JSON envelope / cost in status** | `total_cost_usd`/tokens visible in `AgentRun.Status` | Non-secret accounting; intended for governance ([run-governance](run-governance.md)). No secret leaks into status (Output is the answer text, not the env). |

**SPIFFE/broker:** unchanged — the run pod's identity and broker access follow the existing model ([runtime-and-identity](../features/runtime-and-identity.md)); apiKeyHelper and MCP secretRefs both go through the broker, never inline.

---

## 8. Phasing & effort

Shippable increments, smallest valuable first. Sizes: S ≈ ≤0.5d, M ≈ 1–2d, L ≈ 3–5d, XL ≈ >1wk.

| # | Increment | Size | Depends on |
|---|---|---|---|
| **P1** | **JSON output + Response richness.** `--output-format json` default; `parseClaudeResultJSON`; populate `Response.{TokensIn,TokensOut,CostUSDMilli,SessionID,NumTurns,Cache*}`; `--append-system-prompt` for instructions; fold cost/tokens into `Usage`/status. | **M** | [response-richness](response-richness.md) (shared field shape) |
| **P2** | **Permission posture + ExtraFlags.** `HarnessCLISpec.ApprovalMode`/`AllowedTools`/`DisallowedTools`/`ExtraFlags`; `runCLI` appends ExtraFlags; map approvalMode→Claude flags; default safe headless mode; remove dead `PassthroughEnv`. | **M** | P1; [harness-authoring.md §8](../design/harness-authoring.md) |
| **P3** | **MCP passthrough.** `MCPServerSpec` + `MCPConfigInline`; `mcp_config.go` renderer; `--mcp-config`; admission validation incl. internal-host block; broker secretRefs into rendered env. | **L** | P2; [agentnetwork-datapath-enforcement](agentnetwork-datapath-enforcement.md) (egress for remote MCP) |
| **P4** | **Resumable sessions.** `HOME`-on-AgentFS when persistent; capture `session_id`; `--resume`/`--continue`; `Request.ResumeSessionID` + `AgentSessionStatus.HarnessSessionID`. | **L** | P1; [agentsession-scaling-impl](agentsession-scaling-impl.md) (durable worker that drives turns) |
| **P5** | **apiKeyHelper short-lived creds.** Render broker-backed helper + `--settings`; opt-in; verify TTL-cache behavior (#11639). | **M** | P1; [dynamic-credential-backends](dynamic-credential-backends.md) |
| **P6** | **stream-json + terminal hooks.** `parseClaudeStream`; live progress; image/resume guarantees for interactive. | **L** | P1; [terminal-exposure-http-ssh-tmux](terminal-exposure-http-ssh-tmux.md) |
| **P0** | **Termination-message size budget** (cross-cutting; not Claude-specific) — lift the 4 KiB trace truncation. | — | owned by [response-richness](response-richness.md) |

Recommended first ship: **P1+P2** (richness + safe permissions) — immediate observability/governance win, no new egress surface.

---

## 9. Test plan

**Unit (Go, no network):**
- `claude_test.go`: `buildClaudeArgs` golden argv for each `approvalMode`/`outputFormat`/resume/MCP/ExtraFlags combination; assert `--append-system-prompt` carries instructions (not the user prompt). Use the existing `Cmd commandFunc` seam (`cli.go:17-22`) to inject a fake `claude` that echoes a canned JSON envelope; assert `Response.{TokensIn,TokensOut,CostUSDMilli,SessionID,NumTurns,Cache*}` parse correctly, incl. `is_error:true` → error, `error_max_turns` subtype → `TerminationReason`.
- `parseClaudeResultJSON`: table tests for the §3.2 envelope, malformed/missing-usage (→ zeros, like `parseUsage`), and a `stream-json` NDJSON fixture.
- `mcp_config.go`: render `MCPServerSpec` → expected Claude MCP JSON; secretRef → env placeholder; stdio vs http/sse.
- `ValidateHarness`: enum rejections, MCP/MCPConfigInline mutual exclusion, internal-host MCP URL rejected.
- `runspec_test.go`: HOME-on-AgentFS when persistent; mcp.json + apiKeyHelper mounted; ANTHROPIC_* env mapping.

**E2E (cftest single-node k0s box exists — [cf_tunnel_deploy], live-verified deploy path):**
- **R1 — richness:** deploy a `claude-code` Agent with a real `ANTHROPIC_API_KEY` (or z.ai-style proxy via `ANTHROPIC_BASE_URL`); run a trivial prompt; assert `AgentRun.Status` has non-zero `Usage.Tokens` + `CostUSDMilli` + a `HarnessSessionID`.
- **R2 — permissions:** `approvalMode:never` run that edits a file in AgentFS; assert the edit persists and no hang; a `safe` run with no `allowedTools` is constrained.
- **R3 — MCP:** a stdio MCP server (in-pod) is reachable; a remote MCP at a non-allowed host is blocked by egress (negative test) and allowed once added.
- **R4 — resume:** `AgentSession` turn 1 sets `HarnessSessionID`; turn 2 with `--resume` answers a question that requires turn-1 context (e.g. "what variable did you just define?"). This is the proof that conversation (not just files) carries — the headline outcome.
- **R5 — apiKeyHelper:** a run longer than a deliberately-short broker lease TTL succeeds (helper re-fetch), confirming #11639 doesn't bite our setup.

Verify multiarch (amd64 cftest box + arm64 Graviton) per the build matrix; pin `CLAUDE_CODE_VERSION` so e2e is reproducible.

---

## 10. Risks & open decisions

- **OPEN — headless default permission mode (§4c).** Default to `dontAsk` (safe, limited), force explicit `approvalMode`, or default to `--dangerously-skip-permissions` (sandbox-trusts-it)? **Recommend `dontAsk`.** Maintainer must decide — it sets the safety/UX tradeoff for every Claude Agent.
- **OPEN — where the resume id lives.** `AgentSessionStatus.HarnessSessionID` (controller-persisted) vs threaded on the NATS turn queue (worker-local). Decide jointly with [agentsession-scaling-impl](agentsession-scaling-impl.md). Multiple concurrent turns against one session would corrupt a single `--resume` conversation — sessions must serialize turns (the durable-worker model already does).
- **RISK — Claude CLI surface churn.** Flags/envelope move fast (it's now the "Agent SDK"; `--bare` slated to become the `-p` default; June 15 2026 credit changes). Pin `CLAUDE_CODE_VERSION`, snapshot the envelope shape in a test fixture, and treat parsing defensively (missing fields → zeros, never panic). The Python SDK's structured `ResultMessage` is an alternative to CLI-JSON-parsing — **OPEN: adopt the Agent SDK (Python/TS) instead of parsing CLI JSON?** That trades a subprocess-text contract for an SDK dependency and a different runtime; for now CLI-JSON keeps the harness model intact. Revisit if parsing proves brittle.
- **RISK — `apiKeyHelper` TTL cache bug (#11639).** May re-invoke the helper on *every* request; benign for us (broker is in-pod, cheap) but watch latency. Verify on the live box (R5).
- **RISK — termination-message cap still truncates rich Claude traces** (`cmd/agent/run.go:102`). P1 makes Output a clean answer (helps), but a verbose tool trace still elides to pod logs. Real fix is the size-budget work owned by [response-richness](response-richness.md) — call it a known limitation until then.
- **OPEN — `total_cost_usd` representation in CRD.** Float in status is awkward; proposal is integer **milli-USD** (`CostUSDMilli`). Confirm with [run-governance](run-governance.md) so budget enforcement uses one unit.
- **RISK — `--continue` ambiguity under shared HOME.** `--continue` picks the most-recent conversation in the workspace; with concurrent or interleaved runs that's nondeterministic. Prefer explicit `--resume <id>`; reserve `--continue` for the first turn when no id exists. (See [determinism-and-replay](determinism-and-replay.md) for the broader determinism stance.)
- **NOT built reminder.** Everything in §4–§5 is a proposal. Today `claude-code` is one-shot `claude --print` with opaque output, no MCP, no permission flags, no resume, no apiKeyHelper (§2). Do not cite this spec as implemented.
