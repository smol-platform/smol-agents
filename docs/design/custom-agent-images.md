# Hosting Custom / Daemon Agents (openclaw-class) on the Serving Path

> Status: how-to / design note. Grounded against v0.2.0 source (2026-06-02).
> Audience: platform operators and tenant authors who need to run a
> long-running agent process that does **not** fit the one-shot harness model.

The platform has two execution shapes. The well-documented one is the
**run datapath** (`AgentRun` / `AgentSession`): a harness container is handed a
prompt, executes a turn, folds output, and exits. The other — and the biggest
undocumented deployment path — is the **serving path**: a `SmolAgent` custom
resource that runs your image as a long-lived workload (Knative Service,
Deployment, or StatefulSet). This document covers the serving path for *daemon*
agents: openclaw-class processes, custom Node/Python tool daemons, multi-channel
bots, and anything that listens on a port and never returns.

If you only need to run a coding tool once against a repo, you want the run
datapath and a harness image, not this document. See
[harness-authoring.md](harness-authoring.md).

## When NOT to use a harness

The harness model assumes a turn has a beginning and an end: the operator renders
a Pod, the harness reads a prompt, produces a `RunResult`, and the Pod
terminates. A daemon breaks every one of those assumptions. Use a `SmolAgent`
with `spec.image` set — **not** an `AgentRun`/`AgentSession` harness — when any of
the following is true:

| Signal | Why a harness won't work |
|--------|--------------------------|
| The process **listens on a port and never returns** (HTTP/WS server, webhook receiver) | The run datapath waits for the harness to exit and fold a result; a server never exits. |
| It runs a **persistent internal loop** (poll a queue, watch a channel, hold a session in memory) | There is no per-turn invocation to hang the loop off of. |
| It is **multi-channel / multi-tenant in-process** (Slack + GitHub + cron in one process) | One harness invocation handles one turn for one caller. |
| It needs to keep **warm in-memory state across requests** (model context, tool caches, open connections) | Harness Pods are ephemeral; in-memory state dies with the Pod. |
| It ships its **own scheduler / supervisor** (openclaw-class agent runtimes) | The operator is the supervisor on the run path; a daemon supervises itself. |

The decision is binary: **if the process is supposed to stay up, it is a
`SmolAgent` serving workload with a custom `spec.image`.** The `image` field is a
first-class override on the CR (`operator/api/v1/smolagent_types.go:26-28`), and
the workload builder uses it verbatim when set
(`operator/internal/builders/workload.go:20-25`).

## Why the default image won't run your daemon

The default published agent image is **hardened distroless** — it is built
`FROM gcr.io/distroless/static:nonroot` and contains exactly one binary, the Go
`/agent` driver, plus the compiled eBPF objects
(`deploy/docker/agent.Dockerfile:12-16`). There is **no shell, no Node, no
Python, no pnpm, no package manager, no libc beyond static linkage**. That is
deliberate for the one-shot driver, but it means:

- You cannot `npm install` or `pip install` at runtime — there is no installer
  and no writable, executable site to install into.
- You cannot shell out (`sh -c`, `bash`) — there is no shell on `PATH`.
- The openclaw class of agents, which expect a Node or Python runtime and
  typically spawn child processes, **cannot run on the default image at all.**

So daemon agents **MUST** supply a custom OCI image via `spec.image`. The rest of
this document is the contract that image has to satisfy.

## Custom image contract

Your image runs under a **non-overridable restricted security context**: the pod
runs as uid/gid `65532`, `runAsNonRoot: true`, `seccompProfile:
RuntimeDefault`, every container drops `ALL` capabilities, with
`allowPrivilegeEscalation: false` and a read-only root filesystem on the agent
container (`operator/internal/builders/workload.go:82-107`). Build for that
reality:

- **Run as uid `65532`** (`USER 65532:65532`). The pod `SecurityContext` pins
  this and is not tenant-overridable; an image that assumes root will fail to
  start.
- **Treat the root filesystem as read-only.** The agent container sets
  `readOnlyRootFilesystem: true`. Write only under the mounted `/tmp` emptyDir
  (see mounts below). Point `HOME`, caches, and any scratch at `/tmp`.
- **Listen on `:8080`** for HTTP. The builder declares container port `8080`
  (named `http`) and wires the probes there
  (`operator/internal/builders/workload.go:53-71`).
