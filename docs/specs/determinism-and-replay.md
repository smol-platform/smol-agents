# Spec: Determinism + eval/replay for harness & loop runs

> **✅ Decisions resolved 2026-06-03 — see [decisions.md](../design/decisions.md).** D6: replay is **post-GA**; near-term reproducibility = best-effort seed + N-sample distributions; no `usage.toolCalls` gating. Where this doc still says OPEN/PROPOSED and conflicts, the decision log wins.

> **Status: DESIGN / SPEC — 2026-06-03 (v0.2.0 source).** Implementation-grade plan for making smol-agents runs **as reproducible as the backend allows**, and adding an offline **record/replay** capability plus an `agent eval` regression-suite runner. Every code claim is cited `file:line` against the tree. Proposals are marked **PROPOSED**; anything already in the tree is called out as **DONE** with the citation so we do not re-build it.
>
> **Extends, does not duplicate:** [framework-enhancements.md](../design/framework-enhancements.md) §2D **O2** ("Determinism + eval/replay: wire `Seed` + record/replay harness"). That sketch is the rationale; this file is the build sheet — and it **corrects O2's premise**: the `Seed`-wiring half is already landed (see §2), so the deliverable is now the replay decorator + `eval` subcommand, not the seed plumbing.
>
> **Companion specs (this run):** [response-richness](response-richness.md) (the `Response.ToolCalls`/`Steps` wire — `ToolCalls` will not round-trip through fixtures until that lands; see §6.4), [agent-hermes](agent-hermes.md) §2 (the same "Seed already wired" correction), run-governance (future) (where an `eval`-driven budget/quality gate could live). Background: [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md), [harness-authoring.md](../design/harness-authoring.md) (the `HarnessKind` authoring contract + the Response richness contract that bounds what a fixture can faithfully capture), [agent-model.md](../features/agent-model.md) (the `Mode=harness` vs `Mode=loop` split this spec spans).

---

## 1. Summary

The executor's package doc promises determinism — *"no `time.Now()` outside the injected Clock, all RNG seeded from `AgentRun.Spec.Seed`"* (`pkg/agentruntime/doc.go:6-7`) — and that promise holds for the **in-process control flow**. But the *model output* is only as deterministic as the LLM backend, and there is **no way to pin a backend response** so a run reproduces bit-for-bit offline. There is also no harness-level regression-suite runner: you cannot say "run these 20 fixtures and tell me which changed".

This spec delivers three things, in dependency order:

| # | Increment | What it gives you | Status today |
|---|---|---|---|
| **A** | **Seed → backend** (verify, test, document) | `seed` reaches every backend that honors it (OpenAI-compatible chat, Hermes gateway) so a seed-honoring provider returns stable output | **DONE in code** (`hermes.go:108-110`, `openaillm/client.go:66-68`) — needs a determinism unit test + an honest doc that this is a *hint* (§4.1) |
| **B** | **Record/replay harness** | a `RecordingHarness` decorator captures `sha256(Instructions+Input+Seed+Model) → Response` JSON fixtures when `SMOL_AGENTS_RECORD_DIR` is set; a `HarnessReplay` kind (`kind=replay`) replays them, **erroring on miss** (fail loud, never a silent network hit) | **PROPOSED** — zero replay/fixture code exists (`grep` confirms only TraT/NATS/FakeLLM uses of the word "replay") |
| **C** | **`agent eval` subcommand** | a CLI that runs a directory of `{spec, expected}` cases against fixtures and reports pass/fail/changed for offline regression + CI | **PROPOSED** — `cmd/agent` dispatches only `run` + `serve-session` today (`main.go:36-41`) |

**The honest framing throughout:** provider `seed` is a *hint, not a guarantee*. Temperature, model version drift, server-side batching, and (for Hermes) the gateway's own loop all defeat bit-exact reproduction. **The only thing that gives you a true guarantee is replay against a recorded fixture** — which is exactly why B exists. A is "best-effort stability for the live path"; B is "exact reproduction for eval/CI/debugging".

**Scope boundary.** This spec governs the *harness layer* (`pkg/agentruntime/harness`) and `cmd/agent`. It does **not** add network mocking to the loop-mode `LLM` client (`openaillm`); loop-mode determinism for tests already has `FakeLLM` (`fake.go:14-44`). Record/replay is delivered at the harness seam because that is the single chokepoint both modes' *external* calls would have to pass through if we wanted one — and harness mode is the e2e-green production path (per project memory: *Hermes + z.ai GREEN*). Loop-mode replay is called out as follow-up in §10.

---

## 2. Current state

### What is already DONE (corrects O2's premise — do NOT re-build)

