# Spec: Full Support for OpenClaw ("Molty") over HTTP

> **✅ Decisions resolved 2026-06-03 — see [decisions.md](../design/decisions.md).** D2 (interactive first-class) + D4 (`spec.session{required,interactive}`) + D5/D9 (driver-mode attach via bundled OIDC) for the daemon; D1+D3: serving-pod egress floor default-on, kata enforced. Where this doc still says OPEN/PROPOSED and conflicts, the decision log wins.

> Status: **DESIGN / PROPOSAL** — not built. Grounded against v0.2.0 source (2026-06-03)
> and against the current OpenClaw docs (fetched 2026-06-03; see *External interface
> research*).
> Category: agent. Effort: **XL** (highest of the agent specs).
> Extends: [custom-agent-images.md](../design/custom-agent-images.md) — read that first.
> This spec deepens the custom-image serving path for the specific, hard case of a
> long-running **multi-channel daemon** (OpenClaw) and adds a way to drive it for
> one-shot/turn requests over HTTP.

OpenClaw (mascot **Molty** 🦞; formerly Moltbot / Clawdbot) is a self-hosted,
multi-channel AI-agent **daemon**: everything flows through one long-running
**Gateway** process that owns sessions, channels (WhatsApp / Telegram / Discord /
Slack / Signal / iMessage / 50+), tools, a browser, and an embedded agent runtime.
It is the canonical "openclaw-class" workload that [custom-agent-images.md](../design/custom-agent-images.md)
was written for — but supporting it *well, securely, over HTTP* surfaces gaps in
every layer of the platform: the serving-path workload builder, egress
enforcement, the harness/HTTP drive surface, session durability, and terminal
exposure. This spec is the end-to-end plan and is honest that it is large.

---

## 1. Summary

