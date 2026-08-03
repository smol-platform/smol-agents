# Benchmarking + Verification Platform (`agentbench`)

> **Status: DESIGN — 2026-06-03 (against v0.2.x source, module `github.com/smol-platform/smol-agents`, HEAD `0f64158`).**
> A platform that deploys a suite of full-stack agents against **legitimate LLM backends** (z.ai GLM-4.6, never fakes/stubs), **benchmarks** them (latency, tokens, cost, throughput, concurrency, cold-start, scale-to-zero, isolation-pass, egress-pass), and **verifies** that tools, plugins/harnesses, filesystems, and secrets actually work — each with a *real oracle* that an LLM cannot satisfy by bluffing.
>
> It reuses the `AgentRun`/`AgentSession`/`agentgateway` datapath and the `test/e2e/fullstack` ring/coverage model; it does **not** re-implement pod builders or containment. Every `[BLOCKED]` item names the spec that unblocks it. Companion to [`agent-runtime-fit-analysis-v0.2.0.md`](../research/agent-runtime-fit-analysis-v0.2.0.md).
>
> **Cross-links.** Design: [`framework-enhancements`](framework-enhancements.md), [`agent-platform`](agent-platform.md), [`durable-session-architecture`](durable-session-architecture.md), [`harness-authoring`](harness-authoring.md), [`tool-kinds-roadmap`](tool-kinds-roadmap.md), [`agentnetwork-agentpolicy-interaction`](agentnetwork-agentpolicy-interaction.md), [`secrets-broker-credential-backends`](secrets-broker-credential-backends.md), [`custom-agent-images`](custom-agent-images.md), [`agentfs-fuse-plugin`](agentfs-fuse-plugin.md). Research: [`agentfs-versioning`](../research/agentfs-versioning.md). Features: [`verification`](../features/verification.md), [`egress-credentials`](../features/egress-credentials.md). Specs: see §7. Runbooks: [`secretless-egress`](../runbooks/secretless-egress.md), [`e2e-l2`](../runbooks/e2e-l2.md), [`k0s-local-cluster`](../runbooks/k0s-local-cluster.md).

---

## §1 — Goal, scope, and what is verifiable today

### 1.1 Goal

Prove, with real LLMs and repeatable metrics, that the platform's four user-facing subsystems do what they claim — and be **honest, fail-closed, and anti-stale** about the parts that are stubbed. The platform must:

- **Deploy** a suite of full agents (`Agent` + `ModelProvider` + optional `Tool`/`AgentSession`/`AgentRun`) exercising every wired axis.
- **Benchmark** PERF metrics (latency p50/p95/p99, tokens, cost, throughput, concurrency, cold-start, scale-to-zero) separated from **CORRECTNESS oracles** (boolean pass/fail).
- **Verify** tools, harnesses (plugins), filesystems, and secrets with oracles whose answer is *impossible to produce without the feature actually running*.

### 1.2 The three boxing facts (design only around these)

Three verified facts constrain everything below. Designing tests against capabilities that cannot work would manufacture false passes; instead each is designed *and* parked behind `--allow-blocked`.

1. **Loop-mode tools are UNWIRED.** Tool specs are never marshaled into the run pod — `operator/internal/builders/runspec.go` writes `agent.json`/`run.json`/`provider.json` only (no `tools.json`; constants at `runspec.go:29-31`). The executor is built with an empty `Invokers` map (`pkg/agentruntime/runonce.go:65-69`), and the dispatch path rejects every LLM tool call with `no invoker for kind %q` recorded as `StepToolCallRejected` (`pkg/agentruntime/executor.go:260-262`). A loop agent's tool call fails *mid-run*, with no apply-time error. So "verify tools" today = verify **harness-embedded** tools (Hermes runs its own ~64-tool loop) + document the loop path as `[BLOCKED — loop-mode-tools-and-invokers]`.

2. **There is NO structured tool-call evidence today — not even for Hermes.** `Response.ToolCalls` is forward-compat, populated by **no harness** (`pkg/agentruntime/harness/iface.go:52,64-65`); the aggregate `Usage.ToolCalls int32` is incremented **only** on the loop-mode executor path (`executor.go:281,296,307`), never on the harness path (`runonce.go`). **`AgentRun.status.usage.toolCalls` is therefore `0` for a Hermes run even when the gateway ran five tools.** Any oracle asserting `usage.toolCalls > 0` for a harness agent tests a structurally-always-0 value — **never write it**. Tool execution is proven from *side-effects + output text*, not `status.steps[].toolCalls`.

3. **Token/cost is Hermes-only; CLI kinds report 0 by contract.** `Output` is always set; `TokensIn`/`TokensOut` are `0` for ALL CLI kinds (`iface.go:48-62`); only Hermes parses the OpenAI `usage` block (`pkg/agentruntime/harness/hermes.go:265-276`). Cost/token gates are **N/A (not "fail")** for CLI kinds until [`response-richness`](../specs/response-richness.md) / [`agent-claude-code`](../specs/agent-claude-code.md) §4.1 land.

### 1.3 Verifiable-today vs `[BLOCKED]`

| Dimension | Verifiable **today** | `[BLOCKED]` (future tier) → unblocking spec |
|---|---|---|
| **Tools** | Hermes harness-embedded tool execution (output + side-effect oracles); loop-mode **rejection** (negative oracle) | loop-mode tool *success* (http/mcp/function/agent invokers) → [`loop-mode-tools-and-invokers`](../specs/loop-mode-tools-and-invokers.md); Hermes structured trace → [`agent-hermes`](../specs/agent-hermes.md) §4.1; A2A → [`agent-to-agent-invoker`](../specs/agent-to-agent-invoker.md) |
| **Plugins/harnesses** | `hermes` (real tokens), `claude-code` (text + file side-effects) — both proven live | `codex`/`aider`/`goose` first-run (build-verified, unproven); CLI tokens/cost → [`response-richness`](../specs/response-richness.md), [`agent-claude-code`](../specs/agent-claude-code.md) §4.1; conversational resume → [`agent-claude-code`](../specs/agent-claude-code.md); `pi` = false friend (Inflection, not pi-mono) → [`agent-pi-mono-http`](../specs/agent-pi-mono-http.md) |
| **Filesystems (AgentFS)** | input materialization, cross-pod restore, SIGTERM-survival, kopia/tar snapshot integrity, retention, DB-exclusion (kopia) | files-OUT manifest → [`artifact-egress`](../specs/artifact-egress.md); WAL streaming/sub-backup RPO (driver); Diff in status (status wiring) |
| **Secrets** | static lease + reach (401→200), non-leak grep, blindness gradient (HTTP-blind / CLI-not-blind), SO_PEERCRED bound, TTL cap | declarative dynamic mint → [`dynamic-credential-backends`](../specs/dynamic-credential-backends.md); live TraT sender-constraint (same); proxy injection on run datapath → [`agentnetwork-datapath-enforcement`](../specs/agentnetwork-datapath-enforcement.md) |
| **Containment/policy** | egress metadata-block (NetworkPolicy), budget caps, kata microVM isolation (AWS-metal only) | AgentPolicy enforcement → [`agentpolicy-enforcement`](../specs/agentpolicy-enforcement.md); AgentNetwork per-resource egress → [`agentnetwork-datapath-enforcement`](../specs/agentnetwork-datapath-enforcement.md); bit-exact replay → [`determinism-and-replay`](../specs/determinism-and-replay.md) |

---

## §2 — Architecture

### 2.1 Decision: the runner

**A new Go binary `cmd/agentbench`, driving the cluster via `controller-runtime` client + the `agentgateway` HTTP API, with a thin Makefile target per ring.** Rejected alternatives:

| Option | Verdict | Why |
|---|---|---|
| **`cmd/agentbench` Go binary** | **Chosen** | Already have `controller-runtime`, `pure.RunStatus`, `pkg/sessionqueue` types in-module — the runner unmarshals `kubectl get agentrun -o json` into the *exact same* `pure.RunStatus` the controller folds, so the oracle code is type-checked, not string-scraped. Concurrency (N parallel `AgentRun`s) is `errgroup`, not `xargs`. Reuses `test/e2e/fullstack/shared` env-probe + capability gating. |
| k8s `Job` per case | Rejected as primary | A Job can't watch *other* objects' status or aggregate cross-run percentiles; you'd still need a collector. Kept as an **optional execution mode** (`--driver=job`) for air-gapped clusters where the runner can't hold a watch. |
| Makefile + `kubectl` + `jq` | Rejected as primary | The metrics oracle (tokens-only-on-Hermes, kata kernel diff, percentile math) is too stateful for shell and brittle against the ~3 KiB termination-message truncation (`cmd/agent/run.go:94`). Kept as the **entry point** (`make bench-l1`) that just `go run`s the binary, mirroring `make e2e-l1`. |