O2 in [framework-enhancements.md](../design/framework-enhancements.md) §2D was written against an older tree where `Seed` was *"threaded through `RunHarness`, read by zero harnesses"* and *"Hermes never sets `seed`"*. **Both have since been wired.** Verified against the current tree:

- **Hermes forwards the seed.** `hermes.go:108-110`:
  ```go
  if req.Seed != 0 {
      body["seed"] = req.Seed
  }
  ```
  with a doc-comment (`hermes.go:40-41`, `106-107`) already framing it as *"a hint, not a guarantee (many gateways ignore it)"*. A `BODY_seed` env overrides it (`hermes.go:113-117`).
- **The OpenAI-compatible loop client forwards the seed.** `openaillm/client.go:66-68` — identical shape, same "hint, not a guarantee" comment.
- **The seed is threaded end-to-end already:** `AgentRunSpec.Seed` (`types.go:230`) → `RunTurn`/`RunOnce` → `exec.Run(ctx, agent, run.Input, run.Seed)` (`runonce.go:83`) → `Executor` (`executor.go:130`) → `Request.Seed` (`harness/iface.go:40-41`) → `RunHarness(..., seed)` (`harness_runner.go:33,49`) → the two backends above.

So **O2(A) is effectively complete.** What remains for A is a **determinism regression test** and an **honesty doc** — not plumbing (§4.1).

### What is also DONE (a dependency for fixtures)

Fixtures must capture the whole `Response`, including its tool-call log. The cross-harness Steps/ToolCalls wire that O2 listed as a blocker is itself **landed** (so the fixture's `Response.ToolCalls` field is real, even though no harness *populates* it yet):

- `RunHarness` returns `harness.Response` **whole** (`harness_runner.go:30-51`).
- `Response` carries `ToolCalls []v1.ToolCallRecord` (`harness/iface.go:65`) — declared, populated by **zero** harnesses today (the Response richness contract, `harness/iface.go:47-55`).
- `RunResult.Steps` exists and is folded (`runonce.go:34`, `ResultToWire` at `runonce.go:~94`, controller `Status.Steps = rr.Steps` per [agent-hermes](agent-hermes.md) §2).

> **Consequence for this spec:** a fixture can *store* `ToolCalls`, but until a producer exists ([response-richness](response-richness.md) + the Hermes Responses API in [agent-hermes](agent-hermes.md) §4.1), recorded `ToolCalls` will be empty in practice. The fixture schema is forward-compatible; §6.4 states this limitation plainly.

### What is stubbed / missing (the gap this spec closes)

| Gap | Evidence | Increment |
|---|---|---|
| No determinism test asserting same-seed → same wire body | no `*_test.go` asserts `body["seed"]` is set/stable | A (§4.1) |
| Seed-as-hint is documented in code comments but not surfaced to operators | `hermes.go:40-41` only | A (§4.1) |
| **No record/replay anything** | `grep -rn "RECORD\|Recording\|Replay\|fixture"` over `pkg/`+`cmd/` finds only TraT/NATS/FakeLLM — none are response fixtures | B (§4.2) |
| No `HarnessReplay` kind | `HarnessKind` enum has 8 values, none `replay` (`harness.go:38-60`); `Valid()` at `harness.go:64-71` | B (§4.2) |
| No `eval` subcommand | `cmd/agent/main.go:36-41` dispatches `run`/`serve-session` only | C (§4.3) |
| 4 KiB termination cap can truncate large traces | `cmd/agent/run.go:94` `terminationMessageBudget=3072`, `clampForTerminationMessage` (`run.go:102-115`) | not this spec — see [response-richness](response-richness.md); fixtures sidestep it (§6.3) |

---

## 3. External interface research

**N/A — internal-only.** Record/replay, the `eval` runner, and the seed wiring touch no external API surface beyond the OpenAI-compatible `seed` request field already in use (`hermes.go:109`, `openaillm/client.go:67`), which is OpenAI's documented best-effort determinism knob. No WebSearch/WebFetch required; per the task brief this section is intentionally skipped.

---

## 4. Design

### 4.1 Increment A — Seed: verify, test, document (mostly DONE)

The plumbing exists. The work is to **lock it in** and **stop overselling it**.

```
AgentRunSpec.Seed ──► RunOnce ──► Executor ──► Request.Seed ──► HermesHarness.Run     ──► body["seed"]   (DONE)
                                                            └─► openaillm.Client.Chat ──► body["seed"]   (DONE)
```

- **Unit test (NEW).** Add a table test asserting that, for both producers, `req.Seed=N (N≠0)` puts `seed:N` in the marshaled body and `req.Seed=0` omits it. This freezes the contract against regression (today nothing pins it).
- **Doc (NEW).** Add a short "Determinism" subsection to a tenant-facing doc and a one-line `// seed is best-effort` note where authors set it. The message: *seed reaches any backend that honors it; bit-exact reproduction is NOT guaranteed for live calls — use `kind=replay` (§4.2) when you need exact reproduction.*
- **No CRD change, no new field.** `AgentRunSpec.Seed` already exists.

> **Why A is not "free determinism".** Even with a fixed seed: (1) most hosted providers treat `seed` as best-effort and may ignore it under load; (2) Hermes runs its own multi-tool loop server-side — the *gateway's* internal nondeterminism is invisible to and uncontrollable by us (the harness governs one call, not the gateway's fan-out — see [agent-hermes](agent-hermes.md) §9 trust boundary); (3) `temperature`/`top_p` set via `BODY_*` (`hermes.go:113-117`) or `ModelRef` (`openaillm/client.go:55-60`) reintroduce sampling. A is "as deterministic as the backend allows", full stop.

