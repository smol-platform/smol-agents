# smol-agents Framework Enhancement: Design & Roadmap

> Status as of 2026-05-29 (HEAD `0f64158`). Scope: enhancing the agent framework along the four axes the maintainer asked for — (1) more Hermes Agent features, (2) files, (3) agent-to-agent, (4) other helpful capabilities. Every claim below was verified against the tree; effort/impact/risk reflect adversarial review, not the optimistic first pass.

---

## 1. Executive summary

### Where the framework is today

The platform has **two execution modes** that are not equally mature:

- **`Mode=harness`** is the battle-tested, e2e-green path (per project memory: *Hermes + z.ai GREEN*). A harness is a **single bounded call** — subprocess or HTTP — that the executor treats as an opaque oracle. `Executor.runHarness` (`pkg/agentruntime/executor.go:376`) makes exactly ONE call, folds the answer into exactly one `Step{Kind=Final}`, applies the token + wallclock budget caps, and returns. The plan-act-observe loop and all tool execution live *inside* the harness (e.g. the Hermes gateway), invisible to us. The Hermes harness (`pkg/agentruntime/harness/hermes.go`) is the most fully-wired: OpenAI-compatible `/v1/chat/completions`, real token accounting, `BODY_`/`HEADER_` env conventions, and — the single most subtle correctness detail in the layer — explicit `X-Hermes-Session-Id` management (ephemeral runs mint a fresh random id; persistent runs forward a stable one) precisely because the gateway is **not** stateless.

- **`Mode=loop`** (the native plan-act-observe `Executor`) is solid, deterministic, and Quint-mirrored *in-process* — but it is **chat-only in production**. `RunOnce` (`pkg/agentruntime/runonce.go:49-53`) constructs the executor with **empty `Tools` and empty `Invokers` maps** and the operator never ships tool definitions into the pod (`operator/internal/builders/runspec.go`). So any tool call from a loop agent is rejected at runtime. The full invoker machinery is exercised only by tests.

### The architecture's central asymmetry (and the biggest leverage point)

The model is **far richer than the wire**. The in-process executor builds a complete `[]v1.Step` with `ToolCallRecord` audit, per-step tokens, and a 6-value `StepKind` taxonomy — and the CRD already has a `status.steps[]` schema for it (`operator/config/crd/...agentruns.yaml:80-96`). But the **only** runtime→controller wire type, `RunResult` (`runonce.go:24-30`), **omits Steps entirely** (verified: `ResultToWire` at `runonce.go:58-72` never copies them). The controller's `foldRunResult` folds Output/Usage/TerminationReason/Phase and never sets `Status.Steps`. So the cluster-facing surface is **lossy by design today**: Steps, artifacts, schema-conformance, and tool-call structure all drop out before they reach `kubectl`.

This means the **highest-ROI work is wiring, not building**. A surprising amount of the requested functionality already exists as *dead-but-present scaffolding* (confirmed by grep, not inference):

| Scaffolded-but-unused | Evidence | What it unlocks |
|---|---|---|
| `Response.ToolCalls []v1.ToolCallRecord` | `harness/iface.go:56` — declared, populated by **zero** harnesses | Tool-call visibility |
| `RunResult` has no `Steps` field; `Status.Steps` never written outside tests | `runonce.go:24-30`, `foldRunResult` | Step observability |
| `Request.Seed` | threaded through `RunHarness`, read by **zero** harnesses | Determinism |
| `Response.DurationMs` | computed by every harness, then `_ = durMs` (`executor.go:404`) | Latency metrics |
| `HarnessCLISpec.PassthroughEnv` | type + deepcopy only, no reader | Host-env passthrough |
| `HarnessHTTPSpec.Auth *AuthRef` | inert; auth is exclusively `HEADER_Authorization` | — |
| `AgentSession` CRD + `AgentRunSpec.SessionRef` | registered, **no reconciler**, `SessionRef` read by nobody | Cross-run / multi-turn / A2A aggregation |
| `Phase=RequiresAction` | in enum + transition table + Quint + CRD enum, **emitted nowhere** | Human-in-the-loop gates |
| `runtime/contract.go` step-wise protocol | `StepRequest`/`StepResponse`/`RunRef` defined, used only in tests | Durable resumable runs |
| `ToolKind=agent` + `AgentTargetSpec` | types + validation + deepcopy, **no invoker** | Agent-to-agent |
| `RedactionPolicy` | type + deepcopy only, applied **nowhere** | (a dependency, not a feature) |

### The three biggest leverage points

1. **Wire Steps + tool calls end-to-end** (Hermes Responses API → `Response.ToolCalls` → `RunResult.Steps` → `Status.steps[]`). This single thread of plumbing unlocks observability for *both* the harness path and loop mode, and is a prerequisite for A2A run-tree visibility, human-in-the-loop, and replay.
2. **Auto-bind the harness CWD to the AgentFS mount** (`executor.go:393`). A one-line fix to a documented-but-unimplemented contract: durable storage currently does **nothing** for the default config because the harness runs in `/tmp`, not the mounted volume.
3. **Files in and out** — there is *no* path from `AgentRun.Input` to a file on disk, and *no* path from harness output back as a file/artifact. Closing this turns file-oriented agents (claude-code, aider, Hermes terminal/file toolsets) from crippled to first-class.

### Honest caveats that shape everything

- **The 4KiB termination-message cap is a hard constraint and a footgun.** The controller reads the run result *only* from `/dev/termination-log` (`cmd/agent/run.go:65`), and that file must hold the whole marshaled `RunResult` under ~4KiB. Today only `Output` is truncated (`run.go:62`). **Any** proposal that adds bytes to `RunResult` (Steps, artifacts, pending-action) MUST add total-size budgeting in `run.go`, or it silently truncates mid-JSON, `foldRunResult` fails to unmarshal, and the run regresses to empty Output/Usage/Phase — i.e. it breaks the proven path.
- **Several Hermes "features" are server-side and not API-controllable.** `reasoning_effort`, tool/toolset enablement, memory, skills, delegation, and MoA are all gateway `config.yaml` settings, *not* request parameters. We can transmit `BODY_reasoning_effort` but the stock gateway ignores it. The honest move is to stop overselling what the harness controls.
- **External endpoints are unverified from this repo.** `/v1/responses`, `/v1/runs`, `/v1/capabilities`, `delegate_task`, `mixture_of_agents` come from Hermes docs/source, not our code. The proven path is `/v1/chat/completions`. Anything built on the newer endpoints must **feature-detect and fail safe**, and there is **no operator-managed Hermes gateway** in the codebase — the gateway is always external (URL-only), so any "render `config.yaml`" lever has nothing to attach to.

---

## 2. Proposals by dimension

Effort scale: **XS** < **S** < **M** < **L** < **XL**. Each entry leads with the *corrected* effort from adversarial review, not the proposer's estimate.

### 2A. Hermes Agent features