- **Serve `/healthz` and `/readyz`.** The liveness probe hits `GET /healthz` and
  the readiness probe hits `GET /readyz`, both on `8080`
  (`operator/internal/builders/workload.go:57-71`). A daemon that does not answer
  these will be killed (liveness) or never receive traffic (readiness).
- **Fit the resource envelope.** The agent container is rendered with
  `requests` 100m / 128Mi and `limits` 500m / 512Mi
  (`operator/internal/builders/workload.go:72-81`). A heavier daemon needs the
  platform default raised; these limits are compiled into the builder today, so
  plan capacity accordingly.

The closest in-tree example of the *base-image* pattern (Node + git + shell on a
slim base, non-root `65532`, `HOME=/tmp`) is the Claude Code harness image
(`deploy/docker/harness-claude-code.Dockerfile`). Adapt it for a daemon like so:

```dockerfile
# syntax=docker/dockerfile:1.6
#
# openclaw-style daemon agent image for a SmolAgent serving workload.
# Carries a Node 22 runtime + git + a shell, runs non-root, serves :8080.

FROM node:22-slim
ARG TARGETOS=linux
ARG TARGETARCH

# git + ca-certs for outbound TLS; add only what the daemon truly needs.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force
COPY . .

# Restricted PSA: pod runs as uid 65532 with a read-only root fs.
# HOME and all scratch must live under the writable /tmp emptyDir.
ENV HOME=/tmp \
    NODE_ENV=production \
    PORT=8080
USER 65532:65532

EXPOSE 8080
# The daemon must answer GET /healthz and GET /readyz on :8080.
ENTRYPOINT ["node", "server.js"]
```

Notes that bite people:

- Do everything that writes (install, build, cache warm) at **build time**, as
  root, in the builder layers. At runtime you are uid `65532` on a read-only
  root.
- Multi-arch matters: build an `amd64`+`arm64` manifest list. Use bare
  `ARG TARGETARCH` (no default) so BuildKit derives the arch per platform — same
  pattern as every in-tree Dockerfile (`deploy/docker/agent.Dockerfile:7-10`).
- `EXPOSE 8080` is documentation; the contract that matters is that the process
  actually binds `0.0.0.0:8080` and answers the two probe paths.

## Required mounts, sidecars, and which Features apply

The serving-path Pod template is shared by all three workload kinds —
`BuildAgentPodSpec` feeds `BuildDeployment`, `BuildStatefulSet`, and
`BuildKnativeService` (`operator/internal/builders/workload.go:41-111`,
`178-295`). Your container always gets these mounts
(`operator/internal/builders/workload.go:113-120`):

| Mount | Path | Source | Notes |
|-------|------|--------|-------|
| SPIRE workload-API socket | `/run/spire/agent-sockets` | `csi.spiffe.io` CSI, **read-only** | SVID delivery; the identity feature. |
| Secret-broker UDS | `/run/secret-broker` | `emptyDir` shared with the sidecar | Where the broker socket appears. |
| Config | `/etc/smol-agents` | ConfigMap `<name>-config`, read-only | Rendered agent config. |
| Scratch | `/tmp` | `emptyDir` | Your only writable path. |

A **secret-proxy sidecar** is injected when `features.secrets.enabled` is true
(default true): it shares the SPIRE socket and the `/run/secret-broker` emptyDir
and brokers credentials over a UDS, so your daemon never sees raw secret material
(`operator/internal/builders/workload.go:91-94`, `122-150`). Backends and the
credential model are covered in
[secrets-broker-credential-backends.md](secrets-broker-credential-backends.md).

Which `spec.features` apply to a custom daemon (all default-on; CRD defaults at
`operator/config/crd/smolagents.smol-agents.ai_smolagents.yaml:79-145`):

- **identity** — SPIFFE SVID via the read-only CSI socket. Applies. See
  [runtime-and-identity.md](../features/runtime-and-identity.md).
- **secrets** — broker sidecar + UDS. Applies (this is how a daemon authenticates
  outbound without holding secrets).
- **sandbox** — `runtimeClass: kata-fc` propagated to the Pod. Applies (see
  Security model).
- **transport** — private mTLS on `:8443` (port `private-mtls` is declared at
  `operator/internal/builders/workload.go:55`). Applies if your daemon speaks the
  in-mesh protocol.
- **knative** — autoscaling envelope when `deploymentKind: knative`. Applies, but
  see State & concurrency before trusting scale-to-zero for a stateful daemon.
