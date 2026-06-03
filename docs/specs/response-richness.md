# Spec: Response Richness — Tool Calls, Real Token/Cost Accounting, and Termination-Message Budgeting

> **Status: DESIGN — proposal, not built.** Authored 2026-06-03 against v0.2.0 source.
> Scope: populate `Response.ToolCalls` and real token/cost counts from harnesses,
> thread them into `Status.Steps`/`Status.Usage`, add a cost field path, and add
> **total-size budgeting** for the ~4 KiB pod-termination-message cap (counts +
> byte-sizes in Status; full detail in pod logs and an optional overflow store).
>
> Extends `docs/design/framework-enhancements.md` (O1 + H1) and
> `docs/design/harness-authoring.md` §4 (the Response richness contract). This
> spec is the **residual** of O1 after the keystone wiring landed (see §2): it
> covers what is *still* empty, not the plumbing that already exists.
>
> Everything marked **(PROPOSAL)** is unbuilt. File:line citations are to v0.2.0.

---

## 1. Summary

The runtime's in-memory model is far richer than what reaches the cluster, but
the *plumbing* that carries it (the O1 "keystone wiring") has already shipped:
`RunResult.Steps` exists, `ResultToWire` copies it, `HarnessRunner.RunHarness`
returns the whole `harness.Response`, `runHarness` folds `resp.ToolCalls` into a
`Step`, `foldRunResult` writes `Status.Steps`, and `cmd/agent/run.go` clamps the
termination message to fit the kubelet cap. What is **still dead** is the *data
that flows through that plumbing*: no harness populates `Response.ToolCalls`,
every CLI harness reports `TokensIn/Out = 0`, there is no cost concept anywhere
in the tree, and the clamp sheds tool-call/step detail with **no summary
preserved** and **no overflow store**. This spec finishes the job: parse
structured CLI output (`claude --output-format json` → `total_cost_usd`/`usage`;
`codex … --json`) and Hermes `/v1/responses` items into `ToolCallRecord` +
`Usage` + a new `CostUSD`, and make the 4 KiB clamp **information-preserving** —
when it sheds bodies it must leave behind counts and byte-sizes, and (optionally)
write the full trace to AgentFS/S3. Outcome: `kubectl get agentrun -o yaml`
shows real tokens, real dollars, and a faithful tool-call trace (or a faithful
*summary* of one), for both harness mode and loop mode.

---

## 2. Current state

### 2.1 What ALREADY landed (the O1 keystone wiring — do NOT rebuild)

`docs/design/framework-enhancements.md` (written 2026-05-29) describes O1 as the
unshipped keystone and asserts `RunResult` "omits Steps entirely". **That is now
stale.** As of v0.2.0 the wire carries Steps and the seam carries the whole
Response:

| Already done | Evidence |
|---|---|
| `RunResult.Steps []v1.Step` exists | `pkg/agentruntime/runonce.go:34` |
| `ResultToWire` copies `res.Steps` | `runonce.go:94` (`Steps: res.Steps`) |
| `HarnessRunner.RunHarness` returns whole `harness.Response` (not a lossy tuple) | `pkg/agentruntime/executor.go:20-24`, `harness_runner.go:27-50` |
| `runHarness` folds `resp.ToolCalls` into the `Final` step + sets `Usage.ToolCalls` | `executor.go:397-408` |
| `foldRunResult` writes `run.Status.Steps` | `operator/internal/controllers/agentmodel/agentrun_controller.go:404` |
| Termination-message **clamp** sheds output → tool-call bodies → steps | `cmd/agent/run.go:102-143` |
| CRD `status.steps[]` + `status.steps[].toolCalls[]` (`preserve-unknown-fields`) schema | `operator/config/crd/runtime.agents.smol-agents.ai_agentruns.yaml:127-150` |
| Loop-mode Steps are richly built and now surface for free | `executor.go:174-307` → `ResultToWire` → fold |

So **loop-mode Steps already fold to the cluster** — the "easy win" O1 promised
is banked. The residual is everything below.

### 2.2 What is STILL dead/empty (this spec's target)