The runner is **declarative-in, JSON-out**: it reads a *bench plan* (a directory of `*.bench.yaml` + a `plan.yaml` manifest), reconciles the agent fleet, submits workloads, collects results into a single `results.json`, renders a Markdown report, and tears down. It is **idempotent and fail-closed**: a missing oracle, a `BLOCKED` tier run without `--allow-blocked`, or a cluster missing a required capability is a hard error, never a silent pass — mirroring the e2e `Caps.Has` gate (`test/e2e/fullstack/shared/caps.go`, `env.go`).

### 2.2 Lifecycle (eight phases)

```
agentbench run --plan ./bench/plans/core --kubeconfig $KC --tier perf --out ./results
  1. LOAD      parse plan.yaml + *.bench.yaml → []BenchCase; validate oracles + tiers + plan digest
  2. GATE      probe cluster Caps (runtimeclass kata-fc? metadata-blockable? gateway up? hermes up? s3/minio?)
               → skip/abort cases whose requiredCaps aren't met (honest split, §5)
  3. DEPLOY    apply the fleet: ModelProvider, Agent(s), Tool(s), MemoryStore, AgentSession
               (one namespace per plan run: agentbench-<runID>, GC'd at teardown)
  4. WARM      (optional) fire 1 throwaway AgentRun per agent to separate cold-start from steady-state
  5. SUBMIT    per case: create AgentRun CR  OR  POST gateway turn; honor concurrency/repeat knobs
  6. COLLECT   watch AgentRun→terminal (or poll gateway result); unmarshal pure.RunStatus;
               run the case's oracle; record sample {latency, tokens, cost, pass, evidence}
  7. REPORT    aggregate samples → percentiles + pass/fail GATES → results.json + report.md
  8. TEARDOWN  delete the run namespace (cascades pods/CRs/NetworkPolicies); always runs (defer)
```

Latency is computed from `status.startedAt`/`status.endedAt` (`pure.RunStatus`, `pkg/agentmodel/v1/types.go:316-317`) — **wall-time the controller observed**, not the runner's clock, so it is measured at the same boundary regardless of `--driver`. Tokens come from `status.usage.tokens` (`Usage`, `pkg/agentmodel/v1/budget.go:36`); this is **0 for all CLI harnesses by contract** (`iface.go:48-62`) and the runner records that honestly rather than failing.

### 2.3 Components

