# Event-driven work intake — unified CloudEvents API + Knative Eventing

Epic: `knative-agents-t0d`. Generalizes the rv3.1 event-driven `AgentTeam`
(`agentteam-event-driven.md`) to **every** work-accepting API behind one
CloudEvents ingress and a routing CRD, and makes the platform a first-class
Knative Eventing citizen.

## 1. The problem

Work enters the platform through **bespoke, per-API surfaces** today:

| API | How work enters today | Event-shaped? |
|-----|----------------------|---------------|
| **AgentRun** | `kubectl apply` an AgentRun, or A2A/workflow/team spawn | no — declarative object |
| **AgentSession** | `POST /v1/sessions/{ns}/{name}/turns` on the agentgateway (→ NATS) | partly — turn = a unit of work |
| **AgentTeam** | (rv3.1) per-event coordinator run | **yes** — the model this generalizes |
| **AgentWorkflow** | `kubectl apply` a workflow → operator materializes node runs | no — declarative DAG |
| **Agent** | not directly runnable; needs an AgentRun/Session/Team | n/a (it's a definition) |

There is **no event API**: no CloudEvents endpoint, no way for an external
source (a webhook, a queue, a Knative Broker, another agent's result) to *drive*
work without speaking each API's bespoke shape. The agentgateway is already a
**Knative Service** (scale-from-zero), so the substrate for an event front door
exists — it just isn't generalized.

## 2. The model: events in → work out

Every runnable API is an **event target** (a "sink", in Knative terms). An
inbound **CloudEvent** carries the work; a routing rule binds it to a target;
the target adapter turns the event into that API's native work unit:

```
            ┌──────────────── agentgateway (Knative Service, addressable) ───────────────┐
 CloudEvent │  POST /   (binary or structured content mode)                              │
 ──────────▶│    │  match EventBindings (type/source/subject filter)                     │
  (HTTP /   │    ▼                                                                        │
   NATS /   │  target adapter ──┬─▶ Agent        → create AgentRun(input = event.data)   │
   Knative  │                   ├─▶ AgentTeam    → BuildCoordinatorRun (rv3.1)            │
   Trigger) │                   ├─▶ AgentSession → POST a turn (existing NATS path)       │
            │                   └─▶ AgentWorkflow→ materialize a workflow run             │
            └─────────────────────────────────────────────────────────────────────────────┘
```

- The event **`data`** becomes the work input (the AgentRun/turn/coordinator
  objective). CloudEvent **attributes** (`type`, `source`, `subject`, `id`) drive
  routing + idempotency.
- **Idempotency**: the created object is named from the CloudEvent `id` (e.g.
  `<team>-<id>`), so an at-least-once redelivery is an `AlreadyExists` no-op.
- **Governance is unchanged**: every spawned run still passes admission (D10
  caps), kata sandbox (D3), egress floor, per-tenant NATS ACLs — the event
  surface adds intake, not a governance bypass. A binding is namespaced; it may
  only target same-namespace objects (D1).

## 3. The Event API

### 3.1 `EventBinding` CRD (the Knative-Trigger analog)

```yaml
apiVersion: runtime.agents.smol-agents.ai/v1
kind: EventBinding
metadata: { name: incident-to-squad, namespace: tenant-a }
spec:
  filter:                       # all present attrs must match (CloudEvents subset)
    type:    com.acme.incident.opened
    source:  /acme/pagerduty    # optional
    subject: ""                 # optional
  target:
    kind: AgentTeam             # Agent | AgentTeam | AgentSession | AgentWorkflow
    name: incident-squad
  # optional: a CEL/JSONPath data transform; default = pass event.data through
  inputTemplate: ""
status:
  phase, lastEventID, lastEventTime, dispatched, failed
```

`EventBinding` is to this platform what a **Knative `Trigger`** is to a Broker: a
namespaced filter→target rule. (Hand-edit the CRD per the drift rule; validate +
envtest.)

### 3.2 CloudEvents ingress

The agentgateway gains a CloudEvents endpoint (`POST /`, both **binary** and
**structured** content modes per the CloudEvents HTTP Protocol Binding). It:
1. Parses the CloudEvent.
2. Lists `EventBinding`s in the event's target namespace (or a routing namespace),
   matches by `filter`.
3. For each match, calls the **target adapter** to create the work, idempotent on
   `id`. Returns `202 Accepted` (+ the created object refs) or `2xx` per the
   Knative delivery contract; `4xx` for a malformed event, `5xx` to trigger
   Knative retry/dead-letter.

### 3.3 Target adapters

| target.kind | adapter |
|-------------|---------|
| `Agent` | create `AgentRun{agentRef, input: event.data}` (the generic run path) |
| `AgentTeam` | `builders.BuildCoordinatorRun(team, id, data)` (rv3.1, already built) |
| `AgentSession` | post a turn (`AgentRunSpec{input}`) via the existing gateway/NATS path |
| `AgentWorkflow` | materialize a workflow run seeded with `event.data` |

All four share: event `data` → input, `id` → idempotency key, target ownerRef for
GC, same admission/sandbox governance.

## 4. Knative Eventing integration (specifically)

The platform becomes a **Knative Eventing citizen** in both directions:

**As a SINK (intake).** Because the agentgateway is an addressable Knative
Service that speaks the CloudEvents HTTP binding, a Knative **`Trigger`** can name
it as `spec.subscriber.ref` (or `.uri`), so a **`Broker`** fans cluster events to
the platform with no custom glue:

```yaml
apiVersion: eventing.knative.dev/v1
kind: Trigger
metadata: { name: incidents-to-agents, namespace: tenant-a }
spec:
  broker: default
  filter: { attributes: { type: com.acme.incident.opened } }
  subscriber:
    ref: { apiVersion: serving.knative.dev/v1, kind: Service, name: agentgateway }
```

The platform's own `EventBinding` then does the fine-grained filter→target
routing inside the gateway. (Broker/Trigger does cluster-level fan-out;
`EventBinding` does platform-level target selection — they compose.)

A complete, runnable wiring (lead Agent + AgentTeam + EventBinding + Broker +
Trigger) is in `docs/examples/07-event-driven-knative.yaml`.

**As a SOURCE (results).** A finished run/coordinator/workflow optionally emits a
result **CloudEvent** to a configured sink (a Knative `SinkBinding` resolves the
sink URI). This makes agent outputs drive *downstream* Triggers — composable
event pipelines (agent A's result → event → agent B), the Knative "functions
compose via events" story, applied to agents. (Bead `wbb`.)

**Sources in (multi-source intake).** Any Knative **Source** (ApiServerSource,
PingSource, KafkaSource, a webhook `Source`) → Broker → Trigger → agentgateway →
`EventBinding` → work. That is the "accept work from different sources" ask, for
free, by being CloudEvents/Trigger-native rather than inventing a bespoke
per-source intake.

## 5. Why this shape (vs alternatives)

- **One ingress, N targets** beats per-API event endpoints: a single CloudEvents
  contract, one place for auth/idempotency/governance, every API event-drivable.
- **Reuse Knative Eventing** rather than reimplement a broker: the platform
  already runs on Knative Serving; being Trigger/CloudEvents-native means every
  existing Knative Source is an intake for free, and results compose downstream.
- **`EventBinding` ≠ Knative `Trigger` duplication**: Trigger routes *to* the
  platform (cluster fan-out); `EventBinding` routes *within* the platform (which
  agent/team/session/workflow). Keeping platform-target selection in our own CRD
  keeps it namespaced + governed (D1/D3) without coupling to a specific Broker.

## 6. Build order (epic `t0d`)

1. **`17v`** (this doc) — analysis + model. ✓
2. **`mi4`** CloudEvents ingress on agentgateway (binary + structured) — start here;
   reuse `BuildCoordinatorRun` so it doubles as rv3.1 S3.
3. **`7d3`** `EventBinding` CRD (filter→target) + validation + envtest.
4. **`h0d`** target adapters (Agent/Team/Session/Workflow).
5. **`dc8`** Knative `Trigger`/Broker integration + sample + docs.
6. **`wbb`** emit results as CloudEvents (platform as source) + `SinkBinding`.
7. **`bjd`** live-verify end-to-end on kind + Knative Eventing (CloudEvent →
   Broker → Trigger → gateway → EventBinding → AgentRun/coordinator).

rv3.1 (the AgentTeam coordinator) is one *target* of this general API; its
slices 1–2 (team-env injection + `BuildCoordinatorRun`) are the first adapter.
