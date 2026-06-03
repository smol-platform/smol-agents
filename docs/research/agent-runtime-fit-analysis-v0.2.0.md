<!-- Generated 2026-06-02 by a 20-agent interface+fit audit (10 interface evaluators,
     8 agent-fit evaluators, 2 synthesizers), then reconciled against source by the
     maintainer. Supersedes agent-runtime-fit-analysis.md (pre-v0.2.0, stale). -->

# smol-agents Platform Evaluation (v0.2.0, 2026-06-02)

> Authoritative consolidation of 10 interface evaluations + 8 agent-fit evaluations, reconciled against verified source. Supersedes `docs/research/agent-runtime-fit-analysis.md` (pre-hardening, stale).

---

## 1. Executive summary

**Does the platform meet its goal — securely deploy AI agents and coding tools as hardened, isolated, scalable workloads?** **Partially, and meaningfully more so than the stale report suggests.** The security *substrate* is now real and verified, not aspirational: AgentRun and AgentSession pods pin a hardened RuntimeClass fail-closed (default `kata-fc`, confirmed at `operator/cmd/manager/main.go:45` and `operator/internal/controllers/agentmodel/sandbox.go:21-43`), every run pod gets a default-deny egress NetworkPolicy that blocks the cloud metadata endpoint (`operator/internal/builders/run_sandbox.go:60-123`), secrets are brokered with SO_PEERCRED + optional SPIRE attestation, and the Hermes harness is live-verified end-to-end on real arm64 infra. **Where it falls short of "production multi-tenant" is the control plane around the runtime: governance (AgentPolicy is declared but has zero enforcement code), per-workload egress allow-listing, AgentRun node placement, observability/metrics, and tool invocation for loop-mode agents are all unwired.** The platform is a strong *single-tenant / trusted-tenant* secure runtime today; it is not yet a hardened *multi-tenant* PaaS.

**Top 3 strengths**
1. **Real containment on the execution datapath** — microVM RuntimeClass (fail-closed runc rejection) + default-deny egress cage + non-root/drop-ALL/seccomp pod hardening, applied uniformly to run and session pods. This is the platform's genuine differentiator (`run_sandbox.go`, `sandbox.go`, `agentrun.go:59-67`).
2. **Brokered, agent-blind secret injection + SPIFFE identity** — `pkg/secrets` (SO_PEERCRED + SPIRE/local fallback, TraT sender-constraint, pluggable dynamic mint) plus per-pod SPIRE CSI identity. Defense-in-depth that managed competitors largely don't expose.
3. **Harness breadth that actually ships** — all 8 harness kinds registered and tested; per-kind bundle images (`harness-claude-code/codex/aider/goose`) remove the "bring your own Dockerfile" tax (`operator/internal/builders/harness_image.go:20-25`); Hermes is feature-complete (token accounting, sessions, multimodal+SSRF screening) and live-proven.

**Top 5 gaps**
1. **AgentPolicy is pure scaffolding (P1, security).** CRD + types exist; there is **no reconciler, no admission check, no redaction logic** anywhere in `operator/internal/` (verified: zero matches for `AgentPolicyReconciler`/`AllowedProviders` in controllers). Cluster/namespace guardrails on providers, tools, budget, and output redaction are documented but unenforceable.
2. **AgentNetwork is unwired on the run datapath (P0/P1, security).** No identity-proxy/WireGuard sidecar injection and no per-resource egress allow-list reaches run pods (verified: zero `AgentNetwork` references in `agentrun_controller.go`/`agentrun.go`). The egress cage is a *static* NetworkPolicy (RFC1918 + public 80/443), ignoring `AgentNetwork.Spec.IdentityProxy.Egress.allow`. eBPF redirection is programmed only by the e2e probe, never the operator.
3. **AgentRun/AgentSession pods get no node placement (P1, correctness).** Despite a complete AgentNodePool→placement system for SmolAgent, the run path applies no `nodeAffinity`/toleration/`do-not-disrupt` (verified: zero placement refs in `agentrun_controller.go`/`agentrun.go`). A kata-fc run can land on a non-KVM node and fail to schedule.
4. **Loop-mode tool invocation is dead end-to-end (P0, completeness).** Tool CRDs, JSON-Schema validation, and the `Executor` invoker machinery all exist, but `runspec.go` never marshals tool defs into the pod and `RunTurn` builds an `Executor` with empty `Invokers` (verified). No production `ToolInvoker` exists for `mcp`/`http`/`agent` kinds. Only harness-mode agents (with embedded tool logic) can use tools.
5. **CRD self-documentation is largely absent (P1, doc).** Multiple core CRD YAMLs (`agents.yaml`, `agentsessions.yaml`, `smolagents.yaml`, `tools.yaml`, `modelproviders.yaml`, `agentpolicies.yaml`) have **zero `description:` fields**, so `kubectl explain` is useless. Several are hand-rolled and have drifted from Go types.