| Gap | Evidence | Consequence |
|---|---|---|
| **No harness populates `Response.ToolCalls`** | Declared `iface.go:65`; folded `executor.go:407`; but `runCLI` (`cli.go:27-79`), `doHTTP` (`http.go:77-125`), and Hermes (`hermes.go:184-189`) all return it empty | `Status.Steps[].toolCalls` is always empty in practice; the fold path is fed nothing |
| **CLI tokens always 0** | `runCLI` parses no usage (`cli.go:62-78`); only Hermes calls `parseUsage` (`hermes.go:183`, `265-276`) | `Status.Usage.Tokens` = 0 for claude-code/codex/aider/goose/generic-cli → token budget is unenforceable for CLI kinds and metrics lie |
| **No cost concept anywhere** | grep: zero `CostUSD`/`cost_usd`/`total_cost` in `pkg/`,`operator/`,`cmd/`; `Usage` has only Steps/Tokens/ToolCalls/WallClockUsed (`budget.go:36-41`) | Cannot answer "what did this run cost?"; `claude --output-format json` already *reports* `total_cost_usd` and we drop it |
| **Hermes only parses the `chat` usage shape** | `parseUsage` reads `usage.prompt_tokens`/`completion_tokens` only (`hermes.go:267-276`); Hermes calls `/v1/chat/completions` (`hermes.go:181-182`), which returns one opaque assistant string with **no** tool-call log | Hermes' ~64-tool internal loop collapses to a single `Final` step with no tool visibility |
| **Clamp is lossy WITHOUT a summary or overflow store** | `clampForTerminationMessage` sets `Output="<truncated…>"`, then strips `tc.Arguments`/`tc.Result`, then drops `Steps` entirely (`run.go:103-114`, `elideStepPayloads:125-143`) | A large trace silently degrades to *nothing* in Status; the only record is the stdout pod log, which is ephemeral and unqueryable. No counts/sizes survive |
| **`Seed` threaded but read by zero harnesses for accounting** | `Request.Seed` set (`harness_runner.go:48`); only Hermes forwards it as a request field (`hermes.go:108-110`); CLI harnesses ignore it | Out of scope here (determinism is `docs/specs/determinism-and-replay.md`), but it confirms the Response contract is under-filled |

### 2.3 The authoritative contract being changed

`pkg/agentruntime/harness/iface.go:44-69` and
`docs/design/harness-authoring.md` §4 both currently state, verbatim,
*"ToolCalls is populated by NO harness today"* and *"TokensIn/TokensOut … 0 for
ALL CLI kinds"*. **This spec rewrites that contract** from "forward-compat /
always zero" to "best-effort, populated when the harness can parse it", and adds
a `CostUSD` field. The doc-comment and harness-authoring §4 must be updated in
lockstep (see §5.6) so the source of truth does not lie in the other direction.

---

## 3. Design

### 3.1 Principle: best-effort enrichment, never a regression

The richness fields are **best-effort**. A harness that cannot parse structure
leaves them zero/empty — exactly today's behavior. We never *require* a harness
to fill them, and we never let a parse failure fail a run that would otherwise
succeed (a mis-parsed usage block must not zero a previously-working token count
nor abort the call). This preserves the green Hermes + z.ai path and every CLI
path, and lets richness arrive kind-by-kind.

### 3.2 The three independent work streams

```
┌─────────────────────────────────────────────────────────────────────┐
│ A. Widen the contract (Response + Usage + ToolCallRecord + CRD)        │
│    Response.CostUSD; Usage.CostUSD; ToolCallRecord.{ArgsBytes,         │
│    ResultBytes} for size accounting. One-time, unblocks B and C.       │
└─────────────────────────────────────────────────────────────────────┘
            │                               │
            ▼                               ▼
┌──────────────────────────┐   ┌────────────────────────────────────────┐
│ B. Harness parsers        │   │ C. Termination-message budgeting        │
│   B1 claude --output-     │   │   C1 information-preserving clamp:        │
│      format json          │   │      preserve counts + byte-sizes        │
│   B2 codex … --json       │   │      (toolCallCount, truncated flag,     │
│   B3 Hermes /v1/responses │   │      droppedBytes) in Status             │
│      → ToolCalls + Usage   │   │   C2 (opt) overflow store: full trace →  │
│      + cost (precedence)   │   │      AgentFS/S3, ref in Status           │
└──────────────────────────┘   └────────────────────────────────────────┘
            │                               │
            └───────────────┬───────────────┘
                            ▼
            executor.runHarness folds Response → Step + Usage
                            ▼
            ResultToWire → RunResult → (clamp) → /dev/termination-log
                            ▼
            foldRunResult → Status.{Steps,Usage(+CostUSD)}
```

A is a small one-time change. B and C are independent and individually
shippable; B can land kind-by-kind; C is pure runtime (`cmd/agent` + a sink),
no harness dependency.

### 3.3 Why the harness seam already suffices for ToolCalls