"Full OpenClaw support over HTTP" means a tenant can run a one-line `SmolAgent`
that brings up the OpenClaw Gateway as a hardened serving workload — pinned to
`kata-fc`, fronted by a platform-emitted egress allow-list covering its many
channel/provider/browse endpoints, with its **own off-by-default sandbox forced
on** — and can drive it for **request/response turns over HTTP** (a new
`HarnessKind=openclaw` HTTP adapter and/or an agentgateway route that speaks
OpenClaw's control protocol), while keeping multi-session concurrency and
surviving pod loss. The outcome: OpenClaw runs on the platform with the platform's
isolation guarantees actually holding, instead of as an un-caged daemon that the
operator merely schedules.

The honest framing: **most of this is new work that other specs must land first.**
OpenClaw is not CLI-shaped, so the one-shot harness model does not fit; it is a
WebSocket-first control plane, so the HTTP drive is an adapter, not a passthrough;
and its native sandbox wants nested Docker, which is **incompatible** with the
platform's `kata-fc` + read-only-root + non-root + default-deny posture. Each of
those is a real design decision called out below.

---

## 2. Current state

### What exists (v0.2.0, code-checked)

| Capability | Status | Evidence |
|---|---|---|
| `SmolAgent` serving path with `spec.image` override | **Built** | `operator/api/v1/smolagent_types.go:26-28`; `operator/internal/builders/workload.go:18-25` (`AgentImage`) |
| Hardened serving Pod (uid 65532, RO-root, drop ALL, seccomp `RuntimeDefault`) | **Built, non-overridable** | `operator/internal/builders/workload.go:82-107` |
| `kata-fc` default + fail-closed webhook (rejects `runc` unless `allowHostEscape`) | **Built** | `BuildAgentPodSpec` `workload.go:41-45`; `operator/internal/webhooks/smolagent_webhook.go:35-41`; `pkg/sandbox/sandbox.go` |
| Probes on `:8080` `/healthz` `/readyz`; ports `http:8080`, `private-mtls:8443` | **Built** | `workload.go:50-71` |
| SPIRE CSI socket + secret-broker UDS sidecar mounts | **Built** | `workload.go:91-94,113-150` |
| HTTP harness driver (POST JSON, prompt-field/response-field, retry) | **Built** | `pkg/agentruntime/harness/http.go:48-125`; registry `pkg/agentruntime/harness/iface.go:90-101` |
| agentgateway (HTTP → NATS turn queue for AgentSession) | **Built** | `cmd/agentgateway/main.go:1-154` |
| Durable AgentSession + NATS gateway (P3/P4) | **Built** (skeletal spec) | `cmd/agentgateway`; `pkg/sessionqueue`; see [durable-session-architecture.md](../design/durable-session-architecture.md) |

### What is missing / blocks this spec

| Gap | Why it blocks OpenClaw | Evidence |
|---|---|---|
| **No SmolAgent-emitted NetworkPolicy on the serving path** | OpenClaw talks to many endpoints (provider, GitHub, Slack/Telegram/Discord APIs, browse targets); default-deny with no allow-list = a dead daemon. Egress builders target only the run/session datapaths. | `operator/internal/builders/run_sandbox.go:55-69` (`BuildAgentRunEgressPolicy` / `BuildAgentSessionEgressPolicy` only); SmolAgent reconcilers emit none — see [custom-agent-images.md](../design/custom-agent-images.md) §Security model. |
| **No per-workload egress allow-list field** on `SmolAgent` | No way to express OpenClaw's endpoint set declaratively; `AgentNetwork` is not wired on any datapath. | `smolagent_types.go:8-39` has no egress field; AgentNetwork wiring gap tracked in [agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md). |
| **No `HarnessKind=openclaw`** and no HTTP adapter for OpenClaw's protocol | The generic-http harness POSTs flat JSON and reads one dotted field; OpenClaw drives over **WebSocket** (or its CLI), not a single REST POST. | `harness.go:38-60` (kinds); `http.go:48-74` (GenericHTTP is single POST). |
| **Resource envelope hard-coded** at 500m/512Mi | OpenClaw (Node 24 + browser + channels) needs far more; the serving builder compiles limits in. | `workload.go:72-81`. |
| **No terminal / canvas exposure** | OpenClaw is interactive (WebChat, debug tools, browser canvas); platform has no terminal path. | none — see [terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md). |
| **No sticky-session affinity; AgentFS persists files not heap** | OpenClaw holds warm in-memory session state; lost pod loses live sessions. | [custom-agent-images.md](../design/custom-agent-images.md) §State & concurrency. |
| **eBPF cage inactive on serving path** | Cannot rely on syscall/egress cage for the daemon. | maps programmed only by `cmd/ebpf-probe`; see [agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md). |

> **NOT built callout.** Nothing OpenClaw-specific exists today. `spec.image`
> would let you *schedule* the OpenClaw Gateway, but with no egress allow-list,
> no forced sandbox, an undersized resource envelope, and no drive surface, that
> is a demo, not "full support." Treat every "Concrete change" below as a proposal.

---

## 3. External interface research (OpenClaw / Molty — current)

All facts below were fetched 2026-06-03; OpenClaw moves fast and these supersede
the Jan-2026 training cutoff. Sources are listed at the end of this section.

### 3.1 Process model

- OpenClaw is **one long-running Gateway process** — "everything flows through one
  process." It is a daemon (launchd/systemd user service via
  `openclaw onboard --install-daemon`), **not** a one-shot CLI. This confirms the
  decision in [custom-agent-images.md](../design/custom-agent-images.md): it is a
  `SmolAgent` serving workload, never an `AgentRun` harness.
- **Node 24 recommended, Node 22.19+ minimum.** Install: `npm i -g openclaw@latest`
  (or `pnpm add -g openclaw@latest`).

### 3.2 Network interface & port

- The Gateway is a **WebSocket server**, the single control plane for sessions,
  channels, tools, and events. **Default port `18789`.**
- Config `gateway: { mode: 'local', port: 18789, bind: 'loopback' }` — by default
  it **binds loopback only**. The control UI / dashboard is at
  `http://localhost:18789/openclaw`. WebChat + debug tools are served here.
- The documented inbound protocol is WebSocket RPC; there is an HTTP surface for
  the control UI and (per the RPC reference) **JSON-RPC patterns** — some channel
  integrations use an HTTP+JSON-RPC daemon with an SSE event stream
  (`/api/v1/events`) and health (`/api/v1/check`), others line-delimited JSON-RPC
  over stdio. There is **no documented stable "POST a prompt, get the answer"
  REST endpoint** for the agent itself.

> **Key consequence:** "OpenClaw over HTTP" is **not** a thin reverse-proxy to a
> REST API. The two viable one-shot drive surfaces are (a) the **`openclaw agent`
> CLI** invoked inside the pod, or (b) a **WebSocket RPC client** that opens a
> session, sends a message, and awaits the reply. See Design §4.3.

### 3.3 Programmatic one-shot drive

- CLI one-shot exists: `openclaw agent --message "Ship checklist" --thinking high`
  drives the embedded agent and prints to the channel/stdout. Other CLI verbs:
  `openclaw message send --target <addr> --message "..."`, `openclaw gateway
  status|restart`, `openclaw dashboard`.