**How v0.2.0 changed the picture.** The stale `agent-runtime-fit-analysis.md` scored Safety/Shipping/Scale a uniform **2/5** across all agent types, on the (then-accurate) basis that run pods used runc with no egress control and no published images. **The 4-phase hardening invalidated that baseline on the datapath:** Safety moved to 3–4 for every CLI/HTTP harness (kata-fc + egress cage now default and fail-closed), Shipping to 3–4 (per-kind images + tag pinning + stderr capture), and durable sessions/gateway/NATS scaling landed (P3/P4). The remaining 2-scores are **Scale** (no per-tenant concurrency/quota/autoscaling on the run path — genuinely still 2) and tool-specific config gaps. **One stale report claim is now false and should be retracted, not merely updated:** the old "all run pods are runc" assertion — the operator's flag default is `kata-fc`.

---

## 2. Interface scorecard

| Interface | Doc | Compl. | SecFit | Top gap |
|---|:--:|:--:|:--:|---|
| Agent CRD (`runtime.agents…/v1`) | 2 | 3 | 4 | CRD YAML has **0** field descriptions; AgentPolicy + `memory` + `gracefulCancelTimeoutSeconds` declared-but-dead |
| AgentRun datapath | 3 | 4 | 3 | Egress cage is static (ignores AgentNetwork allow-lists); CRD omits sandbox/input-path-traversal docs |
| AgentSession + scaling (P3/P4) | 1 | 2 | 3 | Skeletal spec (no concurrency/throughput/turn policies); status has no aggregated `Usage`; no multi-tenant NATS isolation |
| Harness (8 kinds) | 3 | 4 | 4 | CLI harnesses discard `ToolCalls` + report tokens=0; pi default-URL false-friend; no per-harness permission flags |
| ModelProvider + Tool CRDs | 2 | 2 | 3 | `mcp`/`http`/`agent`/`function` ToolKinds have **no production invoker**; CRDs undocumented |
| AgentNetwork + AgentPolicy | 2 | 2 | **1** | Neither is wired to the run datapath; AgentPolicy has **no controller at all** |
| AgentNodePool (`agents…/v1`) | 4 | 3 | 4 | AgentRun/AgentSession pods get **no placement** (kata runs can fail to schedule) |
| MemoryStore + MemoryRetriever | 4 | 4 | **5** | Missing enum/path CRD validation; no cross-tenant-denial runbook |
| SmolAgent + SmolAgentPlatform | 2 | 3 | 3 | CRD fields undocumented; `rolloutPolicy` Canary/Manual unimplemented; no custom-image serving spec/e2e |
| Secret Broker + AgentFS storage | 2 | 3 | 4 | Dynamic-credential backends are code-only (no CRD); kopia config undocumented; WAL snapshot declared-not-implemented |