`runHarness` (`executor.go:403-408`) already copies `resp.ToolCalls` into the
single `Final` step and sets `Usage.ToolCalls = len(resp.ToolCalls)`. So **no
executor change is needed for tool-call propagation** — a harness that fills
`Response.ToolCalls` is immediately visible end-to-end. The work is entirely in
(a) the harness parsers and (b) the contract fields they need (cost). This is
why §3.2's stream B is mostly self-contained per harness.

---

## 4. External output formats (grounding for the parsers)

> These are the *shapes the parsers consume*. They are documented here as the
> parser contract; the canonical per-tool specs verify the live CLI flags:
> `docs/specs/agent-claude-code.md`, `docs/specs/agent-codex.md`,
> `docs/specs/agent-hermes.md`. This spec **depends on** those for the exact
> argv/flags — it does not re-research them.

### 4.1 `claude-code` structured output (consumed by B1)

`claude --print --output-format json` emits a single JSON envelope whose result
object carries a `usage` block and a `total_cost_usd` float. The parser reads
`usage.input_tokens` / `usage.output_tokens` (Anthropic naming, **not** OpenAI's
`prompt/completion_tokens`) and `total_cost_usd`. `--output-format stream-json`
emits NDJSON event lines including `assistant`/`tool_use`/`tool_result` blocks —
the source for `ToolCallRecord`s. The exact flag wiring + image/version pins are
owned by `docs/specs/agent-claude-code.md`.

### 4.2 `codex` structured output (consumed by B2)

`codex exec --json` emits a JSON-lines event stream (one event per line)
including token-usage events and tool/command-execution events. The parser reads
the usage event for `Usage` and maps tool/exec events to `ToolCallRecord`. Codex
cost reporting is less uniform than claude's; treat `CostUSD` as best-effort
(often 0). Exact flags owned by `docs/specs/agent-codex.md`.

### 4.3 Hermes `/v1/responses` (consumed by B3)

The Hermes gateway exposes a Responses-style endpoint that returns an `output[]`
array of items (`message`, `function_call`, `function_call_output`) plus a
`usage` block using `input_tokens`/`output_tokens` (Responses naming). The parser
walks `output[]`, concatenates message items into `Output`, and pairs each
`function_call` with its matching `function_call_output` into a
`ToolCallRecord`. **The non-streaming `/v1/chat/completions` body does NOT carry
a tool-call log** (verified design note in `framework-enhancements.md:292`), so
Hermes tool visibility requires the Responses path; until then Hermes
`ToolCalls` stays empty. Endpoint availability + the `API=chat|responses`
selector are owned by `docs/specs/agent-hermes.md`; this spec consumes the
parser contract only.

---

## 5. Concrete changes

### 5.1 Contract types (stream A)

**`pkg/agentruntime/harness/iface.go`** — add cost to `Response` (after
`TokensOut`, `iface.go:61-62`):

```go
// CostUSD is the harness-reported dollar cost of this call, when the backend
// reports one (e.g. claude --output-format json total_cost_usd). Best-effort;
// 0 when unknown. NEVER computed by the platform — only surfaced when the
// harness/provider states it.
CostUSD float64
```

Rewrite the `RESPONSE RICHNESS CONTRACT` block (`iface.go:46-55`) to state:
ToolCalls/TokensIn/TokensOut/CostUSD are **best-effort, populated when the
harness can parse them** (claude-code/codex via `--json`; Hermes via
`/v1/responses`), zero/empty otherwise. Drop the absolute "populated by NO
harness" / "0 for ALL CLI kinds" language.

**`pkg/agentmodel/v1/budget.go`** — add cost to `Usage` (`budget.go:36-41`):

```go
// CostUSD is the cumulative dollar cost reported by the backend(s) for this
// run, summed across steps. 0 when no backend reported a cost. It is an
// observability field, NOT a budget axis (Budget has no cost cap in v0.2.0).
CostUSD float64 `json:"costUSD,omitempty"`
```

> **Decision:** cost is **observability-only**, not a `Budget` axis. A cost cap
> needs per-provider pricing the platform does not have; harness-reported cost
> is authoritative-but-sparse. A `Budget.MaxCostUSD` is deferred to
> `docs/specs/run-governance.md`. Keep `AllowsStep` (`budget.go:67-81`)
> unchanged.

**`pkg/agentmodel/v1/types.go`** — add size accounting to `ToolCallRecord`
(`types.go:305-311`), so the clamp can shed bodies but keep sizes:

