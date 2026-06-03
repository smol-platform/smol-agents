# Design Document — Durable Session Architecture (AgentSession + gateway + NATS turn queue)

> Status: **P3/P4 built and live-verified on the cftest single-node k0s box.**
> This document is the first end-to-end write-up of the durable-session
> subsystem: how a turn flows from the HTTP gateway through NATS JetStream to a
> long-running session worker, how the worker checkpoints to the AgentFS
> workspace, how it recovers after a crash, what isolation it inherits, and
> where the sharp edges are. It also gives the proposed AgentSession scaling
> knobs (`docs/design/agent-session-scaling.md`) a home in the larger picture.
>
> **Scope.** The `AgentSession` CRD and its worker, the `agentgateway` Knative
> Service, and `pkg/sessionqueue`. It does **not** re-design the single-shot
> `AgentRun` datapath (see `docs/features/operator.md` and
> `docs/features/agent-model.md`); durable sessions *reuse* that datapath's pod
> builders and containment, which is called out where it matters.

---

## Overview

An **AgentRun** is one bounded execution: a pod is created, the agent runs to a
terminal state, the result is folded back, the pod goes away. That is the right
shape for "do this task," but it is the wrong shape for a multi-turn
*conversation* — every turn would cold-start a pod and (for a CLI harness)
re-clone the workspace.

An **AgentSession** is the long-lived counterpart. The operator stands up a
**single durable worker pod** (`agent serve-session`) over the agent's AgentFS
workspace. Turns arrive over a durable queue, the worker processes them one at a
time, and after each turn it **checkpoints** the conversation + cumulative usage
into the workspace — so when the pod is rescheduled (node loss, OOM, rollout)
the replacement resumes exactly where it left off.

The subsystem decomposes into three independent layers, each replaceable:

| Layer | Responsibility | Transport-agnostic? |
|---|---|---|
| **Gateway** (`cmd/agentgateway`) | HTTP front door; validate + enqueue a turn, optionally wait for its result | Stateless; talks only to the queue |
| **Queue** (`pkg/sessionqueue`) | Durable turn buffer + result channel between gateway and worker | Pluggable: NATS JetStream (prod) or in-memory (tests/dev) |
| **Worker** (`pkg/agentruntime` + `serve-session`) | Restore state, loop processing turns, checkpoint after each | Pluggable `TurnSource`/`ResultSink`: on-disk inbox (default) or queue-backed |

The design deliberately keeps the **durable-execution core** (worker + on-disk
checkpoint) independent of the **transport** (NATS). The worker's `Source`/`Sink`
default to a filesystem inbox/outbox under the workspace and only switch to NATS
when the operator is started with `--session-nats-url`
(`pkg/agentruntime/session_worker.go:36-39`,
`operator/cmd/manager/main.go:49-50`). That seam is why P3 (durable worker)
shipped and was testable before P4 (gateway/NATS) existed.

---

## Turn-flow sequence

The end-to-end synchronous path (`?wait` set), NATS transport:

```text
  client                gateway (Knative)        NATS JetStream            SessionWorker pod
    │                         │                  AGENT_SESSIONS stream            │
    │ POST .../turns?wait=30s │                         │                         │
    │  body: AgentRunSpec JSON│                         │                         │
    ├────────────────────────►│                         │                         │
    │                         │ validate as AgentRunSpec│                         │
    │                         │ Publish(turn)           │                         │
    │                         ├────────────────────────►│ agentsession.<key>.turns│
    │                         │  ◄── turnId             │                         │
    │                         │                         │   Consume(key, max=16)  │
    │                         │                         │◄────────────────────────┤  pull (poll loop, 2s)
    │                         │                         ├────────────────────────►│  []Turn
    │                         │                         │                         │ RunTurn(agent, spec)
    │                         │                         │                         │ Append → state.json
    │                         │                         │                         │ (checkpoint to AgentFS)
    │                         │                         │   PublishResult         │
    │                         │                         │◄────────────────────────┤ agentsession.<key>.result.<turnId>
    │                         │                         │                         │ Ack(turn)
    │                         │ FetchResult(turnId,wait)│                         │
    │                         ├────────────────────────►│                         │
    │                         │  ◄── result bytes       │                         │
    │  200 {turnId, result}   │                         │                         │
    │◄────────────────────────┤                         │                         │
```