### 4.2 Increment B — Record/replay harness (the real deliverable)

Two pieces: a **decorator** that records, and a **kind** that replays. Both key on the same content hash.

```
                         ┌──────────────────────────────────────────────┐
   live run              │  RecordingHarness{Inner: <real harness>, Dir} │
   (record mode,         │   1. h := fixtureKey(req)                     │
    SMOL_AGENTS_         │   2. resp, err := Inner.Run(ctx, req)         │
    RECORD_DIR set)      │   3. if err==nil: write Dir/<h>.json          │
                         │   4. return resp, err                         │
                         └──────────────────────────────────────────────┘

   offline run           ┌──────────────────────────────────────────────┐
   (kind=replay)         │  HarnessReplay{Dir}                           │
                         │   1. h := fixtureKey(req)                     │
                         │   2. f := read Dir/<h>.json                   │
                         │   3. miss → ERROR (fail loud)                 │
                         │   4. return f.Response, nil                   │
                         └──────────────────────────────────────────────┘
```

#### Fixture key

```go
// fixtureKey is the content address of a harness call. Stable across runs of the
// SAME logical request so record and replay agree. Hash the post-normalization
// prompt (NOT the raw JSON) so cosmetic input reformatting does not miss.
func fixtureKey(req harness.Request) string {
    var b strings.Builder
    b.WriteString(strings.TrimSpace(req.Instructions))
    b.WriteByte(0)
    b.WriteString(strings.TrimSpace(promptFromInput(req.Input))) // SAME helper the harness uses
    b.WriteByte(0)
    fmt.Fprintf(&b, "%d", req.Seed)
    b.WriteByte(0)
    b.WriteString(harnessModelForKey(req)) // see "model in the key" below
    sum := sha256.Sum256([]byte(b.String()))
    return hex.EncodeToString(sum[:])
}
```

- **Hash off the post-`promptFromInput` prompt**, trim-normalized — O2's explicit correction. If we hashed raw `Input`, a record made from `{"prompt":"hi"}` would miss a replay of `"hi"`. Both `harness/cli.go:124` (`promptFromInput`) and `openaillm/client.go:211` already normalize; the recorder must use the *harness's* `promptFromInput` (the CLI one), since the harness layer is where we sit.
- **Model in the key.** The model is selected differently per kind — Hermes from `HERMES_MODEL` env / default (`hermes.go:231-236`), CLI kinds have no model concept, generic-http none. Define `harnessModelForKey(req)` as: Hermes → `hermesModel(effectiveEnv(req))`; everything else → `string(req.Spec.Kind)` (the kind name is a stable, sufficient discriminator for non-Hermes). This keeps the key meaningful without inventing a model field CLI harnesses do not have.
- **Seed is in the key.** Two recordings of the same prompt at different seeds are different fixtures — correct, because a seed-honoring backend produces different output.

> **What the key deliberately ignores:** `Budget`, `WorkingDir`, broker secrets, `BODY_*`/`HEADER_*` env beyond model. Rationale: a fixture is a *prompt→answer* record; budget/timeouts are enforcement, not inputs to the answer. **Documented caveat:** changing `temperature` via `BODY_temperature` does change the live answer but NOT the key — so a fixture recorded at temp=0.7 would replay for a temp=0.2 request. We accept this (the eval author controls both) and document it; widening the key to all `BODY_*` is a follow-up if it bites (§10).

#### Fixture file format

One JSON file per call, named `<sha256>.json`, in `SMOL_AGENTS_RECORD_DIR` (record) or the replay dir (replay):