- Sessions live on disk at `~/.openclaw/agents/<agentId>/sessions`; "direct chats
  collapse to the agent's **main session key**, so true isolation requires **one
  agent per person**." This is load-bearing for our multi-tenancy story (§4.5).

### 3.4 Agent runtime & model config

- Config file: **`~/.openclaw/openclaw.json`** (JSON5). Top-level keys:
  `identity`, `agent`/`agents`, `channels`, `session`, `tools`, `sandbox`,
  `logging`, `gateway`, `env`, `auth`, `models`.
- Model: `agent.model = { primary: 'anthropic/claude-sonnet-4-5', ... }`; custom
  providers under `models.providers.<name> = { baseUrl, apiKey }`. Env
  substitution `"${VAR}"` is supported throughout — **this is our hook for broker
  secret injection** (§7).
- Default runtime is the bundled OpenClaw agent runtime with per-sender sessions;
  it can also dispatch to external coding CLIs.

### 3.5 Sandboxing (OpenClaw's own — off by default)

This is the single most important external fact for this spec.

- `sandbox.mode` defaults to **`"off"`**. Values: `off` | `non-main` | `all`.
- `sandbox.scope`: `agent` (default) | `session` | `shared`.
- `sandbox.backend`: **`docker`** (default) | `ssh` | `openshell`.
- Docker backend: `sandbox.docker.image` defaults to
  `openclaw-sandbox:bookworm-slim` (**no Node inside**); `sandbox.docker.network`
  defaults to **`"none"`**; `sandbox.docker.setupCommand` runs once at container
  create and **requires root + writable root + network egress** to install
  runtimes; `sandbox.docker.binds` (`host:container:mode`) bypass isolation
  (OpenClaw blocks `/etc`,`/proc`,`/sys`,`~/.ssh`,`~/.aws`). `tools.elevated`
  explicitly **escapes** the sandbox.
- OpenClaw's own docs: *"This is not a perfect security boundary."*

> **Hard conflict.** OpenClaw's sandbox = **Docker-in-the-OpenClaw-process**
> (spawn child containers via a Docker daemon, run setupCommands as root, mount
> binds). Our serving pod is **non-root uid 65532, read-only root, drops ALL
> caps, no Docker socket, inside a `kata-fc` microVM, default-deny egress.** You
> cannot run OpenClaw's Docker sandbox inside our pod as-is. The platform's answer
> (§7) is to treat **kata-fc + our egress cage as the real sandbox** and force
> OpenClaw's tool execution into a mode that does not require nested Docker, OR
> accept a documented weaker posture. This is an open decision (§10, D1).

