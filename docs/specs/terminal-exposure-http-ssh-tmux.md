# Spec: Terminal Exposure — Web (HTTP) + SSH/tmux for Interactive Agents

> **✅ Decisions resolved 2026-06-03 — see [decisions.md](../design/decisions.md).** D5: **driver-mode in v1** (not observe-only); D9: human identity = a **bundled self-hosted OIDC (Dex/Keycloak)** — the prerequisite is now decided; `ttyd` loopback sidecar + `AttachGrant`. Only `spec.session.interactive` agents get an attach plane (D4). Where this doc still says OPEN/PROPOSED and conflicts, the decision log wins.

> **Status:** RESEARCH + DESIGN. Not built. Grounded against v0.2.0 source (2026-06-03).
> **Category:** research / design note — proposals below are clearly marked **PROPOSED**.
> **Audience:** platform maintainers deciding how to expose a *live* agent terminal
> (web shell or SSH/tmux) for interactive coding tools (claude-code/codex/pi-mono
> interactive, openclaw canvas, debugging) without breaking the kata-fc + egress +
> SPIFFE/broker security model.
>
> **Builds on (read first, not duplicated here):**
> [custom-agent-images.md](../design/custom-agent-images.md) (the serving path,
> custom-image contract, restricted PSA) and
> [runtime-and-identity.md](../features/runtime-and-identity.md) (SPIFFE/SPIRE,
> two-rail mTLS, broker, sandbox).
>
> **Underpins (future):** agent-pi-mono-http, agent-openclaw-http,
> agent-claude-code (interactive mode). Those specs assume the transport this
> document designs.

---

## 1. Summary