```json
{
  "schemaVersion": 1,
  "key": "<sha256-hex>",
  "recordedAt": "2026-06-03T12:00:00Z",
  "request": {
    "kind": "hermes",
    "model": "glm-4.6",
    "instructions": "You are a fib calculator.",
    "prompt": "fib(12)?",
    "seed": 42
  },
  "response": {
    "output": "144",
    "tokensIn": 31,
    "tokensOut": 4,
    "toolCalls": [],
    "durationMs": 0
  }
}
```

- `request.*` fields are **for human/diff readability and debugging** — they are NOT re-hashed on replay (replay recomputes the key from the *incoming* `Request`, then looks up `<key>.json`). Storing them lets `git diff` of a fixture dir show "this prompt's answer changed".
- `response` is the marshaled `harness.Response`. `durationMs` is recorded as 0 on replay (it is a property of the live call, meaningless offline) — the executor measures its own wall clock anyway (`executor.go:392-398`).
- `toolCalls` is present-but-empty until a producer exists (§6.4).
- `schemaVersion` lets us evolve the format; a replay against an unknown version errors loud.

#### RecordingHarness decorator

`harness.Harness` is a clean one-method interface (`Run(ctx, Request) (Response, error)`, `iface.go:73-81`) — decorator-friendly, exactly as O2 notes. New file `pkg/agentruntime/harness/recording.go`:

```go
type RecordingHarness struct {
    Inner Harness
    Dir   string // SMOL_AGENTS_RECORD_DIR
}

func (r *RecordingHarness) Kind() v1.HarnessKind { return r.Inner.Kind() }

func (r *RecordingHarness) Run(ctx context.Context, req Request) (Response, error) {
    resp, err := r.Inner.Run(ctx, req)
    if err != nil {
        return resp, err // never record failures — fixtures are golden answers
    }
    _ = writeFixture(r.Dir, fixtureKey(req), req, resp) // best-effort; log on failure, never fail the run
    return resp, err
}
```

- **Recording never changes run semantics.** It wraps, calls through, persists on success, returns the real result. A write failure is logged, not fatal — recording is a dev/CI convenience, never load-bearing for a live run.
- **Only successes are recorded** (golden answers). A transient 503 should not become a permanent fixture.

#### HarnessReplay kind

A new harness that *replaces* the network entirely. New file `pkg/agentruntime/harness/replay.go`:

```go
type HarnessReplay struct {
    Dir string // fixtures dir; if empty, read from req env SMOL_AGENTS_REPLAY_DIR
}

func (h *HarnessReplay) Kind() v1.HarnessKind { return v1.HarnessReplay }

func (h *HarnessReplay) Run(_ context.Context, req Request) (Response, error) {
    dir := h.Dir
    if dir == "" {
        dir = effectiveEnv(req)["SMOL_AGENTS_REPLAY_DIR"]
    }
    if dir == "" {
        return Response{}, errors.New("harness: replay requires a fixtures dir (--replay-dir or SMOL_AGENTS_REPLAY_DIR)")
    }
    f, err := readFixture(dir, fixtureKey(req))
    if err != nil {
        return Response{}, fmt.Errorf("harness: replay miss for key %s: %w", fixtureKey(req), err) // FAIL LOUD
    }
    return f.Response, nil
}
```

- **Miss = error, never a network call.** This is the whole point: a `kind=replay` run that hits an unrecorded prompt fails loudly. There is no fall-through to the real backend (which would defeat offline determinism and could leak a live call in CI).
- `kind=replay` is **HTTP-shaped only in spirit** — it makes no network call. In `ValidateHarness` it is its own branch (no `http.url` requirement; a `cli`/`http` block is ignored). See §5.

#### Wiring record-mode into the registry

`RegistryRunner.RunHarness` (`harness_runner.go:30-51`) resolves `r.Registry.For(spec.Kind)` then calls `h.Run`. To make recording transparent, wrap at resolution time when the env flag is set:

```go
h, err := r.Registry.For(spec.Kind)
if err != nil { return harness.Response{}, err }
if dir := os.Getenv("SMOL_AGENTS_RECORD_DIR"); dir != "" && spec.Kind != v1.HarnessReplay {
    h = &harness.RecordingHarness{Inner: h, Dir: dir}
}
return h.Run(ctx, harness.Request{...})
```