```go
// ArgsBytes / ResultBytes are the byte lengths of Arguments / Result as
// originally produced, retained when the clamp elides the bodies for the
// termination-message cap (so a reader still sees "a 40 KiB result happened").
// 0 when the body is present (sizes are derivable) or absent.
ArgsBytes   int64 `json:"argsBytes,omitempty"`
ResultBytes int64 `json:"resultBytes,omitempty"`
```

**`Step`** (`types.go:281-290`) — no new fields; `TokensIn/Out` already exist.

### 5.2 RunResult overflow summary (streams A + C)

**`pkg/agentruntime/runonce.go`** — add a trace-summary block to `RunResult`
(`runonce.go:27-38`) that survives the clamp:

```go
// Trace summarizes the (possibly elided) Steps so the controller's compact
// view still reports shape when the cap forces detail out. Always small.
Trace *TraceSummary `json:"trace,omitempty"`

type TraceSummary struct {
    StepCount     int    `json:"stepCount"`
    ToolCallCount int    `json:"toolCallCount"`
    Truncated     bool   `json:"truncated"`     // clamp shed step/tool-call detail
    DroppedBytes  int64  `json:"droppedBytes"`  // approx bytes of detail elided
    OverflowRef   string `json:"overflowRef,omitempty"` // s3:// or agentfs:// ref to full trace (C2)
}
```

`ResultToWire` (`runonce.go:88-103`) computes `Trace.{StepCount,ToolCallCount}`
from `res.Steps` before any clamp (counts are always cheap and truthful).

### 5.3 Harness parsers (stream B)

New file **`pkg/agentruntime/harness/parse_claude.go`** (B1):
`parseClaudeJSON(stdout []byte) (output []byte, usage usageParse, tcs []v1.ToolCallRecord)`.
Reads the `--output-format json` envelope: `usage.input_tokens` →
`Response.TokensIn`, `usage.output_tokens` → `TokensOut`, `total_cost_usd` →
`Response.CostUSD`, message text → `Output`. For `stream-json`, fold
`tool_use`/`tool_result` lines into `ToolCallRecord{Tool, Arguments, Result,
DurationMs}`.

`ClaudeCodeHarness.Run` (`cli.go:154-165`) gains: when
`req.Spec.CLI` requests JSON output (a new `OutputFormat` field, §5.5) or by
default appends `--output-format json`, call `parseClaudeJSON(out.Bytes())`
instead of returning raw bytes. **Default decision in §10.**

New file **`pkg/agentruntime/harness/parse_codex.go`** (B2):
`parseCodexJSONL(stdout []byte) (...)`. Same shape, line-delimited; maps
exec/tool events to `ToolCallRecord`; usage from the usage event; cost
best-effort. `CodexHarness.Run` (`cli.go:174-180`) appends `--json` and parses.