**Per-interface notes.**
- **Agent CRD** — Solid security wiring (budget enforced per-step with Quint backing; mode-aware validation; sandbox+egress live) but the API surface is under-documented and carries genuine dead fields (`memory`, `gracefulCancelTimeoutSeconds`) plus a non-enforced `AgentPolicy` family.
- **AgentRun datapath** — The best-hardened interface; the P1 containment is land-verified and correctly GC'd (egress policy owned by the run). Documentation lags the code, and the egress allow-list is hard-coded rather than AgentNetwork-driven.
- **AgentSession** — Architecturally sound (checkpoint + NATS at-least-once + idle-park, e2e-proven) but the *spec* is the weakest in the codebase: one-field-ish, no tuning knobs, no `Usage` roll-up, no per-namespace NATS ACL story.
- **Harness** — Broad and well-tested; the central limitation is structural: CLI harnesses are opaque (no tokens, no tool calls), so budget/observability only work for Hermes/HTTP. Several validation/false-friend gaps (pi default URL, generic-cli missing `command` check).
- **ModelProvider + Tool** — ModelProvider's secretless-broker design is clean; Tool's four kinds are mostly *declared-but-never-invoked* in production. This is the biggest "looks done, isn't" surface.
- **AgentNetwork + AgentPolicy** — Lowest SecurityFit (1) and rightly so: rich CRDs with **no datapath enforcement**. The static run egress policy provides a real-but-coarse floor; everything resource-specific is unwired.
- **AgentNodePool** — Best-documented interface (runbook + design doc + samples) and correct for SmolAgent, but the AgentRun/AgentSession placement omission undermines the kata isolation guarantee for the exact workloads that need it most.
- **MemoryStore/Retriever** — The standout: three-plane architecture, deny-by-default + SPIFFE, Quint-verified tenant isolation, fully wired filesystem mounts. Only polish gaps (CRD enum/pattern validation, operator runbooks).
- **SmolAgent** — Clean two-CRD design and strong pod hardening, but hand-rolled CRDs lack descriptions, `Defaults` merge is incomplete in the webhook, Canary rollout is unimplemented, and custom-image serving (the real path for OpenClaw-style daemons) is unspecified and un-e2e'd.
- **Secret Broker + AgentFS** — Strong primitives (multi-layer attestation, kopia content-addressed backups, native-sidecar lifecycle). Operability gaps: dynamic backends configurable only in Go, kopia password/repo setup undocumented, `walSnapshotInterval` is a declared no-op.

---

## 3. Agent-type support matrix (current post-v0.2.0 scores)

| Tool | Harness kind | Exec model | Safety | Ship | Fit | Scale | Support | Effort |
|---|---|---|:--:|:--:|:--:|:--:|---|---|
| **hermes** | `hermes` | HTTP (OpenAI-compat gateway) | 4 | 4 | **5** | 3 | **full** | low |
| **codex** | `codex` | CLI one-shot `codex exec` | 3 | 4 | 4 | 2 | partial | medium |
| **claude-code** | `claude-code` | CLI one-shot `claude --print` | 3 | 3 | 3 | 2 | partial | medium |
| **aider** | `aider` | CLI one-shot | 4 | 3 | 2 | 2 | partial | medium |
| **goose** | `goose` | CLI one-shot `goose run` | 2† | 3 | 3 | 2 | partial | medium |
| **loop / generic** | `generic-cli`/`generic-http` + Mode=loop | dual | 3 | 2 | 3 | 2 | partial | high |
| **pi / pi-mono** | `pi` (HTTP) / `generic-cli` | HTTP **or** subprocess | 2† | 2 | 1 | 2 | partial | high |
| **openclaw** | none (SmolAgent serving) | Deployment/StatefulSet/Knative | 4 | 2 | 1 | 3 | partial | high |

† **Contradiction flagged — see below.** The goose and pi evaluators scored Safety **2** on the premise that run pods are runc with no egress enforcement. That premise is **false** for the same code the codex/claude-code/aider evaluators read (Safety 3–4). The honest CLI-harness Safety on v0.2.0 is **3** (kata-fc default + egress cage, both fail-closed and verified). I have left the evaluators' raw numbers in the table for traceability but treat goose/pi Safety as effectively **3** in the backlog.

**Score movement vs the stale 2/5 baseline.** Every harness gained on Safety (2→3/4) because kata-fc + egress are now default and fail-closed; every shipping CLI gained on Shipping (2→3/4) because per-kind images + tag pinning + stderr capture landed. **Scale did not move (still 2)** for any CLI/loop tool — there is no per-tenant concurrency cap, no `activeDeadlineSeconds`, no run-pod autoscaling. Hermes is the only "full" support and the only one that exploits all four phases.

