# Spec: Full support for the OpenAI Codex CLI (`harness.kind=codex`)

> **✅ Decisions resolved 2026-06-03 — see [decisions.md](../design/decisions.md).** D3: sandbox/approval (`danger-full-access`, `--ask-for-approval never`) are opt-in-only + microVM-gated, never default; codex requires the platform model gateway to speak the OpenAI Responses API (`wire_api=responses`). Where this doc still says OPEN/PROPOSED and conflicts, the decision log wins.

> **Status: DESIGN / SPEC — 2026-06-03 (v0.2.0 source).** Implementation-grade. Scope: take `harness.kind=codex` from a thin `codex exec <prompt>` shim to a first-class harness — JSON event-stream parsing (tokens + tool calls), explicit non-interactive approval/sandbox flags layered correctly under kata-fc, `config.toml` model/provider selection (incl. custom OpenAI-compatible endpoints), resumable sessions, and a live e2e on the cftest box. Every code claim is cited `file:line` against the tree; every external-API claim is cited to OpenAI's Codex docs (training cutoff is Jan 2026 and this CLI moves fast — URLs verified 2026-06-03).
>
> Companion reads: this **extends** the harness-authoring contract in [harness-authoring.md](../design/harness-authoring.md) (do not re-read the whole thing here — §4 Response contract and §8 per-kind flags are the load-bearing parts). Sibling agent specs: [agent-claude-code.md](agent-claude-code.md), [agent-hermes.md](agent-hermes.md). Cross-cutting: [response-richness.md](response-richness.md) (the Steps/tokens wire), [determinism-and-replay.md](determinism-and-replay.md) (Seed + `--json` traces). The sandbox/egress base it composes with is scored in [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md), §1.

---

## 1. Summary

**Full Codex support** means an Agent with `mode=harness, harness.kind=codex` runs the real OpenAI Codex CLI inside the hardened run sandbox and the platform gets back what it can honestly observe from it: the final assistant message (already works), plus **token usage** and a **tool/command/file-edit trace** parsed from Codex's `--json` event stream (today: lost), plus **deterministic non-interactive behavior** under the restricted-PSA + kata-fc sandbox (today: relies on Codex's interactive defaults, which deadlock or escalate). The outcome: `codex` becomes a peer of the Hermes harness on observability (tokens + tool calls in `AgentRun.Status.Steps`), runs unattended without Codex's own sandbox fighting kata's, and is **live-verified end-to-end** against an OpenAI-compatible endpoint on the cftest single-node k0s box.

This is an agent-integration spec: the heavy plumbing (sandbox pinning, egress cage, broker secrets, Step folding) already exists and is reused. The new work is concentrated in **one harness file** (`CodexHarness.Run` + a JSON-event parser), a **CLI-flag seam** shared with the other coding CLIs (the §8 proposal in harness-authoring.md, made concrete here), a **rendered `config.toml`**, and the **Dockerfile + e2e**.

---

## 2. Current state

### What works today