**Sources** (fetched 2026-06-03):
[openclaw/openclaw](https://github.com/openclaw/openclaw) ·
[docs.openclaw.ai](https://docs.openclaw.ai/) ·
[Gateway sandboxing](https://docs.openclaw.ai/gateway/sandboxing) ·
[Sandbox CLI](https://docs.openclaw.ai/cli/sandbox) ·
[Multi-agent](https://docs.openclaw.ai/concepts/multi-agent) ·
[RPC reference](https://docs.openclaw.ai/reference/rpc) ·
[Configuration guide](https://moltfounders.com/openclaw-configuration).

---

## 4. Design

### 4.0 Two layers, decoupled

```
                       ┌──────────────────────────────────────────────┐
   one-shot/turn  ───► │  Drive surface (HTTP)                         │
   callers             │  (a) HarnessKind=openclaw  (WS/CLI adapter)   │
                       │  (b) agentgateway route → OpenClaw protocol   │
                       └───────────────────┬──────────────────────────┘
                                           │ WS :18789 / exec CLI
                       ┌───────────────────▼──────────────────────────┐
   channels (Slack,    │  SmolAgent serving workload (custom Node img) │
   Telegram, browse) ◄─┤  OpenClaw Gateway daemon                      │
                       │  - kata-fc microVM, RO-root, uid 65532        │
                       │  - secret-broker sidecar (UDS) → ${VAR}       │
                       │  - egress: platform allow-list NetworkPolicy  │
                       │  - StatefulSet PVC for ~/.openclaw + AgentFS  │
                       └──────────────────────────────────────────────┘
```

Layer 1 (serving workload) is the must-have and is mostly an extension of
[custom-agent-images.md](../design/custom-agent-images.md). Layer 2 (HTTP drive)
is the "(with http)" ask and is additive — you can ship Layer 1 and reach OpenClaw
via its own channels without Layer 2.

### 4.1 Serving workload (extends custom-agent-images.md)

Run OpenClaw as `SmolAgent{ deploymentKind: statefulset, replicas: 1, image:
<custom node image> }`. The StatefulSet builder already attaches a 1Gi `state`
PVC and stable identity (`workload.go:203-238`), which maps to OpenClaw's
on-disk `~/.openclaw` (config + per-agent sessions). The custom image carries
Node 24, OpenClaw, git, and a small **bind-shim** so the Gateway answers our probe
contract:

- OpenClaw binds `:18789` (its control plane). Our builder probes `:8080`
  (`workload.go:57-71`). The image MUST expose **`/healthz` + `/readyz` on
  `:8080`** — a tiny sidecar-free shim in `server.js` that proxies probe paths
  and binds `0.0.0.0:8080`, while OpenClaw itself listens on `:18789`. (Default
  OpenClaw binds loopback; we set `gateway.bind` to all-interfaces so the drive
  surface can reach `:18789` in-pod or in-cluster — see §7 for the risk.)

### 4.2 Resource envelope

The compiled-in 500m/512Mi (`workload.go:72-81`) is far too small for Node 24 +
headless browser + channels. This spec depends on **per-workload resource
overrides** on `SmolAgent` (proposed in [custom-agent-images.md](../design/custom-agent-images.md)
follow-ups). OpenClaw baseline target: **requests 1 CPU / 2Gi, limits 4 CPU /
8Gi**, tunable. Without this field, OpenClaw OOMs/throttles immediately.

### 4.3 HTTP drive surface — two options

Both target one-shot/turn requests (the platform's bread-and-butter). They are
**not mutually exclusive**; pick by where you want the protocol code.

**Option A — `HarnessKind=openclaw` (HTTP-kind harness, recommended).**
A new harness adapter in `pkg/agentruntime/harness/openclaw.go` that, given an
OpenClaw Gateway URL, drives **one turn** and returns the reply. Because OpenClaw
is WS-first, the adapter opens a WebSocket to `ws://<gw>:18789`, performs the
session/send/await-reply RPC, and folds the text into `harness.Response.Output`.
This reuses the existing HTTP-kind plumbing (`HarnessHTTPSpec.URL`, headers,
retry, budget) and the registry (`iface.go:90-101`). It composes with the run
datapath's existing sandbox + egress (`run_sandbox.go`).

> Note: the existing `GenericHTTPHarness` (`http.go:48-74`) is a **single flat
> POST**; it cannot speak OpenClaw's WS RPC. `HarnessKind=openclaw` is a distinct
> implementation, not a config of generic-http. (See
> [loop-mode-tools-and-invokers.md](loop-mode-tools-and-invokers.md) for the
> broader harness-vs-loop framing and [agent-pi-mono-http.md](agent-pi-mono-http.md) /
> [agent-hermes.md](agent-hermes.md) for sibling HTTP-harness specs.)

**Option B — agentgateway route.** Extend `cmd/agentgateway` (today: HTTP →
NATS turn queue for AgentSession, `main.go:39-91`) so a turn whose target Agent is
an OpenClaw daemon is dispatched to the daemon's WS endpoint instead of (or in
addition to) the NATS worker. This keeps the durable/queued semantics and
horizontal scaling agentgateway already has, and is the better fit for the
**multi-session, lost-pod-durable** requirement (§4.5). Cost: agentgateway gains
an OpenClaw protocol client and a session→pod routing table.

Recommended: ship **Option A** first (smallest, reuses harness registry), then
**Option B** for durable multi-session at scale.

### 4.4 Forcing OpenClaw's sandbox + the platform cage

We do not run OpenClaw's Docker sandbox (nested Docker is incompatible — §3.5).
Instead the platform stamps a **rendered `~/.openclaw/openclaw.json`** (via the
config ConfigMap mounted at `/etc/smol-agents`, `workload.go:117`) that:

1. Sets `sandbox.mode` to a non-`off` value **and** pins `tools.elevated.enabled:
   false` so tools cannot escape. (Even though we don't use Docker isolation,
   forcing mode off `off` keeps OpenClaw's own tool-gating active.)
2. Constrains `tools` to the set the tenant allows (no shell-elevated).
3. Relies on **kata-fc microVM + the platform egress allow-list** (§4.6) as the
   true blast-radius boundary — documented to the tenant as *the* sandbox.

This is the inversion the maintainer must bless (§10 D1): **platform isolation
replaces OpenClaw's self-sandbox**, rather than nesting two sandboxes.

### 4.5 Multi-session concurrency + lost-pod durability

OpenClaw keeps live sessions in memory and on disk under `~/.openclaw`. Two
truths from [custom-agent-images.md](../design/custom-agent-images.md): AgentFS
snapshots **files, not heap**, and there is **no sticky affinity**.

- **Single-instance (default):** `statefulset replicas: 1` + the `state` PVC. On
  pod loss, OpenClaw reloads sessions from the PVC's `~/.openclaw/agents/*/sessions`
  (file-durable). In-flight turn state in heap is lost; the turn is re-driven.
- **Scale-out:** OpenClaw's "one agent per person" session model (§3.3) means
  horizontal scale needs **session→pod routing**, not load-balancing. Route via
  the **agentgateway + NATS** path (Option B) so each session key maps to a stable
  worker; see [agent-session-scaling.md](../design/agent-session-scaling.md) and
  [agentsession-scaling-impl.md](agentsession-scaling-impl.md). Do **not** put
  multiple OpenClaw replicas behind a round-robin Service.
- **Checkpoint cadence:** reuse AgentFS periodic snapshots
  (see [durable-session-architecture.md](../design/durable-session-architecture.md))
  to back up `~/.openclaw` so a lost PVC is recoverable.

### 4.6 Egress allow-list

OpenClaw's endpoint set is large and tenant-specific: the model provider, GitHub,
plus whichever channels are enabled (Slack/Telegram/Discord/Signal/… APIs) and any
browse targets. The platform must emit a **default-deny + allow** NetworkPolicy for
the serving Pod from a declarative `SmolAgent` field (§5), reusing the
`buildEgressPolicy` shape (`run_sandbox.go:73-123`) but with tenant CIDRs/FQDNs.
This is the serving-path analogue of
[agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md) and
must coordinate with it.

### 4.7 Interactive terminal / canvas

OpenClaw's WebChat + debug tools live at `http://localhost:18789/openclaw`, and it
drives a browser canvas. Interactive human access (debug a stuck session, watch
the canvas) routes through the platform terminal/exposure work in
[terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md): an
authenticated reverse path to the in-pod `:18789` UI (and/or a tmux/exec shell),
gated by SPIFFE + the gateway, **never** by binding `:18789` publicly.

---

## 5. Concrete changes

> All proposals. File:line targets are insertion points, not existing code.

### 5.1 CRD: `SmolAgent` serving-path additions

Add to `SmolAgentSpec` (`operator/api/v1/smolagent_types.go:8-39`) and mirror into
`operator/config/crd/smolagents.smol-agents.ai_smolagents.yaml`:

| Field | Type | Default | Validation | Purpose |
|---|---|---|---|---|
| `spec.resources` | `corev1.ResourceRequirements` | nil → compiled 500m/512Mi | standard quantities | Raise the envelope for heavy daemons (OpenClaw). Consumed in `workload.go:72-81`. |
| `spec.egress` | `EgressSpec` | nil → no policy (back-compat) | see below | Per-workload egress allow-list → serving NetworkPolicy. |
| `spec.egress.allowedCIDRs` | `[]string` | `[]` | CIDR each | Public destinations the daemon may reach (80/443). |
| `spec.egress.allowedFQDNs` | `[]string` | `[]` | DNS-1123 | FQDN allow-list (requires DNS-aware policy / CNI; else documented as advisory). |
| `spec.egress.denyMetadata` | `bool` | `true` | — | Keep 169.254/16 blocked (always recommended). |
| `spec.ports[]` | `[]ContainerPortSpec` | nil | name+port | Declare extra ports (e.g. `control:18789`) so probes/Service know them. |

Reuse `EgressSpec`/`AgentNetwork` shape from
[agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md) rather
than inventing a parallel type — coordinate field names with that spec.

### 5.2 Operator: emit serving-path NetworkPolicy

- New builder `BuildSmolAgentEgressPolicy(cr *v1.SmolAgent) *networkingv1.NetworkPolicy`
  in `operator/internal/builders/run_sandbox.go` (or a new `serving_egress.go`),
  generalizing `buildEgressPolicy` (`run_sandbox.go:73-123`) to take tenant CIDRs.
  Pod selector = the SmolAgent's workload labels.
- New feature reconciler (or extend the sandbox/knative reconciler) to create/own
  the policy when `spec.egress` is set, with controller-owner refs. This closes the
  gap noted in [custom-agent-images.md](../design/custom-agent-images.md) §Security.

### 5.3 Harness: `HarnessKind=openclaw`

- Add `HarnessOpenClaw HarnessKind = "openclaw"` to `pkg/agentmodel/v1/harness.go:38-60`;
  add to `Valid()` (`:64-71`) and the HTTP-kind branch of `ValidateHarness`
  (`harness.go:316-326`) so `harness.http.url` is required.
- New `pkg/agentruntime/harness/openclaw.go`: `type OpenClawHarness struct{ ... }`
  implementing `Harness` (`iface.go:71-81`). `Run` opens a WS to
  `spec.HTTP.URL` (e.g. `ws://openclaw.<ns>:18789`), runs session-open → send →
  await-reply, folds the reply into `Response.Output`. Honor `req.Budget`
  (`budgetTimeout`, `iface.go:127-134`) and `HarnessHTTPSpec.Retry`.
- Register in `Default()` (`iface.go:90-101`).
- **Honest contract:** `TokensIn/Out` stay 0 unless OpenClaw returns usage;
  `ToolCalls` unset — consistent with the response-richness contract
  (`iface.go:46-69`; [response-richness.md](response-richness.md)).
- *Alternative drive (no WS client):* a CLI-kind that `exec`s
  `openclaw agent --message <prompt>` inside the running pod. This needs an exec
  path into the serving pod (terminal-exposure) and is messier than the WS adapter;
  list as a fallback only.

### 5.4 agentgateway route (Option B)

- Extend `cmd/agentgateway/main.go:39-91`: when a turn's target Agent resolves to
  an OpenClaw daemon, dispatch to its WS endpoint via the §5.3 client, keyed by
  session, instead of only `Queue.Publish`. Requires a session→pod resolver
  (Endpoints/Service lookup) and reuse of `sessionqueue` for durability.

### 5.5 Reference image + config rendering

- New `deploy/docker/agent-openclaw.Dockerfile`: `FROM node:24-slim`, install git +
  ca-certs + a headless-browser stack as needed, `npm i -g openclaw@<pin>`, add the
  `:8080` probe shim, `USER 65532`, `HOME=/tmp` (writable), `ENV PORT=8080`. Follow
  the multi-arch `ARG TARGETARCH` rule (`deploy/docker/agent.Dockerfile:7-10`).
  Pattern base = `deploy/docker/harness-claude-code.Dockerfile` (Node+git+non-root),
  per [custom-agent-images.md](../design/custom-agent-images.md).
- Operator renders `~/.openclaw/openclaw.json` into the config ConfigMap
  (`workload.go:117`, mounted `/etc/smol-agents`) from `SmolAgent` config, setting
  `gateway.bind`, `sandbox.mode != off`, `tools.elevated.enabled: false`, and
  `${VAR}` placeholders the broker fills (§7). The shim `cp`/links it to
  `$HOME/.openclaw/` at start (RO-root means `~/.openclaw` must be on the PVC or
  `/tmp`).

---

## 6. Data / control flow

**One-shot turn (Option A):**
```
caller → AgentRun{harness.kind=openclaw, http.url=ws://openclaw.ns:18789}
       → run controller renders harness pod (kata-fc + run egress, run_sandbox.go)
       → OpenClawHarness.Run: WS connect → session.open → send(prompt) → await reply
       → fold reply → AgentRun.Status (runonce.go:84 → agentrun_controller.go:404)
```

**Channel-driven (no platform drive):**
```
Slack/Telegram → OpenClaw Gateway pod (egress allow-list permits the channel API)
              → bundled runtime turn → reply on channel; session persisted to PVC
```

**Durable multi-session (Option B):**
```
caller → agentgateway POST /v1/sessions/{ns}/{name}/turns
       → resolve session→OpenClaw pod → WS drive (or NATS worker)
       → result fetched via ?wait / GET .../turns/{id}
```

Secrets (provider/channel tokens) flow **broker UDS → env `${VAR}` → openclaw.json**
substitution; raw secrets never touch the CR (§7).

---

## 7. Security model

How this composes with the existing stack, and the new surface it opens.

| Control | How it applies to OpenClaw | Residual risk |
|---|---|---|
| **kata-fc microVM** (`workload.go:42-45`; webhook fail-closed `smolagent_webhook.go:35-41`) | Daemon runs in its own kernel; this is the **primary** sandbox (replacing OpenClaw's Docker sandbox). | kata under multi-minute daemons not broadly live-proven — smoke-test (per [custom-agent-images.md](../design/custom-agent-images.md)). |
| **Restricted PSA** (`workload.go:82-107`) | uid 65532, RO-root, drop ALL — OpenClaw cannot run its nested-Docker sandbox or write outside `/tmp`/PVC. | OpenClaw features assuming root/Docker are unavailable; tenant must accept (D1). |
| **Egress allow-list** (§5.2, new) | The real network cage: only the model provider + enabled channel/browse endpoints. Metadata 169.254/16 always blocked. | FQDN allow-lists are advisory without DNS-aware CNI; IP churn of SaaS channel APIs needs maintenance. |
| **Secret broker** (`workload.go:91-94,122-150`) | Provider/channel tokens leased over UDS → `${VAR}` substitution in openclaw.json; CR never holds secrets. | OpenClaw logs/telemetry could echo a token — must scrub; broker leases bound by `maxLeaseTTLSeconds`. |
| **SPIFFE identity** (CSI socket, `workload.go:113`) | Daemon gets an SVID for in-mesh mTLS and gateway auth. | OpenClaw doesn't natively speak SPIFFE; mTLS is at the platform edge, not inside OpenClaw. |
| **`:18789` binding** (§4.1) | We flip `gateway.bind` off loopback so the drive surface reaches it. | **New surface:** `:18789` is OpenClaw's full control plane. It MUST be reachable only in-pod or via SPIFFE-gated mesh — never via a public Service/Ingress. Pair with private-mtls (`:8443`) + NetworkPolicy ingress restriction. |
| **Terminal/canvas** (§4.7) | Human access proxied + authn'd via [terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md). | Interactive access is a privileged path; audit + SPIFFE-gate it. |

New attack surface specific to OpenClaw, with mitigations:

- **Exposed control plane (`:18789`).** Mitigation: bind it to the pod network
  only; front the drive with the agentgateway/private-mtls; NetworkPolicy ingress
  to allow only the gateway/harness selector.
- **Disabled self-sandbox.** Because we turn off OpenClaw's Docker sandbox, a
  tool that would have been containerized now runs in the pod (under kata + caps
  dropped). Mitigation: force `tools.elevated.enabled:false`, restrict the tool
  set, and rely on kata+egress; document the trade.
- **Channel webhooks as ingress.** Some channels push inbound webhooks. Any
  inbound path is attack surface; require it to traverse the gateway with authn,
  not a raw pod port.
- **Browser/canvas SSRF.** OpenClaw browses; the egress allow-list is the control.
  Mirror the harness `ImagePolicy` SSRF stance (`images.go:63-119`) for any
  URL-fetching tool, and keep metadata blocked.

---

## 8. Phasing & effort

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **P0** | Reference `agent-openclaw.Dockerfile` (Node 24 + probe shim) + a `SmolAgent` StatefulSet manifest that *schedules* OpenClaw on `kata-fc` with a hand-written NetworkPolicy; live-smoke on cftest. | **M** | nothing (works with v0.2.0 `spec.image`) |
| **P1** | `spec.resources` override → un-throttle the daemon. | **S** | custom-agent-images follow-up |
| **P2** | `spec.egress` field + `BuildSmolAgentEgressPolicy` + reconciler (serving-path default-deny+allow). | **L** | [agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md) (share `EgressSpec`) |
| **P3** | `HarnessKind=openclaw` WS adapter (Option A) + register + validate; drive one-shot turns. | **L** | [response-richness.md](response-richness.md) (contract); confirm OpenClaw WS RPC shape |
| **P4** | Operator renders `openclaw.json` (forced sandbox mode, `tools.elevated:false`, `${VAR}` broker injection). | **M** | [secrets-broker-credential-backends.md](../design/secrets-broker-credential-backends.md) |
| **P5** | agentgateway OpenClaw route + session→pod routing (Option B) for durable multi-session at scale. | **XL** | [agentsession-scaling-impl.md](agentsession-scaling-impl.md), [durable-session-architecture.md](../design/durable-session-architecture.md) |
| **P6** | Interactive terminal/canvas access to `:18789`. | **L** | [terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md) |

Aggregate: **XL.** P0+P1 give a working hardened daemon (~2–3 days). P2–P4 make it
"full support over HTTP." P5–P6 are the scale/interactive long tail.

---

## 9. Test plan

**Unit**
- `OpenClawHarness.Run` against a fake WS server (inject like `HTTPClient` in
  `http.go:16-18`): session-open → send → reply fold; budget timeout cancels;
  retry on transient WS/HTTP errors (`retry.go`).
- `BuildSmolAgentEgressPolicy`: golden NetworkPolicy — DNS + tenant CIDRs +
  80/443 public with metadata blocked; mirror `run_sandbox` table tests.
- Webhook: `harness.kind=openclaw` requires `http.url` (extend
  `harness_test.go:35-39`); CRD round-trips `spec.egress`/`spec.resources`.
- `openclaw.json` renderer: forced `sandbox.mode != off`, `tools.elevated:false`,
  `${VAR}` placeholders present and broker-resolvable.

**E2E (cftest single-node k0s box — live verification exists for this platform)**
1. Build multi-arch `agent-openclaw` image; deploy `SmolAgent` (statefulset,
   kata-fc). Assert Pod schedules under `kata-fc`, passes `/readyz`, OpenClaw
   answers on `:18789` in-pod.
2. Apply `spec.egress`; assert the NetworkPolicy lands and the daemon can reach an
   allowed provider endpoint but **not** a non-allowed host and **not** 169.254.
3. P3: drive a one-shot turn via `HarnessKind=openclaw`; assert reply folds into
   `AgentRun.Status`.
4. P5: two concurrent sessions via agentgateway survive a pod delete (sessions
   reload from PVC; turns re-driven).
5. Negative: confirm `:18789` is **not** reachable from outside the allowed
   selector.

---

## 10. Risks & open decisions

**Decisions the maintainer must make**

- **D1 — Sandbox inversion.** Accept that the platform (kata-fc + egress)
  *replaces* OpenClaw's Docker self-sandbox (which can't run in our pod)? The
  alternative — privileged/DinD to run OpenClaw's sandbox — would gut the security
  posture and is not recommended. **Default proposal: yes, platform isolation is
  the sandbox; force `tools.elevated:false`.**
- **D2 — Drive surface.** Ship Option A (harness WS adapter) first, Option B
  (agentgateway route) for scale? Or skip A and go straight to B? A is smaller and
  reuses the registry; recommended first.
- **D3 — `:18789` exposure.** Confirm we may flip `gateway.bind` off loopback, and
  that ingress is locked to the gateway/harness selector + private-mtls only.
- **D4 — Multi-tenancy granularity.** OpenClaw's "one agent per person" session
  model — do we run one OpenClaw daemon per tenant, or one per principal? Affects
  pod count and routing (§4.5).

**Risks / unknowns**

- **OpenClaw WS RPC is under-documented.** The exact session-open/send/await
  envelope and method names for the *agent* (vs channel-provider patterns) need
  confirmation against the running binary before P3. The RPC reference documents
  channel patterns more than the core agent loop.
- **kata + long-running Node + headless browser** is heavy and not broadly
  live-proven on the serving path; validate memory growth and host disruption.
- **Channel webhooks** add inbound surface and per-channel egress churn; the FQDN
  allow-list may need ongoing maintenance and a DNS-aware CNI to be enforceable.
- **OpenClaw version drift.** It moves weekly; pin the npm version in the image and
  re-test the WS contract on bumps.
- **Secret echo.** OpenClaw logging could surface injected `${VAR}` tokens; verify
  log scrubbing before exposing logs.

---

## See also

- [custom-agent-images.md](../design/custom-agent-images.md) — the serving-path
  foundation this spec extends.
- [agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md) —
  share the `EgressSpec` type and FQDN/CIDR enforcement.
- [terminal-exposure-http-ssh-tmux.md](terminal-exposure-http-ssh-tmux.md) —
  interactive `:18789` / canvas access.
- [agent-session-scaling.md](../design/agent-session-scaling.md) ·
  [agentsession-scaling-impl.md](agentsession-scaling-impl.md) ·
  [durable-session-architecture.md](../design/durable-session-architecture.md) —
  multi-session durability and scale-out routing.
- [secrets-broker-credential-backends.md](../design/secrets-broker-credential-backends.md) —
  `${VAR}` broker injection for provider/channel tokens.
- [response-richness.md](response-richness.md) — the harness `Response` contract
  the OpenClaw adapter conforms to.
- [agent-pi-mono-http.md](agent-pi-mono-http.md) · [agent-hermes.md](agent-hermes.md) —
  sibling HTTP-harness agent specs.
- [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md) —
  where the daemon/serving-path gaps were first scored.