| Name | Kind | File (new unless noted) | Responsibility |
|---|---|---|---|
| `agentbench` | Go binary `main` | `cmd/agentbench/main.go` | CLI entry: `run`, `lint`, `report`; flags `--plan --tier --driver --concurrency --repeat --out --allow-blocked --record` |
| `BenchCase` / `BenchPlan` | types | `pkg/agentbench/plan.go` | Decode + validate `*.bench.yaml` and `plan.yaml`; resolve `tier`, `requiredCaps`, oracle discriminated union; compute plan digest |
| `Fleet` | deployer | `pkg/agentbench/fleet.go` | Apply/await-Ready the CR fleet via `controller-runtime` client; one namespace per run; owner-refs for GC |
| `Driver` (iface) | seam | `pkg/agentbench/driver.go` | `Submit(ctx, case) (handle)` + `Collect(ctx, handle) (RunStatus, Sample)`; impls below |
| `runDriver` | impl | `pkg/agentbench/driver_run.go` | Create `AgentRun`, watch to terminal, read `pure.RunStatus` from `.status` |
| `gatewayDriver` | impl | `pkg/agentbench/driver_gateway.go` | `POST /v1/sessions/{ns}/{name}/turns?wait=` + `GET …/turns/{id}`; result body **is** a `pure.RunStatus` (gateway folds the worker's, `cmd/agentgateway/main.go:39-45`) |
| `jobDriver` | impl | `pkg/agentbench/driver_job.go` | Air-gapped: run-as-Job, collect from a result ConfigMap |
| `Oracle` (iface) | seam | `pkg/agentbench/oracle.go` | `Check(RunStatus, CollectCtx) (Verdict, Evidence)`; registry keyed by oracle `kind` |
| oracle impls | impls | `pkg/agentbench/oracles/*.go` | `output_match`, `output_jsonpath`, `tool_observed`, `tool_rejected`, `fs_roundtrip`, `secret_reach`, `secret_absent`, `isolation_kernel`, `egress_metadata_blocked`, `budget_terminated` (§2.5) |
| `Metrics` | collector | `pkg/agentbench/metrics.go` | Per-sample `{latencyMs, tokens, costUSD, coldStart, phase}`; aggregate p50/p95/p99, throughput, error-rate |
| `Caps` | probe | `pkg/agentbench/caps.go` | Reuse `test/e2e/fullstack/shared` env probe: kata RuntimeClass present? gateway/hermes reachable? metadata range blockable? s3/minio? |
| `Report` | renderer | `pkg/agentbench/report.go` | `results.json` (§2.4) + `report.md` (tables + GATE verdicts) |
| bench plans | data | `bench/plans/{core,scale,isolation,future}/*.bench.yaml` | The shipped suites (§4, §5) |
| `make bench-l1` / `bench-l2` | Makefile | `Makefile` (edit) | Thin wrappers: `go run ./cmd/agentbench run --plan … --tier …` mirroring `e2e-l1`/`e2e-l2` (`Makefile:88-122`) |
| coverage gate | test | `pkg/agentbench/coverage_test.go` | Mirror `test/e2e/fullstack/coverage.go:13`: every bench-case ID maps to an oracle impl; CI fails on orphan |

**Reuse, not rebuild:** `pure.RunStatus`/`Usage`/`Step` (`pkg/agentmodel/v1`), `sessionqueue.SessionKey` (`pkg/sessionqueue`), the kata kernel-diff probe (`test/e2e/fullstack/shared/scenarios.go:689-729`), the metadata-block assertion pattern, and the `Coverage` map idiom (`coverage.go:13`).

### 2.4 Workload format + results schema

A `plan.yaml` lists the fleet + which cases to run; each `*.bench.yaml` is one case. (Full examples in §4.) The durable artifact is `results.json`; `report.md` is rendered from it.

```json
{
  "$schema": "agentbench/v1",
  "runId": "20260603T1812Z-7f3a",
  "planDigest": "sha256:…",
  "plan": "core",
  "tier": "perf",
  "cluster": { "name": "cftest", "runtime": "runc", "caps": ["gateway","hermes","metadata-block","s3"], "node": "159.69.185.87" },
  "backend": { "kind": "hermes", "model": "glm-4.6", "endpoint": "http://hermes-gateway:8642/v1/chat/completions" },
  "startedAt": "2026-06-03T18:12:04Z",
  "finishedAt": "2026-06-03T18:19:41Z",
  "cases": [
    {
      "id": "BC-HERMES-TOOL-1", "agentRef": "bench-hermes", "tier": "correctness",
      "harnessKind": "hermes", "samples": 5, "seed": 42, "nonce": "3f2a9c…",
      "oracle": { "kind": "tool_observed", "verdict": "pass",
                  "evidence": "output contains \"4\"; echo-server log shows GET from pod IP" },
      "metrics": {
        "latencyMs": { "p50": 1840, "p95": 2310, "p99": 2310, "min": 1702, "max": 2310 },
        "tokens": { "in": 312, "out": 47, "total": 359, "perSampleMean": 71.8 },
        "costUSD": 0.00021, "errorRatePct": 0.0
      },
      "gates": [
        { "name": "oracle.pass",      "op": "eq",  "want": true, "got": true,  "pass": true },
        { "name": "latency.p95.ms",   "op": "lte", "want": 5000, "got": 2310, "pass": true },
        { "name": "tokens.total.max", "op": "lte", "want": 800,  "got": 359,  "pass": true }
      ],
      "pass": true
    },
    {
      "id": "BC-LOOP-TOOL-REJECT-1", "agentRef": "bench-loop-http", "tier": "correctness",
      "harnessKind": "loop",
      "oracle": { "kind": "tool_rejected", "verdict": "pass",
                  "evidence": "steps[0].kind=ToolCallRejected; error=\"no invoker for kind \\\"http\\\"\"" },
      "blocked": { "reason": "loop-mode tools unwired", "unblockSpec": "docs/specs/loop-mode-tools-and-invokers.md" },
      "metrics": { "latencyMs": { "p50": 410 }, "tokens": { "total": 0 } },
      "gates": [ { "name": "oracle.pass", "op": "eq", "want": true, "got": true, "pass": true } ],
      "pass": true
    }
  ],
  "summary": { "total": 14, "passed": 12, "failed": 1, "skipped": 1, "blocked": 3,
               "gateFailures": ["BC-CLI-LAT-1:latency.p95.ms"] },
  "verdict": "FAIL"
}
```

`report.md` renders: a cluster/backend header, a per-tier table (`ID | harness | oracle | verdict | p50 | p95 | tokens | cost | gate`), a `BLOCKED` appendix (case → unblock-spec), and a `SKIPPED (cap missing)` appendix — Markdown table style matching `docs/design/*`.

### 2.5 Oracles (correctness) vs metrics (perf) + pass/fail gates

A hard line: an **oracle** returns `pass|fail|skip|blocked` and is the *only* thing that gates correctness; a **metric** is a number that gates only against an explicit threshold. A fast wrong answer fails. Every oracle names the *proof* and the *false-oracle trap* avoided.

| Oracle `kind` | What it proves | Evidence source | False-oracle trap avoided |
|---|---|---|---|
| `output_match` / `output_jsonpath` | LLM produced the required answer | `status.output` (`types.go:321`) | A canned/stub answer — **mitigated** by a *value only obtainable by reasoning over the input* (a fresh per-run nonce materialized via `Inputs`, not in the prompt) |
| `tool_observed` | a tool actually executed (Hermes only today) | `status.steps[].kind==Observation` **and** an out-of-band trace (echo-server log / AgentFS snapshot) | Counts a *rejection* as a call — guarded by `kind==Observation`, **not** `ToolCallRejected`; never gates `usage.toolCalls` on the harness path (§1.2 fact 2) |
| `tool_rejected` | loop-mode tools are unwired (negative oracle) | `status.steps[].kind==ToolCallRejected`, `error=="no invoker for kind …"` (`executor.go:260-262`) | Treats unwired-by-design as a bug — this oracle **asserts the rejection happens** (proof the seam is dead) |
| `fs_roundtrip` | a file written in run N is read in run N+1 / survives pod kill | run-2 `status.output` echoes run-1's SHA256 of `$WORKDIR/<f>` | "file exists" without content match — requires **byte/SHA equality**, not presence |
| `secret_reach` | agent reached a credential-gated endpoint | `status.output` reflects the gated 200 body; endpoint **401s without the lease** | Reaching a public mirror — paired with a no-secret negative control that must 401 |
| `secret_absent` | the secret never leaked | grep run-pod logs + `status.output` + run-spec — **must be absent** on the Hermes path; **expected-PRESENT on CLI** (§1.2, §3.4) | False "blind" claim on CLI — oracle is **kind-aware**: absent for `hermes`, asserts present + flags `not-blind` for `claude-code`/`generic-cli` |
| `isolation_kernel` | pod ran in a kata microVM, not shared kernel | in-pod `uname -r` ≠ kubelet `nodeInfo.kernelVersion` (reuse `scenarios.go:689-729`) | Kata silent-fallback to runc — **fail-closed on AWS-metal**: equal kernels = FAIL; auto-SKIP only on cftest where KVM is absent |
| `egress_metadata_blocked` | the default-deny NetworkPolicy holds | in-pod `curl 169.254.169.254` times out **and** `:443` to allowed host succeeds | CNI not enforcing — requires the **paired** positive (443 ok) + negative (metadata blocked) |
| `budget_terminated` | a budget axis caps the run | `status.terminationReason` prefix `budget:` (`executor.go:248-249`) | A run that finished naturally — asserts the `budget:` prefix |

**Metrics** (gated only by explicit thresholds): `latencyMs p50/p95/p99` (from `startedAt`/`endedAt`); `tokens` (Hermes-real / CLI-zero); `costUSD` (Hermes usage block only — **null** for CLI, never fabricated); `throughputRunsPerMin`; `concurrency` (max in-flight, terminal-bounded); `coldStartMs` (first-run `creationTimestamp→startedAt` minus warm baseline); `scaleToZeroSec` (gateway idle→0 via `AgentSession.idleTimeoutSeconds`); `errorRatePct` (non-`Succeeded` terminal / total).

**Gate semantics.** A case `pass` = (oracle.verdict ∈ {pass, blocked-expected}) **AND** all metric gates pass. Plan `verdict` = `FAIL` if any non-skipped case fails. `skipped` (cap missing) does **not** fail the plan but is surfaced loudly. **A blocked case that unexpectedly *succeeds* is a FAIL** — it means a spec landed and the bench plan is stale (this is the anti-staleness mechanism; the negative oracle flips to positive with no harness rewrite).

### 2.6 Repeatability

- **Seed.** `AgentRunSpec.Seed` (`types.go:230`) is forwarded to harnesses that expose one (Hermes: `hermes.go:108-110`). **Honest caveat:** GLM-4.6 does not guarantee bit-exact determinism (`seed` is a hint, [`determinism-and-replay`](../specs/determinism-and-replay.md)); the platform compensates with `samples: N` (≥3 enforced for any case with a metric gate) and oracles that assert *semantic* correctness (`output_jsonpath` on a computed value / a fresh nonce) rather than string equality. Latency/token metrics are reported as **distributions (p50/p95)**, never single shots.
- **Plan digest.** `sha256(plan.yaml + cases)` is recorded in `runId`/`results.json` so a run is auditable and re-describable.
- **`--record`** persists each sample's full `pure.RunStatus` + the resolved bench plan into `results.json`. True record/replay (deterministic re-execution against a captured transcript) is **[BLOCKED — [`determinism-and-replay`](../specs/determinism-and-replay.md)]**; it drops in behind the same `Driver` seam as a fourth driver when the spec lands.

---

## §3 — Verification matrix per dimension

Each dimension below pairs an **output-encoded** oracle with a **side-effect / dual-channel** oracle wherever possible, so a hallucinated answer is caught by a missing out-of-band trace. A shared rig (`test/verify/`) injects a **fresh per-run nonce** (`openssl rand -hex 16`) plus a randomized arithmetic challenge so a stub/replay cannot satisfy a value it never saw.

### 3.1 Tools

**Execution reality.** Loop-mode tools never execute (§1.2 fact 1). Hermes tools DO execute *server-side inside the gateway* and return one finished assistant string — the executor never sees the tool calls, only their *result*. There is no structured tool-call evidence (§1.2 fact 2), so today the only place a tool's effect appears is `status.output`; structured-trace oracles are designed but `[BLOCKED]`.

| ID | Agent config | Input (gist) | Oracle (proof the tool ran) | Metric | Backend | Tier |
|---|---|---|---|---|---|---|
| **T-LIVE-1** shell-nonce | hermes; nonce injected to gateway env | run `printf $TOOL_VERIFY_NONCE`, return it | `output.nonce == $NONCE` (fresh 128-bit; impossible without the shell tool) + gateway-log grep (soft) | wall-clock; tokens | z.ai GLM-4.6 (Hermes) | LIVE |
| **T-LIVE-2** fetch-echo | hermes + in-cluster echo-server | GET our echo endpoint, return its nonce | `output.fetched_nonce == $ECHO_NONCE` **and echo-server access log shows the GET** (dual-channel, decisive) | tool round-trip latency | z.ai GLM-4.6 | LIVE |
| **T-LIVE-3** file-write→AgentFS | hermes-fs (agentfs/kopia) | write `tool-proof.txt = nonce` | **AgentFS snapshot restore: file == $NONCE** | snapshot size/dur | z.ai GLM-4.6 | LIVE¹ |
| **T-LIVE-4** hash/compute | hermes | exact product + SHA-256 of a nonce'd string | independent recompute, exact match (SHA-256 = load-bearing) | tokens | z.ai GLM-4.6 | LIVE |
| **T-LIVE-5** metadata-block | hermes | tool GET `169.254.169.254` | output shows timeout, **not** creds (NetworkPolicy caged, `run_sandbox.go:39`) | — (security gate) | z.ai GLM-4.6 | LIVE |
| **T-LIVE-6** token-acct | hermes (reuse 1/2/4) | any tool run | `usage.tokens>0` from real OpenAI usage block (NOT `toolCalls`) | tokens p50/p95 | z.ai GLM-4.6 | LIVE |
| **T-NEAR-1** cc-edit | claude-code + agentfs | create `solution.txt = nonce` | **AgentFS restore: file == $NONCE** (subprocess writes to the mount) | wall-clock | z.ai Anthropic (glm-4.6) | NEAR² |
| **T-NEAR-2** cli-read-input | generic-cli + runInputs | read materialized `secret-nonce.txt` | `output` contains $NONCE (`MaterializeInputs`→tool, `runonce.go:124-149`) | wall-clock | any/echo | NEAR³ |
| **T-FUT-0** loop-reject | loop + http Tool ref | call the tool | `StepToolCallRejected: no invoker for kind "http"` + **no `tools.json`** in `<run>-runspec` ConfigMap | reject=100% | zai-openai | **provable today (gap proof)** |
| **T-FUT-1** http-invoker | loop + http Tool → echo-server | call echo-tool w/ nonce | `StepObservation.ToolCalls[].Result == nonce` + echo-server log | per-tool `DurationMs`; `usage.toolCalls` | zai-openai | `[BLOCKED — loop-mode-tools T1+T3]` |
| **T-FUT-2** mcp-invoker | loop + mcp Tool → MCP server | use mcp tool | server sees `initialize→tools/list→tools/call` + `Result==CallToolResult.content[]` | init+call latency | zai-openai | `[BLOCKED — loop-mode-tools T4]` |
| **T-FUT-3** fn-invoker | loop + function Tool | computation | `StepObservation.Result == fn output` | per-call latency | zai-openai | `[BLOCKED — loop-mode-tools T1-T2]` |
| **T-FUT-4** schema-reject | loop + http Tool, strict schema | args violating schema | `StepToolCallRejected: ErrInvalidArgs` pre-invoke (real JSON-Schema) | reject rate | zai-openai | `[BLOCKED — loop-mode-tools T2]` |
| **T-FUT-5** tool-budget | loop + http Tool, `maxToolCalls:2` | force ≥3 calls | `phase=Expired, reason=budget:toolCalls` after 2 invokes (`executor.go:246`) | calls-to-cap | zai-openai | `[BLOCKED — needs T-FUT-1]` |
| **T-FUT-6** a2a | loop + agent Tool | delegate to child | child `AgentRun` created + folded as observation | child latency | zai-openai | `[BLOCKED — agent-to-agent-invoker]` |
| **T-FUT-7** hermes-trace | hermes `api=responses` | T-LIVE-1 prompt | `status.steps[]` carry Hermes `function_call`/`_output`; `usage.toolCalls>0` | per-tool visibility | z.ai GLM-4.6 | `[BLOCKED — agent-hermes §4.1]` |
| **T-FUT-8** tool-cred-mint | loop + http Tool w/ `Auth.secretRef` → GitHub | list repos via the github tool | outbound carries broker-minted token; token absent from output/logs | mint latency | zai-openai | `[BLOCKED — loop-mode-tools §5.2 + dynamic-credential-backends]` |

¹ Downgrades to **NEAR** + `[BLOCKED — workspace not shared with gateway]` if the Hermes gateway does not mount the run pod's AgentFS volume (Hermes file tools run in gateway process space). CLI harnesses (T-NEAR-1) avoid this — the harness *is* the run-pod process.
² `claude-code` text path proven 2026-06-02; the edit-tool→AgentFS join is unproven live. Structured trace `[BLOCKED — agent-claude-code §4.1]`.
³ Unit-proven via `test-cat-harness`; live promotion outstanding.

> **Anti-staleness:** when `loop-mode-tools-and-invokers` lands, T-FUT-0's negative oracle starts **failing** (no rejection occurs) — the signal to flip it to a positive `tool_observed` case. T-FUT-1..8 carry exact assertions ready to run the moment their spec lands.

### 3.2 Plugins / harnesses

**Contract ceiling** (`iface.go:48-65`, authoritative): CLI kinds report `Tokens=0`/`ToolCalls=[]`; only Hermes parses real usage. A harness is called **exactly once** per AgentRun; its plan-act-observe loop is invisible. So oracles read **file side-effects + output text + (Hermes only) real tokens**, never structured records.

| Tier | Kinds | Meaning |
|---|---|---|
| **T1 — PROVEN-LIVE** | `hermes` (z.ai GLM-4.6), `claude-code` (z.ai Anthropic) | end-to-end green on cftest with a real LLM |
| **T2 — BUILD-VERIFIED-UNPROVEN** | `codex`, `aider`, `goose` | bundle image builds + `--version` passes; **zero live runs**; shared risk = backend **auth shape** |
| **T3 — UNIT-ONLY** | `generic-cli`, `generic-http` | injected-fake tests only; a real-backend smoke closes the seam |
| **NON-FIT** | `pi` | **false friend** — Inflection API, not pi-mono CLI; route pi-mono → `generic-cli` + custom image `[BLOCKED — agent-pi-mono-http]` |

| # | Kind | Agent shape (key fields) | Oracle (real-LLM + kind signal) | Tier |
|---|---|---|---|---|
| **H-HERMES** | hermes | `http.url=…/v1/chat/completions`, `HEADER_Authorization` secretRef, `HERMES_MODEL=glm-4.6` | `output` line-1 == `ACK-<NONCE>` + line-2 == correct product (real LLM); `usage.tokens>0` and `steps[0].{tokensIn,tokensOut}>0` (**Hermes-unique**, `hermes.go:265-276`); secret absent from status/logs (agent-blind) | T1 ✅ |
| **H-CC-1** | claude-code | bundle image; `ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic`, `ANTHROPIC_MODEL=glm-4.6`, `ANTHROPIC_AUTH_TOKEN` secretRef; `storage=agentfs/kopia` | `output` contains `done`; **`/var/agentfs/result.txt` == `ACK-<NONCE>`+product** (read back via a second run / kopia snapshot — proves the edit tool ran in-subprocess and AgentFS captured it); `usage.tokens==0` (contract witness, N/A not fail) | T1 ✅ |
| **H-CC-2** | claude-code | + `runInputs:[{path:app.py, inline:…}]` | input materialized (`runonce.go:124-149`); `app.py` diff contains the requested function | T1 ✅ |
| **H-CODEX** | codex | bundle `version` pinned; `OPENAI_BASE_URL`=z.ai OpenAI-compat, `OPENAI_MODEL=glm-4.6`, `OPENAI_API_KEY` secretRef | `codex exec "write fib(n)→fib.py"` → **`fib.py` exists** and `fib(12)==144`. **Auth-shape risk:** Codex may need a config file/login, not just env — diagnosable via captured stderr (`cli.go:55-77`) | T2 (first-run) |
| **H-AIDER** | aider | OpenAI-compat env; **requires `git init` in CWD** | edit a materialized `calc.py` → diff shows the change; `git log` shows aider's commit | T2 (first-run) |
| **H-GOOSE** | goose | `GOOSE_PROVIDER`/`GOOSE_MODEL` + provider key | `goose run --instructions "create hello.py → ACK-<NONCE>"` → `hello.py` runs, prints the nonce. **Risk:** may want `~/.config/goose/config.yaml` (materialize via `runInputs`, not inlined secrets) | T2 (first-run) |
| **H-GHTTP** | generic-http | flat `promptField`/`responseField` | **caveat:** `doHTTP` posts `{promptField: prompt}` (flat), **not** an OpenAI `messages` array → a raw chat-completions endpoint 400s. Use against a flat prompt/response API; for OpenAI shape use `hermes`. Oracle: extracted `responseField` contains the nonce | T3 |
| **H-GCLI** | generic-cli | `spec.command` + `promptFlag` | subprocess exit 0 + stdout contains the nonce (`test-cat-harness` proved file-read) | T3 |
| **H-PI** | pi | Inflection defaults | **SKIP (no legitimate backend)** with z.ai-only test keys; if an Inflection key exists it's just a `generic-http` flat-field nonce check. pi-mono coding → `generic-cli` `[BLOCKED — agent-pi-mono-http]` | NON-FIT |

**T2 first-run checklist (per kind):** (1) apply Agent + trivial AgentRun, watch `status.phase`; a bad flag/missing auth surfaces in `status.error` + captured stderr (`cli.go:55-77`). (2) On auth-shape mismatch: switch to the OpenAI-compat base / add a config file via `runInputs` — **never** inline secrets. (3) On green, promote to **T2-PROVEN** and record the exact endpoint + env shape that worked (feeds the per-kind spec's "What is BUILT").

### 3.3 Filesystems (AgentFS)

**Truth:** files **IN** + whole-workspace **snapshot/restore** are fully wired and live-provable; files **OUT** as a discoverable manifest is `[BLOCKED — artifact-egress]`. AgentFS is runtime-class-agnostic (EmptyDir + HTTPS sidecar, no FUSE/`mount(2)`), so kata adds nothing to correctness. **Oracle routing:** an AgentRun exposes only `Output`/`Steps` (no per-file status, ~3 KiB termination budget, `cmd/agent/run.go:94`), so any "agent reports a file's contents" oracle pushes the bytes/checksum through `Output`; the durable bytes are independently verified at the minio layer or by a read-back run.

Tiers: **L1-cftest** = Hetzner k0s arm64 + minio, runc; **Unit** = `go test ./pkg/agentfs/...`; **L2-metal** = isolation-only (not required for FS correctness).

| # | Case | Tier | Backend | Oracle (PASS iff) |
|---|---|---|---|---|
| **AFS-1** | Input materialization | L1-cftest | tar | `Inputs:[{path:seed.txt, inline:<nonce>}]` + "print seed.txt" → `Output` contains `<nonce>` (fresh UUID defeats hallucination) |
| AFS-1b | Perms + traversal guard | Unit | n/a | `TestMaterializeInputs*`: mode `0600`; `../x` and `/etc/x` rejected (`runonce.go:169-184`) |
| AFS-1c | Secret-sourced input not in spec | L1-cftest | tar | `secretRef` input; `<run>-runspec` ConfigMap has no token bytes; agent echoes a **derived hash** matching `sha256(secret)` (raw value never echoed) |
| **AFS-2** | Durability across runs | L1-cftest | kopia + tar | run A writes `nonce.txt`; run B (same Agent) reads it → `Output(B)` == A's nonce. **Both backends pass.** |
| **AFS-3** | Crash-survival (kill mid-run) | L1-cftest | kopia | run A writes then sleeps; `kubectl delete pod` (SIGTERM → native sidecar final backup within 120s grace); sidecar log `final backup … uploaded`; run B restores → matches |
| AFS-3b | Final-backup-on-shutdown | Unit | tar | `TestScheduler_BackupOnShutdown`: fake S3 gains 1 version on cancel; off without a bucket |
| **AFS-4** | Large-file round-trip (headline kopia-vs-minio) | L1-cftest | kopia | write 64 MiB `big.bin` (record sha256); force checkpoint; run B emits `sha256sum big.bin` → matches pre-snapshot |
| AFS-4b | kopia command + parse contract | Unit | kopia | `TestKopiaStore_Commands` (connect/create flags); `TestKopiaStore_DefaultRunStdoutOnly` (stdout/stderr separation) |
| AFS-5 | Restore mode: pointInTime | L1/Unit | kopia + tar | 3 checkpoints T0/T1/T2; `mode=pointInTime, pointInTime=<T1+ε>` → restored nonce == T1's |
| AFS-5b | versionID + IfMissing | Unit | tar | exact version fetch; `fail` vs `fresh` on empty bucket |
| AFS-6 | Retention GC | L1/Unit | kopia + tar | 5 checkpoints, `maxVersions=3` → exactly 3 newest remain |
| AFS-7 | S3 versioning gate | Unit | tar | `versioning:true` on unversioned bucket → `ErrVersioningOff` |
| AFS-8 | DB-exclusion (torn-write hazard) | L1/Unit | kopia | live `*.db/-wal/-shm` excluded from the kopia snapshot. **Real defect to assert:** the **tar** backend has NO exclusion (walks everything) — AFS-8 passes only for `backend=kopia`; for tar it documents the hazard |
| AFS-9 | EffectiveWorkingDir binding | Unit | n/a | harness CWD == AgentFS mount when `storage=agentfs` and no explicit CLI dir (`harness.go:291-304`) — regression for "wrote to /tmp, lost on exit" |
| AFS-10 | Diff between two checkpoints | L1-cftest | kopia | CLI `kopia diff` classifies added/modified/deleted. `RunStatus.Diffs` oracle is `[BLOCKED — status wiring]` |
| **AFS-F1** | Artifact egress (files OUT) | — | — | `[BLOCKED — artifact-egress]`: no CRD field/collector/fold. Negative oracle today: apiserver rejects `spec.artifacts.outputs[]` |
| AFS-F2 | WAL streaming RPO | — | — | `[BLOCKED — WAL driver]`: `WALInterval:0` hardcoded |
| AFS-L2 | kata-fc + AgentFS | L2-metal | kopia | confirms no kata interaction (AFS-2/3/4 expected identical). **Not required for correctness** |

**Metrics (≥10 runs/backend):** `snapshot-duration-ms` (gate: kopia ≤ tar on the 2nd+ snapshot of a mutated tree — dedup must win); `snapshot-size-bytes` logical+on-wire (gate: kopia on-wire delta < 20% of tar for a 1-file change in a 64 MiB tree); `dedup-ratio` (≥2× on a duplicated large file); `restore-duration-ms` (p95 cold 64 MiB ≤ 30s on cftest); `checkpoint-integrity` (**100%** sha256 match, hard); `restore-success-rate` (**100%**); `final-backup-within-grace` (**100%**, else crash-survival is a lie).

### 3.4 Secrets

**Wiring reality (v0.2.x):** only the **static-backend lease** path is wired end-to-end through the operator. **Dynamic mint** is implemented + unit-tested in `pkg/secrets` but the shipped `secret-proxy` never constructs `Dynamic/TraTVerifier/CredPolicy` (left nil; `buildBackend` accepts only `static`/stub-`vault`) — so mint cases are `[BLOCKED — dynamic-credential-backends]`. Oracle for every case = a real credential-gated target **plus** a fail-closed grep over logs/status/output.

| ID | Scenario | Path | Oracle | Status |
|---|---|---|---|---|
| **SEC.1-pos** | secretRef bearer → in-cluster gated echo `200` | Hermes/HTTP | target access log shows `200` from the agent pod; AgentRun output reflects the 200 body | wired |
| **SEC.1-neg** | same Agent, secretRef removed → `401` | Hermes/HTTP | `phase=Failed` / output carries the 401 (proves the gate is real) | wired |
| **SEC.2** | NON-LEAK grep over logs/status/output | all | **0 hits** of the cleartext across all container logs + `status` + termination-message (fail-closed); broker config **Secret** asserted present **and** is a `Secret`, not a ConfigMap | wired |
| **SEC.3a** | HTTP path agent-blind | Hermes/HTTP | `secretRef` → `Request.Env` `HEADER_*` → injected upstream by the harness client; value never enters model context (200 on wire + SEC.2 clean) | wired |
| **SEC.3b** | CLI secret-in-env (honest gap) | generic-cli | command `echo "SEEN=$MY_TOKEN"`; output **must contain** the value (`mergeEnv`, `cli.go:108-119`, uid 65532). **Inverted oracle:** exposure is *expected* — confirms CLI harnesses are NOT agent-blind. Broker logs still grepped and must stay clean | wired |
| **SEC.4a** | secretRef without a broker = hard error | runtime + operator | `Failed`, never silent-anonymous `Completed`; pinned by `TestExecutor_HarnessMode_SecretRefWithoutBrokerErrors` (`agentruntime: harness env … has a secretRef but no secret broker is configured`) | wired (unit-pinned) |
| **SEC.4b** | SO_PEERCRED binds the lease to uid 65532 | broker | `id -u`==65532 leases; un-granted name / mismatched uid denied (`server.go` `ErrUnauthorized`). UDS EmptyDir mounted **only** into `containers[0]` (defense in depth) | wired |
| **SEC.4c** | TraT sender-constraint (`req_wl==caller`) | broker mint | replay across workloads → `trat not bound to caller`. Live `[BLOCKED — dynamic-credential-backends]`; unit suite is the oracle today | code-verified |
| **SEC.5** | dynamic GitHub mint, agent-blind | proxy + broker | `env \| grep GITHUB` empty; direct `curl api.github.com` → 401; via proxy → 200; lease capped to `MaxLeaseTTL≤15m`; SEC.2 clean | `[BLOCKED — dynamic-credential-backends]` |
| **SEC.S3** | hard `MaxLeaseTTL≤15m` cap | broker | request TTL=1h; lease metadata shows `expiresAt-issuedAt==15m` | wired |

**Metrics:** `lease-latency-ms` p50/p95 (in-pod static backend is in-memory; p95 < 50ms, informational); `broker-socket-wait-ms` (p95 ≪ the 60s `waitForBrokerSocket` timeout); `non-leak-hits` (**hard gate: 0**); `attestation-deny-rate` (100% for un-granted/mismatched in the negative suite); `lease-ttl-cap-adherence` (always, hard 15m); `mint-latency-ms` `[BLOCKED]`.

---

## §4 — Full-agent suite matrix + sample CRs

Each row is a named, applied CR. Legend — Mode: `H`=harness, `L`=loop, `H³`=durable session (per-turn harness call). Storage: `-`/`kopia`/`tar`. Sec: `-`/`static`(lease)/`gated`(secretless mint). Tools: `-`/`hermes`(embedded). Egress: `cage`(default-deny)/`AN`(AgentNetwork allow-list)/`open`(control). Iso: `kata`/`runc`. Session: `1`(one-shot)/`dur`(AgentSession+gateway).

| # | Name | Mode | Kind | Storage | Sec | Tools | Egress | Iso | Session | Cluster | Proves | Oracle (real-LLM) |
|---|---|:--:|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|---|---|
| **A1** | `hermes-baseline` | H | hermes | - | static | - | cage | runc | 1 | cftest | Harness happy-path + **real token accounting** | `Usage.Tokens>0` AND `steps[].{TokensIn,TokensOut}>0`; `Output` contains the computed final line |
| **A2** | `hermes-tools` | H | hermes | - | static | hermes | cage | runc | 1 | cftest | **Harness-embedded tools execute** | un-guessable answer (sha256/large product); gateway pod logs show tool dispatch |
| **A3** | `claude-code-text` | H | claude-code | - | static | - | cage | runc | 1 | cftest | CLI-kind harness, opaque contract | image=`harness-claude-code:<tag>`; `Output`=text; `Usage.Tokens==0` (contract witness) |
| **A4** | `claude-code-fs-kopia` | H | claude-code | kopia | static | - | cage | runc | 1 | cftest | **AgentFS write + kopia → S3 (minio)** | run writes `/var/agentfs/out.txt`; kopia version exists in minio |
| **A5** | `claude-code-fs-restore` | H | claude-code | kopia | static | - | cage | runc | 1 | cftest | **Cross-pod persistence** | new pod restores A4; `cat out.txt` SHA256-equals A4's write |
| **A6** | `claude-code-fs-kill` | H | claude-code | kopia | static | - | cage | runc | 1 | cftest | **SIGTERM-survival (final-backup)** | delete pod mid-run; sidecar logs `final backup uploaded` < 120s; restore yields partial writes |
| **A7** | `claude-code-fs-tar` | H | claude-code | tar | static | - | cage | runc | 1 | cftest | Legacy tar parity | same write→restore as A4/A5, `backend: tar` |
| **A8** | `agentfs-inputs` | H | generic-cli (`cat`) | kopia | - | - | cage | runc | 1 | cftest | **Inputs materialization (F2)** | `runInputs[].inline`→file; `cat` echoes it; `Output`==bytes, perms `0600` (no LLM) |
| **S1** | `secretless-github` | H | hermes | - | gated | - | AN | runc | 1 | cftest | **Agent-blind secretless egress** | via proxy `200` from `api.github.com`; token absent from logs/status; direct `curl` → `401` |
| **S2** | `cli-secret-visible` | H | claude-code | - | static | - | cage | runc | 1 | cftest | **CLI-harness secret CAVEAT** (NOT blind) | secret in subprocess env; agent `env` reads it → secret *appears* in output (by-design contrast with S1) |
| **S3** | `lease-ttl-cap` | H | hermes | - | static | - | cage | runc | 1 | cftest | **Hard `MaxLeaseTTL≤15m`** | request TTL=1h → lease `expiresAt-issuedAt==15m` |
| **B1** | `budget-tokens` | H | hermes | - | - | - | cage | runc | 1 | cftest | **Token-budget → Expired** | `maxTokens` below need; `State==Expired`, `TerminationReason==budget:tokens` |
| **B2** | `budget-wallclock` | H | hermes | - | - | - | cage | runc | 1 | cftest | Wallclock cap | `maxWallClockSeconds=2`; ctx cancels; `TerminationReason==budget:wallclock` |
| **E1** | `egress-metadata-block` | H | generic-cli (`curl`) | - | - | - | cage | runc | 1 | cftest | **NetworkPolicy metadata veto** | `curl 169.254.169.254` → timeout; `curl https://<public>:443` → connects |
| **E2** | `egress-open` | H | hermes | - | - | - | open | runc | 1 | cftest | Containment-off control | no NetworkPolicy; metadata reachable — proves the cage blocks E1 |
| **K1** | `kata-isolation` | H | claude-code | kopia | static | - | cage | **kata** | 1 | **aws-metal** | **Real microVM isolation** | `RuntimeClassName==kata-fc`; in-pod `uname -r` ≠ host kernel; AgentFS file survives pod boundary |
| **D1** | `durable-hermes` | H³ | hermes | kopia | static | - | cage | runc | **dur** | cftest | **Durable session: memory + checkpoint** | 3 turns; turn-3 recalls turn-1's fact; conversation+usage checkpointed |
| **D2** | `durable-restart` | H³ | hermes | kopia | static | - | cage | runc | **dur** | cftest | **Worker crash-recovery** | delete worker between turns; replacement restores; turn-4 still has turn-1 context |
| **D3** | `durable-scale-zero` | H³ | hermes | kopia | static | - | cage | runc | **dur** | cftest | **Scale-to-zero on idle** | `idleTimeoutSeconds=60`; worker exits; next turn cold-starts + restores |
| **🍳 KS1** | `kitchen-sink-claude` | H | claude-code | kopia | static | - | cage | **kata** | **dur** | **aws-metal** | **Everything at once (CLI)** | durable (D1) ∧ AgentFS survives reschedule on metal (K1+A5) ∧ secret absent from `Status` (S2 caveat noted) ∧ `RuntimeClassName==kata-fc` |
| **🍳 KS2** | `kitchen-sink-hermes` | H³ | hermes | kopia | gated | hermes | AN | **kata** | **dur** | **aws-metal** | **Max density, blind path** | real `Usage.Tokens` (A1+A2) ∧ secretless `200`, token absent (S1) ∧ checkpointed conversation on kata metal (D1+K1) |
| **T1** 🔒 | `loop-tools-http` | L | — | - | static | http | cage | runc | 1 | cftest | `[BLOCKED]` loop HTTP tool | **Negative today:** `ToolCallRejected "no invoker for kind \"http\""`. Positive needs [`loop-mode-tools`](../specs/loop-mode-tools-and-invokers.md) T1-T3 |
| **T2** 🔒 | `loop-tools-mcp` | L | — | - | static | mcp | cage | runc | 1 | cftest | `[BLOCKED]` loop MCP tool | Negative: `ToolCallRejected "…\"mcp\""`. Positive needs MCPInvoker (§T4) |
| **T3** 🔒 | `loop-no-tools-json` | L | — | - | - | http | cage | runc | 1 | cftest | `[BLOCKED]` tool spec never ships | `<run>-runspec` ConfigMap has `agent.json`/`run.json`/`provider.json`, **no `tools.json`** |
| **F1** 🔒 | `artifact-egress` | H | aider | kopia | static | - | cage | runc | 1 | cftest | `[BLOCKED]` files-OUT manifest | no `Status.Artifacts[]`; needs [`artifact-egress`](../specs/artifact-egress.md). Today: edits land in AgentFS (A4-style) but no manifest |
| **F2** 🔒 | `policy-gate` | H | hermes | - | - | - | cage | runc | 1 | cftest | `[BLOCKED]` AgentPolicy enforcement | `AgentPolicy{allowedProviders}` ignored; needs [`agentpolicy-enforcement`](../specs/agentpolicy-enforcement.md) |
| **F3** 🔒 | `replay-eval` | H | replay | - | - | - | cage | runc | 1 | cftest | `[BLOCKED]` bit-exact reproduction | `kind=replay` does not exist; needs [`determinism-and-replay`](../specs/determinism-and-replay.md) §B |

> **22 unblocked rows** (A/S/B/E/K/D/KS) cover every wired axis; **7 `[BLOCKED]` rows** (T1-T3, F1-F3, + S1's declarative form) ship as negative-oracle tripwires.

### 4.1 Sample CR — A2 (Hermes embedded tools, the only working tool path, cftest)

```yaml
# Tools run INSIDE the Hermes gateway's own loop (~64 built-in tools), NOT the
# smol-agents executor. Loop-mode tools are BLOCKED (rows T1-T3).
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: { name: hermes-tools, namespace: bench }
spec:
  mode: harness
  instructions: "You have tools. Use them for any computation you cannot do reliably in your head."
  budget: { maxSteps: 6, maxTokens: 32768, maxToolCalls: 16, maxWallClockSeconds: 600 }
  harness:
    kind: hermes
    sessionPolicy: ephemeral
    http: { url: "http://hermes-gateway.hermes-e2e.svc.cluster.local:8642/v1/chat/completions" }
    env:
      - { name: HEADER_Authorization, secretRef: { secretName: hermes-gw-auth, key: authorization } }
      - { name: HERMES_MODEL, value: "glm-4.6" }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentRun
metadata: { name: hermes-tools-001, namespace: bench }
spec:
  agentRef: hermes-tools
  seed: 7
  # Oracle: answer is un-guessable without arithmetic the model offloads to a tool.
  input: { prompt: "Compute 982451653 * 49979687 exactly. End with: PRODUCT=<n>" }
# Verify:
#   .status.usage.tokens > 0                    (Hermes parses the OpenAI usage block)
#   .status.usage.toolCalls is 0                (structurally — do NOT gate on it; §1.2 fact 2)
#   .status.output contains PRODUCT=49096740249106611   (correct product → the tool ran)
#   kubectl -n hermes-e2e logs deploy/hermes-gateway | grep -i tool   (out-of-band dispatch)
```

### 4.2 Sample CR — KS2 (kitchen-sink Hermes: max feature density, aws-metal)

```yaml
# Hermes durable session on kata-fc + AgentFS-kopia + secretless GitHub egress.
# Deploy AFTER the hermes-gateway from .claude/hermes-e2e.yaml is Ready.
# Secrets created imperatively (never on disk):
#   kubectl -n bench create secret generic hermes-gw-auth --from-literal=authorization="Bearer <rand>"
#   kubectl -n bench label secret hermes-gw-auth agents.smol-agents.ai/tenant-secret=true
#   (tenant boundary, 5vr — the operator refuses to read an unlabeled CR-referenced Secret)
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentNetwork                      # secretless egress (runbook-wired broker; declarative form BLOCKED)
metadata: { name: github-secretless, namespace: bench }
spec:
  kind: identityProxy
  agentSelector: { app.kubernetes.io/name: smol-agents }
  identityProxy:
    tts:
      url: https://tts.security.svc/token
      subjectTokenType: urn:ietf:params:oauth:token-type:jwt
      jwksUrl: https://tts.security.svc/jwks
    resources:
      - { name: github, kind: http, localPort: 9200, gateway: https://api.github.com/,
          jwtAudience: spiffe://smol-agents.ai/ns/bench/sa/agent,
          credential: { name: github, scope: "github:repo:read" } }   # broker mints; agent-blind
    egress:
      enforcement: ebpfBoth
      redirectCIDRs: ["0.0.0.0/0"]
      allow: [ { cidr: 140.82.112.0/20, protocol: tcp, ports: [443] } ]   # api.github.com only
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Agent
metadata: { name: ks2-hermes, namespace: bench }
spec:
  mode: harness
  instructions: "Reasoning assistant. Use tools when needed. End with the requested final line."
  budget: { maxSteps: 6, maxTokens: 32768, maxToolCalls: 16, maxWallClockSeconds: 600 }
  sandbox: { runtimeClass: kata-fc }    # REAL microVM — requires aws-metal /dev/kvm (fail-closed)
  harness:
    kind: hermes
    sessionPolicy: persistent
    http: { url: "http://hermes-gateway.hermes-e2e.svc.cluster.local:8642/v1/chat/completions" }
    env:
      - { name: HEADER_Authorization, secretRef: { secretName: hermes-gw-auth, key: authorization } }
      - { name: HERMES_MODEL, value: "glm-4.6" }
  storage:
    kind: agentfs
    agentfs:
      sizeGiB: 10
      mountPath: /var/agentfs
      backend: kopia
      backup:
        s3: { bucket: smol-agents-bench, prefix: ks2-hermes/, region: us-east-1,
              endpoint: http://minio.minio.svc:9000, credentialsRef: { secretName: minio-creds } }
        schedule: "@every 5m"
      restore: { mode: latest, ifMissing: fresh }
---
apiVersion: runtime.agents.smol-agents.ai/v1
kind: AgentSession                      # durable: long-lived worker + NATS turn queue + gateway
metadata: { name: ks2-hermes, namespace: bench }
spec:
  agentRef: ks2-hermes
  idleTimeoutSeconds: 120
```

Drive it (gateway routes per `cmd/agentgateway/main.go:39-45`):

```bash
B=https://gateway.bench.example   # agentgateway Knative URL
# Turn 1 — seed a fact + a tool call (sha256 is un-guessable → proves the embedded tool ran)
curl -s "$B/v1/sessions/bench/ks2-hermes/turns?wait=60s" \
  -d '{"agentRef":"ks2-hermes","seed":1,"input":{"prompt":"My codename is FALCON-9. Compute sha256 of FALCON-9 with your tools, write it to notes.txt. End with: SHA=<hash>"}}'
# Oracle (KS2 composite):
#   .result.usage.tokens > 0          (real accounting; do NOT gate usage.toolCalls)
#   SHA == real sha256 of "FALCON-9"  (un-guessable without the tool)
#   run-pod logs | grep -i FALCON     → secretless GitHub token ABSENT (agent-blind)
#   pod jsonpath {..runtimeClassName} == kata-fc
kubectl -n bench delete pod -l agents.smol-agents.ai/session=ks2-hermes   # kill worker
curl -s "$B/v1/sessions/bench/ks2-hermes/turns?wait=60s" \
  -d '{"agentRef":"ks2-hermes","seed":2,"input":{"prompt":"What is my codename? End with: NAME=<codename>"}}'
#   NAME=FALCON-9  → memory survived worker reschedule (durable + AgentFS checkpoint)
```

> **Templates for implementers:** `.claude/hermes-e2e.yaml` (canonical live cftest gateway + Agent + secret recipe — base for all Hermes rows); `operator/config/samples/agent_claude_code.yaml` (AgentFS+S3 claude-code — A4-A7, K1, KS1); `operator/config/samples/agentnetwork_secretless_github.yaml` (secretless egress — S1, KS2).

---

## §5 — Tiers + cluster split (cftest runc vs AWS-metal kata)

The runner refuses to claim a result it can't prove on the target cluster. The `Caps` probe auto-downgrades: on cftest, `isolation_kernel` cases resolve to `skipped` with evidence `"no kata-fc RuntimeClass; runc fallback (containment downgrade, not failure)"`.

| Tier / case class | cftest (runc, k0s arm64, Hetzner) | AWS Graviton metal (kata-fc, `/dev/kvm`) | requiredCaps |
|---|---|---|---|
| Hermes tool exec, output, tokens, cost | ✅ live | ✅ | `hermes`, `gateway` |
| claude-code text + file side-effect | ✅ live | ✅ | — |
| codex/aider/goose first-run | ✅ (T2) | ✅ | — |
| Loop-mode `tool_rejected` (negative) | ✅ | ✅ | — |
| AgentFS round-trip / kill-survival (kopia/tar + minio) | ✅ | ✅ | `agentfs`, `s3` |
| Secret reach / non-leak / blindness gradient | ✅ static-lease path | ✅ | `broker` |
| `egress_metadata_blocked` | ✅ (kube-router/Cilium enforces) | ✅ | `metadata-block` |
| Budget caps (tokens/wallclock) | ✅ | ✅ | — |
| Durable session memory / restart / scale-zero | ✅ | ✅ | `gateway`, `nats` |
| **`isolation_kernel` (kata microVM)** | ❌ **runc only — auto-SKIP** | ✅ **only here** | `runtimeclass:kata-fc` |
| Concurrency / throughput at scale | ✅ small-N | ✅ large-N (more cores) | — |
| Dynamic-mint, artifact-egress, loop-tool *success*, A2A, claude-code structured tokens, AgentPolicy gating, replay | **`[BLOCKED]`** — `future` tier, not run unless `--allow-blocked` | same | — |

`make bench-l1` → cftest suite (everything except `isolation_kernel`); `make bench-l2` → AWS-metal suite (adds kata isolation), reusing the existing L2 AWS spot bring-up (`Makefile:104` `e2e-l2`, [`e2e-l2`](../runbooks/e2e-l2.md) runbook). On aws-metal, `isolation_kernel` is a **hard fail** if kernels match (silent kata→runc fallback); on cftest it is a documented downgrade.

**Concurrency.** `--concurrency=N` runs an `errgroup` of N in-flight cases; `--repeat=M` re-submits for distribution sampling. AgentRun load = N CRs each in its own pod (true parallelism, node-bound). AgentSession/gateway load = N concurrent `POST …/turns` against the shared worker, exercising the `pkg/sessionqueue` NATS JetStream queue (queue throughput + per-turn latency under contention). `scaleToZeroSec` = go idle past `idleTimeoutSeconds`, watch replicas → 0, then time the next cold turn.

---

## §6 — Live-run runbook

Prereqs (cftest): operator + `hermes-gateway` + minio deployed per `.claude/hermes-e2e.yaml`; throwaway test keys created imperatively (never logged/persisted). See [`k0s-local-cluster`](../runbooks/k0s-local-cluster.md) for cluster bring-up and [`secretless-egress`](../runbooks/secretless-egress.md) for the (runbook-only) dynamic-mint broker wiring.

```bash
NS_BASE=bench
KC=~/.kube/cftest.yaml                       # cftest kubeconfig

# 0. Sanity: gateway + hermes reachable, minio up (Caps probe does this too).
kubectl --kubeconfig $KC -n hermes-e2e get deploy hermes-gateway

# 1. Create throwaway test-key Secret (imperative — never on disk), then label it
#    (tenant boundary, 5vr — the operator refuses to read an unlabeled CR-referenced Secret).
kubectl --kubeconfig $KC -n $NS_BASE create secret generic hermes-gw-auth \
  --from-literal=authorization="Bearer $(openssl rand -hex 24)"
kubectl --kubeconfig $KC -n $NS_BASE label secret hermes-gw-auth agents.smol-agents.ai/tenant-secret=true

# 2. cftest suite (everything except kata isolation).
make bench-l1                                # → go run ./cmd/agentbench run --plan bench/plans/core --tier perf
#   or directly:
go run ./cmd/agentbench run --plan bench/plans/core --kubeconfig $KC \
  --tier perf --concurrency 4 --repeat 5 --out ./results --record

# 3. Inspect results.
jq '.summary, .verdict' ./results/*/results.json
cat ./results/*/report.md

# 4. AWS-metal kata isolation tier (per-run bring-up + terminate, $-capped).
make bench-l2                                # reuses infra/terraform/aws-e2e spot bring-up (e2e-l2 runbook)

# 5. Spot-check a single oracle (e.g. A2 embedded-tool).
kubectl --kubeconfig $KC -n $NS_BASE get agentrun hermes-tools-001 \
  -o jsonpath='{.status.usage.tokens}{"\n"}{.status.output}'
```

Per-dimension hermetic unit lanes (no LLM, no cluster):

```bash
go test ./pkg/agentbench/... -race                                  # plan/oracle/metrics + coverage gate
go test ./pkg/agentfs/... ./pkg/agentruntime/... \
  -run 'AgentFS|Materialize|Kopia|Restore|Backup|Retention|ExcludeGlobs|Scheduler' -race
go test ./pkg/secrets/ -run 'Lease|Mint|PeerNotAllowed|LocalPeer|VersioningRequired' -v
AWS_S3_BUCKET=<bucket> AWS_REGION=us-east-1 go test -tags integration ./pkg/agentfs/... -run TestAWSS3Integration
```

---

## §7 — What this CANNOT verify yet, and the specs that unblock it

Each blocked item ships as a **negative-oracle assertion** so the suite is a live regression tripwire — the negative flips to positive with no harness rewrite the moment its spec lands.

| Blocked capability | Bench rows / oracles | Unblocking spec |
|---|---|---|
| Loop-mode tool **execution** (http/mcp/function) | T1, T2, T3, T-FUT-1..5 | [`loop-mode-tools-and-invokers`](../specs/loop-mode-tools-and-invokers.md) (ship `tools.json` + populate executor `Invokers` + HTTP/MCP invokers + real JSON-Schema validation + tool `Auth`→broker) |
| Agent-to-agent (A2A) child runs | T-FUT-6 | [`agent-to-agent-invoker`](../specs/agent-to-agent-invoker.md) |
| Hermes structured tool-call trace + `usage.toolCalls>0` | T-FUT-7 | [`agent-hermes`](../specs/agent-hermes.md) §4.1 (`/v1/responses`; `input_tokens`/`output_tokens` parser) |
| CLI-kind real tokens/cost/session_id | A3/A7/K1/KS1 perf gaps, H-CC | [`response-richness`](../specs/response-richness.md); [`agent-claude-code`](../specs/agent-claude-code.md) §4.1 (`--output-format json`) |
| Conversational resume (vs file-only persistence) | H-CC CC-4 | [`agent-claude-code`](../specs/agent-claude-code.md) (`--resume`/`--continue`) |
| codex/aider/goose proven-live (auth shape) | H-CODEX/H-AIDER/H-GOOSE | [`agent-codex`](../specs/agent-codex.md), per-kind specs (first-run iteration; not a code blocker) |
| pi-mono coding CLI (vs `pi`=Inflection false friend) | H-PI | [`agent-pi-mono-http`](../specs/agent-pi-mono-http.md) (route via `generic-cli` + custom image) |
| Files-OUT artifact manifest (`Status.Artifacts[]`) | AFS-F1, F1 | [`artifact-egress`](../specs/artifact-egress.md) |
| WAL streaming / sub-full-backup RPO | AFS-F2 | (WAL-streaming driver; Tier-2 DB lane, [`agentfs-versioning`](../research/agentfs-versioning.md)) |
| Diff in `RunStatus`/API | AFS-10 status oracle | (status-fold wiring; primitive exists in `KopiaStore.Diff`) |
| Declarative dynamic credential mint + live TraT sender-constraint | S1 (declarative), SEC.4c, SEC.5, T-FUT-8 | [`dynamic-credential-backends`](../specs/dynamic-credential-backends.md) (DynamicBackend CRD; shipped `secret-proxy` wires static/vault only today) |
| Proxy injection on the run datapath (CLI agent-blind; per-resource egress) | S2 mitigation, AgentNetwork allow-list | [`agentnetwork-datapath-enforcement`](../specs/agentnetwork-datapath-enforcement.md) |
| AgentPolicy enforcement (provider/tool/budget/redaction) | F2 | [`agentpolicy-enforcement`](../specs/agentpolicy-enforcement.md) |
| Bit-exact replay / `kind=replay` | F3, repeatability §2.6 | [`determinism-and-replay`](../specs/determinism-and-replay.md) §B |
| Run governance (activeDeadline, per-ns concurrency cap, placement fallback) | concurrency/scale gates | [`run-governance`](../specs/run-governance.md) |

See the spec dependency DAG in [`docs/specs/README.md`](../specs/README.md) for milestone ordering.

---

## §8 — Honest summary

**Runnable now (cftest + AWS-metal):** Hermes tool/output/token/cost oracles; loop-tool *rejection* (negative); AgentFS round-trip, kill-survival, kopia-vs-tar integrity, retention, DB-exclusion; static-secret reach + Hermes-blindness (with CLI-not-blind flagged); egress metadata-block; budget termination; durable-session memory/restart/scale-zero; concurrency/throughput; and (AWS-metal only) kata kernel isolation. This is **22 unblocked suite rows** covering every wired axis.

**The painful constraints, stated plainly:** (1) loop-mode tools are 100% dead — only the rejection oracle runs; (2) there is **no** structured tool-call evidence anywhere — `usage.toolCalls` is 0 on the harness path, so tool execution is proven from side-effects + output, never `status.steps[].toolCalls`; (3) token/cost is **Hermes-only** — CLI gates are N/A, not failures; (4) determinism is best-effort (seed + N-sample distributions), true replay blocked; (5) kata isolation needs an AWS Graviton metal node — cftest is runc (containment downgrade, not failure).

**Anti-staleness by construction:** every `[BLOCKED]` row ships as a negative-oracle tripwire so the suite materializes the moment its spec lands.

**Key code anchors:** `pkg/agentmodel/v1/types.go:313-322` (`RunStatus`), `:230` (`Seed`); `pkg/agentmodel/v1/budget.go:36-42` (`Usage`); `pkg/agentruntime/harness/iface.go:48-65` (Response contract); `pkg/agentruntime/harness/hermes.go:108-110,265-276` (seed + usage parse); `pkg/agentruntime/harness/cli.go:55-77,108-119` (stderr capture + `mergeEnv`); `pkg/agentruntime/executor.go:246,260-262,281` (budget + reject + counter); `pkg/agentruntime/runonce.go:65-69,124-149` (empty Invokers + MaterializeInputs); `operator/internal/builders/runspec.go:29-31` (no `tools.json`); `operator/internal/builders/run_sandbox.go:39,45,60` (kata-fc + metadata block + egress policy); `cmd/agentgateway/main.go:39-45` (turn routes); `cmd/agent/run.go:94` (~3 KiB termination budget); `pkg/agentmodel/v1/harness.go:291-304` (`EffectiveWorkingDir`); `test/e2e/fullstack/shared/scenarios.go:689-729` (kata kernel oracle); `test/e2e/fullstack/coverage.go:13` (coverage-gate idiom); `Makefile:88-122` (`e2e-l1`/`e2e-l2` pattern).
