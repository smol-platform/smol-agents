# smol-agents as an Agent Runtime: Fit & Scoring Analysis

> ⚠️ **SUPERSEDED (2026-06-02).** This analysis was scored **before** the v0.2.0 4-phase
> hardening (run-pod kata-fc + egress cage, per-kind harness images, durable sessions,
> gateway/queue). Its uniform **2/5 Safety/Shipping/Scale** baseline and several "absent"
> claims (run-pod has no RuntimeClassName, no egress, no per-kind images, Steps dropped)
> are **no longer accurate**. Read **[`agent-runtime-fit-analysis-v0.2.0.md`](./agent-runtime-fit-analysis-v0.2.0.md)**
> for current scores. Retained for historical record only.

*How well the platform hosts five real coding/agent harnesses — OpenClaw, Hermes, pi, Codex, Claude Code — and how it stacks up against managed and OSS competitors.*

> **Scoring note:** All scores reflect **BUILT** capability verified against the code (per the reality-check), **not** roadmap or documented-but-unwired intent. Where the docs over-claim (eBPF egress cage, microVM-for-runs, agent-blind credentials for CLI harnesses), the scores follow the code, not the docs.

---

## 1. Executive Summary

smol-agents is a coherent, fully-OSS, Kubernetes-native agent platform whose genuine, code-verified differentiators are its **identity + secretless-credential substrate** (SPIRE workload identity + broker dynamic-mint + sender-constrained TraT + agent-blind proxy injection), a **formally-modeled (Quint) bounded-execution core** (4-axis budgets, validated run lifecycle, RunResult fold), **durable encrypted AgentFS storage** (kopia/S3 content-addressed snapshots), and breadth of integration behind a single operator. The correctness core is real and e2e-green — but only on single-node rings (L0/L1 in CI; L2/AWS manual-only), and exactly **one** harness (Hermes/HTTP) has ever run green on real infra.

Three things must be stated bluntly because they drive every score: (1) the **AgentRun execution pod — the path that actually runs untrusted code/CLI harnesses — sets NO `RuntimeClassName`** (`operator/internal/builders/agentrun.go:56-71`), so it runs under the cluster-default runtime (typically runc) on a shared host kernel; the kata-fc microVM story applies only to the long-lived SmolAgent *serving* pod. (2) The headline **eBPF egress "cage a compromised agent cannot disable" is UNENFORCED** — `cgroup.Compile/EncodeRedirect/EncodeAllow` are called only from `cmd/ebpf-probe`, the operator programs no maps, **zero NetworkPolicy** is rendered anywhere, and the AgentNetwork controller literally "does NOT inject sidecars." (3) **CLI harness-agnosticism is mechanism-only**: the published distroless `/agent` image has no shell and no CLI binary, no per-kind image is published, the `Version` pin field is dead, and no e2e ever ran a real CLI binary.

### Headline aggregate scores (0–5, BUILT capability)

| Target | Safety/Isolation | Shipping Config & Tools | Request Handling & Scale | Harness Fit | **Overall** |
|---|:---:|:---:|:---:|:---:|:---:|
| **Hermes Agent** (HTTP gateway) | 2 | 3 | 2 | **5** | **3.0** |
| **OpenAI Codex** (`codex exec`) | 2 | 2 | 2 | 3 | **2.2** |
| **Claude Code** (`claude --print`) | 2 | 2 | 2 | 2 | **2.0** |
| **OpenClaw** ("Molty") gateway | 2 | 2 | 2 | 1 | **2.0** |
| **pi-mono** (`pi` coding-agent CLI) | 2 | 2 | 2 | 1 | **1.8** |