- Guarded **strictly on the env flag** (O2's "never in tenant prod"). The operator never sets `SMOL_AGENTS_RECORD_DIR` for tenant runs; it is set by the `eval` subcommand and by developers running `agent run` locally.
- We do **not** wrap `kind=replay` in the recorder (replaying-then-recording is a no-op that would overwrite golden fixtures with themselves).

### 4.3 Increment C — `agent eval` subcommand

A new top-level dispatch branch in `cmd/agent/main.go`, sibling to `run` and `serve-session`:

```go
if len(os.Args) > 1 && os.Args[1] == "eval" {
    os.Exit(runEval(os.Args[2:]))
}
```

New file `cmd/agent/eval.go`. `agent eval` runs a **suite**: a directory of cases, each a dir containing `agent.json` + `run.json` (the exact pair `RunOnce` already loads, `runonce.go:46-52`) and an optional `expected.json`. It runs each case against a fixtures dir in **replay mode** (deterministic) or live (record mode), and reports.

```
agent eval \
  --suite ./testdata/eval/fib \      # dir of case dirs (each: agent.json, run.json, [expected.json])
  --fixtures ./testdata/eval/fib/_fixtures \
  --mode replay \                    # replay (default) | record | live
  --format text                      # text | json  (json for CI)
```

Per case:
1. Load `agent.json` + `run.json` (reuse `agentruntime.RunOnce`'s loaders; factor the read into an exported helper or call `RunOnce` with a per-case dir).
2. Run via `RunOnce`:
   - `--mode replay`: force `agent.Spec.Harness.Kind = replay` **OR** (cleaner — see §10 decision) leave the kind and set `SMOL_AGENTS_REPLAY_DIR`; the recorder/replayer key off the real spec. Recommended: keep the authored kind, run with a **replay registry** (a `Registry` whose every kind resolves to `HarnessReplay{Dir}`), so the fixture key matches what was recorded under the real kind.
   - `--mode record`: set `SMOL_AGENTS_RECORD_DIR=<fixtures>`, run live, persist fixtures.
   - `--mode live`: no fixtures, real backend (smoke test).
3. Compare the resulting `RunResult` against `expected.json` if present: at minimum `phase`; optionally a normalized `output` match (exact, or a `outputContains` substring assertion to tolerate whitespace).
4. Tally: `PASS` / `FAIL` (mismatch) / `CHANGED` (replay hit but output differs from `expected`) / `MISS` (no fixture in replay mode).

Exit non-zero if any case is FAIL/MISS (so CI gates on it). `--format json` emits a machine-readable report for pipelines.

> **`eval` reuses the run datapath, it does not fork it.** It calls `RunOnce`/`RunTurn` (`runonce.go:44-84`) — the same code a real AgentRun pod runs — so an eval pass exercises the genuine executor + harness + fold path, just with the network pinned. No second execution engine.

---

## 5. Concrete changes

### 5.1 CRD / Go types

| Change | File | Detail |
|---|---|---|
| Add `HarnessReplay` kind | `pkg/agentmodel/v1/harness.go` (after `:59`) | `// HarnessReplay replays recorded fixtures instead of calling a backend; offline/eval only.`<br>`HarnessReplay HarnessKind = "replay"` |
| Accept in `Valid()` | `pkg/agentmodel/v1/harness.go:64-71` | add `HarnessReplay` to the `case` list |
| Validate `replay` | `pkg/agentmodel/v1/harness.go:308-333` (`ValidateHarness`) | new `case HarnessReplay:` — **no** `http.url` requirement; a fixtures dir comes from `--replay-dir`/env, not the spec. Optionally validate that `cli`/`http` blocks, if set, are ignored (doc, not error). |
| (No new CRD field) | — | `kind=replay` rides the existing `harness.kind` enum (`preserve-unknown-fields` not needed; it is a string enum). The CRD enum list is **hand-edited** per CRD-generation drift (project memory: *CRD generation drift*) — add `"replay"` to the `harness.kind` enum in `operator/config/crd/...agents.yaml`. |

> **No `AgentRunSpec` change.** Seed already exists (`types.go:230`). Record/replay is driven by **env + CLI flags**, not CR fields — deliberately, so a fixture-replay mode never persists in a tenant's authored CR and a recording flag is never something an operator sets cluster-wide.

### 5.2 New runtime files

| File | Contents |
|---|---|
| `pkg/agentruntime/harness/recording.go` (NEW) | `RecordingHarness` decorator (§4.2); `fixtureKey`, `harnessModelForKey`, `writeFixture`, `readFixture`, the `Fixture` struct (`schemaVersion`/`key`/`recordedAt`/`request`/`response`). |
| `pkg/agentruntime/harness/replay.go` (NEW) | `HarnessReplay` harness (§4.2). |
| `pkg/agentruntime/harness/recording_test.go` (NEW) | round-trip (record → replay → identical `Response`), key-stability (raw vs object-wrapped prompt hash equal), miss-errors-loud, failures-not-recorded. |
| `cmd/agent/eval.go` (NEW) | `runEval` (§4.3): suite walk, per-case `RunOnce`, replay-registry construction, comparison, tally, exit code, `text`/`json` reporters. |
| `cmd/agent/eval_test.go` (NEW) | a 2-case suite under `testdata/`: one PASS, one CHANGED; assert exit code + report. |

### 5.3 Touch points in existing files

| File | Change |
|---|---|
| `pkg/agentruntime/harness_runner.go:38-50` | wrap the resolved harness in `RecordingHarness` when `SMOL_AGENTS_RECORD_DIR` is set and kind≠replay (§4.2). |
| `pkg/agentruntime/harness/iface.go:90-101` (`Default()`) | register `&HarnessReplay{}` so `kind=replay` resolves. (It reads its dir from env/flag, so a zero-value registration is fine.) |
| `cmd/agent/main.go:36-41` | add the `eval` dispatch branch. |
| `pkg/agentruntime/harness/hermes.go` (comment near `:40-41`) | (Increment A) tighten the seed doc to "best-effort; use kind=replay for exact reproduction". |
| `pkg/agentruntime/harness/hermes_test.go`, `openaillm/client_test.go` | (Increment A) add the seed-in-body determinism assertions (§4.1). |
| a tenant-facing doc (e.g. [agent-model.md](../features/agent-model.md) or [harness-authoring.md](../design/harness-authoring.md)) | (Increment A) a short "Determinism & replay" subsection. |

### 5.4 What is explicitly NOT changed

- **No loop-mode (`openaillm`) network mock.** Replay is a *harness* concern; loop-mode tests use `FakeLLM`. Loop-mode live replay is §10 follow-up.
- **No change to `clampForTerminationMessage`** (`run.go:102-115`). Fixtures are written to a dir, not the 4 KiB termination message — they sidestep the cap entirely (§6.3).
- **No operator wiring of record mode.** The operator never sets `SMOL_AGENTS_RECORD_DIR` on tenant pods.

---

## 6. Data / control flow

### 6.1 Record (developer / CI capturing goldens)

```
$ SMOL_AGENTS_RECORD_DIR=./fixtures  agent run --dir ./case-1
   RunOnce(dir) ─► RunTurn ─► Executor.runHarness
        └─► RegistryRunner.RunHarness:
              h := registry.For(spec.Kind)            # e.g. HermesHarness
              h  = RecordingHarness{Inner:h, Dir}     # because env flag set
              resp, err := h.Run(ctx, req)
                  Inner.Run ──HTTP──► gateway ──► resp
                  writeFixture(Dir, fixtureKey(req), req, resp)   # ./fixtures/<sha>.json
   ─► RunResult folded as normal (the live answer is real)
```

### 6.2 Replay (offline / CI gate)

```
$ agent eval --suite ./eval/fib --fixtures ./eval/fib/_fixtures --mode replay
   for each case dir:
     RunOnce(case, replayRegistry)
        └─► RegistryRunner(replayRegistry).RunHarness
              h := HarnessReplay{Dir: fixtures}          # every kind → replay
              resp := readFixture(Dir, fixtureKey(req))  # miss ⇒ ERROR
        └─► RunResult folded from the fixture's Response (NO network)
     compare RunResult.Phase/Output vs expected.json
   print tally; exit 1 if any FAIL/MISS
```

### 6.3 Why fixtures dodge the 4 KiB cap

The termination-message budget (`run.go:94`) bounds only the controller's primary signal. Fixtures are written to `SMOL_AGENTS_RECORD_DIR` on the local FS and read back the same way — they never traverse `/dev/termination-log`. A 100 KB tool-call trace records and replays in full. (The *live* run that produced it still truncates its Status trace per the cap — that is [response-richness](response-richness.md)'s problem, not this spec's.)

### 6.4 ToolCalls round-trip limitation (honest)

The fixture `response.toolCalls` field exists and round-trips structurally, **but no harness populates `Response.ToolCalls` today** (`harness/iface.go:47-55` richness contract — `ToolCalls` is "populated by NO harness"). So until [response-richness](response-richness.md) lands its producer (and Hermes adopts `/v1/responses`, [agent-hermes](agent-hermes.md) §4.1), recorded fixtures capture `output`/`tokensIn`/`tokensOut` faithfully and `toolCalls` as `[]`. The format is forward-compatible — once a producer exists, re-recording fixtures captures tool calls with no schema change. **We will not claim tool-call replay works before its producer ships.**

---

## 7. Security model

Record/replay is a **developer/CI capability**, and the design keeps it out of the trusted production datapath. How it composes with the existing controls:

- **kata-fc sandbox.** Unchanged. `eval` runs `RunOnce` the same as a real run; in replay mode it makes **no** network call, so it is *strictly less* externally-exposed than a live run. The decorator/replayer add no syscalls beyond local file I/O.
- **Egress (default-deny NetworkPolicy + AgentNetwork).** Replay mode makes **zero egress** — it cannot exfiltrate via the model backend because there is no backend call. This is a security *win* for CI: a replay-mode eval suite provably never talks to a provider.
- **Broker / SPIFFE.** Fixtures **must never contain secrets.** Two guards: (1) the fixture key and stored `request` capture `instructions`/`prompt`/`seed`/`model` — **not** broker-leased env or `HEADER_Authorization` (those live in `req.Env`, deliberately excluded from both the key and the stored request). (2) The recorded `response` is the harness `Output` — which for a misbehaving agent *could* echo a secret it was given; this is the **same exposure as today's `Status.Output`**, no worse. **Mitigation/doc:** fixtures are dev artifacts; treat a fixtures dir like test data — do not commit fixtures recorded against production secrets. A `RedactionPolicy` (stubbed, applied nowhere — project memory) would also gate this if/when built; not a dependency here.
- **New attack surface.** A `kind=replay` run reads files from an attacker-controllable dir. Mitigation: the dir is supplied by the **operator/developer running `eval`**, never by a tenant CR (no CR field carries it); `readFixture` joins `<dir>/<sha256>.json` where the basename is a **hex hash** (no path separators, no traversal possible). A malformed fixture fails to unmarshal → loud error, never a silent wrong answer.
- **Record mode is env-gated and prod-off.** `SMOL_AGENTS_RECORD_DIR` is never set on tenant pods by the operator; a recorder that fails to write logs and continues (never alters the live result). There is no code path where a tenant CR turns recording on.

**Net:** replay reduces attack surface (no egress); recording adds only local-FS writes behind an env flag the operator never sets; the one real care is "don't ship fixtures recorded against prod secrets", which is a doc + the key/request exclusion of `req.Env`.

---

## 8. Phasing & effort

| Phase | Increment | Effort | Depends on | Ships |
|---|---|---|---|---|
| **P1** | **A** — seed determinism test + honesty doc | **S** | — (plumbing already DONE) | A pinned, regression-proof seed contract + an operator-facing "seed is a hint" doc. Low risk, immediate. |
| **P2** | **B** — `RecordingHarness` + `HarnessReplay` + `replay` kind + registry wiring | **M** | A (so fixtures carry a meaningful seed in the key) | Offline exact reproduction; the keystone capability. |
| **P3** | **C** — `agent eval` subcommand + reporters + exit codes | **M** | B (replay registry) | CI/regression suite runner; the user-facing payoff. |

**Total: S + M + M.** No XL. No CRD field additions (one enum value, hand-edited). The decorator-friendly `Harness` interface and the already-wired seed make this genuinely incremental.

**Dependency on sibling specs (soft, not blocking):**
- [response-richness](response-richness.md) — needed before fixtures faithfully capture `ToolCalls` (§6.4). B/C ship without it; tool-call replay arrives free when it lands.
- [agent-hermes](agent-hermes.md) §4.1 (`/v1/responses`) — the actual *producer* of Hermes tool calls; same forward-compat note.
- run-governance (future) — a natural consumer (an eval gate as a quality/budget guard); not required to build this.

---

## 9. Test plan

### Unit (`go test ./...`, both modules green)

- **Seed (A):** `hermes_test.go` + `openaillm/client_test.go` — `Seed=42` ⇒ body has `seed:42`; `Seed=0` ⇒ body omits `seed`. (Pins the already-landed wiring.)
- **fixtureKey:** raw `"hi"` and `{"prompt":"hi"}` hash **equal** (post-`promptFromInput` normalization); different seeds ⇒ different keys; different model ⇒ different keys; whitespace-only prompt diffs normalize equal.
- **RecordingHarness:** record a stub `Response` → `readFixture` returns it byte-identical; an `Inner` error ⇒ **no** fixture written; a write failure ⇒ run still returns the live result (recording non-fatal).
- **HarnessReplay:** hit ⇒ returns fixture `Response`; **miss ⇒ error** (assert the error mentions the key, assert no network); empty dir ⇒ clear error.
- **ValidateHarness:** `kind=replay` passes without `http.url`; round-trips through `Valid()`.
- **eval (C):** a `testdata/` 2-case suite — one PASS (output matches `expected.json`), one CHANGED (fixture present, output differs) → assert exit code 1 and the `json` report classifies each case.

### e2e (the cftest single-node k0s box exists for live verification — project memory: *Hermes + z.ai e2e GREEN*, *agentctl deploy: CF-tunnel validated*)

1. **Record against the live Hermes path.** On cftest, run the proven `.claude/hermes-e2e.yaml`-style agent with `SMOL_AGENTS_RECORD_DIR` set (locally via `agent run`, or as a one-off pod env) → capture fixtures for a deterministic prompt (e.g. `fib(12)`).
2. **Replay offline.** `agent eval --mode replay` against those fixtures **with no network** (assert egress is provably zero — run it in an environment with the provider blocked) → identical `RunResult.Output` (the known-good `{"fib12":144,...}` shape from memory).
3. **Miss is loud.** Replay a *different* prompt against the same fixtures → run fails with a `replay miss` error, **not** a network call (verify no provider hit in logs).
4. **Seed stability (best-effort, informational).** Two live runs at the same seed against a seed-honoring backend → note whether output matches; document the result as evidence for the "best-effort" framing (not a hard assertion — providers may ignore seed).

---

## 10. Risks & open decisions

**Risks**

- **Seed gives weak guarantees on the live path.** Providers treat `seed` as best-effort; Hermes's server-side loop is uncontrollable. *Mitigation:* frame A honestly; point operators at replay (B) for exact reproduction. This is the core message of the determinism doc.
- **Fixture key under-specifies the request.** It ignores `temperature`/`top_p`/other `BODY_*`, so a fixture recorded at one temperature replays for another (§4.2 caveat). *Mitigation:* document; the eval author controls both sides. *Follow-up:* if it bites, fold a canonicalized `BODY_*` subset into the key behind a `schemaVersion` bump.
- **Fixtures can capture secrets in `Output`.** Same exposure as `Status.Output` today, no worse, but a fixtures dir is more likely to be committed. *Mitigation:* doc ("don't record against prod secrets"); the key/request already exclude `req.Env`; `RedactionPolicy` would gate it if built.
- **Stale fixtures silently pass.** A fixture recorded against an old prompt keeps replaying after the prompt changes — but because the *key includes the prompt*, a changed prompt MISSES (loud) rather than returning a stale answer. The real stale-risk is `expected.json` drifting from intent — that is the eval author's discipline, surfaced as `CHANGED`.

**Open decisions for the maintainer**

1. **Replay selection mechanism.** Two ways to force replay in `eval`: (a) a **replay registry** (every kind → `HarnessReplay`) keeping the authored `kind` so the fixture key matches what was recorded — *recommended*; or (b) rewriting `agent.Spec.Harness.Kind = replay` per case. (a) preserves key fidelity and needs no spec mutation. Confirm (a).
2. **Where the eval suite & fixtures live.** In-repo `testdata/eval/<name>/` (versioned, CI-gated) vs an external artifact store. Recommend in-repo for the first suites (small, diff-able); revisit if fixtures grow large.
3. **`expected.json` comparison strictness.** Exact `output` match vs `outputContains` substring vs phase-only. Recommend: phase always; `output` exact when present, with `outputContains` as an opt-in for whitespace-tolerant cases. Confirm the default.
4. **Loop-mode (`openaillm`) replay — in scope later?** This spec replays at the *harness* seam only. A loop agent's live `openaillm` calls are not fixture-able yet (tests use `FakeLLM`). Do we want a parallel record/replay `Doer` for `openaillm` (so `Mode=loop` agents get the same offline eval), or is `FakeLLM` + harness-replay sufficient? Recommend deferring until a loop-mode agent is e2e-green (harness mode is the proven path today).
5. **Should `eval` ever gate CI automatically?** I.e. wire it into `make test` / a CI job. Recommend: ship the subcommand first; wire a CI gate once a stable suite exists (avoids a flaky gate on day one). This dovetails with run-governance (future).

---

**Bottom line.** The seed half of O2 is already done — this spec pins it with a test and stops overselling it. The real deliverable is the **record/replay harness** (a thin decorator + a `kind=replay` that fails loud on miss, keyed on `sha256(Instructions+Input+Seed+Model)`) and an **`agent eval` runner** that drives the genuine `RunOnce` datapath with the network pinned. It is S+M+M, adds one enum value and zero CRD fields, reduces egress surface in CI (replay makes no network call), and keeps recording behind an env flag the operator never sets in production. The one honest limitation — `ToolCalls` will not round-trip until [response-richness](response-richness.md) ships a producer — is structural, forward-compatible, and stated plainly rather than papered over.