Routes and timing, from `cmd/agentgateway/main.go`:

- `POST /v1/sessions/{ns}/{name}/turns` — body is an `AgentRunSpec` JSON
  (`main.go:41,54-59`). The gateway derives the session key with
  `sessionqueue.SessionKey(ns, name)` (`main.go:48`), enqueues
  (`Queue.Publish`, `main.go:60`), then:
  - `?wait` **absent or invalid** → `202 Accepted {turnId, status:"queued"}`
    (`main.go:65-68,94-98`).
  - `?wait=<dur>` → blocks on `FetchResult` up to that duration; on a hit
    returns `200 {turnId, result}`, on timeout returns `202 {turnId,
    status:"pending"}` so the client can poll (`main.go:65-75`).
- `GET /v1/sessions/{ns}/{name}/turns/{id}` — fetch a result by turn id;
  `?wait` defaults to **1s** when absent here (`main.go:81-84`); `404 {status:
  "pending"}` if nothing has landed (`main.go:85-90`).
- `GET /healthz` → `204` (`main.go:43`).

**`?wait` is capped at `MaxWait`, default 60s** — the `--max-wait` flag /
`Gateway.MaxWait` (`main.go:35,103-109,122`). A client asking for `?wait=10m`
is silently clamped to 60s. Request bodies are limited to 1 MiB
(`maxTurnBytes`, `main.go:30,49`).

On the worker side the loop is in `SessionWorker.Run`
(`pkg/agentruntime/session_worker.go:106-153`): poll the source, process every
ready turn oldest-first, checkpoint, and if nothing was ready park in
`RequiresAction` and `time.After(pollInterval)` (default **2s**,
`session_worker.go:71-76`) before polling again.

---

## Component + code map

| Component | File | What it does |
|---|---|---|
| **Gateway** | `cmd/agentgateway/main.go` | Stateless HTTP→queue bridge. `postTurn`/`getResult`/`waitFor`. Requires `--nats-url`/`AGENTSESSION_NATS_URL` (`main.go:121-129`). |
| Gateway deploy | `deploy/agentgateway/knative-service.yaml` | Knative Service in `smol-agents-system`. `min-scale:0`, `max-scale:20`, autoscaling `target:50`, `containerConcurrency:50` (lines 22-26); scale-to-zero when idle, NATS is the durable buffer across cold starts. Hardened `securityContext` (non-root, RO rootfs, drop ALL). |
| **Queue interface** | `pkg/sessionqueue/queue.go` | `Queue` iface: `Publish`/`Consume`/`PublishResult`/`FetchResult`/`Close` (lines 28-45). `SessionKey(ns,name)` = `ns + "." + name` — a dot-joined two-token NATS subject fragment (lines 47-50). `ErrNoResult` on `FetchResult` timeout (line 16). |
| NATS impl | `pkg/sessionqueue/nats.go` | JetStream-backed. **One cluster-wide stream `AGENT_SESSIONS`**, subjects `agentsession.>`, **file storage, `LimitsPolicy` retention, `MaxAge: 24h`** (`nats.go:15-17,45-52`). One **durable pull consumer per session** created lazily and reused (`pullSub`, `nats.go:76-88`); `Consume` does `Fetch(max, MaxWait 500ms)` defaulting `max=16` (`nats.go:90-111`). Results are single retained messages on a unique per-turn subject `agentsession.<key>.result.<turnID>` (`nats.go:61,113-134`). At-least-once delivery: a turn redelivers until `Ack`'d. |
| Mem impl | `pkg/sessionqueue/mem.go` | In-process FIFO queue for tests + single-binary dev; `Ack` is a no-op, results retained until fetched. |
| **Worker core** | `pkg/agentruntime/session_worker.go` | `SessionWorker`: restore → loop(poll, process, checkpoint, park) → final checkpoint on SIGTERM. Pluggable `TurnSource`/`ResultSink`; defaults to on-disk `inboxSource`/`outboxSink` under `<workspace>/.smol-session/{inbox,outbox}` (lines 82-88,221-271). |
| Queue adapters | `pkg/agentruntime/session_queue_source.go` | `QueueSource`/`QueueSink` adapt a `sessionqueue.Queue` to the worker's `TurnSource`/`ResultSink`; decode each body as `AgentRunSpec`, wire `Ack` to the queue's at-least-once ack, drop a malformed turn so it can't wedge the queue (lines 20-37). |
| Checkpoint store | `pkg/agentruntime/session.go` | `SessionState`/`SessionTurn` + `SessionStore` (atomic temp-file-then-rename `Save`, `Load` returns fresh `Pending` when absent). State path `<workspace>/.smol-session/state.json` (lines 58-98). |
| Entry point | `cmd/agent/serve_session.go` | `agent serve-session`: load the mounted Agent spec, resolve workspace (`--workspace` or `EffectiveWorkingDir`), build the broker leaser + loop LLM, construct the `SessionWorker`; switch `Source`/`Sink` to NATS when `--nats-url` **and** `--session-key` are both set (lines 76-86). |
| **Controller** | `operator/internal/controllers/agentmodel/agentsession_controller.go` | `AgentSessionReconciler`: render the worker pod via the run-pod builders and wrap it in a 1-replica Deployment. |