The pattern is uniform: **2/5 on Safety, Shipping, and Scale across the board** (the platform's isolation/egress/scale gaps are agent-independent and live on the AgentRun datapath), with all differentiation in **Harness Fit** — driven entirely by whether a *purpose-built* harness kind exists (Hermes 5, Codex/Claude-Code 3/2 via dedicated-but-unrun CLI kinds, OpenClaw/pi 1 as non-fits).

---

## 2. What the Platform Is + Capability Map

smol-agents is a single-controller, async, **CR-driven** runtime. An `AgentRun` CR submission is the only entrypoint; the reconciler renders **exactly one `RestartPolicy=Never` Pod** per run (pod lifecycle == run lifecycle) and polls Pod phase into a validated state machine. There is no synchronous run API and no work queue. A separate long-running **SmolAgent serving path** (Deployment/StatefulSet/Knative) hosts persistent agent services and is where most of the isolation/autoscaling wiring actually lands.

### Capability map (reflects reality-check corrections)

| Capability | Maturity | Evidence |
|---|:---:|---|
| Restricted PodSecurity hardening (non-root 65532, drop-ALL caps, seccomp RuntimeDefault) | **built** | `workload.go:82-111`; `agentrun.go:59-67` |
| Harness kind taxonomy (8 kinds) + admission validation | **built** | `harness.go:38-60`; `iface.go:83-94`; `harness_test.go:27-44` |
| Hermes harness (richest impl: OpenAI-compatible gateway, real token accounting, session policy, SSRF screen) | **built** | `hermes.go:61-190`; 7 unit tests `harness_test.go:232-448` |
| HTTP harnesses pi + generic-http (shared `doHTTP`/retry driver) | **built** | `http.go:28-125`; `retry.go:41-149` |
| Single-shot execution model + RunResult status fold | **built** | `executor.go:79-85,375-434`; `agentrun_controller.go:195-201` |
| 4-axis per-run budget (steps/tokens/wallclock/toolcalls) + Quint model | **built** | `budget.go:16-81`; `executor.go:112,162,182,246` |
| Run lifecycle state machine + transition validation | **built** | `lifecycle.go:11-83`; `agentrun_controller.go:394-430` |
| Cancellation (spec.cancel → pod delete + ctx) | **built** | `agentrun_controller.go:119-123`; `executor.go:104-108` |
| Durable AgentFS restore + native backup sidecar (kopia/S3, SIGTERM upload) | **built** | `storage_mount.go:79-128,160-228`; `kopia_store.go` |
| Secret injection via broker (env secretRef, native sidecar) | **built** | `secret_broker.go:52-149`; `executor.go:354-373` |
| Working-dir binding to AgentFS mount (CLI harnesses only) | **built** | `harness.go:263-285`; `cli.go:42-46`; `storage_mount.go:33-35` |
| SPIRE workload identity delivery (csi.spiffe.io socket + ClusterSPIFFEID) | **built** | `workload.go:114-115,152-162`; `cluster-spiffe-id.yaml` |
| Secretless egress: broker dynamic-mint + TraT + agent-blind proxy injection | **built** (e2e vs fakes) | `server.go:242-290`; `proxy/http.go:24-141`; `scenarios.go:819-848` |
| microVM isolation on **SmolAgent serving** path (kata-fc default + R-SBX-1 runc guard + gVisor fallback) | **built** (request-only; silently degrades) | `workload.go:41-45`; `smolagent_webhook.go:35-41`; `sandbox.go:44-68` |
| Node autoscaling AgentNodePool → Karpenter / ClusterAutoscaler | **built** (envtest-only, never live-verified, **service-path only**) | `agentnodepool_controller.go:58-143`; `karpenter.go:30-31` |
| Helm chart (3 modes, 9 sandbox + 7 eBPF presets, CI-asserted) | **built** | `deploy/helm/*`; `ci.yaml:33-94` |
| agentctl deploy (k8s/aws/hetzner; CF-tunnel field-proven) | **built** | `internal/agentctl/deploy/{k8s,aws,hetzner}` |
| Per-component image overrides + multiarch ghcr publishing | **built** | `images.go:19-76`; `release-images.yaml:28-74` |
| **Harness-mode budget: wallclock + tokens only** (MaxSteps always 1, MaxToolCalls inert, post-hoc) | **partial** | `executor.go:419-427`; `iface.go:120-127` |
| **CLI harnesses non-functional on published distroless image** (need custom OCI image) | **partial** | `agent.Dockerfile:12-16`; `agentrun.go:84-90` |
| **AgentRun pod sandbox** (no RuntimeClassName / nodeAffinity / activeDeadlineSeconds) | **absent** | `agentrun.go:56-71` (verified 0 occurrences) |
| **eBPF egress enforcement on datapath** ("cage") — programmed only by e2e probe | **absent** (designed) | `cgroup/maps.go` called only from `cmd/ebpf-probe`; 0 NetworkPolicy |
| **AgentNet proxy / WireGuard sidecar injection** into run pods | **absent** (designed) | `agentnetwork_controller.go:25-145`; `BuildAgentRunPod` takes no networks param |
| **Tools/MCP wiring into harnesses** (Response.ToolCalls never populated) | **absent** | `iface.go:14-58` (no Tools field; 0 ToolCalls assignments) |
| Per-run/step OTel GenAI trace spans (StartRunSpan/StartStepSpan) | **designed** (dead — no callers) | `otel.go:18-52` |
| `RequiresAction` human-in-loop phase | **designed** (dead — declared, never emitted) | `lifecycle.go:15,72-82` |
| `HarnessSpec.Version` (semver pin) / `HarnessCLISpec.PassthroughEnv` | **absent** (dead fields) | `harness.go:83-85,143-146` |
| Multi-tenancy (Tenant CRD / per-tenant quota / NetworkPolicy) | **absent** (namespace-by-convention) | `smolagent_types.go:7`; `INSTALL.md:836-868` |
| Run-admission concurrency control (MaxConcurrentReconciles / queue) | **absent** (default 1 worker) | `agentrun_controller.go` SetupWithManager (verified) |

---

## 3. Per-Target Fit

### 3.1 Hermes Agent (`hermes` harness / OpenAI-compatible gateway)

**Profile.** NousResearch self-hosted self-improving Python agent with 40+ tools (terminal, execute_code, file write, browser). The `hermes` harness wraps its OpenAI-compatible gateway (`/v1/chat/completions`, default `127.0.0.1:8642`); each AgentRun POSTs a chat-completions request and folds usage back. **The gateway itself is host-level RCE** and is deployed as a **separate, operator-UNMANAGED workload** — the platform wraps only the HTTP client.

| Category | Score |
|---|:---:|
| Safety / Isolation | 2 |
| Shipping Config & Tools | 3 |
| Request Handling & Scale | 2 |
| Harness Fit | **5** |

**Top strengths.** Purpose-built `hermes` kind is the richest impl in the repo — real token accounting (`parseUsage`), `HEADER_`/`BODY_` conventions, seed, multimodal, and a workaround for the gateway's documented non-stateless session-hash bug (`hermes.go:141-161`), with 7 dedicated unit tests. Genuinely built **agent-blind credential path on the HTTP path** (gateway holds the provider key; the wrapper leases only the bearer token via broker; a secretRef with no broker is a hard error). SSRF screening of gateway-fetched image URLs is real and tested. Complete copy-pasteable `agent_hermes.yaml`; wrapper runs from a pinned multiarch ghcr image.

**Top gaps.** The RCE surface (gateway) is unmanaged by the operator, so platform isolation never touches it. No microVM/`activeDeadlineSeconds`/`nodeAffinity` on the run path; harness container drops `readOnlyRootFilesystem`. **Egress completely uncaged** (no NetworkPolicy, no proxy sidecar). Scaling-model mismatch: the gateway is sticky/stateful (per-tenant SQLite), one-process-per-tenant, vs the platform's stateless pod-per-run. **The "battle-tested on real infra" claim exists only in project memory — there is no Hermes e2e in `test/`** (treat as a manual, non-CI-gated run).

**Effort to ship.** **Medium-to-high.** Harness layer is plug-and-play (drop in the sample, point at the gateway URL, lease the bearer token — hours). But you must independently build/deploy/operate the heavy gateway (Python3.11 + git/node/ripgrep/ffmpeg, persistent `~/.hermes` volume) and hand-roll the isolation the platform does NOT apply on this path: per-tenant strong sandboxing, default-deny egress, sticky per-tenant gateway processes, resource quotas.

---

### 3.2 OpenAI Codex CLI (`codex` / `codex exec`)

**Profile.** OpenAI's open-source Rust coding agent. `codex exec` is the one-shot scriptable subcommand that runs the agent to completion. Security rests on an OS sandbox (Seatbelt/bubblewrap+Landlock+seccomp) **plus** the outer boundary; with `--ask-for-approval never` the outer container/microVM + default-deny egress are the *only* guardrails (the inner sandbox silently falls back to full-access without SYS_ADMIN/Landlock).

| Category | Score |
|---|:---:|
| Safety / Isolation | 2 |
| Shipping Config & Tools | 2 |
| Request Handling & Scale | 2 |
| Harness Fit | 3 |

**Top strengths.** Dedicated, type-checked `codex` HarnessKind driving the exact correct shape `codex exec <prompt>` (`cli.go:157-170`) — better than a generic-cli fallback, with a shipped `--sample codex` CR. Pod-per-AgentRun maps cleanly onto Codex's recommended one-process-per-task unit. Working-dir → AgentFS binding + input materialization + durable kopia AgentFS give the edited workspace real persistence. Solid in-pod hardening.

**Top gaps.** The run pod that executes `codex exec` sets **no RuntimeClassName** → runc on a shared kernel (Codex's docs say the outer microVM is the *real* boundary). **No default-deny egress** for run pods — with `--ask-for-approval never` this is the one mandatory guardrail and it's absent. **Codex cannot run on the published distroless image** (no codex binary, no shell, no toolchains) — needs an unpublished custom `harness.image`; no e2e ever ran a real codex binary. Harness budget is tokens-only/post-hoc; **CLI output is stdout-only with stderr → `io.Discard` and tokens/toolcalls hardcoded to 0** (`cli.go:56`).

**Effort to ship.** **High.** Low *code* effort (kind exists) but no turnkey path and the two mandatory safety controls are unbuilt for runs. Must: build/publish an OCI image layering codex + git + toolchains (+ bubblewrap) onto `/agent`; add `RuntimeClassName` to the run pod and verify it doesn't fall back to runc; enforce default-deny egress; add `activeDeadlineSeconds` + per-tenant concurrency; capture stderr and `--json` events.

---

### 3.3 Anthropic Claude Code CLI (`claude --print` / Agent SDK)

**Profile.** Anthropic's official agentic coding CLI (a self-contained native binary; verified v2.1.159). `claude --print/-p` is the headless entrypoint that runs the Bash + Read/Edit/Write + WebFetch + Task tool loop autonomously. It is effectively an arbitrary-code-execution engine; its own docs **require** per-tenant whole-process isolation (microVM/container), non-root, and default-deny egress with cloud-metadata-IP blocking for any unattended `-p` run.

| Category | Score |
|---|:---:|
| Safety / Isolation | 2 |
| Shipping Config & Tools | 2 |
| Request Handling & Scale | 2 |
| Harness Fit | 2 |

**Top strengths.** Dedicated `claude-code` kind with the correct headless invocation `claude --print <prompt>` (`cli.go:137-155`), budget-bound ctx, AgentFS-bound cwd, clean timeout/cancel classification — unit-tested. Strong non-root substrate (refuses `--dangerously-skip-permissions` as root; matches the platform's uid-65532 hardening). Agent-blind secret injection exists (broker leases creds; secretRef-without-broker is a hard error). Durable per-tenant workspace fits Claude Code's writable-workspace need. Correct one-shot lifecycle matches Anthropic's "fresh sandbox per `claude -p` job, capture, tear down" model.

**Top gaps.** **No microVM for the run pod** — the Bash/Write ACE engine runs under runc on a shared kernel; kata-fc lives only on the serving path. **No egress containment** — a hijacked Bash session has unrestricted egress and can hit `169.254.169.254` (the CRITICAL risk in Claude Code's own hosting guide). Non-functional on the distroless image (no shell/git/claude). Env-driven config under-served (`PassthroughEnv` is dead; no `apiKeyHelper`/TTL hook; creds static-mounted, not short-lived). **No CLI cost/output capture** — stderr discarded, token/cost always 0, no `--output-format json` / `total_cost_usd` / `--max-budget-usd` parsing. No multi-turn/MCP/permission-mode wiring (SessionPolicy=persistent is Hermes-only).

**Effort to ship.** **Medium** for a functional-but-unsafe demo (custom image + `ANTHROPIC_API_KEY` via broker, `DISABLE_AUTOUPDATER=1`, single-shot only). **High** for safe multi-tenant: add `RuntimeClassName=kata-fc` to the run pod with a real KVM/devmapper node behind it, enforce egress (eBPF MapDriver or NetworkPolicy/proxy blocking metadata IPs), add `activeDeadlineSeconds` + concurrency limits, parse cost JSON + capture stderr, and supply SPIRE/cert-manager/Knative/Kata yourself.

---

### 3.4 OpenClaw ("Molty") — *confidence: high on profile; runtime internals partly second-hand (medium)*

**Profile.** Viral OSS (TypeScript/Node) self-hosted "personal AI agent" **gateway** (~68k stars) bridging messaging channels to coding agents with shell exec, file edit, browser, and a live Canvas. It is a **long-running multi-channel daemon** (HTTP :18789, webhooks, cron, multi-agent routing), **NOT** a one-shot CLI. Defining safety fact: tools run on the **HOST with full access** for the "main" session and **sandboxing is OFF by default**.

| Category | Score |
|---|:---:|
| Safety / Isolation | 2 |
| Shipping Config & Tools | 2 |
| Request Handling & Scale | 2 |
| Harness Fit | **1** |

**Top strengths.** The SmolAgent **serving path** (Deployment/StatefulSet replicas + Knative scale-to-zero/min-max default 50) is the correct exec-model match for a long-running gateway daemon, and node placement + AgentNodePool→Karpenter autoscaling are wired into exactly this path. The BUILT hardening substrate (restricted PSA + R-SBX-1 fail-closed runc rejection + NoKVMCapacity/gVisor fallback) directly counters OpenClaw's dangerous-by-default posture. Durable encrypted AgentFS can back `~/.openclaw`. SPIRE + brokered secrets give a per-pod identity OpenClaw lacks natively.

**Top gaps.** **No `openclaw` harness kind exists (grep returns nothing)** and no harness fits its model: a persistent multi-channel gateway with its own internal loop + tool catalog is not a one-shot CLI nor a model endpoint. The single-shot CLI driver (one exec, stderr discarded, one StepFinal) is **fundamentally incompatible with a listening daemon that never returns** — OpenClaw must run as a custom-image *serving workload*, bypassing the harness layer entirely. The eBPF egress cage is unenforced (critical for an agent that browses arbitrary URLs + hits many channel APIs). Published distroless image has no Node 24/pnpm/shell — a fully custom OCI image is mandatory. No queue/concurrency governance for a multi-session daemon; a lost gateway pod drops all live sessions (AgentFS restores files only).

**Effort to ship.** **High.** Not a harness target; run as a long-running SmolAgent: (1) build/publish a custom Node-24 OpenClaw image; (2) accept that the harness layer contributes nothing (OpenClaw's tool loop is opaque, MaxToolCalls inert, rely on OpenClaw's own off-by-default sandboxing forced on); (3) verify kata actually lands (needs devmapper bootstrap, unverified live); (4) build your own egress allow-list; (5) wire your own audit/metrics/quotas. Workable single-tenant on a properly bootstrapped kata cluster; a multi-tenant offering is a substantial custom build.

---

### 3.5 pi-mono (`pi` coding-agent CLI by Mario Zechner)

**Profile.** `@earendil-works/pi-coding-agent` — a minimal, provider-agnostic, BYO-key TypeScript/Node coding-agent CLI with a synchronous read/write/edit/bash loop. Deliberately anti-framework: **NO permission popups, NO built-in sandbox** ("run it in a container, or build your own confirmation flow"). The author explicitly makes isolation the platform's job.

| Category | Score |
|---|:---:|
| Safety / Isolation | 2 |
| Shipping Config & Tools | 2 |
| Request Handling & Scale | 2 |
| Harness Fit | **1** |

**Top strengths.** Solid baseline pod hardening *does* apply to the run pod. Bounded-execution core is real and e2e-green, with durable per-tenant AgentFS covering pi's "workspace must survive restarts." Secret-broker mechanism avoids the `--api-key` argv-leak pi warns about. `generic-cli` harness has the right shape (command override, working-dir binding, input materialization with path-traversal guard) **if** a real image is supplied. Mature shipping substrate to build on.

**Top gaps.** **The built-in `pi` HarnessKind is the WRONG product** — it's a thin HTTP driver hardcoded to `api.inflection.ai` (`http.go:35`), Inflection AI's consumer chatbot, **not** the target pi-coding-agent CLI. Using it silently runs a different system; the real pi must use `generic-cli`. Pi's #1/#2 needs are unbuilt on the run path: **no RuntimeClassName** (runs under runc despite pi shipping zero sandbox) and **no egress enforcement at all** (pi's bash-tool curl/git-clone exfil channel is completely uncontrolled). No image bundles Node 22.19+ + bash/git/tmux + pi + `/agent`. **Agent-blind injection does NOT hold for pi**: the broker serves static values to uid 65532; pi's bash tool runs as that same uid and inherits the env — the model can read/exfiltrate the provider key. No `activeDeadlineSeconds` for pi's no-max-step, tmux-spawning loop.

**Effort to ship.** **High and risky as-is.** A toy/single-tenant demo: ~1–2 days (Node-22 image + `generic-cli` AgentRun, NOT `kind=pi`; `PI_OFFLINE`/`PI_CODING_AGENT_DIR` env; broker key). A *safe* multi-tenant dev platform is partly greenfield (~3–6 weeks): set `RuntimeClassName=kata-fc` on runs *and prove the microVM boots*; build the eBPF cage or a default-deny NetworkPolicy/egress-proxy (block 169.254.169.254); add `activeDeadlineSeconds` + concurrency caps; re-do credentials so pi's bash tool can't read the key from its own env.

---

## 4. Aggregate Scorecard

| Target | Safety / Isolation | Shipping Config & Tools | Request Handling & Scale | Harness Fit | **Overall** |
|---|:---:|:---:|:---:|:---:|:---:|
| **Hermes Agent** | 2 | 3 | 2 | **5** | **3.0** |
| **OpenAI Codex** | 2 | 2 | 2 | 3 | **2.2** |
| **Claude Code** | 2 | 2 | 2 | 2 | **2.0** |
| **OpenClaw** *(see §3.4 framing)* | 2 | 2 | 2 | 1 | **2.0** |
| **pi-mono** | 2 | 2 | 2 | 1 | **1.8** |

**Reading the table.** Safety, Shipping, and Scale are **uniformly 2** because their limits are agent-independent and structural — they live on the AgentRun datapath (no microVM, no egress cage, no concurrency governance, distroless image with no CLI runtime). Differentiation is entirely in **Harness Fit**: Hermes wins decisively (native, richest impl, complete sample); Codex and Claude Code have dedicated-but-operationally-unproven CLI kinds (3/2); OpenClaw (no kind, daemon-vs-one-shot mismatch) and pi (false-friend `pi` kind, generic-cli-only) are non-fits at 1.

---

## 5. Safety Analysis

### How the platform makes these agents safe — the real, BUILT controls

- **In-pod hardening (consistently enforced).** Every agent and run pod gets `RunAsNonRoot` (uid/gid/fsgroup 65532), `Capabilities.Drop=[ALL]`, `SeccompProfile=RuntimeDefault`, and (for non-harness containers) `ReadOnlyRootFilesystem` (`workload.go:82-111`, `agentrun.go:59-67`). This is the floor and it is solid — a shared-kernel container baseline for any of the five targets.
- **microVM on the serving path.** SmolAgent pods default `RuntimeClassName=kata-fc`, with a **fail-closed admission guard** that rejects runc/unknown runtimes unless `allowHostEscape=true` (R-SBX-1, `smolagent_webhook.go:35-41`), enforced again in the reconciler, plus a `NoKVMCapacity`/gVisor fallback that never schedules an unisolated "kata" pod (`sandbox.go:44-68`). This is genuinely good — for OpenClaw (which runs as a serving workload) it's directly relevant.
- **SPIRE workload identity + secretless egress.** Pods mount the SPIRE workload-API socket read-only via `csi.spiffe.io`; a `ClusterSPIFFEID` binds `spiffe://smol-agents.ai/ns/<ns>/sa/<sa>`. The secret-proxy verifies a sender-constrained TraT (`req_wl == SO_PEERCRED-attested caller`, fail-closed), then mints a credential the **agentnet proxy injects on the outbound request so the agent never holds it** (`server.go:242-290`, `proxy/http.go:24-141`). This is e2e-proven against fakes with real SO_PEERCRED — the platform's strongest dimension.
- **Brokered secret injection.** A declared `secretRef` with **no broker is a hard error** — no silent unauthenticated call (`executor.go:354-373`).

### The honest gaps — the two controls these agents most need are NOT on the run datapath

| Gap | Reality | Why it matters for these targets |
|---|---|---|
| **AgentRun pods set no `RuntimeClassName`** | `agentrun.go:56-71` (verified 0 occurrences) → runs under runc on a shared host kernel | The highest-risk path (Claude Code/Codex/pi Bash, Hermes-adjacent exec) gets **no microVM**. Their own docs make the outer microVM the mandatory boundary. |
| **eBPF egress "cage" is unenforced** | `cgroup.Compile/EncodeRedirect/EncodeAllow` called only from `cmd/ebpf-probe`; operator programs no maps; **0 NetworkPolicy rendered** anywhere | A hijacked Bash/browser session has unrestricted egress and can reach `169.254.169.254` — the SSRF/credential-theft risk every CLI target flags as CRITICAL. |
| **AgentNet proxy/WireGuard never injected into run pods** | `BuildAgentRunPod` takes no networks param; `proxy.Sidecar.Run` has zero non-test callers; controller "does NOT inject sidecars" — and its `R-AN-PROXY-3` "BuildAgentRunPod renders networks" comment is **provably false** | No operator-rendered mTLS/TraT datapath for runs; the secretless chain is proven only inside the e2e probe. |
| **Agent-blind credentials don't hold for CLI harnesses** | broker leases land in the subprocess env via `mergeEnv → c.Env` (`cli.go:47,98-107`); `BuildBrokerConfigSecret` allow-lists ALL values to the single local uid (`secret_broker.go:108-130`) | For pi/Codex/Claude-Code, the model-driven bash tool (same uid 65532) can read/exfiltrate the leased key. Agent-blindness is real **only** on the HTTP/Hermes/proxy path. |
| **microVM silently degrades** | kata-fc needs a block-device snapshotter plain k0s lacks (e2e Skips it); explicit guard against silent runc fallback; Karpenter never live-verified | Even where kata is requested, the platform can't always *guarantee* a microVM booted. |
| **harness container drops `readOnlyRootFilesystem`** | `agentrun.go:116` (needed for CLI tools writing rootfs) | The strongest fs control is OFF for exactly the CLI/Hermes path. |
| **No `activeDeadlineSeconds`; harness budget tokens-only/post-hoc** | `executor.go:419-427`; verified 0 `activeDeadlineSeconds` | A hung/runaway harness burns the full wallclock as a soft, in-process boundary only; `MaxSteps`=1, `MaxToolCalls` inert. |

**Bottom line on safety:** a strong **identity + secret-handling + pod-hardening** substrate, but the two headline containment guarantees the docs lead with — **microVM-enforced isolation and an egress allow-list — are unwired on the AgentRun datapath that actually executes untrusted code.** Defense-in-depth as documented (attestation + TraT + eBPF) is really **2-of-3** in practice.

---

## 6. Scale & Request-Handling Analysis

### What it can handle today

The **execution/correctness core is BUILT and e2e-green** (single-node): pod-per-AgentRun reconcile (`RestartPolicy=Never`, pod lifecycle == run lifecycle), a Quint-modeled 4-axis budget, a validated run lifecycle state machine with RunResult fold, cancellation (ctx + pod delete), durable AgentFS restore + native sidecar, and in-run HTTP transient-failure retry (429/5xx/backoff honoring `Retry-After`). For **bounded single runs** this is correct and rigorous — more formally specified than any competitor in the set. The **SmolAgent serving path** adds Deployment/StatefulSet replicas + Knative scale-to-zero/min-max (default max 50) for persistent services.

### What it cannot handle today

| Limit | Reality | Consequence |
|---|---|---|
| **No request gateway/queue** | submission is k8s API create + reconcile only; status via 5s poll + Pod watch | No synchronous API, no batching, no priority/fairness. |
| **No run-admission concurrency control** | no `MaxConcurrentReconciles` (verified) → controller-runtime **default = 1 worker**; no per-tenant/per-agent run quota | Concurrent-run scale bounded only implicitly by k8s scheduling + node capacity. |
| **No `activeDeadlineSeconds`** | verified 0 across operator/+pkg/ | Wallclock is a soft, in-process ctx boundary a hung runtime can outlast. |
| **Autoscaling↔run coupling absent** | placement + AgentNodePool→Karpenter wired only to the **service path**; AgentRun pods get no nodeAffinity/toleration/RuntimeClassName | A "kata" AgentRun triggers **no** metal-node provisioning and isn't pinned to a kata node. |
| **Karpenter never live-verified** | envtest/golden coverage only (P1.6 deferred) | The node-provisioning loop is unproven on a real cluster. |
| **No pod-level crash-resume** | `RestartPolicy=Never`; OOM/evicted/node-lost run → terminal Failed; AgentFS restores **files**, not loop-step state | A crashed run is lost; no checkpoint-and-resume. `RequiresAction` (human/tool-in-loop pause) is dead scaffolding. |
| **Multi-tenancy is namespace-soft** | no Tenant CRD, no per-tenant ResourceQuota/NetworkPolicy/quota, no onboarding automation | Each tenant is a manual namespace + SPIRE + Knative + sandbox-runtime lift. |

**Bottom line on scale:** strong on **correct, bounded, durable single runs**; thin on **horizontal-scale governance, run-level fault tolerance, and the run-side half of the autoscaling coupling.** It is honestly a single-controller async scheduler leaning entirely on Kubernetes + Karpenter, with the Karpenter leg unproven and wired to the wrong path for runs.

---

## 7. Competitive Comparison

| Framework | Isolation | Scale | Harness-agnostic | Secrets / Identity | OSS / Self-host | Maturity |
|---|---|---|---|---|---|---|
| **smol-agents (ours)** | **PARTIAL/split** — kata-fc on **serving** path (R-SBX-1 guard, gVisor fallback) but **runs have NO RuntimeClassName** (runc/shared kernel); eBPF egress cage **unenforced**; no NetworkPolicy. Pod-hardening real. | Async pod-per-run, Quint 4-axis budget, lifecycle+fold, durable AgentFS — e2e-green **single-node**. **No** gateway/queue, **1** reconcile worker, no quota, **no activeDeadlineSeconds**, no crash-resume. Karpenter envtest-only + service-path-only. | **PARTIAL** — 8 kinds, +~50-100 LOC each, but **only HTTP works OOB**; 5 CLI kinds need a custom image (none published, Version dead); tools not wired. | **STRONGEST relative dim** — SPIRE + broker dynamic-mint + sender-constrained TraT + **agent-blind proxy injection** (HTTP path); secretRef-without-broker is hard error. CLI caveat: leased key in subprocess env. | **FULLY OSS, no license/phone-home**; Helm (9+7 presets, CI-asserted) + agentctl (k8s/aws/hetzner, CF-tunnel proven). MEDIUM-HIGH from scratch (SPIRE/cert-manager/Knative/Kata are prereqs agentctl won't install). | **EARLY / single-node-tested**; 1 harness (Hermes/HTTP) proven on real infra; solo-maintained, pre-1.0; headline isolation/egress over-claimed by docs. |
| **AWS Bedrock AgentCore** | **STRONGEST managed** — dedicated **Firecracker microVM per session**, destroyed+sanitized on end; tool sandboxes with selectable egress. (2025-26 isolation-bypass research; MMDSv2-only hardening Feb 2026.) | Serverless, ~per-second billing, scale-to-zero, sync/SSE/async/WebSocket, 8h session cap, 100MB payloads, versioned zero-downtime rollouts. | **YES (most)** — ship any ARM64 container on a port-only contract (HTTP/MCP/A2A/AG-UI); SDK optional; **Claude Code/Codex run unmodified**; AWS publishes samples. | **Cleanest agent-blind** — AgentCore Identity vaults tokens bound by (workload-identity, user-id); JWT→workload-token exchange; inbound IAM/OAuth. | **MANAGED-ONLY / AWS-hosted**; no self-host. | **Production-grade**, documented quotas/rollouts. ARM64-only, 8h cap, AWS lock-in. |
| **Google Vertex AI Agent Engine + ADK** | **LEAST transparent** — "fully managed runtime," **no published boundary**; separate gVisor-class code-exec sandbox (~300s). | Managed serverless, sub-second cold starts, managed Sessions+Memory; method-based (sync/async/stream); thin public timeout/concurrency numbers. | **NO / weakest** — **Python-class-only** deployment; no bring-any-container contract; unmodified CLI not supported (drop to GKE/Cloud Run). | Agent Identity (IAM); SA + keys + OAuth into the Python process; no dedicated agent-blind broker. | **MANAGED-ONLY** runtime (ADK framework is OSS/self-hostable on GKE). | Managed GCP service; poorest fit for arbitrary CLI agents; docs in flux. |
| **E2B / Modal / Daytona** | **VARIES** — E2B = **Firecracker microVM**; Modal = **gVisor** (no microVM); Daytona = Docker default (Kata optional). | Ephemeral fast-start fabrics. E2B ~150ms, 24h cap, ~100 concurrent. Modal sub-second, **0→50,000+**, best GPU. Daytona ~90ms, snapshot+**fork**, unlimited. | **FULLY agnostic at process level** — all run arbitrary binaries (E2B ships a `claude` template; Daytona documents Claude Code). Modal control-plane is Python-SDK-locked. | E2B has **egressTransform** (egress proxy injects cred headers — mirrors our design). Modal/Daytona = env-injected secrets. | **SPLIT** — E2B Apache-2.0 (heavy self-host) + Daytona AGPLv3 self-host **yes**; Modal managed-only. | Commercially mature execution **primitives** (not control planes); far more battle-tested at concurrency. |
| **Cloudflare Agents (Workers+DO+Sandboxes)** | **Container-grade, not microVM** — isolated Linux containers fronted by Durable Objects; lower tier = V8 isolates. | Auto-scaled, globally placed, sleep-after-10min scale-to-zero, snapshot recovery, durable fibers; cold start unquantified. | **PARTIAL** — Sandbox tier (PTY, full toolchains) is agnostic; the **Agents SDK is opinionated TypeScript** (adopt it and you're in CF's model). | **Egress proxy** attaches credentials (like E2B/our proxy); identity = per-sandbox DO; no SPIFFE. | **MANAGED-ONLY** runtime (sandbox-sdk source OSS but CF-network-only). | GA, edge-native, polished durable execution; container-grade only. |
| **kagent / K8s+Kata (incl. Agent Sandbox, SIG-Apps)** | **TWO things** — kagent core = **container-level/RBAC** ("sandbox" is RBAC, not kernel); **Agent Sandbox** = real pluggable gVisor + Kata microVM per-pod via runtimeClassName. | kagent = sync A2A, long-running Deployments + HPA/KEDA, **no scale-to-zero**. Agent Sandbox = per-sandbox lifecycle, pause/resume, **scale-to-zero**, **SandboxWarmPool** for near-instant cold start. | **SPLIT** — kagent BYO must **speak A2A** (wrap CLI in a shim); **Agent Sandbox is FULLY agnostic** — any image/any binary via plain podTemplate (the "arbitrary CLI in a microVM" shape). | Standard K8s Secrets (ModelConfig/podTemplate); **no built-in broker** — but Agent Sandbox composes cleanly with SPIFFE/Vault/sidecar you add. | **FULLY OSS + self-hostable** (Apache-2.0; CNCF Sandbox / kubernetes-sigs). | Open but **early** at the API layer; Agent Sandbox is **v1alpha1** and **execution-only** (no orchestration/workflow/broker). **Our closest architectural OSS competitor.** |

### Where we win

- **Identity + secretless-credential substrate** is the genuine differentiator and is BUILT/e2e-proven (vs fakes, real SO_PEERCRED): SPIRE + broker dynamic-mint + sender-constrained TraT + agent-blind proxy injection. Deeper than kagent/Agent Sandbox (BYO broker), Modal/Daytona (env secrets), Vertex (inject-SA); **peer** to AWS AgentCore Identity / E2B egressTransform / Cloudflare egress proxy — and we ship it as **composable OSS**, not a closed managed service.
- **Fully OSS + self-hostable, no license key, no phone-home, on commodity K8s.** We beat the entire managed tier (AWS/Vertex/Azure/Modal/Cloudflare are managed-only) and LangGraph (license-gated + beacon callout) on openness/air-gap-ability, and ship more OOB than the OSS K8s tier (real operator + CRDs, Helm with CI-asserted presets, agentctl multi-target deploy, multiarch ghcr).
- **Formally-modeled bounded-execution core** (Quint 4-axis budget + validated lifecycle + RunResult fold + cancellation + in-run retry). **No competitor in the set advertises a formally-modeled budget/lifecycle.**
- **Durable, encrypted, content-addressed AgentFS** (kopia/S3 snapshots, native backup sidecar, SIGTERM upload, secretKeyRef-projected creds) as a first-class primitive — richer than the OSS K8s tier's bare PVC.
- **Defense-in-depth design coherence** behind one operator (isolation + identity + egress + budgets + durable storage + node provisioning) is wider than any single OSS competitor (kagent orchestration-only, Agent Sandbox execution-only, Dapr durability-only) — *when the unwired pieces land.*

### Where we lose

- **MATURITY** — early-stage, single-node-tested, one HTTP harness proven, solo-maintained, pre-1.0. Competitors are GA/production with documented quotas. The single biggest gap.
- **ISOLATION-FOR-RUNS** — the AgentRun pod sets no RuntimeClassName (runc/shared kernel) and the eBPF egress cage is unenforced. AWS gives per-session Firecracker; E2B/Agent-Sandbox-with-Kata give microVM; even Modal/Cloudflare give a real boundary + egress credential injection. For our highest-risk path we give **neither**.
- **HARNESS-AGNOSTICISM is mechanism-only for CLI agents** — AWS AgentCore (any container, port-only) and Agent Sandbox (any binary, plain podTemplate) host an unmodified `claude`/`codex` **today**; we require a custom OCI image the operator must build, and tools/MCP aren't wired.
- **SCALE GOVERNANCE & FAULT TOLERANCE** — no gateway/queue, 1 reconcile worker, no quota, no `activeDeadlineSeconds`, no crash-resume; Karpenter envtest-only and service-path-only. Modal scales 0→50,000+; Agent Sandbox ships warm pools.
- **MULTI-TENANCY** is namespace-by-convention; managed platforms deliver hard tenant isolation as a product property.
- **DAY-2 OBSERVABILITY** is immature — service-level OTLP + operator Prometheus only; per-run/step GenAI spans dead, CLI output stdout-only with cost hardcoded to 0; no dashboards/alerts/SLOs. Every managed competitor ships first-class tracing/cost accounting.

---

## 8. Gaps & Prioritized Recommendations

To become a top-tier safe AI-dev platform, the path to parity is concrete and known. Prioritized by impact on the verified gaps:

### P0 — Close the run-datapath containment gap (the existential gap)

1. **Wire `RuntimeClassName` (+ nodeAffinity + toleration + do-not-disrupt) into `BuildAgentRunPod`.** Today runs execute under runc on a shared kernel. Bind AgentRun pods to a kata-capable AgentNodePool so a "kata" run actually triggers provisioning *and* boots a microVM — and add a post-boot assertion against silent runc fallback (the e2e already guards this; promote it to the operator).
2. **Enforce egress on run pods.** Either wire the eBPF `MapDriver` into the operator (the pure `Compile/EncodeRedirect/EncodeAllow` path is built and tested — only the production driver + sidecar injection are missing) **or**, as a faster floor, render a **default-deny NetworkPolicy + egress-proxy** that allow-lists the model endpoint(s) + required registries and **blocks 169.254.169.254 / RFC1918 / link-local**. Fix the false `R-AN-PROXY-3` claim and actually inject the agentnet proxy/WireGuard sidecar into run pods.
3. **Fix CLI agent-blindness.** For CLI harnesses, move from env-injected static leases to **dynamic-mint via the egress proxy** so the model's bash tool (uid 65532) cannot read the provider key from its own environment.

### P1 — Make CLI harness hosting operationally real

4. **Publish per-kind harness OCI images** bundling each CLI (claude/codex/aider/goose) + a POSIX shell + git + `/agent` at the right path, multiarch. Wire `HarnessSpec.Version` (currently dead) to resolve the image tag, and add a per-kind default image so a literal `harness.image` is no longer mandatory.
5. **Capture stderr + parse structured output.** Stop discarding stderr (`cli.go:56`); parse `--json`/`--output-format json` for `session_id`, `total_cost_usd`, and real token/toolcall counts. This unblocks per-run cost chargeback and the (currently dead) GenAI trace spans.
6. **Add a real CLI-harness e2e** on a custom image (not `/bin/sh` stubs), and a Hermes e2e in `test/` to make the "proven on real infra" claim CI-reproducible.

### P2 — Scale governance, fault tolerance, multi-tenancy

7. **Add `activeDeadlineSeconds` on run pods** and **`MaxConcurrentReconciles` + per-tenant/per-namespace run-concurrency quota**. Consider an admission queue for fairness/priority.
8. **Run-level crash-resume** (or at minimum a documented retry policy) — today an OOM/evicted run is terminally lost; AgentFS restores files but not loop-step state.
9. **First-class multi-tenancy** — a Tenant CRD (or onboarding automation) that wires per-tenant ResourceQuota + NetworkPolicy + SPIRE ClusterSPIFFEID + Knative + sandbox-runtime, replacing the manual namespace-by-hand lift.
10. **Live-verify Karpenter** on a real cluster (P1.6) and wire the autoscaling↔isolation loop to the **run** path, not just services.

### Honest framing for stakeholders

The platform's **substrate is differentiated and real** — OSS openness, SPIRE + secretless credentials, a formally-modeled bounded-execution core, and durable encrypted storage are things the competitors largely don't combine. But it currently hosts agents the way the *docs* describe, not the way the *code* enforces: for the AgentRun path that runs untrusted code, **there is no microVM and no egress cage today.** Until P0 lands, hosting any of Codex / Claude Code / pi via `codex exec` / `claude -p` / `pi` on this platform **contradicts those agents' own mandatory hosting requirements**, and Hermes (the best-fit, 5/5-harness target) still requires you to hand-roll containment around the unmanaged RCE gateway. The recommendations above are the concrete, scoped path from "well-architected design with a proven correctness core" to "top-tier safe AI-dev platform."