> **Refactor note:** `runCLI` (`cli.go:27-79`) currently *constructs* the
> `Response`. To let a kind post-process stdout, either (a) have `runCLI` return
> the raw `*capWriter` bytes + duration and let each `Run` build the `Response`
> (cleanest), or (b) add an optional `parse func([]byte) Response` parameter to
> `runCLI`. Prefer (a): it keeps the bounded-capture + ctx + stderr logic shared
> while moving Response construction to the kind. Tokens/cost/tool parsing then
> lives per-kind, matching `docs/design/harness-authoring.md` §3 ("a subprocess
> is an opaque text oracle — *unless the kind parses it*").

**Hermes Responses parser** (B3) — owned jointly with
`docs/specs/agent-hermes.md` (the `API` selector + endpoint gating live there).
This spec supplies the parser contract:

New `parseResponsesOutput(body []byte) (output []byte, tcs []v1.ToolCallRecord)`
in `hermes.go` and a widened **`parseUsage`** that reads **both** shapes with
explicit precedence so a `0` in one never zeroes the other:

```go
// parseUsage reads OpenAI usage in BOTH chat shape (prompt_tokens/
// completion_tokens) and responses shape (input_tokens/output_tokens),
// preferring whichever is non-zero. A run that gets one shape must never be
// zeroed by the other being absent — token budget is a safety invariant.
func parseUsage(body []byte) (in, out int64, costUSD float64) { ... }
```

Hermes `Response` (`hermes.go:184-189`) then sets `ToolCalls: tcs` and
`CostUSD: costUSD` when `API=responses`.

### 5.4 Information-preserving clamp (stream C1)

**`cmd/agent/run.go`** — `clampForTerminationMessage` (`run.go:102-143`) must,
**before** it elides, stamp size metadata so the elision is visible:

- `elideStepPayloads` (`run.go:125-143`): before setting `tc.Arguments=nil`,
  set `tc.ArgsBytes = int64(len(tc.Arguments))`; same for `Result`/`ResultBytes`.
  (Today it nils the bodies and loses the fact entirely.)
- When `wire.Steps = nil` is taken (`run.go:113`), set
  `wire.Trace.Truncated = true` and `wire.Trace.DroppedBytes` to the marshaled
  size of the dropped steps, and **keep `wire.Trace`** (it's tiny and survives).
- Keep the existing order (output → tool bodies → steps); it already sheds
  least-value-first per `framework-enhancements.md:45`.

Result: even a fully-clamped run reports `usage` (incl. `costUSD`), phase,
reason, **and** `trace.{stepCount,toolCallCount,truncated,droppedBytes}` in
Status. `terminationMessageBudget` (`run.go:94`, 3072) is unchanged; `Trace` is
budgeted in by being part of the marshaled `RunResult` the `termMessageFits`
check (`run.go:117-120`) already measures.

### 5.5 New CRD/spec fields

**`HarnessCLISpec`** (`pkg/agentmodel/v1/harness.go:144-166`) — opt-in JSON
output (so a tenant can keep raw text if a CLI's JSON mode is flaky):

```go
// OutputFormat requests structured output from CLIs that support it
// (claude-code: "json"/"stream-json"; codex: "json"). "" or "text" = raw
// stdout (today's behavior). When set to a JSON mode, the harness parses
// tokens/cost/tool-calls from stdout into the Response.
// +kubebuilder:validation:Enum=text;json;stream-json
// +optional
OutputFormat string `json:"outputFormat,omitempty"`
```

CRD: `spec.harness.cli.outputFormat` (enum) added to the agents CRD harness
block. **Decision on default in §10** (default `text` = no behavior change vs
default `json` = richness on by default).

### 5.6 CRD status edits

`operator/config/crd/runtime.agents.smol-agents.ai_agentruns.yaml`:

- **`status.usage`** (lines 119-126) is a **closed** object (explicit
  `properties`, no `preserve-unknown-fields`), so an unknown `costUSD` is
  **pruned**. Add:
  ```yaml
  costUSD: { type: number, description: 'Backend-reported dollar cost, when known (0 otherwise).' }
  ```
- **`status.steps[].toolCalls[]`** (lines 143-150) is `preserve-unknown-fields`
  → `argsBytes`/`resultBytes` need **no** CRD edit.
- **`status.trace`** (new) — add a small object:
  ```yaml
  trace:
    type: object
    description: Summary of the (possibly clamp-elided) step trace.
    properties:
      stepCount:     { type: integer }
      toolCallCount: { type: integer }
      truncated:     { type: boolean, description: 'Step/tool-call detail elided for the termination-message cap; see overflowRef or pod logs.' }
      droppedBytes:  { type: integer, format: int64 }
      overflowRef:   { type: string, description: 's3:// or agentfs:// ref to the full trace (when an overflow store is configured).' }
  ```
- Add `RunStatus.Trace *TraceSummary` to `pkg/agentmodel/v1/types.go`
  (`RunStatus`, `types.go:314-322`) and have `foldRunResult`
  (`agentrun_controller.go:398-415`) set `run.Status.Trace = rr.Trace`.

> **Codegen:** `Usage`, `Step`, `ToolCallRecord`, `RunStatus` all have generated
> DeepCopy (`pkg/agentmodel/v1/zz_generated.deepcopy.go:961,1036,890,1136`).
> `Usage.CostUSD` (scalar) and `ToolCallRecord.{ArgsBytes,ResultBytes}` (scalar)
> need no DeepCopy change; `RunStatus.Trace *TraceSummary` (pointer) **does** —
> run `make -C operator deepcopy` after adding `TraceSummary` and regenerate the
> mirror in `operator/api/agentmodel/v1`. The pure↔operator `RunStatus` mirror
> means `run.Status.Trace = rr.Trace` is a direct assignment (no bridge), same
> as Steps today.

> **CRD-generation caveat (project memory):** `operator/config/crd` is **not**
> reproducibly regenerated from Go (see MEMORY `crd_generation_drift`). Hand-edit
> the YAML above; do **not** blindly `make manifests`.

### 5.7 Overflow store (stream C2, optional/phased)

When the clamp would drop step detail and an overflow sink is configured, write
the full untrimmed `RunResult` JSON to object storage and put the ref in
`Trace.OverflowRef`. Reuse the existing `pkg/agentfs` S3 seam — the production
driver is `pkg/agentfs.AWSS3` with `Put(key string, body io.Reader, meta
PutMeta) (Version, error)` (`pkg/agentfs/types.go:56-58`, impl
`s3_aws.go:94`), and `FakeS3` (`fakes.go:85`) for tests. The run already may
have AgentFS backup creds; gate C2 on their presence so it is a no-op otherwise.
Key shape: `runs/<namespace>/<run>/trace.json`. **C2 is deferred** behind C1 —
C1 alone (counts + sizes + truncated flag) closes the "silent zeroing" hole;
C2 adds full-fidelity recovery for audit.

---

## 6. Data / control flow (end to end)

```
AgentRun(input)  →  pod runs `agent run`  →  RunOnce → RunTurn → Executor.Run
  Mode=harness:
    runHarness → HarnessRunner.RunHarness → Harness.Run
      claude-code: claude --print --output-format json
                   → parseClaudeJSON → Response{Output, TokensIn/Out, CostUSD, ToolCalls}
      codex:       codex exec --json
                   → parseCodexJSONL → Response{Output, TokensIn/Out, CostUSD?, ToolCalls}
      hermes:      POST /v1/responses
                   → parseResponsesOutput + parseUsage(responses shape)
                   → Response{Output, TokensIn/Out, CostUSD, ToolCalls}
    ← Response
    runHarness folds → Step{Final, TokensIn/Out, ToolCalls}
                     + Usage{Steps:1, Tokens, ToolCalls:len, CostUSD, WallClockUsed}
  Mode=loop:
    Executor loop already builds Steps + Usage (executor.go:174-307) — unchanged;
    CostUSD stays 0 (loop LLM client reports no cost in v0.2.0).
  → Result
ResultToWire → RunResult{Phase, Output, Steps, Usage(+CostUSD), Trace{counts}}
clampForTerminationMessage:
  if too big → stamp ArgsBytes/ResultBytes, elide bodies; if still too big →
  drop Steps but KEEP Trace{truncated:true, droppedBytes}; (C2) write full
  RunResult to S3, set Trace.OverflowRef
→ /dev/termination-log (≤3072B)  +  full RunResult → stdout (pod log)
foldRunResult → Status.{Output, Steps, Usage(+CostUSD), Trace}
→ kubectl get agentrun -o yaml shows tokens, $, tool calls (or a faithful summary)
```

**Budget interaction:** harness-reported `TokensIn+TokensOut` now feed
`Usage.Tokens` for CLI kinds too, so the existing post-hoc token cap in
`runHarness` (`executor.go:420-427`) finally has real numbers to enforce against
for claude-code/codex (today it's a no-op because tokens are 0). `CostUSD` does
**not** enter `AllowsStep` (§5.1 decision).

---

## 7. Security model

How this composes with the existing posture (kata-fc sandbox + static
default-deny egress + broker + SPIFFE — see `docs/features/runtime-and-identity.md`,
`docs/research/agent-runtime-fit-analysis-v0.2.0.md`):

- **No new network surface in the common path.** B1/B2 add **CLI flags only**
  (`--output-format json`, `--json`) to subprocesses already running inside the
  kata-fc microVM under the static egress policy — no new egress, no new sidecar.
- **B3 (Hermes Responses) reuses the existing gateway HTTP path** and its
  `ImagePolicy` SSRF screen (`hermes.go:84-87`, `images.go`); `/v1/responses`
  hits the same `spec.http.url` host. No new dial target.
- **Tool-call/result bodies in Status are a data-exfil & PII surface.** Status is
  broadly readable (anyone with `get agentrun`). `ToolCallRecord.Arguments`/
  `Result` may contain secrets the agent touched. **Mitigations:** (1) the
  clamp's byte-eliding already strips most body content for large traces; (2) the
  overflow store (C2) keeps full detail in object storage gated by AgentFS creds,
  not in the broadly-readable Status; (3) `RedactionPolicy` exists in the type
  (`types.go:368-370`) but is **applied nowhere** — this spec does **not** build
  redaction (tracked separately; folding raw tool bodies is no worse than today's
  unredacted `Status.Output`), but it is the right home for it once built. Call
  this out as residual risk, do not silently widen it.
- **Cost is harness-asserted, not platform-trusted.** `CostUSD` comes from the
  CLI/gateway output; a compromised/buggy backend can under- or over-report it.
  Because cost is **not** a budget axis, a forged cost cannot bypass an
  enforcement gate — it only mis-reports observability. Document it as advisory.
- **Overflow store keys are tenant-scoped** (`runs/<ns>/<run>/…`) and inherit the
  bucket's SSE (`AWSS3Config.SSEAlgorithm`, `s3_aws.go`); they MUST NOT be
  world-readable. The ref in Status is a pointer, not the bytes.
- **Parse robustness = a DoS/availability surface.** A malformed/huge
  `--json`/Responses body must not OOM the run pod or wedge the parser. Reuse the
  bounded capture (`capWriter`, `cli.go:83-101`, default 1 MiB) and bound the
  Responses body read; a parse error degrades to raw `Output` + zero
  tokens/cost, never a panic or hang (composes with the wallclock budget that
  bounds the whole call).

---

## 8. Phasing & effort

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **A. Contract widening** | `Response.CostUSD`; `Usage.CostUSD` + CRD `status.usage.costUSD`; `ToolCallRecord.{ArgsBytes,ResultBytes}`; `RunResult.Trace`/`TraceSummary` + CRD `status.trace` + fold; rewrite `iface.go` contract block + `harness-authoring.md` §4; deepcopy regen | **S** | — |
| **C1. Information-preserving clamp** | stamp `ArgsBytes/ResultBytes` + populate `Trace` in `clampForTerminationMessage`; the "easy, dependency-free" durability win | **S–M** | A |
| **B1. claude-code parser** | `parse_claude.go`; `HarnessCLISpec.OutputFormat`; refactor `runCLI` to per-kind Response build; tokens+cost+tool-calls | **M** | A; `docs/specs/agent-claude-code.md` (flags/image) |
| **B2. codex parser** | `parse_codex.go`; JSONL events → Usage + ToolCalls | **M** | A, B1 (shared `runCLI` refactor); `docs/specs/agent-codex.md` |
| **B3. Hermes Responses parser** | `parseResponsesOutput` + dual-shape `parseUsage` + `CostUSD` | **M** | A; `docs/specs/agent-hermes.md` (owns `API` selector + endpoint gating) |
| **C2. Overflow store** | full `RunResult` → AgentFS/S3 on clamp; `Trace.OverflowRef` | **M** | A, C1; AgentFS S3 creds present |

Total ≈ **L** across phases. **Ship order:** A → C1 (banks durability with zero
external dependency) → B1 (highest-value harness, claude already reports cost) →
B2/B3 in parallel → C2 last.

**Cross-spec dependencies:** the per-harness *flags/images/endpoints* are owned
by the agent specs (`agent-claude-code`, `agent-codex`, `agent-hermes`); this
spec owns the *Response contract + parsers + clamp/budgeting* they plug into.
Conversely those three specs **depend on** this one for the enriched `Response`
shape. Loop-mode tool richness (real `ToolCallRecord`s from actual invokers) is
gated on `docs/specs/loop-mode-tools-and-invokers.md` — until invokers exist,
loop Steps carry tool calls only for rejected/observation paths the executor
already records.

---

## 9. Test plan

### Unit (table-driven, no cluster)

- **Parsers** (the load-bearing risk — silent zeroing of a safety invariant):
  - `parseClaudeJSON`: golden `--output-format json` envelope → asserts
    `TokensIn`/`TokensOut` from `input_tokens`/`output_tokens` (NOT
    `prompt/completion`), `CostUSD` from `total_cost_usd`, `Output` text; a
    `stream-json` fixture → `ToolCallRecord`s; a malformed body → raw `Output` +
    zeros (no error, no panic).
  - `parseCodexJSONL`: usage event → Usage; exec/tool events → ToolCalls; partial
    last line tolerated.
  - `parseUsage` (Hermes): **chat-only** body, **responses-only** body, and a
    body with both — assert the non-zero shape wins and a `0` in one never zeroes
    the other (direct regression test for the `framework-enhancements.md:64`
    precedence hazard).
  - `parseResponsesOutput`: `output[]` with interleaved `message` +
    `function_call`/`function_call_output` → correct `Output` concat + paired
    `ToolCallRecord`s.
- **Harness `Run` via the existing seams**: `ClaudeCodeHarness`/`CodexHarness`
  with a fake `Cmd commandFunc` (`cli.go:17-22`) returning a JSON fixture →
  asserts the `Response` is enriched and argv includes the format flag;
  `OutputFormat:"text"` → raw bytes, zero enrichment (no-regression).
- **Clamp** (`cmd/agent/run.go`): a `RunResult` with N steps × big tool bodies →
  assert termination message ≤ 3072B AND `Trace.{stepCount,toolCallCount}` exact,
  `ArgsBytes/ResultBytes` populated when bodies elided, `Trace.Truncated=true` +
  `DroppedBytes>0` when steps dropped. A small result → `Trace.Truncated=false`,
  steps intact.
- **`runHarness` fold** (`executor.go`): a `Response{CostUSD, ToolCalls}` →
  `Usage.CostUSD` set, `Usage.ToolCalls=len`, `Step.ToolCalls` carried.
- **`foldRunResult`** (`agentrun_controller_test.go`): a termination message with
  `usage.costUSD` + `trace` → `Status.Usage.CostUSD` + `Status.Trace` set.
- **Overflow store (C2)**: `FakeS3` (`fakes.go:85`) → assert `Put` called with
  `runs/<ns>/<run>/trace.json` and `Trace.OverflowRef` set; absent creds → no
  Put, no ref.

### E2E (the cftest single-node k0s box — see project memory `hermes_zai_e2e_proven`)

- **CLI richness live:** a claude-code AgentRun on the live z.ai-backed harness
  image with `outputFormat:json` → `kubectl get agentrun -o yaml` shows
  `status.usage.tokens > 0` and `status.usage.costUSD > 0` (today both 0 for
  CLI). This is the headline acceptance check.
- **Clamp durability:** force a large tool trace; assert Status retains
  `usage`/`trace` (not zeroed) and `trace.truncated=true`, with full detail in
  `kubectl logs` (and `overflowRef` if C2 enabled).
- **Hermes Responses (when `agent-hermes` lands `API=responses`):** assert
  `status.steps[].toolCalls` is non-empty and tokens come from the responses
  usage shape. Until then, assert the chat path is byte-identical (no
  regression).

---

## 10. Risks & open decisions

**Open decisions (maintainer must choose):**

1. **`HarnessCLISpec.OutputFormat` default — `text` vs `json`.** `text` = zero
   behavior change, richness is opt-in (safe, but most users never get tokens/
   cost). `json` = richness by default, but depends on each CLI's JSON mode being
   stable across versions and risks breaking a user who greps raw stdout from
   `Status.Output`. **Recommendation:** default `text` for generic, but have the
   *claude-code* and *codex* kinds default to their JSON mode (claude's is
   mature and reports cost) — i.e. default is kind-specific, overridable. Confirm
   with the per-harness spec owners.
2. **Cost as a budget axis — now or never.** This spec deliberately makes
   `CostUSD` observability-only. If a `Budget.MaxCostUSD` is wanted, it belongs
   in `docs/specs/run-governance.md` and needs a pricing source for kinds that
   don't self-report. Decide whether to reserve the field now.
3. **C2 overflow store — AgentFS/S3 vs a dedicated bucket.** Reusing AgentFS
   creds is cheap but couples trace-overflow to durable-storage being configured.
   A platform-level "trace bucket" is cleaner but is new infra. **Recommendation:**
   start with AgentFS reuse (no-op when absent); revisit if traces matter for
   ephemeral agents that have no AgentFS.

**Honest unknowns / risks:**

- **CLI JSON output stability.** `claude`/`codex` JSON schemas move between
  versions (training cutoff Jan 2026; these tools ship weekly). The parser must
  be defensive: unknown fields ignored, missing usage → zeros, never fatal. The
  exact current schema is owned/verified by `agent-claude-code` / `agent-codex`
  and MUST be pinned to the bundle image version there.
- **Mis-parse silently zeroing the token budget** is the single highest
  correctness risk (token cap is a Quint safety invariant). The dual-shape
  `parseUsage` precedence test (§9) and "parse error → raw Output + zeros, run
  still succeeds" rule are the guards. A *wrong-but-nonzero* parse is worse than a
  zero (it could let a run exceed `MaxTokens` post-hoc) — prefer conservative
  parsing that zeros on ambiguity.
- **`framework-enhancements.md` is stale on O1's status** (claims Steps unwired;
  they're wired). That doc should get a one-line "O1 keystone landed in v0.2.0;
  residual tracked in `docs/specs/response-richness.md`" note so the design
  record stays honest — but editing it is out of this spec's scope unless the
  maintainer wants it folded in.
- **Per-step token attribution remains fictional for harnesses.** A harness
  reports *aggregate* usage; the single `Final` step carries the total. Do not
  claim per-step fidelity for harness mode (`framework-enhancements.md:72`).
- **Redaction is unbuilt and this spec widens the unredacted surface.** Folding
  tool-call bodies into Status is no worse than today's raw `Status.Output`, but
  it is more of it. If redaction lands first, gate body-folding behind it.