- **hermes (full, low effort)** — Richest, most-native harness; live-verified on Hetzner arm64 + glm-4.6. Real token accounting, ephemeral/persistent session IDs, multimodal with SSRF screening, retry/backoff. Missing: SSE streaming (hard-coded `stream=false`), `ToolCalls` parsing from responses, and the over-strict `sessionPolicy=persistent ⇒ storage` rule (Hermes memory is gateway-side). Zero blockers to ship.
- **codex (partial, medium)** — Cleanly wired with published `harness-codex` image and correct `codex exec` shape. Missing: `--ask-for-approval never` not enforced/documented (silent fallback to interactive breaks unattended runs), no token/tool-call parsing from `--json`, no live e2e, and Codex's Seatbelt inner sandbox SYS_ADMIN dependency is undocumented under kata.
- **claude-code (partial, medium)** — Correct one-shot `claude --print` with AgentFS working-dir binding and broker creds. Missing: MCP config + permission-mode passthrough, cost/JSON parsing, and — critically — `sessionPolicy=persistent` is *structural only* (it stands up a session pod but each turn runs a fresh `--print` with zero context carryover). Persistent claude-code sessions are durable-workspace, not resumable-context.
- **aider (partial, medium)** — Hard guarantees (sandbox+egress+budget) all present, but **operationally incomplete**: no model/provider wiring (`--model` not passed; relies on env-var inference), no sample YAML, no git-init guarantee (aider needs a repo), and — the big one for a *coding* agent — **no artifact egress** (diffs/edited files evaporate when the pod ends) and no input-file materialization path beyond AgentFS pre-seeding. File-oriented agents are crippled until F3 (artifact capture) lands.
- **goose (partial, medium)** — Registered, bundled, correct `goose run --instructions` shape. Missing: provider/model selection (env-only), no JSON output parsing (loses tokens/tool calls/session id), no real-binary e2e. Safety should read 3 (see contradiction).
- **loop / generic (partial, high)** — **Asymmetric maturity.** Mode=harness (generic-cli/http) is production-ready as an escape hatch. Mode=loop is deterministic and property-tested in-process but **unplugged for tools**: definitions never reach the pod and no real invoker exists (verified P0). Note: the loop evaluator's *other* P0 — "Steps dropped before reaching cluster" — is **factually wrong** (Steps are folded to `Status.Steps`; see §4); the real residual there is the 4 KiB termination-message truncation (P2).
- **pi / pi-mono (partial, high)** — Two incompatible "pi"s: built-in `pi` kind hard-codes Inflection's consumer API (a false-friend for Mario Zechner's pi-mono CLI, which needs `generic-cli` + a custom image). Genuine gaps: no published pi-mono image, no `activeDeadlineSeconds` for tmux-spawning loops, env-readable creds for pi's bash tool. **But its headline P0 ("AgentRun pods run under runc by default") is wrong** — see §4.
- **openclaw (partial, high)** — Correctly diagnosed as a *daemon*, not a harness: must run as a custom-image SmolAgent serving workload. Serving infra (Deployment/StatefulSet/Knative + SPIRE + brokered secrets + AgentFS + egress cage) is suitable and mostly built. Blockers: no Node.js base image (must build), the static egress cage can't express OpenClaw's 40+ integration endpoints (needs per-workload allow-list — the unwired AgentNetwork), and no in-memory session durability across pod loss.

---

## 4. Reconciled contradictions (resolved against source)

Three evaluator claims conflict with the actual code. Resolutions, with evidence:

1. **"AgentRun run pods default to runc / `DefaultRunRuntimeClass` is empty" (pi-mono P0/P1, and the basis for goose/pi Safety=2) → FALSE.** The operator flag `--default-run-runtime-class` defaults to **`kata-fc`** (`operator/cmd/manager/main.go:45-46`), and `resolveSandbox` falls back to `kata-fc` even when the value *is* empty (`operator/internal/controllers/agentmodel/sandbox.go:26-28`), rejecting runc fail-closed unless `--allow-host-runtime`. The pi-mono evaluator's cited path (`sandbox.go:330-332`) is the pass-through wrapper, not the default source. **CLI-harness Safety on v0.2.0 is 3, not 2.**
2. **"Steps are dropped before reaching the cluster (4 KiB architecture)" (loop P0) → FALSE as stated.** `RunResult` has a `Steps` field (`pkg/agentruntime/runonce.go:34`), `ResultToWire` populates it (`:84`), and `foldRunResult` copies it to `run.Status.Steps` (`operator/internal/controllers/agentmodel/agentrun_controller.go:404`). This matches the memory note that the Steps wire-up was *fixed* after the framework-enhancements review. **The real residual is P2:** Steps ride the pod termination message, which Kubernetes caps (~4 KiB), so large traces get truncated — fix is to size-budget Steps, not to wire them.
3. **"Loop-mode tools unwired" (loop P0) → TRUE, confirmed.** `operator/internal/builders/runspec.go` and `cmd/agent/run.go` never marshal/deserialize tool definitions; `RunTurn` constructs the `Executor` with empty `Invokers`; no production `ToolInvoker` implementation exists for `mcp`/`http`/`agent`. This P0 stands.