### The "synthetic AgentRun" technique

The session controller does **not** duplicate the run-pod assembly. It
fabricates a throwaway `AgentRun` whose only meaningful fields are a name
(`<session>-session`) and namespace + `AgentRef`, then drives the *shared*
run-pod / run-spec / broker builders with it
(`agentsession_controller.go:91-96`):

- `builders.BuildRunSpecConfigMap` → the `agent.json` + `provider.json` the
  worker reads (`:104`).
- `builders.BuildBrokerConfigSecret` + `AttachSecretBroker` → secret broker,
  only when there are secrets to serve (`:113-121,134-136`).
- `builders.BuildAgentSessionEgressPolicy` → the default-deny egress cage
  selecting the worker pods (`:124`).
- `builders.BuildAgentRunPod` + `builders.ApplyRunSandbox` → the security
  context, AgentFS mounts, run-spec volume, and the resolved RuntimeClass
  (`:132-133`).

Ownership of every object is the **AgentSession's**, set in `ensureOwned`
(`:171-183`). The pod's command is then rewritten to `agent serve-session
--dir=<runspec> --agent-ref=<ref>` (plus `--idle-timeout` when
`spec.idleTimeoutSeconds > 0`), `RestartPolicy` is forced to `Always` for the
Deployment template, and `AGENTSESSION_NATS_URL`/`AGENTSESSION_KEY` are injected
**only when the operator has a NATS URL** (`:137-150`). The 1-replica Deployment
(`sessionDeployment`, `:224-238`) is what gives node-loss/crash survival; a
comment notes Phase 4 may swap it for a Knative Service for scale-to-zero, but
**today the worker is a Deployment, not a Knative Service** (the *gateway* is the
Knative piece). Phase advances `Pending → Running` once
`Deployment.Status.AvailableReplicas > 0` (`:157-160,202-208`).

---

## Checkpoint format + growth

`SessionState` is a single JSON file at
`<workspace>/.smol-session/state.json` (`session.go:62-64`):

```jsonc
{
  "agentRef": "coder",
  "phase": "Running",
  "turns": [
    {
      "index": 0,
      "input":  { /* AgentRunSpec.Input, verbatim */ },
      "output": { /* folded RunResult output */ },
      "phase": "Succeeded",
      "usage": { "steps": 7, "tokens": 4120, "toolCalls": 3, "wallClockUsed": "12s" },
      "terminationReason": "",
      "startedAt": "2026-06-02T...","endedAt": "2026-06-02T..."
    }
    // ... one entry per processed turn, appended forever
  ],
  "cumulativeUsage": { "steps": 7, "tokens": 4120, "toolCalls": 3, "wallClockUsed": "12s" },
  "createdAt": "...", "updatedAt": "..."
}
```

`SessionState.Append` stamps the turn index, appends to `Turns`, and folds the
turn's usage into `CumulativeUsage` (`session.go:43-53`). The whole document is
re-marshalled (`MarshalIndent`) and atomically rewritten after **every** turn
(`session_worker.go:131`, `SessionStore.Save` `session.go:85-98`).

**Two honest limitations of this format:**

1. **`Turns` is unbounded — there is no compaction.** Every turn appends one
   `SessionTurn` (full input + full folded output) forever
   (`session.go:46-47`). The file is rewritten in full on each turn, so cost is
   O(turns) bytes written per turn → **O(turns²) total write volume** over a
   long conversation. A chatty session with large inputs/outputs will see the
   checkpoint grow without limit and per-turn save latency creep up. There is no
   ring buffer, summarization, or external turn log. Bounding/compacting this is
   a proposed first-class concern in `docs/design/agent-session-scaling.md`.
2. **No `Version` field — no migration path.** `SessionState` has no schema
   version (`session.go:34-41`). If the struct changes shape, an old
   `state.json` is decoded best-effort by `encoding/json` (unknown fields
   dropped, missing fields zero-valued); there is no explicit upgrade hook.
   Adding a `Version` is a prerequisite for any future on-disk format change.

Rough per-turn size: a `SessionTurn` is the JSON of its `Input` + `Output` plus
~6 small scalar fields and two RFC3339 timestamps — so the floor is a few
hundred bytes of envelope and the variable cost is dominated by the turn's input
and folded output payloads.

---

## Crash recovery

The worker is built to survive the pod being killed and rescheduled. Recovery
hinges on **two independent durable stores being restored before the worker
loop starts**:

1. **The agent's files** — restored by the **AgentFS init container**.
   `BuildAgentRunPod` → `AttachStorageFS` appends a restore init container that
   pulls the workspace from S3 (kopia or legacy-tar backend) and exits, *then* a
   native serving sidecar, *then* the main container
   (`operator/internal/builders/storage_mount.go:102-114`; see
   `docs/research/agentfs-versioning.md`). Because `state.json` lives **inside**
   that same workspace (`.smol-session/state.json`), it is snapshotted and
   restored as part of the agent's files — one consistent checkpoint of "the
   whole session" (`session.go:29-41,55-58`).
2. **The turn log + cumulative usage** — restored by `SessionStore.Load` at the
   top of `SessionWorker.Run` (`session_worker.go:107-117`). A missing file is
   *not* an error: a brand-new session "resumes" empty as `Pending`
   (`session.go:67-72`). The worker stamps `CreatedAt`/`AgentRef`, flips to
   `Running`, and registers a `defer store.Save(state)` so even an unclean exit
   writes a final checkpoint (`session_worker.go:112-120`).

**Init-vs-main ordering** matters: the AgentFS restore is a *regular* init
container (runs to completion before the workspace is usable), the AgentFS API
is a *native* sidecar (starts and stays up), and the `serve-session` main
container is `Containers[0]`. So by the time the worker calls `store.Load`, the
restored `state.json` is already on disk.

**Divergence under at-least-once delivery.** NATS JetStream is at-least-once: a
turn that is processed but whose pod dies *before `Ack`* will be **redelivered**.
The redelivered turn is processed again and **appended again** — there is no
turn-id dedup in `handleTurn`/`Append` (`session_worker.go:188-219`). Likewise a
crash *after* the checkpoint `Save` but *before* `Ack` re-runs the turn (the
LLM/tool side effects repeat) and the result is published twice. The window is
small (Save → Publish → Ack is three fast calls, `session_worker.go:210-218`),
but it exists: **session turns are effectively at-least-once, not
exactly-once.** Treat turn processing as idempotent-tolerant, or accept the
occasional duplicate turn entry after a crash. The on-disk `inboxSource` is
better here (it `Ack`s by *removing the file*, so a re-read sees it gone) but
still re-runs a turn whose removal didn't complete before the crash.

---

## Security / isolation

A session worker inherits the **full AgentRun datapath containment**, because it
is built by the same builders (see "synthetic AgentRun" above). Concretely:

| Control | Mechanism | Source |
|---|---|---|
| Sandbox runtime | `resolveSandbox` fail-closed → default `kata-fc`; refuses `runc` (host kernel) unless `--allow-host-runtime`; refuses to schedule if the hardened RuntimeClass isn't registered | `agentmodel/sandbox.go:21-43`; `agentsession_controller.go:83-89` |
| Egress cage | Default-deny-egress NetworkPolicy: DNS + in-cluster (any port) + public 80/443 only, **link-local/metadata `169.254.0.0/16` blocked** | `builders/run_sandbox.go:55-123`; applied at `agentsession_controller.go:124` |
| Workload identity / PSA | Restricted Pod Security + the run pod's security context (non-root, drop caps) carried by `BuildAgentRunPod` | `agentsession_controller.go:132`; `docs/features/runtime-and-identity.md` |
| Secrets | Broker sidecar, leased over a UDS — only attached when there are secrets to serve | `agentsession_controller.go:113-136`; `docs/features/egress-credentials.md` |

So the durable worker is **not** less contained than a single-shot run; it is
the *same* microVM-plus-egress-cage, just long-lived. (This corrects the older
"runs are unisolated runc" framing — see
`docs/research/agent-runtime-fit-analysis-v0.2.0.md`.)

> ### ⚠ Multi-tenancy warning — the NATS stream is cluster-wide
>
> There is **one** JetStream stream, `AGENT_SESSIONS`, with subjects
> `agentsession.>`, shared by every session in every namespace
> (`pkg/sessionqueue/nats.go:16,45-52`). The *only* tenant scoping is the
> subject key `agentsession.<namespace>.<name>.turns` derived from
> `SessionKey(ns,name)` (`queue.go:47-50`). Nothing in this codebase configures
> NATS accounts, users, or subject-level ACLs — the gateway and every worker
> connect to the same NATS with the same (no-)credentials
> (`nats.go:35-39`, `serve_session.go:77`). **Cross-tenant isolation of the turn
> queue therefore depends entirely on NATS ACLs the platform does not set up
> today.** On a shared multi-tenant cluster, a workload that can reach NATS could
> publish to or consume from another tenant's session subject. Hardening this
> (per-tenant NATS accounts / subject permissions, or a stream-per-namespace) is
> an open item; the egress cage above limits which *pods* can reach NATS, but
> does not partition the stream.

Note also: the AgentSession datapath, like the AgentRun datapath, has **no
`AgentNetwork` allow-list wiring and no `AgentPolicy` enforcement** — the egress
control is the static NetworkPolicy above, which ignores per-Agent CIDR
allow-lists. See
`docs/design/agentnetwork-agentpolicy-interaction.md` for the gap and the plan.

---

## The two "session" concepts (do not conflate)

The platform uses the word "session" for two **independent** things. Owning this
distinction here so other docs (notably `docs/design/harness-authoring.md`) can
link to it instead of re-explaining:

| | **`HarnessSpec.SessionPolicy`** | **The `AgentSession` CRD** |
|---|---|---|
| What it controls | Whether a **single `AgentRun`** reuses the Agent's durable workspace / forwards a stable provider session id across runs | A **separate long-lived worker pod** that handles **multi-turn conversations** over the NATS queue |
| Values / shape | `ephemeral` (default, fresh process+context per run) or `persistent` (share state via the Agent's `Storage`) | A CRD with its own reconciler, Deployment, checkpoint, and gateway route |
| Where | `pkg/agentmodel/v1/harness.go:62-99`; `persistent` requires `spec.storage` (`validation.go:47-51`) | `pkg/agentmodel/v1/types.go:276-304`; `agentsession_controller.go` |
| Lifetime | Bounded — the run still terminates | Long-lived — resident until idle-timeout / deletion |

They are orthogonal: a `persistent` harness gives *one* run a warm workspace; an
`AgentSession` gives a *conversation* a warm worker. A given Agent can use
either, both, or neither.

---

## Tuning guide

The durable knobs that exist today, and the tradeoff each governs:

| Knob | Where | Tradeoff |
|---|---|---|
| **Consume batch size** (`Max`) | `serve_session.go:83` (`Max:16`); `nats.go:95-98` default 16 | Larger batch → fewer NATS round-trips and higher throughput, but turns within a batch are processed serially (`handleTurn` is sequential, `session_worker.go:178-184`), so a slow turn delays its batch-mates. |
| **Poll interval** | `--poll` / `PollInterval` (default 2s, `serve_session.go:33`, `session_worker.go:71-76`) | Shorter poll → lower idle-to-first-turn latency but more wakeups/CPU and more NATS `Fetch` calls. NATS `Fetch` itself blocks up to 500ms (`nats.go:98`), so the effective floor is poll + that. |
| **Idle timeout** | `spec.idleTimeoutSeconds` → `--idle-timeout` (`types.go:282-289`, `serve_session.go:34`) | `>0` lets the worker exit when idle (`session_worker.go:143-146`) so it can scale to zero — at the cost of a cold start (pod reschedule + AgentFS restore) on the next turn. `0` keeps it resident (lowest latency, highest standing cost). |
| **Gateway `?wait` / `MaxWait`** | `?wait` per request, capped by `--max-wait` (default 60s, `main.go:103-109`) | Longer wait → more synchronous (client gets the result inline) but holds a gateway HTTP connection and counts against `containerConcurrency:50`. |
| **Gateway autoscaling** | `knative-service.yaml:22-26` (`min/max-scale`, `target`, `containerConcurrency`) | `min-scale:0` saves cost but adds a gateway cold-start; raise `min-scale` for steady traffic. |
| **Stream retention** | `nats.go:50-52` (`LimitsPolicy`, `MaxAge:24h`, file storage) | Longer `MaxAge` → more replay/redelivery safety after long worker downtime, at higher disk cost on the NATS node. A turn older than 24h is dropped from the stream. |

These are currently **hardcoded defaults / per-flag**, not first-class CRD
fields. Promoting batch size, poll interval, idle timeout, and concurrency to
`AgentSessionSpec` (with sane defaults and per-tenant caps) is the proposal in
`docs/design/agent-session-scaling.md` — that doc is the home for the scaling
fields; this doc is the home for the runtime they tune.

---

## Known limitations

- **Durable WORKSPACE, not resumable internal CONTEXT.** A CLI harness
  (claude-code, etc.) gets its *files* back across a restart (AgentFS restore),
  and the *turn log* is replayed into `SessionState`. But the harness process's
  **in-memory conversation context is not resumed** — each turn re-invokes the
  harness via `RunTurn` (`session_worker.go:96-101`). For loop-mode agents the
  turn log is the context; for an opaque CLI harness, "resume" means "same
  workspace, fresh process." Forwarding a stable provider session id is a
  separate `HarnessSpec.SessionPolicy=persistent` concern (above), not something
  the AgentSession worker does for you.
- **At-least-once turns, no dedup** — a crash between checkpoint and `Ack`
  re-runs and re-appends a turn (see *Crash recovery*).
- **Unbounded checkpoint, no schema version** — see *Checkpoint format +
  growth*.
- **Cluster-wide NATS stream, no ACLs** — see the multi-tenancy warning.
- **No per-tenant session concurrency/quota**, and neither run nor session pods
  set `activeDeadlineSeconds` — a wedged turn has no hard wall clock at the pod
  level. `AgentSessionStatus.Runs` is a **legacy field that is never written**
  (`types.go:301-303`). These are scoping/quota gaps tracked in
  `docs/design/agent-session-scaling.md` and
  `docs/research/agent-runtime-fit-analysis-v0.2.0.md`.
- **Gateway↔operator coupling is by convention** — the gateway's NATS URL and
  the operator's `--session-nats-url` must point at the *same* NATS for turns to
  reach workers (called out in `deploy/agentgateway/knative-service.yaml:6-8`).
  Nothing validates that they agree.

---

## Related

- `docs/features/operator.md` — the operator and the rest of the agent-model
  controllers.
- `docs/features/agent-model.md` — `Agent`/`AgentRun`/`AgentSession` CRDs.
- `docs/design/agent-session-scaling.md` — proposed first-class scaling knobs
  (batch, poll, idle, concurrency, retention) and per-tenant quota.
- `docs/design/harness-authoring.md` — the `HarnessSpec.SessionPolicy` side of
  "session," which links back to this doc for the CRD side.
- `docs/design/agentnetwork-agentpolicy-interaction.md` — the egress
  allow-list / policy-enforcement gap shared with the run datapath.
- `docs/research/agent-runtime-fit-analysis-v0.2.0.md` — the v0.2.0 ground-truth
  audit that motivated documenting this subsystem.