| Capability | Status | Evidence |
|---|---|---|
| `codex exec <prompt>` as a subprocess harness | ✅ wired | `CodexHarness.Run` (`pkg/agentruntime/harness/cli.go:174-180`) builds argv `["exec", prompt]` and delegates to `runCLI` |
| Instructions prepended to prompt | ✅ | `cli.go:176-178` (`Instructions + "\n\n" + prompt`) |
| Registered + admission-gated | ✅ | const `HarnessCodex` (`pkg/agentmodel/v1/harness.go:42`), `Valid()` arm (`harness.go:66`), `Default()` registration (`iface.go:83-94` per harness-authoring §1), `ValidateHarness` CLI arm (`harness.go:327-332`) |
| Per-kind bundle image | ✅ exists, NOT published-verified | `perKindHarnessImage[HarnessCodex]="harness-codex"` (`operator/internal/builders/harness_image.go:22`); Dockerfile `deploy/docker/harness-codex.Dockerfile` (npm `@openai/codex@${CODEX_VERSION}`, runs uid 65532, `HOME=/tmp`) |
| `OPENAI_API_KEY` via broker | ✅ (mechanism) | broker-leased env merged by `mergeEnv` (`cli.go:108-119`); inherits `os.Environ()` then overlays `Request.Env` + literals |
| Output folded to `AgentRun.Status` | ✅ | executor wraps the bounded call in one `StepFinal` (`pkg/agentruntime/executor.go:404-407`), folded by `foldRunResult` (`agentrun_controller.go:~404`) |
| Run pod is sandboxed + egress-caged | ✅ (shared) | RuntimeClass pin `ApplyRunSandbox` + default-deny egress `BuildAgentRunEgressPolicy` (`operator/internal/builders/run_sandbox.go:45-63`); fail-closed resolution in the controller (verified facts #1) |

### What is stubbed / missing (the gap this spec closes)

> **NOT BUILT — the five gaps.** Each is a concrete code fact, not a vibe.

1. **No `--json` parsing → zero observability.** `CodexHarness.Run` (`cli.go:174-180`) runs **plain** `codex exec <prompt>` and returns whatever `runCLI` captured on stdout. `runCLI` never parses tokens (`cli.go:27-79`), so per the Response contract (harness-authoring §4) `codex` reports `TokensIn=TokensOut=0` and `ToolCalls=nil` **always**. The executor faithfully folds those zeros into the single `StepFinal` (`executor.go:397-407`). Result: a Codex run that edited 12 files and burned 40k tokens shows up as one opaque blob with `tokens:0, toolCalls:0`. **Codex actually emits all of this** on stdout as JSONL when invoked with `--json` (§3) — we throw it away.

2. **No approval/sandbox flags → interactive deadlock or unwanted escalation.** The argv is hard-coded to `["exec", prompt]` (`cli.go:179`) with **no `--ask-for-approval` and no `--sandbox`**. `codex exec` is non-interactive, but Codex's *default* approval policy is `on-request` and its default sandbox is `workspace-write` ([config-reference](https://developers.openai.com/codex/config-reference)); under our restricted-PSA pod Codex's own Linux sandbox (bubblewrap/unprivileged-userns) **cannot initialize** (§7), so it either errors, falls back unpredictably, or escalates to approval prompts that a non-interactive run can never answer. There is no seam to set these flags short of a full `spec.command` override that discards every curated default (`cli.go:31-36`).

3. **No `config.toml` / model + provider selection.** Nothing renders a `~/.codex/config.toml` (or `CODEX_HOME`) into the run pod. An Agent cannot pick the model, point Codex at a custom OpenAI-compatible gateway (the platform's own model gateway), or set `wire_api`. The only knob is whatever `OPENAI_API_KEY` happens to be leased, against OpenAI's default base URL. (`runspec.go` renders `agent.json`/`run.json`/`provider.json` but no Codex config — `runspec.go:58-68`.)

4. **No resumable session.** `SessionPolicy=persistent` for a CLI kind means "reuse the AgentFS workspace" (harness-authoring §5; `EffectiveWorkingDir`, `harness.go:291-304`) — file state survives, but Codex's **own conversation thread** does not. Codex supports `codex exec resume [SESSION_ID]` / `--last` (§3), which we never invoke, so each run starts a cold thread even when the workspace is durable.

5. **NOT live-run.** No e2e exercises `kind=codex` against a real endpoint. The harness has a unit test seam (`Cmd commandFunc`, `cli.go:17-22`) but no proof the real `codex` binary in the bundle parses our flags, reads our config, and folds a non-trivial trace. (Contrast the Hermes path, which is live-green per `MEMORY.md`.)

---

## 3. External interface research (OpenAI Codex CLI — verified 2026-06-03)

> Source of truth: OpenAI's Codex docs at `developers.openai.com/codex` and the `openai/codex` GitHub repo. The CLI is the Rust rewrite distributed as the npm package `@openai/codex` (what the bundle installs — `harness-codex.Dockerfile:23`). **Pin the version** (§5): flag/event names move.

### 3.1 `codex exec` — non-interactive execution

`codex exec` (short `codex e`) is the scripted, finish-without-human-interaction entrypoint. ([cli/reference](https://developers.openai.com/codex/cli/reference), [noninteractive](https://developers.openai.com/codex/noninteractive)) Flags we care about (names quoted verbatim):

| Flag | Values / form | Meaning | Source |
|---|---|---|---|
| `--json` (a.k.a. `--experimental-json`) | boolean | stdout becomes **newline-delimited JSON (JSONL)**, one event per state change | [cli/reference](https://developers.openai.com/codex/cli/reference), [cli/features](https://developers.openai.com/codex/cli/features) |
| `--ask-for-approval`, `-a` | `untrusted` \| `on-request` \| `never` | when Codex pauses for human approval | [cli/reference](https://developers.openai.com/codex/cli/reference) |
| `--sandbox`, `-s` | `read-only` \| `workspace-write` \| `danger-full-access` | OS sandbox policy for model-generated commands | [cli/reference](https://developers.openai.com/codex/cli/reference), [concepts/sandboxing](https://developers.openai.com/codex/concepts/sandboxing) |
| `--dangerously-bypass-approvals-and-sandbox` (alias `--yolo`) | boolean | **no sandbox, no approvals** — intended for use *inside an already-sandboxed container* | [llms-full](https://developers.openai.com/codex/llms-full.txt) |
| `--model`, `-m` | e.g. `gpt-5.5` | override configured model | [cli/reference](https://developers.openai.com/codex/cli/reference) |
| `--config`, `-c` | `key=value` (repeatable) | override any config.toml value on the CLI | [cli/reference](https://developers.openai.com/codex/cli/reference) |
| `--output-last-message`, `-o` | path | write the assistant's **final message** to a file | [cli/reference](https://developers.openai.com/codex/cli/reference) |
| `--cd`, `-C` | path | set workspace root before running | [cli/reference](https://developers.openai.com/codex/cli/reference) |
| `--skip-git-repo-check` | boolean | allow running outside a git repo | [cli/reference](https://developers.openai.com/codex/cli/reference) |

**Resume:** `codex exec resume [SESSION_ID]`; omit the id and pass `--last` to continue the most recent session from the cwd (`--all` to include sessions outside the cwd). ([cli/reference](https://developers.openai.com/codex/cli/reference))

> **Deprecation note:** `codex exec --full-auto` is kept as a *deprecated* compatibility path; the docs steer non-interactive runs to `codex exec --sandbox workspace-write` + an explicit approval policy. ([agent-approvals-security](https://developers.openai.com/codex/agent-approvals-security)) We do **not** use `--full-auto`.

### 3.2 The `--json` event stream (what we parse)

With `--json`, stdout is JSONL. Event types observed in the docs + repo issue tracker: **`thread.started`, `turn.started`, `turn.completed`, `turn.failed`, `item.*` (e.g. `item.completed`), and `error`.** `item.*` payloads cover **agent messages, reasoning, command executions, file changes, MCP tool calls, web searches, and plan updates**. ([cli/features](https://developers.openai.com/codex/cli/features); event-type list cross-checked against [openai/codex#14736](https://github.com/openai/codex/issues/14736), which notes `thread.started`/`turn.started`/`item.completed`/`turn.completed` and that the model name is *not yet* included in the stream.)

> **HONEST CAVEAT — schema is not contractually frozen.** OpenAI ships the JSON event stream behind `--experimental-json` and the per-event field shapes evolve (the model-name issue above is one example; token-usage placement is another moving target). Our parser MUST be **defensive**: match on a discriminator field, tolerate unknown event/item types, and *never fail the run* on a parse miss — fall back to the contract baseline (`Output` from `--output-last-message`, tokens/tool-calls left at zero). The exact field paths for token usage and per-item detail are **pinned at implementation time against the bundled CLI version** by capturing a real `--json` run (the conformance fixture, §9), not guessed from this doc.

### 3.3 Sandbox mechanism (the part that collides with kata-fc)

- `--sandbox read-only` — inspect files; cannot edit or run commands without approval.
- `--sandbox workspace-write` — read anywhere, **edit within the workspace**, run routine local commands inside that boundary; outbound network is **off** unless `sandbox_workspace_write.network_access = true`.
- `--sandbox danger-full-access` — **no filesystem or network boundary** from Codex's side.

OS enforcement: **macOS → Seatbelt**; **Linux/WSL2 → bubblewrap (`bwrap`), which requires "support for unprivileged user namespace creation"**; if `bubblewrap` is absent Codex falls back to a bundled helper that *also* needs unprivileged userns. On restrictive distros you must load an AppArmor profile or set `kernel.apparmor_restrict_unprivileged_userns=0`. ([concepts/sandboxing](https://developers.openai.com/codex/concepts/sandboxing)) **For containerized environments the docs explicitly say to run with `--sandbox danger-full-access` (or `--dangerously-bypass-approvals-and-sandbox`) because the inner Linux sandbox can't function.** ([llms-full](https://developers.openai.com/codex/llms-full.txt)) This is the crux of §7.

### 3.4 Auth + config

- **API key:** Codex reads the key from the env var named by the active provider's `env_key` (default provider `openai` → `OPENAI_API_KEY`). The docs also show `CODEX_API_KEY=<api-key> codex exec …` in automation examples. ([cli/features](https://developers.openai.com/codex/cli/features), [config-reference](https://developers.openai.com/codex/config-reference)) Interactive `codex login` (ChatGPT account) is the *other* auth path — **not usable in a headless pod**; we use API-key auth only.
- **Config file:** `~/.codex/config.toml` (user) or `.codex/config.toml` (project); base dir is `$CODEX_HOME` (default `~/.codex`). ([config-reference](https://developers.openai.com/codex/config-reference))
- **Model / provider keys:** `model`, `model_provider` (default `openai`), and a `[model_providers.<id>]` table with `base_url`, `env_key`, `wire_api` (only `"responses"` supported). `openai_base_url` overrides the built-in provider's base URL. `approval_policy`, `sandbox_mode`, `sandbox_workspace_write.{writable_roots,network_access}` mirror the CLI flags. ([config-reference](https://developers.openai.com/codex/config-reference), [config-sample](https://developers.openai.com/codex/config-sample))

---

## 4. Design

### 4.1 Principles

1. **Reuse, don't fork.** Sandbox pin, egress cage, broker env, Step folding, and the `runCLI` driver are all shared and already correct. The Codex-specific surface is: argv assembly, a JSON-event parser, a rendered `config.toml`, and a session-resume hook.
2. **Honor the Response contract by *raising* it, not bending it.** Today `codex` is correctly at the CLI baseline (zeros). We move it up by parsing the structured stream Codex already emits — populating `Response.TokensIn/Out` and `Response.ToolCalls`, which the executor *already* folds (`executor.go:397-407`). **No executor change needed** — the fields exist (`iface.go:56-58`) and are wired; only the harness must fill them.
3. **Belt-and-suspenders sandboxing.** Codex's inner sandbox is redundant with (and broken under) kata-fc + restricted PSA + the egress NetworkPolicy. We disable the inner sandbox deliberately and document the layering (§7) — the microVM is the boundary, not bubblewrap.
4. **Defensive parsing.** A JSON schema drift degrades gracefully to the baseline; it never fails an otherwise-successful run.

### 4.2 Execution shape

```
AgentRun (mode=harness, harness.kind=codex)
  └─ operator: render run pod
       ├─ image            = harness-codex bundle (harness_image.go:22)        [exists]
       ├─ RuntimeClassName = kata-fc (ApplyRunSandbox, fail-closed)            [exists]
       ├─ NetworkPolicy    = default-deny egress (BuildAgentRunEgressPolicy)   [exists]
       ├─ ConfigMap runspec: agent.json + run.json + provider.json             [exists]
       │                     + codex-config.toml   ◄── NEW (4.4)
       └─ entrypoint       = /agent run --dir=/etc/smol-agents/run             [exists]
              └─ RunOnce → RunTurn → executor → CodexHarness.Run               [exists path]
                   └─ codex exec --json \                                       ◄── NEW argv (4.3)
                        --sandbox danger-full-access \                          (kata is the cage, §7)
                        --ask-for-approval never \
                        --skip-git-repo-check \
                        --output-last-message /tmp/last.txt \
                        -C <workspace> \
                        [resume <session-id>]  "<prompt>"
                   ├─ stdout JSONL ─► parseCodexEvents() ─► Response{           ◄── NEW (4.5)
                   │      Output:    <last assistant msg or /tmp/last.txt>,
                   │      TokensIn/Out: from usage event,
                   │      ToolCalls: from item.* (command/file_change/mcp/...) }
                   └─ executor folds Response → StepFinal (tokens+toolcalls)    [exists]
```

### 4.3 Argv assembly (the new `CodexHarness.Run`)

Default argv (when the new flag seam is unset) becomes:

```
codex exec --json \
  --sandbox danger-full-access \
  --ask-for-approval never \
  --skip-git-repo-check \
  --output-last-message <tmpfile> \
  -C <EffectiveWorkingDir or /tmp> \
  "<instructions + prompt>"
```

Rationale per flag:
- `--json` — turn on the event stream we now parse (§4.5).
- `--sandbox danger-full-access` — **the kata-fc microVM is the sandbox**; Codex's inner bubblewrap can't run under restricted PSA anyway (§7). This is the docs-blessed choice for containerized runs (§3.3).
- `--ask-for-approval never` — non-interactive; there is no human to approve. Safe *only because* of the outer sandbox (§7).
- `--skip-git-repo-check` — the AgentFS workspace may not be a git repo.
- `--output-last-message <tmpfile>` — a **reliable** final-answer source independent of JSONL parsing; the harness reads this file for `Output` and uses the JSONL only for tokens/tool-calls (defensive: if JSON drifts, `Output` still lands).
- `-C <workspace>` — align Codex's workspace root with `EffectiveWorkingDir()` (the AgentFS mount when durable; `harness.go:291-304`). `runCLI` already sets `c.Dir` (`cli.go:42-46`); `-C` makes Codex's own notion of the root match.

### 4.4 `config.toml` rendering

The operator renders a `codex-config.toml` into the runspec ConfigMap and the harness points `CODEX_HOME` at the mount dir. The Agent's model/provider intent maps to TOML:

```toml
# rendered from AgentSpec.Harness.CLI.Codex + the resolved provider
model = "gpt-5.5"                  # from codex.model (or -m flag)
model_provider = "platform"       # when a custom endpoint is set; else "openai"

approval_policy = "never"         # mirrors --ask-for-approval (belt-and-suspenders)
sandbox_mode   = "danger-full-access"

[model_providers.platform]        # only when codex.baseURL set (e.g. the platform gateway)
name     = "smol-agents gateway"
base_url = "http://model-gateway.smol-agents.svc:8080/v1"
env_key  = "OPENAI_API_KEY"       # broker-leased key lands here
wire_api = "responses"
```

> **Why a file and not only `-c key=value`?** The `[model_providers.<id>]` table is a nested block; rendering a file is cleaner and auditable, and matches how the other coding CLIs will carry config. CLI `-c` overrides remain available via the flag seam (§5) for one-offs.

### 4.5 The JSON-event parser (`parseCodexEvents`)

A new, well-tested pure function (`codex.go`, see §5) consumes the JSONL stream and produces the richness fields. It is the only Codex-specific logic of any size.

```
parseCodexEvents(stdout []byte, lastMessageFile string) (output []byte, tokensIn, tokensOut int64, calls []v1.ToolCallRecord)
```

Behavior:
- Scan line-by-line; `json.Unmarshal` each into a `struct{ Type string; ... }` peek, ignore lines that don't parse (Codex may interleave non-JSON on stderr; we read stdout, but be defensive).
- On a **usage**-bearing event (the field path pinned by the conformance fixture, §9 — candidates: `turn.completed.usage`, a dedicated `token_count`/`usage` item), accumulate `tokensIn`/`tokensOut`. Accumulate across turns for a resumed/multi-turn run.
- On `item.completed` (or equivalent) of an **actionable** item kind — command execution, file change, MCP tool call, web search — append a `v1.ToolCallRecord` (`pkg/agentmodel/v1/types.go:305-311`): `Tool` = the item kind/name (e.g. `"shell"`, `"apply_patch"`, `"mcp:<server>.<tool>"`), `Arguments` = the item's input JSON, `Result` = truncated output, `DurationMs` if present. **Plan/reasoning/agent-message items are NOT tool calls** — they are not appended (they'd inflate `Usage.ToolCalls`, which is budget-relevant: `executor.go:398`).
- `Output`: prefer the contents of `lastMessageFile` (the `--output-last-message` file); if absent/empty, fall back to the last `agent_message` item's text from the stream; if both absent, fall back to raw stdout (last resort).
- **Never returns an error.** A totally unparseable stream yields `output=<lastMessageFile or raw stdout>, tokens=0, calls=nil` — exactly today's behavior. Drift degrades to baseline.

> **DESIGN — token accounting is best-effort, like Hermes.** Unlike Hermes (which parses one OpenAI `usage` block, `hermes.go:265-276`), Codex emits usage across a multi-turn stream. We sum it. If the bundled CLI version doesn't emit usage in `--json` (the model-name gap in [#14736](https://github.com/openai/codex/issues/14736) shows fields are still landing), tokens stay 0 and we say so in the harness doc — we do **not** fabricate counts (harness-authoring §4).

---

## 5. Concrete changes

> Targets are `file:line` against v0.2.0. **Proposals are marked.** Anything not marked "exists" is new.

### 5.1 CRD / API types — `pkg/agentmodel/v1/harness.go`

Add a Codex-specific config block, reachable from `HarnessCLISpec`. This also lands the generic CLI flag seam from [harness-authoring.md §8](../design/harness-authoring.md) (proposal) so all coding CLIs share it.

```go
// HarnessCLISpec (extend; harness.go:145-166)
type HarnessCLISpec struct {
    // ... existing PromptFlag, WorkingDir, MaxOutputBytes ...
    // PassthroughEnv stays DEAD — remove in this change (harness-authoring §7/§8).

    // ExtraArgs are appended verbatim to the kind's default argv (before the
    // prompt). Lets a tenant pass CLI-specific flags without discarding the
    // curated defaults via a full command override. (harness-authoring §8)
    // +optional
    ExtraArgs []string `json:"extraArgs,omitempty"`

    // Codex carries codex-specific knobs; ignored for other kinds.
    // +optional
    Codex *HarnessCodexSpec `json:"codex,omitempty"`
}

// HarnessCodexSpec configures harness.kind=codex.
type HarnessCodexSpec struct {
    // Model overrides the model (rendered to config.toml `model` and/or -m).
    // +optional
    Model string `json:"model,omitempty"`

    // BaseURL points Codex at a custom OpenAI-compatible endpoint (e.g. the
    // platform model gateway). When set, the operator renders a
    // [model_providers.platform] block with wire_api="responses" and sets
    // model_provider="platform". Empty => OpenAI default provider.
    // +optional
    BaseURL string `json:"baseURL,omitempty"`

    // Sandbox maps to --sandbox. Default (empty) => "danger-full-access"
    // because the kata-fc microVM is the real boundary (§7). Set to
    // "workspace-write" ONLY if the cluster runs a kernel where Codex's inner
    // bubblewrap can initialize (uncommon under restricted PSA).
    // +kubebuilder:validation:Enum=read-only;workspace-write;danger-full-access
    // +optional
    Sandbox string `json:"sandbox,omitempty"`

    // Approval maps to --ask-for-approval. Default (empty) => "never"
    // (non-interactive). "untrusted"/"on-request" only make sense with a
    // human in the loop and will stall a headless run on escalation.
    // +kubebuilder:validation:Enum=untrusted;on-request;never
    // +optional
    Approval string `json:"approval,omitempty"`

    // NetworkAccess sets sandbox_workspace_write.network_access. Only meaningful
    // when Sandbox="workspace-write". The OUTER egress NetworkPolicy still
    // governs what the pod can actually reach (§7); this is Codex's inner view.
    // +optional
    NetworkAccess bool `json:"networkAccess,omitempty"`
}
```

Validation (`ValidateHarness`, `harness.go:308-343`): no new *required* fields. Add an arm so that `codex.approval` in {`untrusted`,`on-request`} emits a **warning-level** admission note ("interactive approval with a headless run will stall on escalation") — or reject if the platform prefers fail-fast. Enums are enforced by kubebuilder markers on the CRD; mirror them in `ValidateHarness` for the pure path. **CRD regen caveat:** per `MEMORY.md` the operator CRDs are *not* reproducibly generated — hand-edit `operator/config/crd/runtime.agents.smol-agents.ai_agents.yaml` to add `harness.cli.codex.*` + `harness.cli.extraArgs`, and the operator API mirror `operator/api/agentmodel/v1` types, rather than blindly `make manifests`.

### 5.2 Harness implementation — `pkg/agentruntime/harness/`

**New file `codex.go`** holds `parseCodexEvents` (§4.5) + the argv builder. **Rewrite `CodexHarness.Run`** (`cli.go:174-180`):

```go
func (h *CodexHarness) Run(ctx context.Context, req Request) (Response, error) {
    prompt := promptFromInput(req.Input)
    if req.Instructions != "" {
        prompt = req.Instructions + "\n\n" + prompt
    }
    cfg := codexCfgFrom(req.Spec)                 // sandbox/approval defaults (§4.3)
    lastMsg := filepath.Join(os.TempDir(), "codex-last-"+ranToken()+".txt")

    args := []string{"exec", "--json",
        "--sandbox", cfg.sandbox,                 // default danger-full-access
        "--ask-for-approval", cfg.approval,       // default never
        "--skip-git-repo-check",
        "--output-last-message", lastMsg,
    }
    if wd := req.WorkingDir; wd != "" {           // align Codex root with CWD
        args = append(args, "-C", wd)
    }
    if sid := req.SessionID; sid != "" && req.Spec.SessionPolicy == v1.SessionPersistent {
        // resume an existing thread (§4.6); subcommand form: exec resume <id>
        args = []string{"exec", "resume", sid, "--json", /* same flags */}
    }
    args = append(args, req.Spec.CLI.extraArgs()...) // §5.1 seam, before prompt
    args = append(args, prompt)

    // runCLI gives us bounded stdout + budget timeout + ctx cancellation.
    resp, runErr := runCLI(ctx, req, "codex", args, h.Cmd)

    // Raise the Response above the CLI baseline by parsing the JSONL we asked for.
    out, tin, tout, calls := parseCodexEvents(resp.Output, lastMsg)
    resp.Output, resp.TokensIn, resp.TokensOut, resp.ToolCalls = out, tin, tout, calls
    return resp, runErr   // preserve runCLI's error (timeout/cancel/exit) verbatim
}
```

Notes:
- **`runCLI` is reused unchanged** — env merge (`cli.go:108-119`), `MaxOutputBytes` cap (default 1 MiB; bump via `cli.cli.maxOutputBytes` since JSONL is chattier — document this), `c.Dir`, budget timeout, stderr capture into the error.
- `CODEX_HOME` is set by `mergeEnv` overlay: the operator injects `CODEX_HOME=<runspec-mount>/codex` (or the harness sets it relative to a writable `/tmp` copy of the config — see §5.4 read-only-mount caveat).
- **Session id** needs a new `Request.SessionID` field (`iface.go` Request, currently `iface.go:14-...`) threaded from `AgentSession`/`AgentRunSpec`. For the single-run path it's empty; for the durable-session worker (`RunTurn`, `runonce.go:61-84`) it carries the Codex thread id. **This is the one cross-cutting addition** and overlaps [response-richness.md](response-richness.md) + [agentsession-scaling-impl.md](agentsession-scaling-impl.md); coordinate the field there.

### 5.3 Operator wiring — `operator/internal/builders/`

1. **Render `config.toml`** — extend `BuildRunSpecConfigMap` (`runspec.go:49-83`) to add a `codex-config.toml` key when `agent.Spec.Harness.Kind == codex`. New helper `renderCodexConfig(agent, provider) string` (new file `runspec_codex.go`) producing the TOML in §4.4. The provider's in-cluster gateway URL comes from the same `RunProvider` the loop path uses (`runspec.go:38-42`).
2. **Mount + `CODEX_HOME`** — the runspec ConfigMap is already mounted read-only at `RunSpecMountPath=/etc/smol-agents/run` (`runspec.go:23,97-99`). Codex needs `$CODEX_HOME/config.toml`; point `CODEX_HOME` there **or** (read-only caveat, §5.4) have `/agent` copy the rendered TOML into a writable `$HOME/.codex/`. Prefer the copy: Codex writes session state under `CODEX_HOME` and the ConfigMap mount is read-only.
3. **Image** — no change; `HarnessImage` already resolves the bundle (`harness_image.go:22,33-45`).
4. **Sandbox/egress** — no change; `ApplyRunSandbox` + `BuildAgentRunEgressPolicy` already stamp the pod (`run_sandbox.go:45-63`).

### 5.4 Bundle image — `deploy/docker/harness-codex.Dockerfile`

- **Pin the version.** Change `ARG CODEX_VERSION=latest` (`harness-codex.Dockerfile:19`) to a pinned semver and bump deliberately; flag/event-schema drift (§3.2) is a real risk with `latest`.
- **No bubblewrap needed.** Because we run `--sandbox danger-full-access`, do **not** install/enable `bwrap` — it would only fail under restricted PSA anyway (§7). Document this in the Dockerfile comment.
- **Writable `$HOME`.** `ENV HOME=/tmp` already set (`:27`); `/agent` copies the rendered `config.toml` to `/tmp/.codex/config.toml` and exports `CODEX_HOME=/tmp/.codex` before exec.
- Multiarch (amd64+arm64) per `MEMORY.md` — already handled by the shared build (`ARG TARGETARCH`, `:14`).

### 5.5 Tests — `pkg/agentruntime/harness/codex_test.go`

- Unit: `parseCodexEvents` against a **captured real JSONL fixture** (§9) — assert tokens summed, only actionable items become `ToolCallRecord`s, `Output` from the last-message file, and that a corrupt/truncated stream yields baseline (no error).
- Unit: `CodexHarness.Run` argv via the `Cmd` seam (`cli.go:17-22`) — assert exact flags incl. defaults and `extraArgs` ordering, and the resume subcommand when `SessionID` + persistent.

---

## 6. Data / control flow (end-to-end)

```
1. User applies Agent{mode:harness, harness:{kind:codex, cli:{codex:{model,baseURL,...}}}}
   + AgentRun{input:"refactor X", inputs:[files]}.
2. AgentRunReconciler resolves sandbox class (kata-fc, fail-closed) + provider,
   renders runspec ConfigMap = agent.json + run.json + provider.json + codex-config.toml,
   builds the run pod (RuntimeClass pinned, egress NetworkPolicy applied), leases
   OPENAI_API_KEY from the broker into the harness env.
3. Pod starts: /agent run --dir=/etc/smol-agents/run.
   RunOnce → RunTurn: MaterializeInputs writes input files into EffectiveWorkingDir
   (AgentFS mount when durable) → executor.
4. executor → CodexHarness.Run: copies config.toml to $CODEX_HOME, builds argv
   (--json --sandbox danger-full-access --ask-for-approval never -C <wd> ...),
   runCLI execs `codex exec` with broker env merged.
5. Codex runs its OWN plan-act-observe loop inside the microVM, hitting the model
   endpoint (in-cluster gateway or OpenAI) THROUGH the egress NetworkPolicy.
   It streams JSONL to stdout and writes the final answer to the last-message file.
6. runCLI returns bounded stdout + duration (+ any exit error w/ stderr snippet).
   parseCodexEvents lifts Output + tokens + tool-calls out of the stream.
7. executor wraps it in ONE StepFinal carrying tokens+toolCalls (executor.go:404-407);
   ResultToWire → RunResult; cmd/agent writes it to /dev/termination-log
   (clampForTerminationMessage trims to 3072 B — Output truncates first, then
   tool-call payloads, then steps — run.go:96-119) and full to stdout.
8. foldRunResult copies Output/Steps/Usage/Reason into AgentRun.Status
   (agentrun_controller.go ~404). User sees tokens + a tool-call trace, not a blob.
```

**Durable-session variant:** the session worker (`RunTurn`, `runonce.go:61-84`) passes a stable Codex `SessionID`; step 4 becomes `codex exec resume <id>`; the AgentFS workspace + Codex thread both survive across turns.

---

## 7. Security model

This is the subtle part: **Codex ships its own sandbox, and we deliberately turn it off.** Stating why, loudly.

### 7.1 The layering

| Layer | Who enforces | What it stops | Status |
|---|---|---|---|
| **microVM kernel isolation** | kata-fc RuntimeClass (fail-closed; `--default-run-runtime-class=kata-fc`, `resolveSandbox` rejects runc unless `--allow-host-runtime`) | container escape → host kernel | ✅ exists (verified facts #1) |
| **Egress cage** | `BuildAgentRunEgressPolicy` NetworkPolicy: DNS + in-cluster + public 80/443; **169.254/16 (metadata) blocked** | SSRF to cloud metadata, arbitrary outbound exfil | ✅ exists (`run_sandbox.go:55-123`) |
| **Pod hardening** | restricted PSA: non-root (uid 65532), drop-ALL caps, seccomp, no privilege escalation | in-VM privilege gain; **and incidentally breaks Codex's bubblewrap** | ✅ exists (verified facts #1) |
| **Broker secret boundary** | secrets leased as env, never inlined in spec | API key in CRD/etcd/logs | ✅ exists (`cli.go:108-119`; harness-authoring §7) |
| ~~Codex inner sandbox (bubblewrap)~~ | ~~Codex~~ | ~~file/network from Codex's view~~ | ❌ **intentionally disabled** (`--sandbox danger-full-access`) |

### 7.2 Why disable Codex's inner sandbox

Codex's Linux sandbox needs **unprivileged user-namespace creation** for bubblewrap (§3.3). Our run pod is **non-root with drop-ALL capabilities under restricted PSA** — `bwrap` cannot create the userns/mounts it requires, so `--sandbox workspace-write` would error or fall back unpredictably *inside* the microVM. OpenAI's own guidance for "already inside a container" is exactly `--sandbox danger-full-access` / `--yolo` (§3.3, [llms-full](https://developers.openai.com/codex/llms-full.txt)). **The microVM is a strictly stronger boundary than bubblewrap** (separate kernel vs. same-kernel namespaces), so we lose nothing by disabling the inner layer — Codex can do whatever it wants *inside the microVM*, and the microVM + NetworkPolicy bound the blast radius.

> **DESIGN DECISION (state it for the maintainer):** default `codex.sandbox=danger-full-access` + `codex.approval=never`. This means **Codex runs arbitrary commands and edits files unattended** — acceptable *only* because every row above holds on the run datapath. If a deployment runs without kata-fc (`--allow-host-runtime`, the escape hatch in verified facts #1), this default is **dangerous** and the operator must either (a) refuse `codex.sandbox=danger-full-access` when the resolved RuntimeClass is runc, or (b) flip the Codex default to `workspace-write` and accept it may not initialize. **Recommended:** the controller couples the two — if the resolved sandbox is *not* a microVM class, reject `danger-full-access` (fail-closed), mirroring `resolveSandbox`'s posture.

### 7.3 New attack surface vs. mitigations

| Surface | Risk | Mitigation |
|---|---|---|
| Codex hits the model endpoint through egress | exfil via a malicious "model" URL in `config.toml` | `baseURL` is operator/Agent-controlled, not model-controlled; egress NetworkPolicy still bounds reachable hosts; metadata blocked. AgentNetwork allow-list enforcement is **future** ([agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md)) — today egress is the static policy (verified facts #3). |
| `--json` stream / tool-call args land in `AgentRun.Status` | secret echoed by Codex into a command shows in Steps | same exposure as any harness output; `clampForTerminationMessage` elides tool-call arg/result bodies first under the cap (`run.go:96-119`); a `RedactionPolicy` is **NOT applied anywhere** today (verified facts #8) — out of scope, note it. |
| `config.toml` carries `env_key=OPENAI_API_KEY` | key leak | the *name* is in config; the *value* is broker-leased into env at runtime, never written to the TOML (§4.4). |
| `extraArgs` passthrough | tenant re-enables a footgun (e.g. `--yolo` with a weaker outer sandbox) | enum-constrained `sandbox`/`approval` are the supported path; `extraArgs` is raw — document that it is power-user, and that the §7.2 controller coupling still governs the *resolved* RuntimeClass regardless of args. |

---

## 8. Phasing & effort

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **C1 — Non-interactive flags + config.toml** | Rewrite `CodexHarness.Run` argv (`--json --sandbox danger-full-access --ask-for-approval never --skip-git-repo-check --output-last-message -C`); add `HarnessCodexSpec` + `ExtraArgs`; render `codex-config.toml` + `CODEX_HOME` copy; pin Dockerfile version; CRD edits. **`Output` improves (last-message file); tokens/tool-calls still 0.** | **M** | — (self-contained; reuses sandbox/egress/broker) |
| **C2 — JSON event parsing (tokens + tool calls)** | `parseCodexEvents` + conformance fixture; populate `Response.TokensIn/Out/ToolCalls`; unit tests. **Lights up Steps observability with no executor change.** | **M** | C1; pins schema against the bundled CLI (§9) |
| **C3 — Resumable session** | `Request.SessionID`; `codex exec resume <id>` when persistent; thread the id from the session worker. | **S–M** | C1; [agentsession-scaling-impl.md](agentsession-scaling-impl.md) (SessionID plumbing) + [response-richness.md](response-richness.md) (Request field) |
| **C4 — Live e2e on cftest** | Publish pinned bundle; run a real `kind=codex` AgentRun against an OpenAI-compatible endpoint on the single-node k0s box; assert non-empty Output + non-zero tokens + ≥1 tool call in `AgentRun.Status.Steps`. | **M** | C1+C2 (+C3 optional); needs a key for an OpenAI-compatible endpoint |
| **C5 — Sandbox/RuntimeClass coupling guard** | Controller refuses `codex.sandbox=danger-full-access` when resolved RuntimeClass is non-microVM (§7.2). | **S** | C1; shares logic with `resolveSandbox` (`sandbox.go`) |

**Cross-spec dependencies:** the `extraArgs`/per-kind-flag seam is shared with [agent-claude-code.md](agent-claude-code.md) and the harness-authoring §8 proposal — land it once. `Request.SessionID` is shared with [response-richness.md](response-richness.md) and [agentsession-scaling-impl.md](agentsession-scaling-impl.md). The egress/AgentNetwork tightening referenced in §7 is owned by [agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md) — this spec only consumes the existing static cage.

---

## 9. Test plan

### Unit
- **`parseCodexEvents` (the meat).** Drive it from a **captured real `--json` fixture**: run the pinned bundled `codex exec --json` once by hand against a tiny prompt, save the JSONL as `testdata/codex_exec.jsonl`, and assert: (a) tokens summed across `turn.completed`/usage events; (b) only command/file-change/MCP/web-search items become `ToolCallRecord`s (plan/reasoning/agent-message excluded); (c) `Output` taken from the last-message file; (d) a **truncated** copy of the fixture yields baseline `(rawOutput, 0, 0, nil)` with no error. This fixture is the contract anchor against schema drift (§3.2).
- **`CodexHarness.Run` argv** via the `Cmd commandFunc` seam (`cli.go:17-22`): exact default flags; `extraArgs` appended before the prompt; resume subcommand under `SessionID`+persistent; `-C` reflects `WorkingDir`.
- **`renderCodexConfig`**: TOML golden — `[model_providers.platform]` present iff `baseURL` set, `wire_api="responses"`, `env_key` name only (never a value).
- **`ValidateHarness`**: enum rejects bad `sandbox`/`approval`; interactive-approval warning/rejection per §5.1 choice.

### Integration / e2e (cftest single-node k0s, per `MEMORY.md`)
- **Live `kind=codex` run.** Apply Agent (`mode:harness, kind:codex, cli.codex.{model,baseURL=in-cluster gateway}`) + AgentRun; pod schedules with kata-fc + egress policy; assert `AgentRun.Status`: `state=Completed`, non-empty `Output`, `Usage.Tokens>0`, ≥1 `StepFinal.ToolCalls` entry. This is the proof the whole chain (config → flags → JSON parse → fold) holds against the real binary. Mirrors the Hermes live-green bar (`MEMORY.md`).
- **Sandbox-collision regression.** Confirm Codex does **not** attempt bubblewrap (no userns error in logs) under restricted PSA — i.e. `--sandbox danger-full-access` is actually taking effect.
- **Resume (if C3):** two sequential turns share a thread; second turn's prompt references first turn's context and succeeds.
- **Negative:** with `--allow-host-runtime` (runc) + `codex.sandbox=danger-full-access`, the C5 guard rejects admission (or downgrades) — proves the §7.2 coupling.

> **No new test infra needed** — the `Cmd`/`HTTPClient` seams (harness-authoring §2 step 6) and the cftest box already exist; the only new asset is the captured JSONL fixture.

---

## 10. Risks & open decisions

### Risks
1. **`--json` schema drift (highest).** OpenAI ships it as `--experimental-json`; field paths for usage and item detail evolve ([#14736](https://github.com/openai/codex/issues/14736) shows fields still landing). *Mitigation:* defensive parser that degrades to baseline (§4.5), a pinned CLI version (§5.4), and the captured-fixture conformance test (§9). The parser must never fail a run on a parse miss.
2. **`danger-full-access` outside kata-fc.** If a deployment runs `--allow-host-runtime`, the default Codex config is genuinely dangerous (arbitrary commands on a shared kernel). *Mitigation:* the C5 controller coupling (§7.2) — strongly recommended, not optional, for any non-kata deployment.
3. **Token accounting may be 0 on some CLI versions.** If the bundled Codex doesn't emit usage in `--json`, we honestly report 0 (don't fabricate). *Mitigation:* document per-version in the harness note; the fixture test will catch it at bump time.
4. **Read-only ConfigMap mount vs. Codex writing under `CODEX_HOME`.** Codex persists session state under `CODEX_HOME`; the ConfigMap mount is read-only. *Mitigation:* `/agent` copies the rendered `config.toml` into a writable `/tmp/.codex` and points `CODEX_HOME` there (§5.3/§5.4).
5. **JSONL is chattier than plain output** — can hit the 1 MiB `MaxOutputBytes` cap (`cli.go:49-52`) on long runs, truncating the *stream* (and thus tokens/late tool-calls). *Mitigation:* bump the default cap for `kind=codex`, or have the parser tolerate a truncated tail (it already does); `Output` is unaffected because it comes from the last-message file.

### Open decisions for the maintainer
- **D1 — Default sandbox/approval.** Confirm `danger-full-access` + `never` as the defaults (this spec's recommendation, given kata-fc). The alternative — default `workspace-write` — will frequently fail to initialize under restricted PSA. **Recommendation: `danger-full-access`/`never` + the C5 coupling guard.**
- **D2 — Interactive approval admission.** When `codex.approval` is `untrusted`/`on-request` (which *will* stall a headless run on escalation): **reject at admission** or **accept with a warning**? Recommendation: reject (fail-fast) unless a future human-in-the-loop path ([human-in-the-loop.md](human-in-the-loop.md)) can answer the prompt.
- **D3 — `config.toml` vs `-c` only.** Render a file (this spec) vs. push everything through repeatable `-c key=value`. Recommendation: file (auditable, handles the nested provider table), with `extraArgs` `-c` for one-offs.
- **D4 — `SessionID` ownership.** Confirm `Request.SessionID` lands in [response-richness.md](response-richness.md)/[agentsession-scaling-impl.md](agentsession-scaling-impl.md) rather than being Codex-private, so resume is uniform across harnesses (Claude Code has `--resume` too — [agent-claude-code.md](agent-claude-code.md)).
- **D5 — Provider `wire_api`.** Codex only supports `wire_api="responses"` ([config-reference](https://developers.openai.com/codex/config-reference)). Confirm the platform model gateway speaks the OpenAI **Responses** API (not just Chat Completions) when `baseURL` points at it; if it only does Chat Completions, `baseURL` to the gateway won't work and Codex must hit OpenAI directly (or the gateway must add a Responses shim). **This is a hard external constraint — verify before C4.**