No other material contradictions; evaluators agree that AgentPolicy enforcement, AgentNetwork run-path injection, AgentRun placement, and `activeDeadlineSeconds` are all genuinely missing (independently re-verified).

---

## 5. Prioritized gap backlog (deduped across evaluators)

### P0 — blocking for the stated goal
| # | Gap | Interfaces | Fix | Effort |
|---|---|---|---|---|
| P0-1 | **AgentNetwork not injected on the run/session datapath** — no proxy/WireGuard sidecar, no per-resource egress allow-list reaches pods | AgentNetwork, AgentRun, openclaw | Resolve bound AgentNetworks in `agentrun_controller`/`agentsession_controller`; new `builders.AttachAgentNetwork`; merge `IdentityProxy.Egress.Allow[]` into the egress policy | L |
| P0-2 | **Loop-mode tool invocation dead end-to-end** — defs never shipped to pod; no real invokers | Tool/ModelProvider, loop | Marshal resolved Tool specs into the run ConfigMap (`runspec.go`); deserialize in `cmd/agent/run.go`; add `pkg/agentruntime/invokers/{http,mcp}.go`; admission-reject loop agents referencing unimplemented kinds | L–XL |
| P0-3 | **No custom-image OpenClaw/daemon image or spec** — published image is distroless `/agent` only | SmolAgent, openclaw | Document + sample custom-image serving (`docs/design/custom-agent-images.md`); validate `spec.image`; per-workload egress override | M (image+sample); larger for multi-tenant |

### P1 — required for safe multi-tenant
| # | Gap | Interfaces | Fix | Effort |
|---|---|---|---|---|
| P1-1 | **AgentPolicy entirely unenforced** — no controller, no admission, no redaction | AgentPolicy, Agent | Build `AgentPolicyReconciler` + validating admission on AgentRun (providers/tools/budget union); apply `redaction.patterns` in `foldRunResult` | L |
| P1-2 | **AgentRun/AgentSession pods get no node placement** — kata runs can fail to schedule | AgentNodePool, AgentRun, AgentSession | Resolve Agent sandbox → AgentNodePool by isolation; new `ApplyRunPodPlacement` (nodeAffinity+toleration+do-not-disrupt); add golden test | M |
| P1-3 | **CRD self-documentation missing across core CRDs** — `kubectl explain` empty | Agent, AgentSession, SmolAgent, Tool, ModelProvider, AgentPolicy | Regenerate from Go (where controller-gen) or hand-add `description:` (hand-rolled SmolAgent/Agent); add enum/bounds/pattern validation while there | M (broad but mechanical) |
| P1-4 | **No per-tenant concurrency/quota or `activeDeadlineSeconds`** — Scale=2 platform-wide | all run-path tools | Add `activeDeadlineSeconds ≈ 1.5×MaxWallClockSeconds` to run pods; per-Agent/namespace concurrency cap in the AgentRun reconciler; pod resources on session Deployment | M |
| P1-5 | **AgentSession spec skeletal** — no concurrency/throughput/turn/`Usage` knobs; no multi-tenant NATS isolation | AgentSession | Add `MaxConcurrentTurns`/`TurnBatchSize`/`Turn*Timeout`/`MaxTurnInputBytes`/`TurnHistoryLimit`; aggregate `Status.Usage`; document/enforce per-namespace NATS ACL | M–L |
| P1-6 | **CLI-harness model/provider config not first-class** — aider/goose/codex rely on env-var inference; `BASE_URL`/`MODEL` visible in CR | Harness, aider, goose, codex | Add `HarnessSpec.Model`/`ProviderRef` (or `CLI.ExtraFlags`); document approval-mode enforcement for codex (`--ask-for-approval never`) | S–M |
| P1-7 | **No artifact egress for file-producing harnesses** — aider diffs/edits lost | Harness, aider, loop | Implement `AgentRunSpec.Artifacts` (glob → S3 upload) post-run | XL |
| P1-8 | **Dynamic-credential backends are code-only** — no CRD to register GitHub-App/provider mint | Secret Broker | `DynamicCredentialPolicy` CRD (or extend AgentNetwork) + operator wiring into broker config | M |

