# Platform Decisions — resolved 2026-06-03

> **Status: DECIDED (maintainer interview, 2026-06-03).** This is the canonical record
> that resolves the "Open decisions / OPEN / PROPOSED" items scattered across
> `docs/specs/*` and `docs/design/*`. **Where any other doc still says OPEN/PROPOSED and
> conflicts with a decision here, this record wins.** Each decision lists the docs it
> settles.

## Framing (the four that cascade)

### D1 — Tenancy: **multi-tenant, untrusted**
Agents are hosted for separate, mutually-distrusting tenants. **Consequence:** the
governance work is **P0/mandatory, not optional** — `agentpolicy-enforcement`,
`agentnetwork-datapath-enforcement`, `run-governance` (per-tenant quotas), **output
redaction**, and **per-namespace NATS ACLs** must all land before GA. The v0.2.0 fit
report's "not yet multi-tenant" gaps are blockers, not nice-to-haves.

### D2 — Interaction model: **batch AND interactive, both first-class**
`AgentRun` (programmatic/batch) and `AgentSession` + attach (interactive/human-steered)
are equal citizens, built on one seam. **Consequence:** do the
[turn-model/runtime split](turn-model-vs-runtime.md) fully; webterm/attach is real work,
not deferred.

### D3 — Default posture: **strict / fail-closed**
Unset knobs **deny**. Concretely:
- validating webhook `failurePolicy: Fail`;
- SmolAgent serving-pod **egress floor default-ON**;
- kata **enforced in production** — prod must **not** run `--allow-host-runtime` (cftest's runc is a dev-only exception);
- claude-code `--dangerously-skip-permissions`, codex `danger-full-access` / `--ask-for-approval never` are **opt-in only, admission-gated to a microVM runtime, and never the default** (the `HarnessCLISpec.ExtraFlags` mechanism already exists).

### D4 — "Requires a session": **explicit Agent CRD field**
Add `spec.session { required: bool, interactive: bool }` to the Agent (validated,
`kubectl explain`-able). `required` ⇒ a resident session pod; `interactive` ⇒ an attach
plane. **Settles:** [turn-model-vs-runtime.md §3/§7](turn-model-vs-runtime.md).

## Sessions, attach, identity

### D5 — Attach: **driver-mode in v1** (not observe-only)
Humans can *drive* (not just watch) live sessions, gated by an `AttachGrant`.

### D9 — Human identity: **bundled self-hosted OIDC (Dex or Keycloak)**
The platform **ships an IdP** so it's self-contained for multi-tenant self-host;
`AttachGrant` authenticates humans against it. SPIFFE remains the **machine** identity;
this adds the **human** identity rail. **Settles:** terminal-exposure R5 + turn-model §7.2.

### D6 — Cross-turn memory: **provider-session (HTTP) + AgentFS workspace (CLI); defer loop-resume**
Hermes carries gateway-side memory via a stable session id; CLI harnesses carry the
durable AgentFS *workspace*. **Loop-mode conversational resume and HITL mid-run
continuation are DEFERRED to a later milestone.** **Settles:** turn-model §2.4;
`human-in-the-loop` ships only the cheap **harness pre-run gate** now (continuation-pod /
step-wise resume is post-GA); `agent-claude-code` resumable-session is post-GA.

### webterm tech: **ttyd loopback sidecar** + tmux, proxied through `cmd/agentterminal` (M4).

## Tools & credentials

### D7 / D11 — MCP transport: **HTTP (Streamable) per-agent + stdio via a cluster allow-list**
Loop-mode tools speak MCP over Streamable-HTTP (per-agent). **stdio MCP servers are
allowed only from an operator-maintained cluster allow-list of approved images** —
tenants may not run arbitrary stdio servers. Reconciles "allow stdio" with strict
multi-tenant. **Settles:** `loop-mode-tools-and-invokers` MCP-transport decision.

### D8 — Dynamic credentials: **`DynamicCredentialPolicy` CRD, operator-granted**
Declarative, auditable; tenants *reference* a policy, cannot self-grant. Replaces the
inline-Go runbook wiring. **Settles:** `dynamic-credential-backends` + `secrets-broker-credential-backends`.

## Scale / SLO

### D10 — Target: **mid scale (~100s concurrent)**
**Consequence:** `run-governance` ships **per-tenant concurrency caps + an admission
queue with fairness/priority + run-path node autoscaling** (not just soft caps);
`agentsession-scaling-impl` ships the turn-concurrency + retention knobs; benchmark
perf-gates are set to **mid-scale SLOs** (not single-digit). Sharded-NATS / 1000s+ is
**out of near-term scope**.

## Baked defaults (direct consequences — no separate decision needed)

| Item | Decision | From |
|---|---|---|
| RedactionPolicy enforcement | **Build it** | D1 |
| NATS per-namespace ACLs | **Required** | D1 |
| `pi` harness kind | **Rename → `inflection-pi`** (+ deprecation alias); `pi-mono` is the CLI | clarity |
| Cost in `Status` | integer **milli-USD**, observability-only (never a budget axis) | response-richness |
| Seed / determinism | **best-effort** + N-sample distributions; replay is post-GA | determinism-and-replay |
| `usage.toolCalls` | **No oracle/gate ever reads it** (structurally 0 on the harness path) | bench |
| Turn-model package | **`pkg/turnmodel`** (sibling to `pkg/agentruntime`); runtime exports only `TurnExecutor` | D2 |
| Maturity bar | **product-grade for self-host** (declarative config, bundled OIDC, runbooks) | D1+D9 |
| Operator-managed Hermes gateway | **on the roadmap** (`HermesGateway` CRD), staged after URL-only (gateway = RCE blast radius) | D1 |
| Codex model routing | platform model gateway **must speak the OpenAI Responses API** (`wire_api=responses`); verified per-cluster | agent-codex |

## Roadmap impact

The [README milestones](../specs/README.md) re-prioritize under these decisions:
- **M1 (containment + governance) is now mandatory/P0** (was framed "multi-tenant prerequisite"): agentpolicy enforcement + redaction, agentnetwork datapath + serving egress floor (default-on), run-governance quotas/queue/autoscaling, the `DynamicCredentialPolicy` CRD, and NATS ACLs.
- **OIDC IdP + `AttachGrant`** join the interactive tier (M4/M5); the bundled Dex/Keycloak is a new M1-ish infra dependency.
- **Loop-resume / HITL continuation / replay** are explicitly **post-GA**.