- **observability** — OTel emission. Applies.

The **restricted PSA security context is enforced and not overridable** by any
feature flag (`operator/internal/builders/workload.go:82-107`). There is no knob
to run as root or keep ALL caps on the serving path.

## Security model

**Sandboxing (kata-fc + R-SBX-1).** The Pod gets `runtimeClassName` from
`features.sandbox.runtimeClass`, defaulting to `kata-fc`
(`operator/internal/builders/workload.go:42-45`; CRD default at
`...smolagents.yaml:120`). The validating webhook is **fail-closed**: a
`runtimeClass` of `runc` — or any name `ParseKind` cannot resolve, which falls
back to `runc` (`pkg/sandbox/sandbox.go:61-66`) — is **rejected** unless
`features.sandbox.allowHostEscape: true`
(`operator/internal/webhooks/smolagent_webhook.go:35-41`). So by default your
daemon runs inside a Firecracker microVM with its own kernel, and you cannot
silently drop to a shared-kernel runtime.

> **Validate kata on the serving path before you trust it.** `kata-fc` is
> code-wired for serving workloads, but it has been live-verified mainly on
> short-lived *runs*, not on multi-minute daemons. A long-running microVM has
> different failure modes (memory growth, host disruption, snapshotter quirks).
> Smoke-test your specific daemon under `kata-fc` on a representative node before
> relying on the isolation in production.

**Egress is default-deny — and the SmolAgent controller does not render the
NetworkPolicy for you.** This is the sharpest edge for daemons, which usually
talk to many integration endpoints (a model provider, GitHub, Slack, an internal
API). Two things are true at once:

1. The intended posture is a **namespace default-deny egress NetworkPolicy**, and
   a tenant-authored allow NetworkPolicy composes with it by **union** (Kubernetes
   NetworkPolicies are additive — the effective allow-list is the union of all
   policies selecting the Pod).
2. The platform's default-deny egress builders today target the **run datapath
   only** — `BuildAgentRunEgressPolicy` and `BuildAgentSessionEgressPolicy`
   (`operator/internal/builders/run_sandbox.go:60-68`). The `SmolAgent`
   feature reconcilers emit **no** NetworkPolicy for the serving Pod. So **you (or
   the platform operator) must author both the namespace default-deny and the
   per-daemon allow-list** for serving workloads.

There is **no per-workload egress configuration on the serving path**: the
`agentrun`/`agentsession` datapaths carry zero `AgentNetwork` references, and the
`SmolAgent` CR has no egress allow-list field. Until per-workload egress config
exists, a daemon with many endpoints needs a **layered tenant-authored
NetworkPolicy** (default-deny + an explicit allow per endpoint), unioned onto the
namespace policy. The interaction between `AgentNetwork`, `AgentPolicy`, and
hand-written NetworkPolicies is detailed in
[agentnetwork-agentpolicy-interaction.md](agentnetwork-agentpolicy-interaction.md);
the broader egress design is in [agentnet.md](../features/agentnet.md).

**The eBPF cage is NOT active on this path.** The `ebpf` feature governs a
per-Pod program *list* in the CR, but the runtime eBPF enforcement (the
syscall/network "cage") is not engaged for serving workloads — it has only ever
run in the e2e probe. Do not model your daemon's egress or syscall posture as if
eBPF were enforcing it; NetworkPolicy union is your real control surface today.

## State & concurrency

**AgentFS persists files only — not in-memory sessions.** Durable AgentFS
snapshots a *filesystem*; it does not checkpoint a live process's heap, open
sockets, or in-memory conversation state. A daemon that keeps session state in
memory loses it on restart, eviction, or scale-down. There is **no
sticky-session affinity** built into the serving path, so you cannot assume a
given client keeps hitting the same replica.

Practical guidance:

- **Single-tenant / single-instance daemons:** set `deploymentKind: statefulset`
  with `replicas: 1`. The StatefulSet builder attaches a 1Gi `state` PVC and
  gives the Pod a stable identity (`operator/internal/builders/workload.go:203-238`).
  This is the simplest correct shape for an openclaw-style daemon that owns its
  own state on disk.
- **Multi-replica daemons:** you must **externalize state** (Redis, Postgres,
  object storage) — do not rely on local memory or per-replica disk for anything
  shared. For turn-based fan-out across replicas, route work through the NATS
  gateway rather than load-balancing a stateful in-memory daemon. See
  [agent-session-scaling.md](agent-session-scaling.md) and
  [durable-session-architecture.md](durable-session-architecture.md).