### P2 — completeness / robustness
| # | Gap | Interfaces |
|---|---|---|
| P2-1 | **Steps truncated by 4 KiB termination-message cap** (not dropped — see §4); size-budget the trace | AgentRun, loop |
| P2-2 | **`Response.ToolCalls` + token counts never populated for CLI/Hermes harnesses** — parse `--json`/OpenAI usage | Harness, all CLI, hermes |
| P2-3 | **Static egress cage ignores per-workload needs** (also openclaw); honor `AgentNetwork.Egress.Enforcement` | AgentRun, AgentNetwork |
| P2-4 | **`sessionPolicy=persistent ⇒ storage` over-strict for Hermes** (gateway-side memory) | Harness, hermes |
| P2-5 | **Knative serving prereq flags unchecked**; **`rolloutPolicy` Canary/Manual unimplemented**; **`Defaults` merge incomplete** in webhook | SmolAgent |
| P2-6 | **MemoryRetriever CRD missing enum/path validation**; no cross-tenant-denial runbook | Memory |
| P2-7 | **kopia config undocumented**; **`walSnapshotInterval` declared-not-implemented**; missing `mountPath`/`kopia⇒s3` CRD constraints | Secret Broker/AgentFS |
| P2-8 | **pi false-friend / generic-cli `command` not validated**; pi-mono image/e2e missing | Harness, pi |
| P2-9 | **No live e2e** for codex/goose/loop/serving-custom-image (only Hermes proven) | multiple |

### P3 — polish
Dead-field cleanup (`memory`, `gracefulCancelTimeoutSeconds`, `PassthroughEnv`); SessionState schema versioning; operator metrics/structured-logging docs; AgentNodePool `CapacityAvailable` (declared, never set); `harness/doc.go` lists only 5 of 8 kinds; `runc` AgentNodePool anti-pattern guard.

---

## 6. Competitive comparison