#### H1. Surface Hermes tool-call structure via the Responses API (`/v1/responses`) — **effort L, impact HIGH**

**Problem.** `hermes.go` calls only `/v1/chat/completions`, which returns one opaque assistant string. The gateway runs its own loop with ~64 built-in tools, but all of it collapses into a single `Final` step: `Response.ToolCalls` (`iface.go:56`) is never populated and `runHarness` hard-codes `Usage.ToolCalls:0` (`executor.go:396,399`). `/v1/responses` natively emits the agent's own `function_call`/`function_call_output` items.

**Design sketch.**
- **CRD/Go type:** add `HarnessHTTPSpec.API string` (enum `chat` default | `responses`) in `pkg/agentmodel/v1/harness.go`; `ValidateHarness` accepts it for `kind=hermes`.
- **Harness:** when `API=responses`, POST `{model, instructions, input, store:false}` to the gateway base; add `parseResponsesOutput(body)` that walks the `output[]` array — concatenating message items into `Output`, mapping each `function_call`+`function_call_output` pair into a `v1.ToolCallRecord`. Extend `parseUsage` to read **both** `prompt/completion_tokens` (chat) and `input/output_tokens` (responses) — with explicit precedence so a `0` in one shape never zeroes the other.
- **Wire surgery (the real work):** change `HarnessRunner.RunHarness` to return `harness.Response` instead of the flat 5-tuple that drops `ToolCalls` (`harness_runner.go:32,49` — verified it returns `resp.Output, resp.TokensIn, resp.TokensOut, resp.DurationMs, err`). Add `Steps []v1.Step` to `RunResult`; copy in `ResultToWire`; in `foldRunResult` set `run.Status.Steps = rr.Steps`. **Add total-size budgeting in `cmd/agent/run.go`** (see caveat above) — store tool **name + arg/result byte counts** in Status, not raw result bodies, with full detail in pod logs. This conveniently solves both the size cap and the redaction gap.

**Flow.** `AgentRun(input)` → `agent run` → `HermesHarness.Run(API=responses)` → POST `/v1/responses` → parse output items → `Response{Output, ToolCalls}` → `runHarness` folds Steps + `Usage.ToolCalls` → `RunResult.Steps` → `Status.steps[]`.

**File targets.** `harness/hermes.go`, `v1/harness.go`, `harness_runner.go`, `executor.go`, `runonce.go`, **`cmd/agent/run.go`** (the proposal omitted this — it is mandatory), `agentrun_controller.go`, `harness_test.go`, `executor_harness_test.go`.

**Already-scaffolded — extend not rebuild.** `Response.ToolCalls`, the full `v1.Step`/`ToolCallRecord`/`StepKind` taxonomy (`types.go:200-231`), and the `status.steps[]` CRD schema all exist and are dead. `toolCalls[]` is `preserve-unknown-fields`, so **no CRD edit** is needed. We wire, not add.