- **Knative + scale-to-zero:** fine for a stateless request/response daemon
  (`features.knative.scaleToZero` defaults true,
  `...smolagents.yaml:136`). For a daemon holding warm state or a persistent
  internal loop, prefer `deploymentKind: deployment`/`statefulset`, or set
  `features.knative.minScale: 1` so the process stays warm.

## Worked examples

### Node.js openclaw-style daemon (single instance, durable disk)

```yaml
apiVersion: agents.smol-agents.ai/v1
kind: SmolAgent
metadata:
  name: openclaw
  namespace: tenant-acme
spec:
  trustDomain: smol-agents.ai
  deploymentKind: statefulset   # owns its own on-disk state
  replicas: 1                   # no sticky affinity → keep it to one
  image: ghcr.io/acme/openclaw-agent:1.4.0   # custom Node image (see Dockerfile)
  features:
    sandbox:
      runtimeClass: kata-fc     # default; do NOT drop to runc
    secrets:
      enabled: true             # broker sidecar brokers provider creds over UDS
    knative:
      enabled: false            # long-lived daemon, not request/response
    identity:
      mode: strict
```

### Python tool-daemon (request/response, kept warm)

```yaml
apiVersion: agents.smol-agents.ai/v1
kind: SmolAgent
metadata:
  name: pytools
  namespace: tenant-acme
spec:
  trustDomain: smol-agents.ai
  deploymentKind: knative
  image: ghcr.io/acme/pytools-daemon:0.9.2   # Python base, uid 65532, serves :8080
  features:
    sandbox:
      runtimeClass: kata-fc
    knative:
      enabled: true
      scaleToZero: false        # keep one replica warm for tool caches
      minScale: 1
      maxScale: 10              # stateless tools only — externalize anything shared
```

Pair **either** example with a tenant-authored NetworkPolicy (namespace
default-deny + an explicit egress allow per endpoint), since the serving path
emits none for you.

### Local validation

Before AWS, validate on a local kind cluster with kata wired in:

1. Build and load your image: `docker build` (multi-arch) → `kind load
   docker-image ghcr.io/acme/openclaw-agent:1.4.0`.
2. Install kata on the cluster (`kata-deploy`) and create the `kata-fc`
   `RuntimeClass`, then confirm the node advertises it. (The OrbStack/macOS host
   cannot back a microVM directly; expect to validate kata on a Linux node — see
   the operator runbooks.)
3. Apply the singleton `SmolAgentPlatform`, then your `SmolAgent` CR.
4. Confirm the Pod schedules under `kata-fc`, passes `/readyz`, and that your
   hand-written NetworkPolicy actually permits the endpoints the daemon needs
   (default-deny will silently break egress otherwise).

## Effort & caveats

- A single-tenant demo (custom image + StatefulSet CR + a hand-written
  NetworkPolicy + local kata validation) is roughly **~1 day** of work, dominated
  by getting the image's runtime assumptions to match the restricted PSA and by
  authoring egress allow-rules.
- **Validate kata on the serving path before trusting it.** Multi-minute daemons
  under `kata-fc` are not yet broadly live-proven; smoke-test your daemon under it
  first.
- **Egress is on you.** The serving path renders no NetworkPolicy; the eBPF cage
  is inactive; there is no per-workload egress config. Layer a default-deny +
  allow NetworkPolicy by union.
- **State is on you.** AgentFS persists files, not memory; there is no sticky
  affinity. Single-instance via StatefulSet, or externalize state for
  multi-replica.

## See also

- [agent-platform.md](agent-platform.md) — node provisioning and how kata-capable
  nodes are sourced for serving workloads.
- [operator.md](../features/operator.md) — the `SmolAgent` / `SmolAgentPlatform`
  CRDs, feature flags, and reconcile spine.
- `SmolAgent` CRD `spec.image` —
  `operator/config/crd/smolagents.smol-agents.ai_smolagents.yaml:68-69`
  (Go type at `operator/api/v1/smolagent_types.go:26-28`).
- [harness-authoring.md](harness-authoring.md) — the *other* path (one-shot
  harness images for `AgentRun`/`AgentSession`).
- [agentnetwork-agentpolicy-interaction.md](agentnetwork-agentpolicy-interaction.md)
  — how egress policies compose.
- [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md)
  — where the daemon/serving-path gaps were first scored.