Interactive coding agents (claude-code in REPL mode, codex interactive, Mario
Zechner's pi-mono CLI, the openclaw canvas) and live debugging all need a human
to **see and drive a PTY inside a running agent pod** — not the one-shot
`AgentRun`/`AgentSession` "prompt in, `RunResult` out" datapath the platform
ships today. This spec researches the field (ttyd / gotty / wetty / xterm.js,
sshpiper, tmux session sharing, asciinema recording, CloudTTY, Knative websocket
behaviour) and proposes a **terminal-exposure feature** for the **serving path**
(`SmolAgent` workloads): a **ttyd-class web-terminal sidecar** fronted by the
existing gateway/Knative for browser access, plus a **tmux-backed, optionally
sshpiper-fronted SSH path** for persistent, shareable, attachable sessions. The
outcome is a single, auditable, fail-closed way to attach a terminal to an agent
that *composes* with the kata-fc microVM boundary, the restricted PSA (uid
65532, read-only rootfs), the default-deny egress cage, and SPIFFE/broker
AUTHN+AUTHZ — so "give me a shell into the agent" never becomes "remote code
execution as the platform."

The recommended primary approach is **web terminal first** (ttyd sidecar +
xterm.js, auth delegated to a gateway that does SPIFFE/OIDC, tmux server inside
the agent container for persistence/sharing, asciinema recording to AgentFS),
with **SSH/tmux as a phase-2 power-user path** behind an SSH gateway. This is a
greenfield feature: **none of it exists in the tree today.**

---

## 2. Current state

### What exists (serving path we attach to)

| Capability | Where | Note |
|---|---|---|
| Long-lived agent pod template | `operator/internal/builders/workload.go:41-111` (`BuildAgentPodSpec`) | Shared by Deployment / StatefulSet / Knative. |
| HTTP port `8080` (`http`) + private mTLS `8443` (`private-mtls`) | `workload.go:53-56` | The only declared ports. No terminal port. |
| Restricted PSA, **non-overridable** | `workload.go:82-110` | uid/gid `65532`, `runAsNonRoot`, `readOnlyRootFilesystem: true`, drop `ALL`, `seccompProfile: RuntimeDefault`. |
| kata-fc RuntimeClass default | `workload.go:42-45,97`; webhook fail-closed `operator/internal/webhooks/smolagent_webhook.go:35-41` | runc rejected unless `allowHostEscape`. |
| Secret-proxy sidecar (UDS broker) | `workload.go:91-94,122-150` | Injected when `features.secrets.enabled`. SPIRE socket + `/run/secret-broker` emptyDir shared. |
| Writable scratch only at `/tmp` (emptyDir) | `workload.go:118,172` | Everything else is read-only. tmux socket / asciinema casts must live here. |
| Knative Service render | `workload.go:250-295` (`BuildKnativeService`) | Emits min/max-scale annotations only; **no `timeoutSeconds`, no websocket-friendly config.** |
| StatefulSet 1Gi `state` PVC | `workload.go:223-235` | Stable single-replica identity — the natural home for a persistent SSH/tmux daemon. |
| Stateless HTTP gateway (NATS publisher) | `cmd/agentgateway/main.go:39-117`; deploy `deploy/agentgateway/knative-service.yaml` | **Turn API only** (`POST /v1/sessions/.../turns`). No `/exec`, no websocket upgrade, no PTY. |
| Run-datapath egress cage | `operator/internal/builders/run_sandbox.go:55-123` | DNS + RFC1918 + public 80/443; metadata blocked. **Serving path emits no NetworkPolicy** (see custom-agent-images.md §Security). |

### What is stubbed / missing (the whole feature)

- **No terminal transport of any kind.** Tree-wide grep for `ttyd|gotty|wetty|tmux|xterm|web.terminal` finds only an unrelated bootstrap `sshd` dial helper (`internal/agentctl/deploy/cloud/tailscale.go:19`, `bootstrap.go:45`) and the pi-mono gap notes in
  [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md). **Nothing exposes a PTY.**
- **No `corev1.Service` for serving pods.** Service builders exist only in the memory subsystem (`operator/internal/controllers/memory/builders.go:269,411`). SmolAgent serving relies on the Knative route or a Deployment with no Service builder — there is no stable endpoint a web terminal could be routed through yet.
- **`kubectl exec` is the only way in today**, which requires kube-RBAC `pods/exec`, bypasses the broker/SPIFFE attach story entirely, and gives an unaudited shell. Not a product surface.
- **`AgentSession` is skeletal** — `AgentRef` + `IdleTimeoutSeconds` only (`pkg/agentmodel/v1/types.go:331-338`). No interactive/terminal concept. `AgentSessionStatus.Runs` is never written (dead; verified facts §8).
- **Gateway is turn-only and stateless** (`cmd/agentgateway/main.go`). It cannot proxy a long-lived bidirectional stream; it publishes a JSON `AgentRunSpec` to NATS and returns.
- **No AUTHZ model for "who may attach"** — the broker authorizes *the agent's egress*, not *a human's inbound attach*. There is no notion of a per-session attach token, viewer-vs-driver, or attach audit.

> **DESIGN BANNER.** Everything in §4–§7 is a proposal. The platform has **zero**
> terminal exposure today; do not read any "the sidecar does X" as implemented.

---

## 3. External interface research

> Training cutoff is Jan 2026 and these tools move; confirmed live 2026-06-03.
> Versions and flags below are load-bearing for the Concrete changes.

### 3.1 Web-terminal servers

| Tool | Lang | Stack | Latest | Auth | TLS | Write-default | Maint. | Verdict |
|---|---|---|---|---|---|---|---|---|
| **ttyd** | C | libuv + libwebsockets + xterm.js (WebGL2) | **1.7.7** (2024-03-30) | `--credential user:pass` (basic); `--auth-header` (trust reverse-proxy header) | `--ssl` + `--ssl-cert/-key/-ca` | **read-only** (write needs `-W/--writable`) | active (886 commits) | **Recommended** |
| gotty | Go | xterm.js | dormant (orig. `yudai`; fork `sorenisanerd`) | basic only | yes | read-only (`-w/--permit-write` to allow) | low (fork-maintained) | avoid (see caveats) |
| wetty | Node/TS | xterm.js + ws; **SSHes to a backend** | active | via SSH backend / reverse proxy | via proxy | n/a (it *is* an SSH client) | active | only if SSH-first |
| xterm.js + custom WS | TS + your server | you write the PTY bridge | n/a | you implement | you implement | you implement | n/a | fallback / full control |

Sources: [tsl0922/ttyd](https://github.com/tsl0922/ttyd),
[ttyd manpage](https://github.com/tsl0922/ttyd/blob/main/man/ttyd.man.md),
[sorenisanerd/gotty](https://github.com/sorenisanerd/gotty),
[butlerx/wetty](https://github.com/butlerx/wetty),
[xtermjs.org](https://xtermjs.org/).

**ttyd flags that matter for us** (confirmed from the manpage):
`-p/--port`, `-i/--interface` (bind `127.0.0.1` so only the in-pod gateway can
reach it), `-c/--credential`, `--auth-header <name>` (delegate authN to the
gateway), `-O/--check-origin` (block cross-site WebSocket hijacking), `-W/--writable`,
`-m/--max-clients`, `-o/--once`, `-t` (client xterm options),
`--ssl`/`--ssl-cert`/`--ssl-key`/`--ssl-ca`, `-S`/`-C` (cwd / client cert CA).

### 3.2 Critical security caveats (load-bearing)

- **ttyd auth-bypass advisory.** NCC Group documented a credential-validation
  flaw in **ttyd ≤ 1.3.0**: a WebSocket `JSON_DATA` frame *without* an `AuthToken`
  was treated as authenticated, letting an unauthenticated client run shell
  commands as the ttyd user. Mitigation: **pin ≥ 1.7.x and never rely on ttyd's
  own basic-auth as the only control.** ([NCC Group advisory](https://www.nccgroup.com/research/technical-advisory-remote-shell-commands-execution-in-ttyd/)).
  → We delegate authN to the gateway (`--auth-header`) and treat ttyd's own auth
  as belt-and-suspenders, and we bind ttyd to loopback so it is never directly
  reachable.
- **gotty RCE / write danger.** gotty is "not reliably secure by default";
  `-w/--permit-write` plus running as root is an arbitrary-command-execution
  vector, and the original project is dormant. ([gotty repo](https://github.com/sorenisanerd/gotty),
  [tecmint](https://www.tecmint.com/gotty-share-linux-terminal-in-web-browser/)).
  → **Reject gotty.**
- **Cross-Site WebSocket Hijacking (CSWSH).** Any web-terminal WS endpoint must
  validate `Origin` (ttyd `-O`) **and** require a per-session bearer token, or a
  malicious page can ride an authenticated browser session.

### 3.3 SSH proxying

- **sshpiper** (`tg123/sshpiper`) — "the missing reverse proxy for ssh/scp":
  downstream (client) → plugin (routing + auth mapping) → upstream (real sshd).
  Plugins include `username-router`, `restful` (defer auth/routing to an HTTP
  backend), `githubapp`, `failtoban`. **The `restful` plugin is the hook**: route
  `ssh agent-<ns>-<name>@gateway` to the right pod's sshd and authorize via our
  own HTTP endpoint (which checks a SPIFFE-bound attach grant). ([tg123/sshpiper](https://github.com/tg123/sshpiper),
  [README](https://github.com/tg123/sshpiper/blob/master/README.md)).
- In-pod sshd alternative: run OpenSSH `sshd` as uid 65532 on a non-priv port
  inside the agent pod. Doable but heavier: needs host keys (mount as a Secret),
  PAM-less pubkey-only config, and a writable runtime dir under `/tmp`. sshpiper
  centralizes key management and audit; **prefer the gateway, fall back to in-pod
  sshd only for single-tenant StatefulSet daemons.**

### 3.4 tmux session sharing (persistence + attach + viewer/driver)

- Persistent, attachable session: `tmux -S <socket> new-session -d -s agent`;
  attach with `tmux -S <socket> attach -t agent`. Socket must be on the writable
  `/tmp` emptyDir.
- **Read-only attach** (`attach -r`) is a **convenience, not a security boundary**
  — a read-only client can still be promoted. Real per-user access control needs
  tmux's `server-access` command (recent tmux). ([tmux-users list](https://tmux-users.narkive.com/gF4pmU4K/read-only-sharing-of-a-read-write-session),
  [Wikimedia collaborative tmux](https://wikitech.wikimedia.org/wiki/Collaborative_tmux_sessions)).
  → Viewer-vs-driver must be enforced **at the gateway** (don't trust `-r`).
- `wemux` (`zolrath/wemux`) wraps tmux for multi-user mirror/pair/rogue modes —
  reference for the UX, not a dependency. ([zolrath/wemux](https://github.com/zolrath/wemux)).

### 3.5 Session recording / audit

- **asciinema** (`asciinema/asciinema`) records a PTY to the lightweight
  `.cast` (asciicast v2) JSON format; `asciinema rec out.cast`, replay/stream
  supported; ideal for storing to AgentFS for audit/replay. ([asciinema/asciinema](https://github.com/asciinema/asciinema),
  [docs](https://docs.asciinema.org/manual/cli/)). Fallbacks: `script -t timing`
  (ships everywhere), `ttyrec` (binary, dated).

### 3.6 Kubernetes prior art

- **CloudTTY** (`cloudtty/cloudtty`, CNCF landscape) — a Kubernetes **web
  terminal operator** that is **literally built on ttyd**: a CRD spawns a pod
  that runs ttyd and `kubectl exec`s into a target, exposed via a Service/Ingress.
  This is the closest design precedent and validates "ttyd sidecar + CRD +
  route." ([cloudtty/cloudtty](https://github.com/cloudtty/cloudtty),
  [site](https://cloudtty.github.io/cloudtty/)).
- **OpenShift Web Terminal Operator** and **Argo CD web-based terminal** — both
  proxy `kubectl exec` over a websocket behind their own authN; Argo CD's docs
  flag the exec terminal as a sensitive, opt-in, RBAC-gated feature. ([Argo CD web terminal](https://argo-cd.readthedocs.io/en/stable/operator-manual/web_based_terminal/)).
- **sshwifty** — browser SSH/telnet client; **requires HTTPS** on k8s. Reference
  only. ([sshwifty on k8s](https://oopflow.medium.com/how-to-setup-sshwifty-on-kubernetes-56290d087d0b)).

### 3.7 Knative websocket behaviour (load-bearing constraint)

- Knative `TimeoutSeconds` is **time-to-first-byte**, so it does **not** kill an
  idle established WebSocket — but there are documented gaps: the **queue-proxy
  ~5-minute** request lifetime bites long requests, websocket-through-**activator**
  is under-tested, and there is **no idle timeout** knob. Long-lived terminal WS
  through Knative is *possible* but fragile under scale-from-zero/activator paths.
  ([knative/serving #15352](https://github.com/knative/serving/issues/15352),
  [#15830 activator WS](https://github.com/knative/serving/issues/15830),
  [#10852 idle timeout](https://github.com/knative/serving/issues/10852),
  [#13784 maxConcurrency + WS](https://github.com/knative/serving/discussions/13784)).
  → **Decision: terminal traffic should NOT go through the autoscaling Knative
  data path of the agent pod.** Pin the terminal-bearing workload to
  `deploymentKind: deployment`/`statefulset` (warm, no scale-to-zero), and route
  the websocket through a **purpose-built gateway path** (not the NATS turn
  gateway, not the Knative activator). The gateway itself may be Knative with
  `min-scale: 1`, but the *agent* terminal pod must stay warm.

---

## 4. Design

### 4.1 Shape of the feature

Two transports, one auth/audit spine, attached to the **serving path only**
(interactive agents are daemons by definition — see custom-agent-images.md
"When NOT to use a harness"):

```
                          ┌──────────────────────── attach plane (humans) ───────────────────────┐
  browser ── HTTPS/WSS ─► │  agent-terminal-gateway (new)                                          │
                          │   • OIDC/SPIFFE authN  • attach-grant authZ  • viewer/driver  • audit  │
  ssh client ── SSH ────► │  sshpiper restful plugin ─► same authZ endpoint                        │
                          └───────────────┬───────────────────────────────────┬───────────────────┘
                                          │ WSS (token, Origin-checked)        │ SSH (routed)
                          ┌───────────────▼────────────────┐    ┌─────────────▼────────────────┐
                          │  SmolAgent pod (kata-fc microVM)│    │ (same pod)                    │
                          │  ┌──────────┐  ┌──────────────┐ │    │  ┌──────────┐                 │
                          │  │ ttyd     │  │ tmux server  │ │    │  │ sshd*    │  *opt, StatefulSet│
                          │  │ :7681 lo │─►│ /tmp/tmux.sock│◄┼────┼──┤ uid65532 │                 │
                          │  │ -O -W    │  │ session "main"│ │    │  └──────────┘                 │
                          │  └──────────┘  └──────┬───────┘ │    └───────────────────────────────┘
                          │   asciinema rec ──────┘ → /tmp → AgentFS (audit cast)                │
                          │  agent process (claude-code / pi-mono / openclaw) runs IN tmux        │
                          │  secret-proxy sidecar (UDS broker)   SPIRE CSI socket (RO)            │
                          └──────────────────────────────────────────────────────────────────────┘
                                   egress: default-deny NetworkPolicy (must be authored)
```

Design invariants:

1. **The agent process runs *inside* the shared tmux session**, not as the
   container PID 1. ttyd and (optional) sshd both `attach` to the same
   `/tmp/tmux.sock`. This gives persistence (process survives client
   disconnect), multi-viewer sharing, and one recording surface.
2. **ttyd binds loopback (`-i 127.0.0.1`)** and is never directly routable; the
   only way in is the gateway, which terminates TLS, authenticates, authorizes
   the attach grant, sets the `--auth-header`, and reverse-proxies the WS.
3. **Viewer vs driver is enforced at the gateway**, not by tmux `-r` or ttyd
   `-W` alone (both are advisory). A "viewer" grant proxies to a read-only ttyd
   instance / read-only tmux pipe; a "driver" grant to the writable one.
4. **The terminal pod stays warm** (no Knative scale-to-zero on the agent);
   only the gateway autoscales.
5. **Recording is mandatory and tamper-resistant**: asciinema writes to `/tmp`,
   a checkpoint sidecar streams casts to AgentFS (object store), and the agent
   uid cannot delete already-shipped casts.

### 4.2 Why a sidecar (not baking ttyd into every image)

The web terminal is a **platform-injected sidecar**, mirroring the secret-proxy
pattern (`workload.go:91-94`). Tenants keep shipping a normal interactive image
(Node/Python base, uid 65532, runs claude-code/pi-mono); the operator injects
ttyd + the tmux bootstrap. Benefits: one audited ttyd version (dodges the ≤1.3.0
class of bugs), uniform flags (`-O`, `-i 127.0.0.1`, `--auth-header`), and no
per-tenant footgun. The tmux server itself runs in the **agent container** (it
must wrap the agent process and share `/tmp`), bootstrapped by a thin entrypoint
shim the operator supplies via the config mount.

### 4.3 Component inventory (all PROPOSED)

| Component | Kind | Lives | Role |
|---|---|---|---|
| `terminal` ttyd sidecar | injected container | agent pod | loopback web-terminal → tmux socket |
| tmux bootstrap | entrypoint shim + config | agent container | `new-session` wrapping the agent cmd; persistence |
| asciinema/checkpoint | injected sidecar | agent pod | record `/tmp` casts → AgentFS |
| `cmd/agentterminal` gateway | new Go binary | gateway Deployment/Knative(min-scale 1) | TLS, authN, attach-grant authZ, viewer/driver, WS reverse-proxy, audit log |
| sshpiper (restful plugin) | upstream component (phase 2) | gateway | SSH front door → pod sshd; authZ via gateway endpoint |
| in-pod sshd | injected container (phase 2, StatefulSet) | agent pod | direct SSH attach for power users |
| `AttachGrant` / attach token | new CRD or signed token | control plane | who may attach, role (viewer/driver), TTL, audience |

---

## 5. Concrete changes

> All PROPOSED. file:line targets are insertion points against v0.2.0.

### 5.1 CRD: `SmolAgent.spec.features.terminal`

Add a `Terminal` feature block (pattern matches existing `features.*` in
`operator/api/v1/smolagent_types.go`; CRD defaults alongside
`operator/config/crd/smolagents.smol-agents.ai_smolagents.yaml:79-145`):

```go
// TerminalFeature exposes a live PTY for interactive agents. Default OFF —
// terminal exposure is a deliberate, audited opt-in.
type TerminalFeature struct {
    // +kubebuilder:default=false
    Enabled bool `json:"enabled,omitempty"`

    // Web enables the ttyd web-terminal sidecar (loopback) reverse-proxied by
    // the agent-terminal gateway. +kubebuilder:default=true
    Web bool `json:"web,omitempty"`

    // SSH enables an in-pod sshd / sshpiper upstream (StatefulSet-only).
    // +kubebuilder:default=false
    SSH bool `json:"ssh,omitempty"`

    // Multiplex wraps the agent process in a shared tmux session so it persists
    // across client disconnects and supports multi-viewer attach.
    // +kubebuilder:default=true
    Multiplex bool `json:"multiplex,omitempty"`

    // Record streams an asciicast of the session to AgentFS for audit.
    // +kubebuilder:default=true
    Record bool `json:"record,omitempty"`

    // ReadOnlyDefault makes attaches viewers unless the grant says driver.
    // +kubebuilder:default=true
    ReadOnlyDefault bool `json:"readOnlyDefault,omitempty"`

    // IdleTimeoutSeconds tears the terminal (not the agent) down after no
    // attach for this long. 0 = never. +kubebuilder:default=1800
    IdleTimeoutSeconds int32 `json:"idleTimeoutSeconds,omitempty"`
}
```

**Webhook guard** (`operator/internal/webhooks/smolagent_webhook.go`, near the
existing R-SBX-1 check at :35-41): if `terminal.enabled && terminal.ssh` then
require `deploymentKind` ∈ {deployment, statefulset} (reject `knative` — §3.7);
if `terminal.enabled`, warn/deny `features.knative.scaleToZero: true`.

### 5.2 Builders

New `operator/internal/builders/terminal.go`:

- `terminalSidecar(cr) corev1.Container` — ttyd container, restricted PSA
  identical to `secretProxyContainer()` (`workload.go:122-150`): drop ALL,
  RO-rootfs, non-root, no priv-esc. Args:
  `ttyd -i 127.0.0.1 -p 7681 -O --auth-header X-Smol-Attach -W tmux -S /tmp/tmux/agent.sock attach -t main`.
  Mounts `/tmp` (shared emptyDir, for the tmux socket).
- `recorderSidecar(cr) corev1.Container` — asciinema/checkpoint streamer →
  AgentFS (reuse the AgentFS sidecar pattern referenced in custom-agent-images.md).
- `WireTerminal(pod *corev1.PodSpec, cr)` — appends the sidecar(s) and adds a
  shared `tmux` emptyDir mount + a `7681` containerPort named `ttyd`. Called from
  `BuildAgentPodSpec` (`workload.go:91-94`) gated on `cr.Spec.Features.Terminal.Enabled`.
- `BuildAgentTerminalService(cr) *corev1.Service` — a `ClusterIP` Service
  exposing nothing public (the gateway dials pod IP); a **new** Service builder
  (none exists for serving today — §2). Pattern after
  `operator/internal/controllers/memory/builders.go:269`.
- Reuse `BuildAgentSessionEgressPolicy` (`run_sandbox.go:67`) for the terminal
  pod so egress stays default-deny; **the terminal plane is INGRESS** and not
  governed by that egress policy — add an explicit ingress allow (from the
  gateway pod selector) in the same builder.

The tmux bootstrap is delivered through the existing config ConfigMap mount
(`workload.go:117`, `/etc/smol-agents`): a `terminal-entrypoint.sh` that does
`tmux -S /tmp/tmux/agent.sock new-session -d -s main "<agent cmd>"` then waits.
The tenant image's `ENTRYPOINT` is wrapped only when `Multiplex` is on.

### 5.3 New binary: `cmd/agentterminal` (the attach gateway)

Separate from `cmd/agentgateway` (turn API) because the concerns are disjoint
(bidirectional WS proxy + SSH authZ vs JSON turn publish). Surface:

```
GET  /v1/agents/{ns}/{name}/terminal           # WSS upgrade → reverse-proxy to pod ttyd :7681
POST /v1/agents/{ns}/{name}/terminal/grants    # mint an AttachGrant (viewer|driver, TTL) → token
GET  /healthz
# SSH authZ callback (sshpiper restful plugin) — phase 2:
POST /v1/ssh/authorize                          # {user, pubkey} → {upstream pod, allow}
```

Responsibilities:

1. **AuthN** — validate caller identity. Primary: OIDC bearer from the platform
   IdP. In-mesh callers: SPIFFE mTLS via `pkg/transport` `PublicMTLS`
   (`runtime-and-identity.md` §2). Reject otherwise.
2. **AuthZ** — resolve an `AttachGrant` (CRD or signed token, §5.4): does this
   subject have viewer/driver on this agent, unexpired, audience-bound?
3. **Reverse-proxy** the WebSocket to the resolved pod's ttyd on `127.0.0.1:7681`
   (pod-IP dial inside the cluster), injecting the `X-Smol-Attach` header that
   ttyd trusts (`--auth-header`). Enforce `Origin`. Driver grants → writable ttyd;
   viewer grants → a read-only ttyd instance (second sidecar, no `-W`) or a tmux
   read-only pipe.
4. **Audit** — emit a structured event per attach/detach (subject, agent, role,
   src IP, grant id) via `pkg/observability`; correlate with the asciinema cast id.
5. Deploy as `deploy/agentterminal/` Deployment (or Knative `min-scale: 1`).
   **Not** through the agent's own Knative data path (§3.7).

### 5.4 AttachGrant (authZ object)

Two options (open decision §10): **(A) `AttachGrant` CRD** (`agents.smol-agents.ai`,
namespaced), fields `{agentRef, subject, role: viewer|driver, expiresAt}`,
reconciled to nothing (it's pure data the gateway reads) — auditable, RBAC-gated
creation. **(B) signed bearer token** minted by the gateway (`POST .../grants`),
JWT with `aud=spiffe://…/terminal`, short TTL — no etcd writes, no controller.
Recommendation: **B for the data path** (low latency, no CRD churn) with **A as
the durable policy record** (who is *allowed* to mint driver grants). The grant
audience binds to the agent's SPIFFE ID so a token can't be replayed at another
agent.

### 5.5 Images

New Dockerfiles under `deploy/docker/`:
`terminal-sidecar.Dockerfile` (pin **ttyd ≥ 1.7.7** + tmux + asciinema on a slim
base, `USER 65532`, multi-arch per the bare-`ARG TARGETARCH` rule from
custom-agent-images.md). Interactive agent images (claude-code/pi-mono/openclaw)
remain the tenant's — see the sibling per-agent specs.

---

## 6. Data / control flow

**Web attach (driver), end to end:**

1. User authenticates to the platform IdP → OIDC token.
2. `POST /v1/agents/acme/coder/terminal/grants {role: driver}` → gateway checks
   the user is RBAC-permitted (and an `AttachGrant`/policy allows driver) → mints
   a short-TTL token bound to `spiffe://smol-agents.ai/ns/acme/…/coder/terminal`.
3. Browser opens `WSS /v1/agents/acme/coder/terminal` with the token + correct
   `Origin`.
4. Gateway validates token + Origin, resolves the pod (Service/endpoints), dials
   `podIP:7681`, sets `X-Smol-Attach`, upgrades and pipes the WebSocket.
5. ttyd (loopback) `attach`es to `/tmp/tmux/agent.sock` session `main`; the user
   drives the live agent PTY. asciinema is already recording session `main`.
6. On detach, the **agent process and tmux session persist** (next attach
   resumes). After `terminal.idleTimeoutSeconds` with no attach, the terminal
   sidecars idle-exit; the agent keeps running per its own lifecycle.
7. The cast is flushed to AgentFS; the audit event closes with byte counts.

**SSH attach (phase 2):** `ssh coder.acme@ssh-gw` → sshpiper `restful` plugin
calls `POST /v1/ssh/authorize` (pubkey + user) → gateway authZ → sshpiper routes
to the pod's sshd → sshd (uid 65532) runs `tmux attach -t main`. Same recording,
same audit.

---

## 7. Security model

Composition with the five foundations (`runtime-and-identity.md`):

| Layer | How terminal exposure composes | New surface | Mitigation |
|---|---|---|---|
| **kata-fc microVM** | Inherited unchanged — ttyd/tmux/sshd all run *inside* the same microVM as the agent (`workload.go:97`). A terminal break-out is bounded by the hypervisor, not just namespaces. | A live PTY is a richer post-exploitation surface *inside* the VM. | Boundary already assumes the agent is hostile; the PTY adds nothing the agent process couldn't already do in-VM. Validate kata on the warm serving path (custom-agent-images.md caveat). |
| **Restricted PSA (uid 65532, RO-rootfs, drop ALL)** | ttyd/sshd run under the same non-overridable context (`workload.go:82-110`). Shell writes only `/tmp`. No new caps, no priv-esc. | A human shell may try to write outside `/tmp`. | RO-rootfs blocks it; tmux socket + casts confined to `/tmp` emptyDir. |
| **Egress cage (default-deny)** | A human at the PTY has exactly the agent's egress — DNS + RFC1918 + public 80/443, metadata blocked (`run_sandbox.go:73-123`). They **cannot** reach 169.254.169.254 or open arbitrary outbound. | The serving path emits no egress policy by default (custom-agent-images.md §Security) — a terminal makes that gap *interactive*. | **Make egress NetworkPolicy mandatory when `terminal.enabled`**: have `WireTerminal` also require/emit `BuildAgentSessionEgressPolicy`. Webhook denies `terminal.enabled` without a default-deny egress posture. |
| **Broker / secretless (SPIFFE)** | The human shell runs as uid 65532 and **can call the broker UDS** and read leased secrets, exactly like the agent (verified facts: broker serves static values to uid 65532; the bash tool already inherits them). | **A human attaching gains the agent's credentials.** This is the sharpest new risk. | (1) Driver grants are high-privilege — gate hard, short TTL, full audit. (2) Consider a broker policy that denies interactive callers (distinguish PID via `SO_PEERCRED`: ttyd-spawned shell ≠ agent PID) — *open decision* §10. (3) Prefer viewer grants by default (`readOnlyDefault: true`). |
| **AUTHN/AUTHZ of the attach itself** | **New** — the platform had no inbound human-attach concept. | Anyone reaching the gateway could attach. | Gateway-enforced OIDC/SPIFFE + `AttachGrant` (§5.4); ttyd bound to loopback so it's never directly reachable; `--auth-header` trusts only the gateway. |
| **Web/WS-specific** | ttyd ≥1.7.7 pinned; `-O` Origin check; per-session token; no client-direct exposure. | CSWSH, the ttyd ≤1.3.0 auth-bypass class, clickjacking. | Pin version; gateway is the only authN; Origin + token required; serve only over TLS/WSS; `frame-ancestors 'none'` CSP at the gateway. |
| **tmux sharing** | `-r` read-only is advisory only; promotion possible. | A "viewer" could become a driver. | **Do not trust tmux `-r`.** Viewer = a separate read-only ttyd/pipe; use tmux `server-access` where available; enforce role at the gateway. |
| **Audit / recording** | asciinema cast → AgentFS, immutable once shipped; gateway attach/detach events. | A driver could try to wipe local casts. | uid 65532 cannot delete already-shipped (object-store) casts; gateway audit is out-of-band. |

**Net new attack surface:** (1) an interactive credential-bearing shell (broker
access) — the dominant risk; (2) an inbound WebSocket/SSH front door — addressed
by gateway authZ + loopback ttyd; (3) the gateway becomes a high-value target
(it can attach to any agent) — must itself run hardened, SPIFFE-identified, and
RBAC-scoped per namespace.

---

## 8. Phasing & effort

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **P0** | `features.terminal` CRD block + webhook guards (deny knative+ssh, require egress policy) + `WireTerminal` no-op wiring. No transport yet. | **S** | — |
| **P1 (primary)** | ttyd sidecar (loopback, `-O`, `--auth-header`) + tmux bootstrap (persistence) + `cmd/agentterminal` gateway with OIDC authN + signed-token authZ + WSS reverse-proxy + Origin check. Driver only. `BuildAgentTerminalService`. | **L** | P0; gateway hardening reuses `pkg/transport`. |
| **P2** | Viewer-vs-driver (read-only ttyd / tmux `server-access`); asciinema recording → AgentFS; full attach/detach audit. | **M** | P1; AgentFS sidecar (custom-agent-images.md). |
| **P3** | SSH path: sshpiper `restful` plugin → gateway authZ → in-pod sshd (StatefulSet). | **L** | P1; depends on **terminal-exposure** being settled; SSH key/host-key management. |
| **P4** | `AttachGrant` CRD as durable policy record; broker policy that distinguishes interactive callers (deny/limit secret leases to PTY-spawned PIDs). | **M** | depends on **agentpolicy-enforcement** + **dynamic-credential-backends**. |

Cross-spec dependencies (sibling specs, KEYS):

- **agentnetwork-datapath-enforcement** — for a *tighter-than-default* egress
  allow-list on the interactive pod (the static cage is the floor).
- **agentpolicy-enforcement** — to express "who may attach" / "driver requires
  approval" as policy, and to gate the broker for interactive callers (P4).
- **agentsession-scaling-impl** — interactive sessions are the warm,
  no-scale-to-zero counterpart to durable sessions; share the StatefulSet/warm
  posture.
- **human-in-the-loop** — a driver attach *is* a HITL intervention; the
  approval/notification flow should reuse that machinery.
- **artifact-egress** — asciinema casts are an egress artifact; reuse its
  AgentFS-to-object-store path and retention.
- **dynamic-credential-backends** — P4 broker policy for interactive callers.

---

## 9. Test plan

**Unit (per module, both `go test ./...` green):**

- `builders/terminal_test.go` — `WireTerminal` adds the ttyd sidecar with
  restricted PSA (assert drop-ALL, RO-rootfs, non-root, loopback `-i 127.0.0.1`,
  `-O` present, no `-c` plaintext creds), the shared `tmux` emptyDir, and the
  `7681` port; off when `terminal.enabled=false`.
- webhook test — `terminal.enabled && ssh` with `deploymentKind: knative`
  rejected; `terminal.enabled` with no egress posture rejected.
- `cmd/agentterminal` handler tests — reject missing/expired/wrong-audience
  token (401/403); reject bad `Origin`; viewer grant cannot reach the writable
  ttyd; happy-path upgrades and pipes (httptest WS).
- AttachGrant token: audience-bound to the agent SPIFFE ID; replay at another
  agent fails.

**E2E (use the cftest single-node k0s box — verified facts; it has kata-fc):**

1. Deploy a `SmolAgent` (interactive image, `deploymentKind: statefulset`,
   `features.terminal.enabled: true`) and the `agentterminal` gateway.
2. Confirm the pod schedules under kata-fc, ttyd is **not** reachable directly
   (loopback bind), and the agent process is wrapped in tmux session `main`.
3. Mint a driver token; open a WSS; type a command; assert the live PTY echoes
   and the agent (e.g. pi-mono/claude-code interactive) responds.
4. Disconnect; reattach; assert the tmux session and agent process **persisted**.
5. Assert default-deny egress holds *from the PTY*: `curl 169.254.169.254`
   fails; an unlisted public host on a non-80/443 port fails; DNS works.
6. Mint a viewer token; assert keystrokes do **not** reach the PTY.
7. Assert an asciinema cast landed in AgentFS and an attach/detach audit event
   was emitted.
8. (P3) `ssh` through sshpiper with an authorized pubkey lands in tmux `main`;
   an unauthorized pubkey is rejected by the `restful` callback.

---

## 10. Risks & open decisions

**Risks**

- **Interactive shell = the agent's credentials.** A driver attach inherits the
  broker's leased secrets (uid 65532). This is the headline risk; until the P4
  broker-policy split exists, a driver grant is effectively "act as the agent,
  including its provider keys." Treat driver grants as privileged.
- **Knative + websocket fragility** (§3.7) — do not route terminal WS through the
  agent's autoscaling Knative path or the activator. If a maintainer insists on
  Knative for the agent pod, terminals will be flaky under scale events.
- **kata-fc on warm daemons is under-proven** (custom-agent-images.md caveat) —
  an interactive pod is long-lived; smoke-test kata under it.
- **tmux `-r` is not a security boundary** — viewer isolation must be real
  (separate read-only endpoint), not `-r`.
- **Gateway is a crown jewel** — it can attach to any agent; compromise = broad
  PTY access. Must be namespace-RBAC-scoped, SPIFFE-identified, hardened.

**Open decisions for the maintainer**

1. **AttachGrant: CRD (A), signed token (B), or both (recommended).** Latency vs
   auditability. (§5.4)
2. **Should the broker deny interactive (PTY-spawned) callers by default?** Using
   `SO_PEERCRED` the broker *can* tell the agent PID from a ttyd-spawned shell —
   but that breaks legit interactive workflows that need creds. Policy knob? (§7,
   P4)
3. **ttyd sidecar vs bake-into-image.** Sidecar (recommended: one audited
   version) vs requiring tenants to ship ttyd (simpler operator, worse safety).
   (§4.2)
4. **SSH at all?** sshpiper + in-pod sshd is real surface and key-management
   work. Is the web terminal sufficient for v1, deferring SSH to P3/never? (§8)
5. **Default `terminal.web` value.** Off-by-default feature, but if `enabled`,
   should `web` default true (proposed) — confirm.
6. **Where does OIDC come from?** This spec assumes a platform IdP for human
   authN; the platform today only has SPIFFE (machine identity). Wiring a human
   IdP is a prerequisite this spec depends on but does not design.
7. **Recording retention / privacy.** Casts may contain secrets typed by a
   human; retention, encryption, and access to casts need a policy (defer to
   artifact-egress).

---

## See also

- [custom-agent-images.md](../design/custom-agent-images.md) — the serving path,
  custom-image contract, restricted PSA this feature attaches to.
- [runtime-and-identity.md](../features/runtime-and-identity.md) — SPIFFE/broker/
  sandbox foundations the security model composes with.
- [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md)
  — where interactive-agent gaps (pi-mono tmux loop, etc.) were scored.
- [agentnetwork-agentpolicy-interaction.md](../design/agentnetwork-agentpolicy-interaction.md)
  — how egress policies compose with the terminal pod.
- Sibling specs (future): agent-pi-mono-http, agent-openclaw-http,
  agent-claude-code, human-in-the-loop, artifact-egress, agentpolicy-enforcement,
  agentnetwork-datapath-enforcement, agentsession-scaling-impl,
  dynamic-credential-backends.