**Risk.** Default MUST stay `chat` (don't regress the green path). The endpoint is externally gated and unverifiable here — `API=responses` against a gateway lacking it must **fail loud, not silently empty**. Responses usage field names differ; mis-parse silently zeroes the token budget (a safety invariant) — add a direct unit test for the `input_tokens/output_tokens` shape. Tool results bypass the loop-mode `OutputSchema` check (Hermes' internal tools aren't registered `v1.Tool`s) — acceptable, but doc-comment it so the `StepToolCall` taxonomy isn't conflated. Per-step token attribution is fictional (the gateway reports aggregate usage) — don't claim per-step fidelity.

---

#### H2. AgentSession-driven stable Hermes session id — **effort M, impact HIGH**

**Problem.** Hermes cross-run memory/skills are scoped by `X-Hermes-Session-Id` / `X-Hermes-Session-Key`. `hermes.go` forwards these on `SessionPolicy=persistent` — but only from `HERMES_SESSION_ID`/`HERMES_SESSION_KEY` env literals the spec author hard-codes. The `AgentSession` CRD and `AgentRunSpec.SessionRef` are 100% dead (no reconciler; `SessionRef` read by nobody). So "a conversation that remembers across runs" requires hand-setting a stable id on every run, and nothing aggregates the tree. The existing `agent_hermes.yaml` sample even hard-codes one shared id at the **agent** level, so *every* run piles into one transcript — exactly the wrong default.

**Design sketch (corrected — simpler than the proposal).** Do the injection **entirely in the controller** by mutating the per-run **copy** of the Agent spec before it is marshaled into `agent.json`. The proposal's "inject into the harness env, zero harness change" is **wrong as written**: harness env lives on `AgentSpec.Harness.Env`, not on `AgentRun`, and there is no per-run env or per-run `SessionPolicy` seam (`runHarness` reads both exclusively from `agent.Spec.Harness`, `executor.go:386-393`).
- New `AgentSessionReconciler` registered in `operator/cmd/manager/main.go`. **Recompute `Status.Runs` + `Status.Usage` from scratch each reconcile** by listing child runs (robust against the level-triggered double-count race), rather than incremental `+=`.
- In `BuildRunSpecConfigMap` / the AgentRun reconciler: when `run.Spec.SessionRef != "" && agent.Spec.Mode==harness`, deep-copy `agent.Spec`, set `Harness.SessionPolicy=persistent`, and append `{HERMES_SESSION_ID: "sess-<AgentSession.UID>"}` (+ optional key). **Derive from UID, never Name** (immutable across recreate).
- Status additions: `AgentSessionStatus.Usage v1.Usage`, plus an optional `AgentSessionSpec.MemoryScope` to override the derived key (ephemeral transcript, persistent memory — addresses the `X-Hermes-Session-Key` "partial" gap).
- **Write a field-wise Usage sum — do NOT reuse `Usage.Add`** (`budget.go:84` increments `Steps + 1`; it is a per-step incrementer, not a `Usage+Usage` sum — using it corrupts the roll-up).

**Validation coupling to resolve.** `ValidateAgent` (`validation.go:48-52`) rejects `persistent` unless `spec.storage` is set — but Hermes memory lives gateway-side, not in AgentFS. Either relax the rule for `HarnessHermes` or require the agent be authored persistent; **do not just force the field** and sidestep an admission invariant.

**File targets.** `agentrun_controller.go`, `operator/cmd/manager/main.go`, `v1/types.go`, `operator/api/agentmodel/v1/types.go`, `runspec.go`. **(Remove `agentrun.go` from the target set — `harnessContainer`'s env is dead because the run pod overrides the entrypoint to `/agent run`.)**

**Already-scaffolded.** `AgentSession`, `SessionRef`, `AgentSessionStatus.Runs`, scheme registration all exist and are dead. `hermes.go` already reads the env keys. **Note:** `AgentSession` lacks `+kubebuilder:subresource:status` today — must be added or `Status().Update` fails. CRD YAML for new fields is hand-edited (CRD-generation drift).

**Risk.** Concurrent runs sharing one id interleave into one gateway transcript (docs warning). Deleting an `AgentSession` does **not** purge gateway-side memory (retention note for tenants). Derive id from **namespaced UID** to prevent cross-tenant memory bleed.

---

#### H3. Multimodal (image) input via content-parts — **effort S–M (URL-only), impact MEDIUM**

**Problem.** `hermes.go:75` builds the user message as a single plain string. The gateway accepts OpenAI multimodal content (`{type:text}` + `{type:image_url}`, http(s) URLs or `data:` URIs). Today images can never reach a Hermes agent. (Non-image uploads are rejected by Hermes itself, so images are the only meaningful target.)

**Design sketch.**
- **No CRD change.** Images ride inside `AgentRunSpec.Input` (already `object` + `preserve-unknown-fields`, verified `agentruns.yaml:46-48`). Recognized shape: `{prompt|question|input|message: string, images: [{url} | {b64, mime}]}`.
- Add `imagesFromInput(json.RawMessage) []string` (sibling to `promptFromInput`, kept **pure** and in one place to avoid a third copy). In `HermesHarness.Run`, when images are present, widen `messages` content from `string` to `[]part`; **emit a plain string when absent** so the tested text path stays byte-identical (`TestHermesHarness` asserts `content=="hi"`).

**File targets.** `harness/hermes.go`, `harness/cli.go` (helper home), `harness_test.go`.

**Already-scaffolded.** `promptFromInput` reused verbatim for the text part; `Input` is already opaque JSON; the messages array already exists.

**Risk / invariant gate (load-bearing).** The **delivery ceiling is the ~1 MiB ConfigMap cap**, not the 16 MiB HTTP read cap — `BuildRunSpecConfigMap` marshals `Input` into a ConfigMap (`runspec.go:54`), so a base64 `data:` URI blows the ConfigMap before the pod starts. **Make URL passthrough the v1 deliverable**; treat inline `data:` URIs as sharply capped (tens of KB) and enforce the cap **before marshal**. The gateway-side egress is a real **SSRF/exfil channel AgentNet cannot see** (Hermes is a separate Service; `pkg/agentnet` governs only the agent pod) — default to `data:` URIs and forbid/opt-in http(s) URLs for untrusted tenants. **Scope note:** the native `Mode=loop` path (`openaillm/client.go:164`, `chatMessage.Content` is `string`) has the identical gap and is arguably where vision matters most — state Hermes-only as an explicit limitation, track openaillm as follow-up.

---

#### H4. Resilient Hermes calls: retry/backoff, 429 `Retry-After`, structured error classification — **effort M (transport-only), impact HIGH**

**Problem.** Any transient failure is fatal: a 5xx, 429, or network blip becomes `Phase=Failed` with `TerminationReason="harness:"+err` (`hermes.go:152`, `http.go:126`). No retry, no `Retry-After`, no distinction between 401 (auth), 400 (bad request), 429 (overload), 503 (down). Pods are `RestartPolicy:Never` and there is no controller-level re-run, so a 1-second hiccup wastes the whole run.

**Design sketch.**
- **CRD/Go type:** `HarnessHTTPSpec.Retry *RetrySpec {MaxAttempts int (default 1 = current behavior), BackoffBaseMs, MaxBackoffMs}`; `ValidateHarness` clamps `MaxAttempts` to `[1,5]`.
- **Classification:** `classifyHTTP(status, body) (reason string, retryable bool)` → stable taxonomy `harness:auth` (401/403), `harness:bad_request` (400/422), `harness:rate_limited` (429), `harness:overloaded` (5xx). Retry only network/429/5xx; honor `Retry-After` clamped to `MaxBackoffMs`; **always respect the budget ctx** (`Budget.Validate` requires `MaxWallClockSeconds > 0`, so an admitted run is always wallclock-bounded — verified `budget.go:53-55`).

**Two mechanics the proposal glosses — these ARE the work:**
1. **Body reuse.** Both call sites build the body as `bytes.NewReader(raw)` (`hermes.go:101`, `http.go:96`) and set no `GetBody`. A naive retry re-issues `client.Do` on a drained reader → **empty body on attempt 2+**. The shared helper MUST take a **request factory** `func() (*http.Request, error)` (or set `GetBody`).
2. **Session id capture.** The ephemeral id is minted **inline** at `hermes.go:134` during request construction. A per-attempt factory would mint a **new session per retry** — the unbounded-accumulation bug in reverse. Capture the id **once outside the factory closure**.

**Scope discipline.** `hermes.go` and `doHTTP` do **not** share request construction (different bodies; `doHTTP` reads `HEADER_` only from `Spec.Env`, `http.go:104-108`). The shared helper must be **transport-only** (ctx + client + request-factory → response + capped body + classification + retry). Scoped that way it's M; scoped to unify body construction it's a rewrite. **Flag, do not fix:** the pre-existing latent gap that `doHTTP` ignores broker-resolved `Request.Env` for headers (per surgical-change rule).

**File targets.** `harness/hermes.go`, `harness/http.go`, `harness/iface.go` (or new `errors.go`), `v1/harness.go`, `executor.go` (refine the `harness:`+err fold at `executor.go:406-413`), `harness_test.go`.

**Already-scaffolded.** The error→TerminationReason fold, `budgetTimeout` ctx (`iface.go:120`), and `ErrTimeout`/`ErrCancelled` sentinels all exist. **Honest framing:** this surfaces *actionable reasons*; there is no automated resubmit — recovery is an external orchestrator's job.

---

#### H5. Honesty fix for `reasoning_effort` (the salvageable slice of capability-discovery) — **effort XS, impact LOW-but-correct**

**Verdict downgraded.** The full capability-discovery proposal is **premature**: the probe + endpoint-gating exist solely to make H1's `/v1/responses` adoption version-safe, but there is no `API` field and no `/v1/responses` path yet — building a discovery layer to gate endpoints that don't exist is textbook premature abstraction, and a mandatory `GET /v1/capabilities` before every ephemeral single-shot run is a guaranteed wasted round-trip (+404 on the stock gateway) for 100% of current deployments.

**What to actually do now (XS).** `hermes.go:31` literally cites `BODY_reasoning_effort=high` as a working example, but the stock gateway treats `reasoning_effort` as server-side `config.yaml`. Rewrite that comment to drop the implication, note it's a possibly-ignored server-side knob; mirror the caveat in `agent_hermes.yaml`. No CRD/interface churn.

**Defer:** the capability probe, endpoint gating, and caps-driven session-header names — refile **bundled with H1** when Responses/Runs is actually implemented, at which point the probe earns its round-trip. Defer the typed `ReasoningEffort` CRD field until a gateway honors it (otherwise it's a validated field for a no-op).

---

### 2B. Files & storage

#### F1. Auto-bind harness WorkingDir to the AgentFS mount — **effort S, impact HIGH** ⭐ *highest-leverage files fix*

**Problem.** The contract docs (`harness/iface.go:25-27`, `harness.go:133-136`) both state "WorkingDir is set when Storage.AgentFS is configured", but **no code implements it**. `executor.go:393` calls `agent.Spec.Harness.WorkingDirOrEmpty()`, which returns *only* the static `CLI.WorkingDir` a human typed. `AttachStorageFS` mounts the durable volume at `MountPath` into the harness container, but the executor never tells the harness to run there. **Result: an Agent with durable AgentFS + a CLI harness runs in `/tmp` and its writes land outside the backed-up volume** unless the author hand-matches `CLI.WorkingDir` to `MountPath` in a second place. Durable storage silently does nothing for the default config.

**Design sketch.**
- **Pure Go method:** `func (a AgentSpec) EffectiveWorkingDir() string` — returns `CLI.WorkingDir` if set, else `Storage.AgentFS.MountPath` (default `/var/agentfs`) when `Storage.Kind==agentfs`, else `""`.
- In `runHarness` replace `WorkingDirOrEmpty()` with `EffectiveWorkingDir()`. `runCLI` already honors `req.WorkingDir` (`cli.go:42`), so CLI harnesses immediately run in the durable dir. Fix both doc comments to the real precedence.

**File targets.** `v1/harness.go`, `executor.go`, `harness/iface.go`, `executor_harness_test.go`.

**Already-scaffolded — extend not rebuild.** `AttachStorageFS` already mounts the volume into the harness container; `runCLI` already applies `Request.WorkingDir`. This connects the two. Verified: exactly **one** caller of `WorkingDirOrEmpty` repo-wide.

**Risk.** Low — override wins, so specs that set `CLI.WorkingDir` are unaffected; no-AgentFS agents still get `""`. **DRY nit:** `/var/agentfs` already exists as a kubebuilder default (`storage.go:39`) and `defaultStorageMountPath` (`storage_mount.go:33`) — add **one** exported `v1.DefaultAgentFSMountPath` const rather than a fourth copy. **This is a prerequisite for F2 and F3.**

---

#### F2. First-class run input files: materialize `AgentRunSpec.Inputs` into the workspace — **effort L, impact HIGH**

**Problem.** No path from a run request to a file. `AgentRunSpec.Input` is JSON → `run.json` → a prompt string. The only way to seed the AgentFS mount is an S3 restore of a prior snapshot. A caller wanting "here are the files, work on them" has no entry point.

**Design sketch.**
- **CRD/Go type:** `AgentRunSpec.Inputs []RunInputFile`, where `RunInputFile{Path string; Inline string | InlineBase64 string | SecretRef *AuthRef | S3Ref *RunInputS3}` (exactly one source). `ValidateAgentRun` enforces relative path + no traversal + single-source.
- **Runtime:** `func MaterializeInputs(ctx, workspace string, inputs []v1.RunInputFile, leaser SecretLeaser) error` called by `RunOnce` **before** `exec.Run`. Inline payloads ride `run.json`; `SecretRef` payloads lease via the broker (never inlined); files written `0600`.
- **Controller:** `prepareRun` gathers `secretRef`/S3 `credentialsRef` into the broker values map exactly like harness env secretRefs today (`agentrun_controller.go:281-292`).

**File targets.** `v1/types.go`, `v1/validation.go`, `runonce.go`, `agentrun_controller.go`, `agentruns.yaml`.

**Corrections from review (raise effort to L, not M).**
- **Hard prerequisite:** `MaterializeInputs(ctx, workspace, ...)` has no `workspace` to pass — `EffectiveWorkingDir` (F1) does not exist yet and the AgentFS mount path is never plumbed into `RunOnce`. F1 is a true prerequisite.
- **The ROFS rationale is wrong.** The proposal cites the *loop* container (`agentrun.go:142`, ROFS=true) to argue "inputs need AgentFS for writability". But the **harness container** (`agentrun.go:116`) is **ROFS=false** — and the harness path (Hermes/claude-code/aider) is the target. So the "require AgentFS for non-inline inputs" rule is over-strict; AgentFS should be required only for **durability across runs**, not as a writability precondition.
- **Defer the S3 source.** "No new egress" is misleading: S3 I/O runs in the sidecar today; routing `AWSS3.Get` through the **main** container needs creds projected into a container that doesn't get them + AgentNet allow-listing for a different principal. Ship **inline + secretRef first**, S3 as follow-up.
- `safeJoin` (`fs_storage.go:125`) is **unexported** — export or duplicate; enforce traversal guards at **both** admission and write-time. Bound inline payloads (tens of KB; steer larger to secretRef/S3).

**Already-scaffolded.** ConfigMap mount, broker secretRef-gathering, and `safeJoin` all exist and are reused.

---

#### F3. Run artifacts: capture files OUT to S3 with a manifest in `Status` — **effort XL, impact HIGH**

**Problem.** No file egress. The only output is `Response.Output` (capped 1 MiB) → termination message (truncated 2 KiB) → `Status.Output`. Files an agent produces (claude-code edits, aider diffs, reports, images) are lost or buried in a full-volume tar with no per-file discovery. Zero artifact concept exists (grep-confirmed). A framework fronting Hermes cannot return "the 3 files I created".

**Design sketch.**
- **CRD:** `AgentSpec.Artifacts *ArtifactSpec {Outputs []ArtifactRule{Name, Glob, MaxBytes, ContentType}; S3 *S3BackupSpec}` (defaults to `Storage.AgentFS.Backup.S3` under `artifacts/<run>/`).
- **CRD:** `RunStatus.Artifacts []ArtifactRef {Name, Path, S3Bucket, S3Key, S3VersionID, SizeBytes, ContentType, SHA256}`.
- **Wire/Runtime:** `RunResult.Artifacts` (additive); `CollectArtifacts(ctx, workspace, rules, s3)` globs + per-file `MaxBytes` + uploads via existing `pkg/agentfs` `S3.Put` → manifest of **refs** (not bytes), so the termination message stays tiny.

**File targets.** `v1/types.go`, `v1/storage.go`, `runonce.go`, `pkg/agentfs/types.go`, `agentrun_controller.go`, `agentruns.yaml`.

**Why XL, not L.**
- **Depends on F1 *and* on F1 actually wiring `WorkingDir=MountPath`** — otherwise `CollectArtifacts` globs an empty/ephemeral `/tmp`.
- **Wrong container / credential boundary.** Putting upload in `RunOnce` (the agent/harness container) duplicates the agentfs-sidecar's S3 role and **puts AWS creds into the least-trusted container an untrusted harness runs in**. Strongly prefer: have the **sidecar** (which already holds creds + the mount) collect + upload on shutdown and write the manifest for the run container to fold. The proposal picks the wider-surface option without weighing this.
- **AgentNet egress guard is aspirational** (operator-side `AgentNetwork` enforcement is unimplemented per memory) — don't claim "S3 must be in the allow-list" as a live mitigation.
- **VersionID may be hollow** if artifacts share the agentfs bucket — `HasVersioning()` is only checked on the backup path; the artifact `Put` must independently verify, or the integrity story is overstated.
- **Failure semantics:** the run already `Completed` with valid `Output` before upload. Don't clobber phase to `Failed` on a post-hoc upload failure — use a separate `ArtifactsState`/condition.

**Already-scaffolded.** `S3.Put` (`types.go:58`) and `S3BackupSpec` (bucket/prefix/SSE/KMS/credentialsRef) exist; this adds the missing **per-run-keyed** path alongside the fixed `agentfs.sqlite` key.

---

#### F4. Final backup-on-completion + fix a latent pod-hang bug — **effort M–L, impact HIGH** ⭐ *also a correctness fix*

**Problem (RPO).** The serve sidecar backs up only on its ticker (default `@hourly`); SIGTERM just cancels the scheduler, no final backup (`cmd/agentfs-sidecar/main.go:51,76`). WAL is a genuine no-op (`fs_storage.go:120`, sidecar hardcodes `WALInterval=0`). A short run whose pod is deleted before the first tick loses **all** produced files.

**Latent bug the proposal missed (raises the stakes).** The storage agentfs-sidecar is currently a **regular long-running container** appended to `pod.Spec.Containers` (`storage_mount.go:97`), on a `RestartPolicy:Never` pod. A regular container that blocks in `serve` until SIGTERM **never exits on its own**, so the pod can **never reach `PodSucceeded`** after the agent exits — it stays `Running` forever and `foldRunResult` never fires. This is exactly the failure the broker sidecar avoids via the native-sidecar pattern (memory: *fold gap FIXED via native sidecar, operator:0.1.2*). There is no e2e exercising durable AgentFS, so this has likely never run. **`AttachMemoryFS` has the identical hang** (`memory_mount.go:199`). → **The native-sidecar move is a correctness fix, not an optimization. Fix both `storage_mount.go` and `memory_mount.go`.**

**Design sketch.**
- Move agentfs-sidecar to a **native sidecar** (InitContainer with `RestartPolicy:Always`); on SIGTERM run **one final `Manager.Backup()`** before exit. Kubelet sends SIGTERM only after the main container exits, so the tree is quiescent.
- **Drop the sentinel-file trigger** (the proposal's belt-and-suspenders). With a true native sidecar the SIGTERM-final-backup deterministically covers "work done"; the sentinel is a racier, redundant second path (violates simplicity-first).
- Doc: mark `BackupPolicy.WALSnapshotInterval` "not yet enforced".

**File targets.** `cmd/agentfs-sidecar/main.go`, `pkg/agentfs/scheduler.go`, `storage_mount.go`, **`memory_mount.go`** (proposal scoped only storage), `v1/storage.go`. Plus their tests.

**Corrections.**
- **Index math is a rewrite, not a "check".** `storage_mount.go:103-104` does `for i := range Containers[:lastIdx]` specifically to skip the appended regular sidecar. Once the sidecar moves to InitContainers, that loop must iterate **all** regular containers, **and** the volume must additionally be mounted into the native-sidecar init container itself. Same for `memory_mount.go`.
- **Broker index-0 assumption survives but assert it** with a regression test (`AttachSecretBroker` mounts the UDS into `Containers[0]`, `secret_broker.go:74`).
- **Grace-period math is the real risk.** No `terminationGracePeriodSeconds` is set (defaults 30s). `SnapshotTo` buffers the **entire** gzipped tar into a `bytes.Buffer` in memory (`backup.go:40`) — a multi-GiB volume risks both >30s SIGKILL mid-upload **and** sidecar OOM. Cap grace at a ceiling, **stream the snapshot** instead of buffering, keep the ticker as the genuine safety net.

**Already-scaffolded.** `Manager.Backup()`, the scheduler, the native-sidecar pattern (broker), and SIGTERM handling all exist — this extends them.

---

### 2C. Agent-to-agent & tools

> **Foundational reality:** A2A is 100% types-only, **and** all loop-mode tool calling is non-functional in production (same root cause — empty `Invokers` map in `RunOnce`). The run pod has **zero apiserver connectivity** today, doesn't know its own namespace/name (no downward API), and there is **no RBAC builder** anywhere in `operator/internal/builders`. These are not "hooks to extend" — they are net-new plumbing that A2A sits on top of.

#### A1. Wire `ToolKind=agent` as a synchronous child-AgentRun invoker — **effort XL, impact HIGH**

**Problem.** `ToolKind=agent` + `AgentTargetSpec` are defined, validated, deepcopy'd — and acted on by nothing. An LLM emitting an agent-kind call is rejected (`executor.go:221`/`256`). Worse, `RunOnce` builds the executor with empty `Tools`+`Invokers` and the operator ships no tool defs into the pod — so loop-mode tool calling is broken for *all* kinds, agent-kind is just the most-missing.

**Design sketch.**
1. **Ship tools:** extend `BuildRunSpecConfigMap` to marshal resolved `[]pure.Tool` into a new `tools.json`; the AgentRun controller loads referenced Tool CRs (reuse the `r.Get` loop from `agent_controller.go:91-106`).
2. **Populate executor:** `RunOnce` reads `tools.json`, builds `exec.Tools` + registers `exec.Invokers[v1.ToolAgent]`. **This is the missing seam that also unlocks function/http/mcp invokers later.**
3. **`AgentRunInvoker`** (new `pkg/agentruntime/invoker_agent.go`): `Invoke` resolves the target, creates a child `AgentRun` (args → child `Input`, JSON-in/JSON-out), sets parent label + OwnerReference, **polls** until terminal (recommend a poll loop over a real Watch — matches the controller's 5s style, keeps the in-pod client minimal), returns `Observation{Output, Usage}`.
4. **`RunClient` seam** + new SA RBAC (create/get/watch AgentRuns, own namespace only).
5. **Budget roll-up:** add `obs.Usage` into parent `Usage.Tokens`/`ToolCalls` — **exclude WallClock** (the parent already accrues wall time during the blocking `Invoke`; adding the child's double-counts). Child `BudgetOverride = min(parent-remaining, tool cap)`.

**File targets.** `invoker_agent.go` (new), `runonce.go`, `iface.go`, `executor.go`, `runspec.go`, `agentrun_controller.go`, `agent_serviceaccount.go`, `runtime/contract.go`.

**Why XL (the proposal's L is optimistic).**
- **Zero apiserver connectivity:** `cmd/agent/run.go` imports no kube client. `RunClient` is a brand-new in-pod controller-runtime client + in-cluster rest config threaded through `RunOnce`'s signature.
- **No self-identity:** no `POD_NAMESPACE`/`POD_NAME` downward-API env — must add to `BuildAgentRunPod`, plus surface parent UID for the OwnerReference.
- **No RBAC builder exists:** must add Role+RoleBinding builder + Agent-controller `ensureRole` + operator `kubebuilder:rbac` markers (hand-edited per CRD/RBAC drift).
- **kata + apiserver reachability** unvalidated: run pods default to `kata-fc` (`workload.go:97`); a microVM reaching the in-cluster apiserver Service needs the CNI path *and* (see invariant) the egress allow-list to permit it.

**Invariant flag.** AgentNet enforces an **allow-list** egress model (`pkg/agentnet/cgroup/maps.go`); the apiserver IP is **not** implicitly allowed. An A2A agent under an `AgentNetwork` would have its child-spawn calls dropped unless apiserver/kube-dns are explicitly allow-listed. Broker-only-secrets and per-child SPIFFE are **not** violated (each child gets its own broker config + SPIFFE id; args carry no secrets). The new SA RBAC is a real authority widening — scope tightly.

**Already-scaffolded.** The types, the `ToolInvoker` seam + dispatch-by-kind (`executor.go:256`), `InProcessInvoker` as proof the seam works, `RunRef` (unused), and the entire child-AgentRun lifecycle/broker/SPIFFE/fold path are reused. **Recommendation: ship Steps 1/2/5 first** (low-risk, unlocks all invokers); gate Steps 3/4 behind the connectivity/identity/RBAC prerequisites.

---

#### A2. Surface Hermes server-side multi-agent (delegation + MoA) — **effort L, impact HIGH** (Part A only; **drop Part B**)

**Problem.** Hermes already has real multi-agent capability (delegation, mixture-of-agents) — but as server-side toolsets, not API params. Two gaps: we can't observe the sub-agent activity, and we have no lever to enable it.

**Verdict: split and descope.**
- **Part A (keep, but it's L not M):** adopt the Responses API on the Hermes harness to surface `function_call`/`function_call_output` items (incl. `delegate_task`/`mixture_of_agents`) into `Response.ToolCalls`. **This is identical to H1** — the same interface widening + new parser + `RunResult.Steps` plumbing + size cap. Treat A2 Part A and H1 as one work item. Note: `extractField` (`http.go:135`) returns a single string and **cannot** parse the heterogeneous output-items array — the parser is net-new. MoA fans out to ~5 models; verify the gateway aggregates that into the usage block or Budget under-counts.
- **Part B (drop for now):** rendering gateway `config.yaml` from `HarnessSpec.Hermes.EnabledToolsets`. **There is no operator-managed Hermes gateway** — it's always external (URL-only). The field would be advisory/dead and add permanent CRD surface (with regen drift) for a no-op. Refile if/when the operator owns a gateway deployment.

**Trust-boundary doc.** Hermes subagents execute server-side, outside our kata/SPIFFE/AgentNet/schema governance — the platform governs the single call to the gateway, not its internal fan-out. (`securityNotes`' "AgentNet already governs the gateway call" is **unverified** — no agentnet↔hermes linkage in code.)

---

#### A3. Implement `AgentSession` as the A2A run-tree aggregator — **effort M, impact MEDIUM**

This is the **aggregator half of H2**, generalized: build the missing `AgentSessionReconciler`, consume `SessionRef`, watch AgentRuns via a session label/field-index, append `Status.Runs`, roll up `Usage`, add `LastRun`/`Phase`/`TotalUsage` + printcolumns. Child runs from A1 inherit `SessionRef`, so a delegation tree shows under one session.

**Same corrections as H2** apply (inject via the per-run spec copy in `runspec.go` not `agentrun.go`; field-wise Usage sum not `Usage.Add`; UID not Name; add the `subresource:status` marker; recompute-from-scratch to avoid the two-writer race). **Additional A2A-specific caution:** A2A children sharing one Hermes session id is the **exact unbounded-conversation bug** the ephemeral path guards against — and there is **no per-run session-policy override** today (`SessionPolicy` is agent-wide). Either scope strictly to the conversational multi-turn case (one stable id per session) and **defer** the children-share-a-session idea, or add a per-run knob (→ effort L). `SessionRef` is unvalidated — either check `AgentRef` consistency or document blind aggregation.

> **Consolidation note:** H2 and A3 are the same reconciler. Build it once. H2 is the "stable id for multi-turn" framing; A3 is the "aggregate the run tree" framing.

---

#### A4. Async fan-out (Hermes Runs API + non-blocking child runs) — **Part A: effort S–M; Part B: XL+ (defer)**

**Split mandatory.**
- **Part A (approve, standalone):** a Hermes Runs-API path — `POST /v1/runs` → poll `GET /v1/runs/{id}` → `POST /v1/runs/{id}/stop` on `ctx.Done()`. This is a real **correctness fix**: today `ctx` cancel only aborts the HTTP request while the gateway-side run is **orphaned**, yet the Harness contract says "ctx cancellation MUST terminate the run" (`iface.go:68-71`). Response shape unchanged → fold path untouched. **Decouple from H1's `API` field** — ship as a new `HarnessKind=hermes-runs` (or a bool), submit+poll+stop first, **defer SSE** (the load-bearing win is stop-on-cancel, not progress). **Prerequisite reality:** verify `/v1/runs` actually exists on the deployed gateway — the proven path is `/v1/chat/completions`; the Runs API is asserted, not confirmed.
- **Part B (defer to a future milestone):** controller-driven non-blocking fan-out via `Phase=RequiresAction` + the dormant step-wise contract. This is an **architecture inversion** (pod-runs-loop → controller-orchestrated durable steps), not "extend dormant primitives". It's gated behind (i) production loop-mode tool wiring, (ii) A1, and (iii) durable per-run history persistence (a paused parent has nowhere to store history — `RunResult` drops Steps, termination message is 4KiB-capped). XL+ multi-milestone.

**Note on cancellation visibility:** even Part A's `/stop` won't surface partial output, because the controller calls `markTerminal(PhaseCancelled)` immediately after `deletePod` (`agentrun_controller.go:120-122`) and never re-reads the pod. The win is **gateway billing-stop**, not partial-output recovery.

---

### 2D. Other helpful capabilities

#### O1. Surface harness tool calls + sub-steps into `RunStatus.Steps` — **effort M, impact HIGH** ⭐ *the keystone wiring*

This is the **dependency-free core** of H1 (and A2 Part A): populate `Response.ToolCalls` in Hermes, widen `HarnessRunner` to carry it, add `RunResult.Steps`, fold in the controller, add total-size budgeting in `run.go`.

**Lead with the under-sold win:** `ResultToWire` dropping `res.Steps` means **loop-mode Steps** (already richly built at `executor.go:174-307`) are **also** never surfaced. The plumbing (RunResult.Steps + fold) fixes loop observability **for free and with zero external dependency** — that's the higher-confidence value. **Honest correction:** parsing tool calls "from the same body" assumes the non-streaming `/v1/chat/completions` response carries a tool-call log — it does **not** (the rich data is in the SSE `hermes.tool.progress` event and `/v1/responses`). So the Hermes tool-call population degrades to empty until H1/A4-SSE lands; the loop-mode Steps win does not.

**Mechanics confirmed.** `amv1.AgentRun.Status` is `pure.RunStatus`, so `run.Status.Steps = rr.Steps` is a direct assignment with no type bridge; `DeepCopyInto` already copies Steps — **no codegen needed**. The CRD `status.steps[]` schema already exists. **Scope discipline:** do NOT gate this on building `RedactionPolicy` enforcement (it's a stub applied nowhere; folding raw Steps is no worse than today's unredacted Output) — track redaction separately.

**File targets.** `harness/hermes.go`, `harness/iface.go`, `harness_runner.go`, `executor.go`, `runonce.go`, **`cmd/agent/run.go`**, `agentrun_controller.go`, `executor_harness_test.go`.

---

#### O2. Determinism + eval/replay: wire `Seed` + record/replay harness — **effort M, impact MEDIUM**

**Problem.** `Executor.Run` promises determinism but `Seed` is dead for harnesses (threaded through `RunHarness`, read by zero harnesses); Hermes never sets `seed`. No way to capture a gateway response for offline replay.

**Design sketch.**
- **(A)** In `hermes.go` body construction: `if req.Seed != 0 { body["seed"] = req.Seed }`. **Also patch `openaillm/client.go`** — verified the loop path *also* never sets `seed` (`Chat()` builds model/messages/temperature/top_p/max_tokens only), so determinism is broken in *both* modes; the proposal's "harness path is non-deterministic" framing is incomplete. Frame as "as deterministic as the backend allows".
- **(B)** `HarnessReplay` (`kind=replay`) + `RecordingHarness` decorator: record `{sha256(Instructions+Input+Seed+Model) → Response}` JSON fixtures when `SMOL_AGENTS_RECORD_DIR` is set; replay reads them, erroring on miss (fail loud, no silent net hit). Add an `eval` subcommand to `cmd/agent`.

**Corrections.**
- `cmd/agent` only dispatches `run` today (`main.go:34`) — `eval` needs a new branch.
- **Fixtures-in-WorkingDir is broken for Hermes** until F1 lands (HTTP harness `WorkingDir` is `""`) — fixtures need an explicit dir.
- ToolCalls won't round-trip until O1 widens the `RunHarness` seam.
- Hash off the **post-`promptFromInput`** prompt, trim-normalized, or record/replay disagree. Gate recording strictly on the env flag (never in tenant prod).

**Already-scaffolded.** `Seed` plumbing exists; the `Run(ctx, Request)` interface is decorator-friendly.

---

#### O3. Human-in-the-loop approval gates (wire `RequiresAction`) — **effort XL, impact HIGH**

**Problem.** `RequiresAction` is a first-class Phase in the enum, transition table, Quint, and CRD enum — emitted **nowhere**. No pause-for-approval before high-blast-radius tool/agent actions.

**Design sketch.**
- **(A) Loop-mode gate:** `AgentSpec.ApprovalPolicy{RequireApprovalForTools, RequireApprovalForKinds}`. In the loop, before invoker dispatch, a matching pending call → emit `StepKind=AwaitingApproval`, set `Phase=RequiresAction`, return early with `RunResult.PendingAction{Tool, Arguments}`. A human patches `AgentRunSpec.Decision{Approve, Reason}` (mirrors OpenAI `submit_tool_outputs`); the controller spawns a continuation pod seeded with prior Steps + the approval.
- **(B) Harness-mode gate:** necessarily coarse — a **pre-run** gate (`RequireApprovalBeforeRun`) since the gateway owns its loop opaquely. Cheap (an extra `Pending→pod-create` guard).

**Why XL (the proposal's L is wrong on a load-bearing claim).** "The executor already replays prior Steps as history (enabling resume)" is **false**. `Executor.Run`'s signature is `Run(ctx, agent, input, seed)` — **no prior-steps parameter**; line 94 hard-inits `steps := []v1.Step{}` empty; the history replay feeds only *within-process* steps. Loop-mode resume is **net-new**: a new executor entry point seeding prior steps + approval, controller-persisted history for the continuation pod, `RunOnce` loader, and — the highest correctness risk — **carrying prior Usage across the pause boundary** or `BudgetNeverExceeded` (a Quint Safety invariant) is violated. The "right" primitive (`runtime/contract.go` `StepRequest.History`) exists but is unused — adopting it is itself an architecture decision. Quint has the *state* but **no action transitioning into it** — new Quint actions required.

**Ship-order correction.** The proposal ships loop-mode first ("self-contained") but that's the **expensive** half. Ship the **cheap harness pre-run gate first** to deliver a usable valve while the resume work lands. **Redaction caveat:** `PendingAction.Arguments` lands in broadly-readable Status — the `RedactionPolicy` mitigation is itself unbuilt. **Add a `RequiresAction` TTL→Cancelled** or runs hang forever. **Hard dependency on O1** (the AwaitingApproval step + history must be persisted/replayable).

**Mechanics sound.** The operator API embeds pure types directly, so new fields flow to CRDs via `make -C operator deepcopy`. No invariant broken (broker still mints only when the resumed pod runs).

---

#### O4. Structured error classification + retry for HTTP harnesses

**This is H4** (the same `classifyHTTP` + `doHTTPWithRetry` + `RetrySpec` work, generalized to `doHTTP` as well as Hermes). Same corrections: body-reuse via request factory, transport-only scoping, and the verbatim-taxonomy claim is wrong (the executor unconditionally prepends `harness:` at `executor.go:412`, so a clean taxonomy needs the same interface widening as O1). The Idempotency-Key de-dup rests on **unverified** gateway behavior — downgrade to best-effort and lean on "only retry pre-body 5xx/network/429". **Build H4/O4 once.**

---

## 3. Prioritized roadmap

### Consolidation first (these proposals are duplicates — build once)

| Canonical item | Absorbs | Why |
|---|---|---|
| **O1** Steps/ToolCalls wire | H1 (responses parse half), A2-Part-A | All three need the same `RunHarness` widening + `RunResult.Steps` + `run.go` size cap |
| **H4** Resilient HTTP | O4 | Same `classifyHTTP`/retry helper |
| **H2/A3** AgentSession reconciler | each other | One reconciler; two framings (stable id vs run-tree aggregation) |

### Quick wins (S/M effort, high impact — mostly *wiring existing capability*)

| # | Item | Effort | Impact | Depends on | Note |
|---|---|---|---|---|---|
| 1 | **F1** Auto-bind WorkingDir to AgentFS mount | **S** | HIGH | — | One-line executor fix; unblocks F2/F3/O2. Highest ROI. |
| 2 | **H5** `reasoning_effort` honesty doc fix | **XS** | low-but-correct | — | Stops operators trusting a no-op knob. |
| 3 | **O1** Steps/ToolCalls wire (start with **loop-mode** Steps) | **M** | HIGH | size-cap in `run.go` | Loop-mode Steps surface with **zero external dep**; keystone for H1/O3/A1-observability. |
| 4 | **H4 + O4** Retry/backoff + error classification | **M** | HIGH | (interface widen pairs with O1) | Default `MaxAttempts=1` = exact current behavior. |
| 5 | **H3** Multimodal image input (**URL-only**) | **S–M** | MEDIUM | — | URL passthrough sidesteps the ConfigMap cap. |
| 6 | **O2(A)** Wire `Seed` in Hermes **and** openaillm | **S** | MEDIUM | — | Trivial; makes the determinism claim partially true. |
| 7 | **A4-Part-A** Hermes Runs `stop`-on-cancel | **S–M** | MEDIUM | verify `/v1/runs` exists | Correctness fix: stop orphaning gateway runs on cancel. |

### Larger bets (M–XL — real building or architecture shifts)

| # | Item | Effort | Impact | Depends on | Note |
|---|---|---|---|---|---|
| 8 | **H2/A3** AgentSession reconciler (id + aggregation) | **M** | MED–HIGH | — | Net-new reconciler; resolve the `persistent→storage` admission rule. |
| 9 | **F4** Final-backup + native-sidecar fix | **M–L** | HIGH | — | Also fixes the latent **pod-hang** bug; fix memory_mount too. |
| 10 | **F2** Run input files (inline + secretRef) | **L** | HIGH | **F1** | Defer S3 source to follow-up. |
| 11 | **H1** Hermes Responses API (full) | **L** | HIGH | **O1**, feature-detect | Surfaces real tool-call structure; fail-safe vs missing endpoint. |
| 12 | **F3** Run artifacts to S3 + manifest | **XL** | HIGH | **F1**(+WorkingDir), prefer sidecar upload | Re-evaluate credential placement before building. |
| 13 | **A1** A2A child-run invoker | **XL** | HIGH | in-pod kube client, downward API, RBAC builder, egress allow-list | Ship Steps 1/2/5 (unlock all invokers) before 3/4. |
| 14 | **O3** Human-in-the-loop gates | **XL** | HIGH | **O1**; harness pre-run gate first | Loop-mode resume is net-new (Usage carry-over is the risk). |
| 15 | **A4-Part-B** Controller async fan-out | **XL+** | MEDIUM | A1 + loop-tool wiring + durable history | Architecture inversion; future milestone. |

### Recommended sequence

```
Phase 0 (days):        F1 ───────────────┐  H5 (doc)  O2(A) seed
                                          │
Phase 1 (1–2 wks):     O1 (loop Steps) ───┼──► H4/O4   H3(URL)   A4-A(stop-on-cancel)
                              │           │
Phase 2 (2–4 wks):     H1 ◄───┘     F2 ◄──┘     H2/A3 (session)     F4 (backup + hang fix)
                        │                              │
Phase 3 (project):     A1 (Steps 1/2/5 → 3/4)    O3 (harness gate → loop resume)    F3
                                          │
Phase 4 (future):      A4-B (async fan-out, gated on A1 + durable history)
```

**Critical-path note:** F1 → (F2, F3, O2-fixtures) and O1 → (H1, O3, A1-observability) are the two backbones. Do them first; almost everything high-impact hangs off them.

---

## 4. Open questions / decisions needed from the maintainer

1. **Termination-message budget policy.** Adding Steps/artifacts/pending-action to `RunResult` collides with the ~4KiB kubelet cap. Preferred fallback: **counts-only in Status, full detail in pod logs** (also solves redaction). Acceptable, or do we want an overflow store (write full Steps to AgentFS/S3 and reference them)? This decision shapes O1, H1, F3, O3.

2. **Hermes gateway: external-only, or will the operator own it?** Today it's always external (URL). This kills A2-Part-B (`config.yaml` rendering) and the typed `ReasoningEffort` field as no-ops. If the operator will eventually deploy+manage a Hermes gateway, several "server-side feature" gaps (delegation/MoA enablement, memory, skills, reasoning_effort) become reachable. Which direction?

3. **Are `/v1/responses`, `/v1/runs`, `/v1/capabilities` available on the deployed gateway build?** Unverifiable from this repo; the proven path is `/v1/chat/completions` (glm-4.6 via z.ai). H1, A2-Part-A, and A4 all assume these endpoints. If unconfirmed, do we feature-detect-and-fallback, or pin a gateway version?

4. **Redaction: build it, or descope it?** `RedactionPolicy` is a type applied nowhere. O1/O3 surface tool args + results (file contents, command output) into broadly-readable `Status`. Do we (a) descope (counts-only / Output is already unredacted today) or (b) treat building the redaction engine as in-scope (pushes effort up)? Recommendation: (a) now, track (b) separately.

5. **A2A egress + RBAC posture.** A1 needs the run pod to (a) reach the apiserver — which, under an `AgentNetwork`, requires explicitly allow-listing the apiserver/kube-dns IPs in the eBPF allow-list — and (b) hold a new create/watch-AgentRun Role. Are we comfortable widening the run pod's authority from zero apiserver writes? And do we validate kata-microVM→apiserver reachability before committing? Note operator-side `AgentNetwork` enforcement is itself currently unimplemented.

6. **`SessionPolicy` granularity.** It's agent-wide today, and `persistent` requires `spec.storage` (wrong for gateway-side Hermes memory). For A3/A1 to safely mix a stable conversational session with fresh-per-child A2A runs, we likely need a **per-run session-policy override** (raises A3 to L). Add the knob, or scope sessions strictly to single-agent multi-turn for v1?

7. **Resume architecture for O3/A4-B.** Two paths: retrofit resume onto the monolithic in-process executor, or adopt the dormant `runtime/contract.go` step-wise (controller-drives-each-step) protocol. The latter is cleaner for durable/resumable runs but is a larger architecture change. Which engine do we commit to before building human-in-the-loop and async fan-out?

8. **Determinism expectations.** Provider `seed` is a hint, not a guarantee, and the stock Hermes chat handler may not forward it. Is "as deterministic as the backend allows" + the replay harness (O2-B) sufficient for the eval/regression use case, or do we need stronger guarantees (which only replay can give)?

---

**Bottom line.** The framework's biggest wins are *unblocking* what already exists: bind the workspace (F1), stop dropping Steps on the wire (O1), and make files flow in and out (F2/F3/F4). These are mostly wiring, carry low risk when scoped carefully, and turn a lossy, chat-shaped surface into an observable, file-capable, multi-agent platform. The deeper bets (A2A, human-in-the-loop, async fan-out) are genuinely net-new plumbing — sequence them behind the two backbones and behind explicit decisions on the apiserver/RBAC posture and the resume architecture.
