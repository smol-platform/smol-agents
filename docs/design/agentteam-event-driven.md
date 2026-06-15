# Event-driven AgentTeams (rv3.1 — the trigger model)

## The model

An `AgentTeam` is a **durable definition** (lead, members, pattern, budget,
convergence) — *not* a unit of work. Work arrives as **events**, and **each
event instantiates a fresh coordinator** (one run of the `lead` agent) that
orchestrates the members for that event, then terminates. This mirrors Knative:

| Knative                          | smol-agents AgentTeam                              |
|----------------------------------|---------------------------------------------------|
| Service (scale-from-zero)        | `AgentTeam` definition (no idle cost)             |
| Trigger / Broker delivers event  | team event ingress delivers a CloudEvent          |
| Function instance per request    | **one lead coordinator `AgentRun` per event**     |
| Instance is ephemeral, GC'd      | run is owned by the team; GC'd with it / on TTL   |

So a team has **no `spec.objective`** field; the event payload IS the objective.
A team is reusable across unlimited events; each event gets its own isolated,
budgeted, observable coordinator run.

## Per-event coordinator run

For an event `e`, the ingress creates an `AgentRun`:

- `spec.agentRef = team.spec.lead` — the coordinator agent (loop mode).
- `spec.input = e.data` — the event payload is the coordinator's objective.
- label `runtime.agents.smol-agents.ai/team = <team>` — so `BuildAgentRunPod`
  injects the team NATS context (rv3.1 slice 1) and the coordinator's
  `kind=task`/`kind=teammate`/`kind=agent` invokers activate.
- ownerRef → the `AgentTeam` (literal UID) — the run + its A2A-spawned member
  subtree GC with the team (or on completion TTL).
- name `<team>-<event-id>` — idempotent per event (a redelivered CloudEvent with
  the same id is a no-op create, AlreadyExists).

The coordinator then drives members via the already-wired invokers: `kind=agent`
(A2A delegate), `kind=task` (claim/complete shared work), `kind=teammate` (peer
messages), and the pure `RunGeneratorVerifier`/`WidthLimiter`/`ThreeWayMerge`.

## Event ingress (phased)

1. **HTTP / CloudEvents** (this is the function-style front door): a
   `POST /v1/teams/{ns}/{team}/events` endpoint (on the existing agentgateway, a
   Knative Service — scale-from-zero already) that validates the CloudEvent and
   creates the per-event coordinator run. **First ingress to build.**
2. **NATS subject** (later): a team subscribes to a subject; each message →
   a coordinator run. Reuses the platform's NATS backbone + per-tenant ACLs.
3. **Knative Trigger** (later): register the team's HTTP endpoint as a Knative
   `Trigger` subscriber so a Broker fans cluster events to teams natively.

## Build slices (rv3.1)

- ✅ **S1** — operator injects `TEAM_*` env into team-context pods (PR #15).
- **S2** — `BuildCoordinatorRun(team, eventID, data)` primitive + tests (the
  per-event run; the reusable core every ingress calls).
- **S3** — HTTP/CloudEvents ingress on agentgateway calling S2; idempotent per
  event-id; per-event run created + GC'd.
- **S4** — A2A team-env propagation: a coordinator's A2A children inherit the
  team label so they too can claim tasks / message peers.
- **S5** — lifecycle hooks (`EvaluateHooks`) + generator-verifier convergence
  (`RunGeneratorVerifier` driven by a coordinator), member re-queue.

## Governance (unchanged, non-negotiable)

Per-event runs pass the same admission queue + per-namespace concurrency caps
(D10), carry the team's budget context, and are kata-sandboxed like any run. A
team is same-namespace by default (D1); a coordinator may only delegate to its
declared members.