**Where smol-agents wins (and most managed/OSS runtimes don't match):**
- **Hardware-isolated execution by default.** kata-fc microVM per run, fail-closed, is stronger than the shared-kernel/gVisor or container-only isolation most code-exec sandboxes ship. e2b, Modal, Daytona, Northflank, and Cloudflare Sandbox/Workers lean on gVisor/V8-isolate/container boundaries; **Fly Machines** (Firecracker) is the closest peer on isolation, but Fly is infra, not an agent runtime. AWS Bedrock AgentCore advertises microVM session isolation but is a closed managed service with no portability.
- **Default-deny egress cage + metadata-endpoint block, declaratively.** Few managed agent runtimes expose a per-workload egress policy that blocks the cloud metadata SSRF target out of the box. This is a real, verified security property here.
- **Brokered, agent-blind secrets + SPIFFE workload identity.** SO_PEERCRED + SPIRE attestation, sender-constrained TraT, and on-demand dynamic minting are beyond what OpenAI Assistants/Responses, LangGraph Platform, dapr-agents, or kagent offer (those rely on platform-managed keys or app-level env). This is genuinely differentiated.
- **Open, Kubernetes-native, multi-cloud.** vs. the lock-in of Bedrock AgentCore / OpenAI Assistants / Cloudflare Durable Objects.

**Where it's behind:**
- **Managed scaling & cold-start.** Cloudflare Agents/Durable Objects, Modal, and Bedrock AgentCore deliver effortless scale-to-zero + fast cold start + per-request billing. smol-agents has Knative scale-to-zero on the *gateway/serving* path, but the *run* path has no autoscaling, no concurrency caps, and kata cold-start is heavy (Scale=2).
- **Observability & cost accounting.** OpenAI/Bedrock/LangGraph give first-class traces, token/cost metering, and dashboards. Here, CLI harnesses report tokens=0 and no tool calls; operator metrics are emitted but undocumented; there's no built-in spend tracking.
- **Multi-tenancy maturity.** Bedrock AgentCore and the managed SaaS runtimes have hardened tenant isolation, quotas, and audit. smol-agents has the *primitives* (SPIFFE, namespaces, deny-by-default memory) but the *governance plane* (AgentPolicy, per-namespace NATS ACLs, quotas) is unenforced or unwired.
- **Stateful agent ergonomics.** Cloudflare Durable Objects and LangGraph Platform make durable multi-turn state trivial. smol-agents has durable sessions (P3) but they're loop/Hermes-focused; CLI harnesses get durable *workspace*, not resumable *context*.
- **Agent dev experience / SDK.** dapr-agents, LangGraph, and OpenAI provide rich SDKs + tool ecosystems. smol-agents' loop-mode tool invocation is unwired (P0-2), so its in-process agent framework can't yet use tools — a gap competitors closed long ago.

**Net positioning:** smol-agents is the strongest *open, self-hostable, hardware-isolated* secure runtime in this set — a credible answer to "I need to run untrusted coding agents on my own cluster with microVM + egress + brokered secrets." It is **not** competitive yet on managed scaling, observability, or turnkey multi-tenancy, which is where the P1 backlog concentrates.

---

## 7. Recommended roadmap

Sequenced so each milestone is independently shippable and unblocks the next.

**Milestone A — "Close the containment loop" (security, ~2–3 wks).** Make the advertised isolation actually hold for every workload and tenant.
- P1-2 AgentRun/AgentSession node placement (kata runs schedule reliably).
- P0-1 AgentNetwork sidecar + per-resource egress injection on the run/session path.
- P2-3 honor `AgentNetwork.Egress.Enforcement`; collapse the static cage into the resource-driven one.
- *Exit:* a kata-fc run with a bound AgentNetwork schedules on a KVM node and egresses only to its allow-list. Live-verify on the L2 AL2023 ring.

**Milestone B — "Governance & guardrails" (multi-tenant, ~2–3 wks).** Turn declared policy into enforced policy.
- P1-1 `AgentPolicyReconciler` + admission (providers/tools/budget) + redaction in fold.
- P1-4 `activeDeadlineSeconds` + per-tenant concurrency caps + session-pod resources.
- P1-5 AgentSession spec knobs + `Status.Usage` roll-up + per-namespace NATS ACL doc/enforce.
- P1-8 dynamic-credential backend CRD.
- *Exit:* a hostile Agent CR is rejected at admission; a runaway run is force-killed; per-namespace quotas hold.

**Milestone C — "Agents that actually do work" (capability, ~3–4 wks).** Make tools and files real for non-Hermes agents.
- P0-2 wire loop-mode tool defs into pods + `http`/`mcp` invokers.
- P1-6 first-class harness model/provider config + codex approval-mode enforcement.
- P1-7 artifact egress (unblocks aider and all file-producing harnesses).
- P2-2 token/tool-call parsing for CLI + Hermes harnesses (feeds budget + observability).
- *Exit:* a loop agent calls an HTTP tool; an aider run's diffs land in S3; budgets reflect real tokens.

**Milestone D — "Operable & documented" (trust, ~2 wks, parallelizable).**
- P0-3 custom-image serving spec + sample + one serving e2e (OpenClaw-class).
- P1-3 CRD descriptions + validation across all core CRDs (resolves CRD-generation drift noted in memory).
- P2-9 live e2e for codex/goose/loop; P2-5/-6/-7 docs + validation; P3 dead-field cleanup + metrics/logging docs.
- *Exit:* `kubectl explain` is useful on every CRD; every supported tool has a green e2e and a runbook.

**Dependency notes:** A precedes B (placement + egress are prerequisites for trusting policy enforcement). C is independent of A/B but is the larger eng lift and benefits from B's admission hooks. D should run continuously alongside A–C, not after.
