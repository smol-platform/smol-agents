# AgentSession Scaling — Implementation Spec

> **Status: SPEC (implementation-grade) — proposed, NOT built. 2026-06-03.**
> Category: stub→impl. This is the *implementation* companion to the architecture in
> [`docs/design/agent-session-scaling.md`](../design/agent-session-scaling.md): exact Go struct
> additions, kubebuilder markers, deepcopy obligations, CRD-YAML edits, `serve-session` flag/env
> plumbing, the NATS `StreamConfig` update path, the `processTurns` semaphore + `SessionState`
> mutex, the `Status.Usage` field-wise roll-up, the admission cross-check, and the
> gateway singleton decision. Read the design doc first for the *why*; this doc is the *what to type*.
>
> **Nothing in §3–§9 exists in the tree as of v0.2.0.** Verified 2026-06-02:
> `grep -rn 'MaxConcurrentTurns\|TurnBatch\|turnRetentionSeconds\|Status.Usage' pkg/ operator/ cmd/`
> returns nothing. `AgentSessionSpec` is `{agentRef, idleTimeoutSeconds}`
> (`pkg/agentmodel/v1/types.go:331-338`); `AgentSessionStatus` is `{phase, observedGeneration, runs}`
> (`pkg/agentmodel/v1/types.go:340-353`). Every "today" value below is a live hard-coded constant.

---

## 1. Summary

`AgentSession` is the durable, long-running agent runtime (P3/P4 — built and live: a 1-replica
Deployment running `agent serve-session`, AgentFS-checkpointed `SessionState`, NATS-JetStream turn
delivery via `agentgateway`). But its CRD is skeletal: a tenant can only set `agentRef` and
`idleTimeoutSeconds`. Everything that governs throughput, latency, turn size, retention, history
growth, and worker resources is a constant compiled into a binary or baked into a manifest, and the
worker is **single-threaded per session**. This spec lifts those eleven constants into
`AgentSessionSpec`/`Status` with **defaults equal to today's values** (so an un-annotated session is
bit-for-bit unchanged), wires each to exactly one plumbing point, adds the concurrency machinery
(semaphore + state mutex + per-turn deadline) that `maxConcurrentTurns > 1` requires, rolls the
durable `SessionState.CumulativeUsage` up into `status.usage` (field-wise sum, **not** `Usage.Add`),
deprecates the dead `status.runs`, and adds an admission webhook that rejects a dangling `agentRef`
at `kubectl apply` instead of looping `Pending` for 15 s forever. Outcome: per-session, declarative
scaling from "1 dev agent" to "100-tenant pool" with no image rebuild.

---

## 2. Current state

### What exists (built, live)

| Capability | Where |
|---|---|
| Durable session worker (load state → loop → checkpoint → park) | `pkg/agentruntime/session_worker.go:106-153` (`SessionWorker.Run`) |
| `SessionState` checkpoint (turns + `CumulativeUsage`, atomic save/restore) | `pkg/agentruntime/session.go:34-98` |
| Per-turn fold + cumulative usage advance | `session.go:45-53` (`SessionState.Append`) |
| `serve-session` CLI (loads agent spec, builds worker, NATS or on-disk inbox) | `cmd/agent/serve_session.go:27-95` |
| NATS JetStream turn transport (publish/consume/result) | `pkg/sessionqueue/nats.go:34-139` |
| Stateless Knative gateway (POST turn, GET result, `?wait`) | `cmd/agentgateway/main.go` |
| Operator reconciler (synthetic-AgentRun → run-pod → 1-replica Deployment + egress + broker) | `operator/internal/controllers/agentmodel/agentsession_controller.go:66-169` |
| `idleTimeoutSeconds` → `--idle-timeout` flag plumbing | `agentsession_controller.go:138-140` → `serve_session.go:34,71` |
| NATS wiring (`AGENTSESSION_NATS_URL`/`_KEY` env when `--session-nats-url` set) | `agentsession_controller.go:144-149`, `main.go:49,115` |
| Existing webhook infra (template for admission) | `operator/internal/webhooks/{setup.go,agentnetwork_webhook.go}` |

### What is stubbed / hard-coded / missing

| Gap | Hard-coded value | Location |
|---|---|---|
| **Worker concurrency = 1** | `processTurns` is a serial `for` loop | `session_worker.go:178-184` |
| Turn batch (fetch per poll) | `Max: 16` literal | `serve_session.go:83`; fallback `nats.go:95-97` |
| Poll interval | `--poll` default `2*time.Second` | `serve_session.go:33` → `session_worker.go:71-76` |
| NATS fetch wait per poll | `nats.MaxWait(500*time.Millisecond)` | `nats.go:98` |
| Stream retention | `MaxAge: 24*time.Hour` (single shared stream `AGENT_SESSIONS`) | `nats.go:16,46-52` |
| Max turn body | `maxTurnBytes = 1 << 20` (1 MiB), one global cap | `main.go:30,49` |
| No per-turn deadline at worker | `runTurn` passes parent `ctx` straight through | `session_worker.go:96-101,191` |
| Turn history unbounded | `state.Turns` grows forever; no compaction | `session.go:45-53` |
| Worker resources fixed | inherits `BuildAgentRunPod` defaults | `agentsession_controller.go:132` |
| `status.usage` absent | `SessionState.CumulativeUsage` never reaches the controller | `session.go:38` vs `agentsession_controller.go:210-219` (`writeStatus` only sets phase + observedGeneration) |
| `status.runs` dead | declared but never written | `types.go:350-352`; CRD yaml line 49-51 says "LEGACY/DEAD" |
| Bad `agentRef` → silent `Pending` loop | `r.Get` Agent → `NotFound` → 15 s requeue | `agentsession_controller.go:75-78` |
| No per-tenant quota | unbounded `AgentSession` → unbounded resident pods | (nowhere) |

### Two structural facts that shape the design

1. **`SessionState` is single-writer by construction.** `handleTurn` mutates `state.Phase`
   (`session_worker.go:189`), stamps `st.Index = len(state.Turns)` (`session_worker.go:209`), and
   calls `state.Append` (`session.go:45`), and `Run` checkpoints after each `processTurns`
   (`session_worker.go:131`). Going concurrent (`maxConcurrentTurns > 1`) breaks this — it is the
   load-bearing change of this spec.
2. **There is one cluster-wide gateway and one shared NATS stream.** The gateway is stateless
   (`main.go`); the stream is `defaultStream = "AGENT_SESSIONS"` for *all* sessions (`nats.go:16`).
   Per-session retention and per-session body caps therefore cannot be a single start-time constant.

> Out of scope (cross-link only): `AgentPolicy`/`AgentNetwork` are **not** enforced on the session
> datapath — see [`docs/specs/agentpolicy-enforcement.md`](./agentpolicy-enforcement.md) and
> [`docs/specs/agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md). The
> ~4 KiB pod-termination-message cap that truncates large turn traces is a *run* concern tracked in
> [`docs/specs/response-richness.md`](./response-richness.md); session turns fold via `SessionState`
> on AgentFS, not via the termination message, so they are not bounded by 4 KiB.

---

## 3. CRD field additions

There are **two** parallel type trees and one hand-maintained CRD YAML. All three must be edited:

- `pkg/agentmodel/v1/types.go` — the **pure** Go types (source of truth, JSON tags, doc comments).
- `operator/api/agentmodel/v1/types.go` — the operator wrapper; it embeds the pure types directly
  (`Spec pure.AgentSessionSpec`, `Status pure.AgentSessionStatus`, lines 108-109), so **no struct
  duplication** is needed there — only the deepcopy regen (§3.4).
- `operator/config/crd/runtime.agents.smol-agents.ai_agentsessions.yaml` — hand-maintained
  (the tree is NOT reproducible from `make manifests`; see the CRD-drift note). Edit by hand.

### 3.1 Spec fields → `AgentSessionSpec` (`pkg/agentmodel/v1/types.go:331`)

Add the following after `IdleTimeoutSeconds`. Defaults equal today's hard-coded value, so an
omitted field is a no-op. kubebuilder markers go on the *pure* type (controller-gen reads it for
deepcopy; the CRD YAML carries the validation independently since it is hand-written — keep them in
sync).

```go
// MaxConcurrentTurns bounds how many turns the worker runs at once. 1 (default)
// preserves the serial loop and FIFO ordering; >1 fans turns out under a
// semaphore and REQUIRES the state mutex (see docs/specs/agentsession-scaling-impl.md §5)
// and gives up strict turn ordering. Plumbed to --max-concurrent-turns.
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:Maximum=64
// +optional
MaxConcurrentTurns int32 `json:"maxConcurrentTurns,omitempty"`

// TurnBatchSize is the max turns fetched per poll (NOT the concurrency). Plumbed
// to --turn-batch → QueueSource.Max → NATSQueue.Consume(max). Default 16.
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:Maximum=256
// +optional
TurnBatchSize int32 `json:"turnBatchSize,omitempty"`

// TurnPollIntervalMs is the inbox/queue poll cadence in ms. Lower = snappier
// pickup, more idle NATS fetches. Plumbed to --poll (rendered <n>ms). Default 2000.
// +kubebuilder:validation:Minimum=100
// +kubebuilder:validation:Maximum=60000
// +optional
TurnPollIntervalMs int32 `json:"turnPollIntervalMs,omitempty"`

// TurnDeliveryTimeoutSeconds is a hard ctx-cancel per turn at the worker
// boundary, independent of Budget.MaxWallClockSeconds. Effective per-turn
// deadline = min(this, Budget.MaxWallClockSeconds) (0 on either = the other).
// Plumbed to --turn-timeout. Default 300.
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:Maximum=86400
// +optional
TurnDeliveryTimeoutSeconds int32 `json:"turnDeliveryTimeoutSeconds,omitempty"`

// TurnRetentionSeconds is the NATS stream MaxAge for this session's turns. This
// is a STREAM-level setting, not a worker flag (see §3.3). Default 86400 (24h).
// +kubebuilder:validation:Minimum=60
// +kubebuilder:validation:Maximum=2592000
// +optional
TurnRetentionSeconds int32 `json:"turnRetentionSeconds,omitempty"`

// MaxTurnInputBytes caps the turn request body the gateway accepts (defence in
// depth: the worker re-validates on decode). Enforced at the front door (§3.3).
// Default 1048576 (1 MiB).
// +kubebuilder:validation:Minimum=1024
// +kubebuilder:validation:Maximum=10485760
// +optional
MaxTurnInputBytes int32 `json:"maxTurnInputBytes,omitempty"`

// TurnHistoryLimit caps len(SessionState.Turns) on disk; oldest turns are
// dropped once exceeded (their usage is already folded into CumulativeUsage, so
// totals survive). Plumbed to --turn-history-limit. Default 10000.
// +kubebuilder:validation:Minimum=10
// +kubebuilder:validation:Maximum=1000000
// +optional
TurnHistoryLimit int32 `json:"turnHistoryLimit,omitempty"`

// Resources overrides the worker container's compute. nil = the built-in
// BuildAgentRunPod defaults. Applied by the controller after pod build (§5.4).
// +optional
Resources *ResourceRequirements `json:"resources,omitempty"`
```

> **Decision — `Resources` type.** The pure package `pkg/agentmodel/v1` deliberately does **not**
> import `k8s.io/api/core/v1` today (verified: `grep -rln 'k8s.io/api/core/v1' pkg/agentmodel/`
> returns nothing — it imports only `metav1` + `encoding/json`). Two options:
> - **(a)** Import `corev1` into the pure package and use `*corev1.ResourceRequirements`. Zero
>   translation, but it pulls a heavy k8s dep into the previously-clean pure model (which is imported
>   by `pkg/agentruntime`, the in-pod binary).
> - **(b) (recommended)** Define a minimal pure `ResourceRequirements{Limits, Requests
>   map[string]string}` in the pure package and translate to `corev1.ResourceList`
>   (`resource.MustParse`) in the operator builder. Keeps the pure package dep-free and the in-pod
>   binary small; costs one ~20-line translation helper in
>   `operator/internal/builders/agentrun.go`.
>
> **Recommendation: (b).** This is a real maintainer decision (see §10); the field name above is
> written for (b). If (a) is chosen, swap to `*corev1.ResourceRequirements` and skip the helper.

### 3.2 Constructor accessors (defaults in code)

CRD markers/YAML bound the *range*; the *default-equals-today* behaviour lives in Go accessors so
the in-pod binary and the controller agree on `0 → today's value`. Add to `pkg/agentmodel/v1`
(new small file `agentsession_defaults.go`, mirroring `Budget`'s helper style):

```go
func (s AgentSessionSpec) ConcurrentTurns() int32 { if s.MaxConcurrentTurns > 0 { return s.MaxConcurrentTurns }; return 1 }
func (s AgentSessionSpec) BatchSize() int32        { if s.TurnBatchSize > 0 { return s.TurnBatchSize }; return 16 }
func (s AgentSessionSpec) PollMs() int32           { if s.TurnPollIntervalMs > 0 { return s.TurnPollIntervalMs }; return 2000 }
func (s AgentSessionSpec) TurnTimeoutSec() int32   { if s.TurnDeliveryTimeoutSeconds > 0 { return s.TurnDeliveryTimeoutSeconds }; return 300 }
func (s AgentSessionSpec) RetentionSec() int32     { if s.TurnRetentionSeconds > 0 { return s.TurnRetentionSeconds }; return 86400 }
func (s AgentSessionSpec) MaxInputBytes() int32    { if s.MaxTurnInputBytes > 0 { return s.MaxTurnInputBytes }; return 1 << 20 }
func (s AgentSessionSpec) HistoryLimit() int32     { if s.TurnHistoryLimit > 0 { return s.TurnHistoryLimit }; return 10000 }
```

The controller renders flags from these accessors; the worker also accepts `0` flags and applies
the same defaults (defence in depth — a worker started by hand still behaves correctly).

### 3.3 The two knobs that are NOT worker flags

Five fields become a `serve-session` flag. Two cross a process boundary:

- **`turnRetentionSeconds` → NATS stream.** `NewNATSQueue` creates the stream once via
  `js.AddStream` (`nats.go:46-52`), and `AddStream` returns `ErrStreamNameAlreadyInUse` for an
  existing stream — it does **not** reconcile `MaxAge`. There is one shared stream for all sessions,
  so a single `MaxAge` cannot express per-session retention. **Mechanism (recommended): shared
  stream + `UpdateStream` to the cluster-wide max.** The operator (which already holds `NATSURL`,
  `agentsession_controller.go:47`) computes `max(turnRetentionSeconds)` across live sessions and
  calls `js.UpdateStream`. Coarse (one cluster-wide `MaxAge`) but a single call and no stream-count
  blow-up. The per-session stream alternative (`AGENT_SESSIONS_<key>`) is precise but multiplies
  JetStream stream count by session count — reserve for hard per-session retention SLAs. Either way
  `sessionqueue` grows a `MaxAge time.Duration` parameter (replace the constant at `nats.go:51`); see
  §6.
- **`maxTurnInputBytes` → gateway.** The gateway is stateless and shared, so it cannot bake a
  per-session limit at start. The `io.LimitReader(r.Body, maxTurnBytes)` at `main.go:49` becomes a
  per-request lookup: the gateway reads the `AgentSession` (`{ns}/{name}` from the path, already in
  hand at `main.go:48`) and uses its `MaxInputBytes()`, with a short-TTL cache keyed by session key
  (this adds a `GET AgentSession` to the hot path — cache it). The worker re-validates on decode
  (it already drops malformed turns, `session_queue_source.go:28-33`). See §6 for the gateway's new
  k8s client dependency.

### 3.4 Status fields → `AgentSessionStatus` (`pkg/agentmodel/v1/types.go:340`)

Surface the durable `SessionState` the controller does not read today. Add after `ObservedGeneration`:

```go
// Usage is the cumulative resource consumption across all turns, mirroring
// SessionState.CumulativeUsage (session.go:38). Folded field-wise (NOT via
// Usage.Add, which always increments Steps by 1 — see §4).
// +optional
Usage Usage `json:"usage,omitempty"`

// Turns is the number of turns COMPLETED in this session (monotonic counter,
// NOT len(state.Turns) — that shrinks when TurnHistoryLimit compacts; see §4).
// +optional
Turns int32 `json:"turns,omitempty"`

// FailedTurns counts turns whose Phase==Failed or Error!="".
// +optional
FailedTurns int32 `json:"failedTurns,omitempty"`

// LastTurnTime is when the most recent turn completed (SessionState.UpdatedAt).
// +optional
LastTurnTime *metav1.Time `json:"lastTurnTime,omitempty"`
```

Deprecate (do not delete yet — keeps stored objects decodable) the existing `runs`:

```go
// Runs is DEAD: never written by any reconciler (writeStatus only sets phase +
// observedGeneration, agentsession_controller.go:210-219) and a session folds
// turns into SessionState, not into child AgentRun CRs. Replaced by
// status.turns (count) + status.usage (aggregate).
// Deprecated: remove after one minor release.
// +optional
Runs []string `json:"runs,omitempty"`
```

### 3.5 CRD-YAML edits (hand-maintained)

Edit `operator/config/crd/runtime.agents.smol-agents.ai_agentsessions.yaml` by hand (the file is
52 lines; the tree is not reproducible from controller-gen). Under `spec.properties` add:

```yaml
                maxConcurrentTurns:        { type: integer, minimum: 1,    maximum: 64,       description: 'Turns run at once; 1 (default) keeps serial FIFO.' }
                turnBatchSize:             { type: integer, minimum: 1,    maximum: 256,      description: 'Turns fetched per poll; default 16.' }
                turnPollIntervalMs:        { type: integer, minimum: 100,  maximum: 60000,    description: 'Poll cadence ms; default 2000.' }
                turnDeliveryTimeoutSeconds:{ type: integer, minimum: 1,    maximum: 86400,    description: 'Hard per-turn ctx deadline; default 300.' }
                turnRetentionSeconds:      { type: integer, minimum: 60,   maximum: 2592000,  description: 'NATS stream MaxAge; default 86400.' }
                maxTurnInputBytes:         { type: integer, minimum: 1024, maximum: 10485760, description: 'Gateway turn-body cap; default 1048576.' }
                turnHistoryLimit:          { type: integer, minimum: 10,   maximum: 1000000,  description: 'On-disk turn-log cap; default 10000.' }
                resources:
                  type: object
                  description: 'Worker container compute override; null = built-in defaults.'
                  properties:
                    limits:   { type: object, additionalProperties: { type: string } }
                    requests: { type: object, additionalProperties: { type: string } }
```

Under `status.properties` add `usage` (object: steps/tokens/toolCalls/wallClockUsed), `turns`,
`failedTurns` (integers), `lastTurnTime` (string, format date-time). Add a printer column:
`- { name: Turns, type: integer, jsonPath: .status.turns }`.

### 3.6 Deepcopy obligations

`pkg/agentmodel/v1/zz_generated.deepcopy.go:241-271` currently has *value* deepcopy for
`AgentSessionSpec`/`Status` (both are plain — `*out = *in`). The new `*ResourceRequirements` (spec)
and `*metav1.Time` (status) are **pointers**, so the generated `DeepCopyInto` must now allocate.
Regenerate, do not hand-edit:

```bash
make -C operator deepcopy   # controller-gen object:headerFile=hack/boilerplate.go.txt
```

This regenerates **both** `pkg/agentmodel/v1/zz_generated.deepcopy.go` and
`operator/api/agentmodel/v1/zz_generated.deepcopy.go` (the operator type embeds the pure type, so
its generated `(*AgentSession).DeepCopyInto` calls the pure spec/status deepcopy). `Usage` is all
value fields → no pointer handling. Verify the diff only touches the four changed structs.

---

## 4. Status roll-up — field-wise, NOT `Usage.Add`

`Usage.Add` exists (`pkg/agentmodel/v1/budget.go:84-91`) but is **step-oriented**: it always does
`Steps: u.Steps + 1`. It is wrong for aggregating a turn's usage into a session total (a turn that
ran 5 steps would only bump `Steps` by 1). The correct roll-up is **field-wise sum**, which
`SessionState.Append` already does for the durable copy (`session.go:48-51`):

```go
s.CumulativeUsage.Steps         += t.Usage.Steps
s.CumulativeUsage.Tokens        += t.Usage.Tokens
s.CumulativeUsage.ToolCalls     += t.Usage.ToolCalls
s.CumulativeUsage.WallClockUsed += t.Usage.WallClockUsed
```

`status.usage` is a straight mirror of `SessionState.CumulativeUsage` (already field-wise summed) —
the controller copies it verbatim, no re-summing. **Do not call `Usage.Add` anywhere in this path.**

`status.turns` is the trap: once `TurnHistoryLimit` compaction (§5.3) drops old entries,
`len(state.Turns)` *shrinks*. So the status counter must be a **monotonic** value, not the slice
length. Add a `TotalTurns int` to `SessionState` (incremented in `Append` *before* any compaction);
`status.turns = state.TotalTurns`. `status.failedTurns` likewise becomes a counter
(`FailedTurns int` on `SessionState`, incremented in `Append` when `t.Phase == PhaseFailed || t.Error != ""`).

**Worker→status link (recommended: controller reads the checkpoint).** Status is operator-derived
today (Deployment availability → phase, `agentsession_controller.go:157-168`); `SessionState` lives
on AgentFS and never reaches the controller. Two options:

1. **Worker patches `agentsession/status` directly** after each checkpoint (`session_worker.go:131`).
   Lowest latency, but grants the worker SA `patch` on the status subresource (it has zero AgentSession
   RBAC today) — a real blast-radius widening.
2. **Controller reads the checkpoint** on its existing requeue. The worker writes a tiny
   `status-summary.json` (just usage/totalTurns/failedTurns/updatedAt) next to `state.json`
   (`DefaultSessionStatePath`, `session.go:62-64`) on each checkpoint; the controller's reconcile
   reads it (via the AgentFS path or a `kubectl cp`-style exec — but simplest: a sidecar that already
   has the volume mounted publishes it). No new worker RBAC; eventually-consistent.

**Recommendation: option 2** — matches the "minimal worker blast radius" architecture and reuses the
requeue loop. The controller's `writeStatus` (`agentsession_controller.go:210-219`) gains the four
fields; bump the requeue to a steady cadence (e.g. 30 s) while `Running` so usage advances even
without a Deployment event. Revisit option 1 only if status staleness becomes a complaint.

---

## 5. Concurrency model (the load-bearing change)

### 5.1 New `SessionWorker` fields

Add to `SessionWorker` (`pkg/agentruntime/session_worker.go:24-43`):

```go
MaxConcurrentTurns int           // 0/1 => serial (today's behaviour)
TurnTimeout        time.Duration // 0 => no worker-level per-turn deadline
HistoryLimit       int           // 0 => unbounded (today's behaviour)
mu                 sync.Mutex    // guards state.Phase / state.Append / index stamping
```

### 5.2 Semaphore in `processTurns`

`processTurns` (`session_worker.go:172-186`) today:

```go
for _, t := range turns {
    if ctx.Err() != nil { return handled, ctx.Err() }
    w.handleTurn(ctx, state, t)   // synchronous, one at a time
    handled++
}
```

Replace with a width-`MaxConcurrentTurns` semaphore (1 ⇒ identical serial behaviour):

```go
width := w.MaxConcurrentTurns
if width < 1 { width = 1 }
sem := make(chan struct{}, width)
var wg sync.WaitGroup
for _, t := range turns {
    if ctx.Err() != nil { break }
    sem <- struct{}{}
    wg.Add(1)
    go func(t InboundTurn) {
        defer wg.Done()
        defer func() { <-sem }()
        tctx, cancel := w.turnCtx(ctx)
        defer cancel()
        w.handleTurn(tctx, state, t)
    }(t)
    handled++
}
wg.Wait()
```

### 5.3 `handleTurn` under the mutex

`handleTurn` (`session_worker.go:188-219`) mutates shared `*SessionState`. With concurrency the
**state writes must be serialized** — and `st.Index = len(state.Turns)` (`session_worker.go:209`)
plus `state.Append` (`session.go:45`, which also does `t.Index = len(s.Turns)`) must be in the
**same critical section**, or two turns claim the same index. The fix:

- Run the LLM/harness (`w.runTurn`, the slow part) **outside** the lock — that is where concurrency
  buys throughput.
- Take `w.mu` only for the `state.Phase`/index-stamp/`Append`/`Publish`-sequencing writes.

```go
func (w *SessionWorker) handleTurn(ctx context.Context, state *SessionState, t InboundTurn) {
    w.mu.Lock(); state.Phase = v1.PhaseRunning; w.mu.Unlock()
    started := w.now()
    res, runErr := w.runTurn(ctx, t.Spec) // SLOW; concurrent; no lock held
    st := SessionTurn{ /* …as today… */ StartedAt: started, EndedAt: w.now() }
    if runErr != nil { st.Error = runErr.Error(); if st.Phase == "" { st.Phase = v1.PhaseFailed } }

    w.mu.Lock()
    st.Index = len(state.Turns) // stamp + Append atomic under the lock
    state.Append(st, w.now())   // advances CumulativeUsage + TotalTurns + FailedTurns
    w.compact(state)            // §5.4 — drop oldest beyond HistoryLimit
    w.mu.Unlock()

    if err := w.sink().Publish(ctx, t.ID, st); err != nil { w.log("publish result", "turn", t.ID, "err", err) }
    if t.Ack != nil { if err := t.Ack(); err != nil { w.log("ack turn", "turn", t.ID, "err", err) } }
}
```

> **Ordering caveat (document as a hard trade-off, not a bug).** Concurrent turns lose FIFO. The
> on-disk inbox relies on lexical filename order (`session_worker.go:239`) and NATS gives
> per-consumer order, but running a batch concurrently makes *completion* order non-deterministic.
> Sessions needing strict ordering MUST keep `maxConcurrentTurns: 1` (the default).

### 5.4 Per-turn deadline + history compaction

```go
func (w *SessionWorker) turnCtx(parent context.Context) (context.Context, context.CancelFunc) {
    d := time.Duration(w.Agent.Spec.Budget.MaxWallClockSeconds) * time.Second // budget.go:27
    if w.TurnTimeout > 0 && (d == 0 || w.TurnTimeout < d) { d = w.TurnTimeout }
    if d <= 0 { return parent, func() {} } // neither set → no worker deadline (today)
    return context.WithTimeout(parent, d)
}

func (w *SessionWorker) compact(state *SessionState) { // caller holds w.mu
    if w.HistoryLimit > 0 && len(state.Turns) > w.HistoryLimit {
        drop := len(state.Turns) - w.HistoryLimit
        state.Turns = append(state.Turns[:0], state.Turns[drop:]...) // usage already in CumulativeUsage
    }
}
```

Today there is **no** worker-level per-turn deadline — `runTurn` passes the parent `ctx` through
(`session_worker.go:96-101`), and only the executor's per-step `Budget.AllowsStep`
(`budget.go:67-81`) bounds wall-clock — evaluated *before* each step, so a single hung step (stuck
harness HTTP) is never interrupted. `turnCtx` makes cancellation real at the worker boundary.

`compact` must run inside the same `w.mu` section as `Append` so a reader (the checkpoint `Save`)
never sees a torn slice. **Checkpoint serialization:** `Run` calls `store.Save(state)` after
`processTurns` returns (`session_worker.go:131`) — *after* `wg.Wait()`, so the save sees a quiescent
state and needs no extra lock. (Do **not** checkpoint inside `handleTurn`.)

### 5.5 `activeDeadlineSeconds` — intentionally NOT set for sessions

A session worker is long-lived (parks in `RequiresAction` when idle, `session_worker.go:138-146`;
scales to zero via `IdleTimeoutSeconds`). A pod-level `activeDeadlineSeconds` would kill a healthy
idle session — **wrong** here. Bound per-turn with `turnDeliveryTimeoutSeconds`; bound lifecycle
with `IdleTimeoutSeconds`. (One-shot `AgentRun` pods are a separate datapath where a pod deadline
would make sense — out of scope; see [`docs/specs/run-governance.md`](./run-governance.md).)

---

## 6. Plumbing — exact files & edits

### 6.1 `cmd/agent/serve_session.go`

Add flags (`serve_session.go:33-36` block) and thread into `SessionWorker`:

```go
maxConc   := fs.Int("max-concurrent-turns", 0, "turns run at once; 0/1 = serial")
turnBatch := fs.Int("turn-batch", 0, "turns fetched per poll; 0 = 16")
turnTO    := fs.Duration("turn-timeout", 0, "hard per-turn deadline; 0 = none")
histLimit := fs.Int("turn-history-limit", 0, "on-disk turn-log cap; 0 = unbounded")
```

Set on the worker (`serve_session.go:64-73`): `MaxConcurrentTurns: *maxConc`, `TurnTimeout: *turnTO`,
`HistoryLimit: *histLimit`. Replace the literal `Max: 16` (`serve_session.go:83`) with a batch that
defaults to 16: `Max: batchOr16(*turnBatch)`. `--poll` already exists (`serve_session.go:33`); the
controller now renders it from `PollMs()`.

### 6.2 `pkg/agentruntime/session_worker.go`

Fields (§5.1), semaphore (§5.2), `handleTurn` lock discipline (§5.3), `turnCtx`/`compact` (§5.4).

### 6.3 `pkg/agentruntime/session.go`

`SessionState` gains `TotalTurns int` + `FailedTurns int` (json `totalTurns`/`failedTurns`).
`Append` increments `TotalTurns` and (conditionally) `FailedTurns` alongside the existing
cumulative-usage sum (`session.go:48-52`). `compact` lives on the worker (§5.4), not here, so
`SessionState` stays a pure data type.

### 6.4 `pkg/sessionqueue/nats.go` + `queue.go`

- `NewNATSQueue` gains a `MaxAge time.Duration` parameter (replace the constant at `nats.go:51`); a
  `0` keeps the 24 h default. Update the one caller in the gateway (`main.go:130`) and in
  `serve_session.go:77` (workers can pass `0` — only the operator sets retention).
- Add `UpdateRetention(ctx, maxAge time.Duration) error` to the `Queue` interface
  (`queue.go:28-46`) and `NATSQueue`: `js.UpdateStream(&nats.StreamConfig{Name: q.stream, …,
  MaxAge: maxAge})`. `MemQueue` (tests) implements it as a no-op.
- The `Consume` fallback `if max <= 0 { max = 16 }` (`nats.go:95-97`) stays as defence in depth.

### 6.5 `cmd/agentgateway/main.go`

- Add a k8s client (controller-runtime `client.New` with the in-cluster config) — the gateway is a
  Knative Service today with **no** apiserver access. New RBAC: `get` on `agentsessions` in the
  served namespaces. This is a real new dependency (see §8 security).
- Replace `maxTurnBytes` const usage at `main.go:49` with a per-session lookup
  `g.maxInputBytes(ctx, ns, name)` → reads the `AgentSession`, returns `spec.MaxInputBytes()`, caches
  by `SessionKey` with a short TTL (e.g. 30 s); falls back to `1<<20` on miss/error.
- Keep the global `maxTurnBytes` as the hard ceiling for the *cache-miss* path and as the
  `LimitReader` outer bound (never read more than the CRD `Maximum` of 10 MiB even if a session asks
  for more — `min(perSession, 10MiB)`).

### 6.6 `operator/internal/controllers/agentmodel/agentsession_controller.go`

- Render the new flags onto `cmd` (after `serve_session.go`-style block at
  `agentsession_controller.go:137-141`):

```go
cmd := []string{"/agent", "serve-session", "--dir=" + builders.RunSpecMountPath, "--agent-ref=" + session.Spec.AgentRef}
sp := session.Spec
cmd = append(cmd,
    fmt.Sprintf("--max-concurrent-turns=%d", sp.ConcurrentTurns()),
    fmt.Sprintf("--turn-batch=%d", sp.BatchSize()),
    fmt.Sprintf("--poll=%dms", sp.PollMs()),
    fmt.Sprintf("--turn-timeout=%ds", sp.TurnTimeoutSec()),
    fmt.Sprintf("--turn-history-limit=%d", sp.HistoryLimit()),
)
if sp.IdleTimeoutSeconds > 0 { cmd = append(cmd, fmt.Sprintf("--idle-timeout=%ds", sp.IdleTimeoutSeconds)) }
```

- Apply `spec.resources` to the worker container after `BuildAgentRunPod` (`agentsession_controller.go:132`):
  `if sp.Resources != nil { builders.ApplyResources(pod, sp.Resources) }` — new helper in
  `operator/internal/builders/agentrun.go` translating the pure `ResourceRequirements` to
  `corev1.ResourceRequirements` (option (b) in §3.1) and setting `pod.Spec.Containers[0].Resources`.
- Call `UpdateRetention` once per reconcile when `NATSURL != ""`: compute the max
  `RetentionSec()` across live sessions (a cheap `List` in the namespace, or cluster-wide if
  retention is global) and call `q.UpdateRetention`. Connecting a `Queue` from the controller is a
  new dependency — alternatively fold this into the gateway, which already holds a `Queue`. **Simpler:
  let the gateway own retention reconciliation** (it already lists sessions for §6.5); the controller
  stays NATS-client-free except for the env it already injects. Decide in §10.
- Fold `SessionState` summary into status (§4, option 2): extend `writeStatus`
  (`agentsession_controller.go:210-219`) to set `usage/turns/failedTurns/lastTurnTime` from the
  summary read, and bump the `Running` requeue to ~30 s.

### 6.7 Admission webhook (`operator/internal/webhooks/`)

There is existing webhook infra — `agentnetwork_webhook.go` is the cleanest validator template
(stateless `CustomValidator`, no Platform lookup) and `setup.go` is the registration glue. Add
`agentsession_webhook.go`:

```go
type agentSessionWebhook struct{ client client.Client }

func (w *agentSessionWebhook) ValidateCreate(ctx context.Context, s *amv1.AgentSession) (admission.Warnings, error) {
    return nil, w.validate(ctx, s)
}
func (w *agentSessionWebhook) ValidateUpdate(ctx context.Context, _, n *amv1.AgentSession) (admission.Warnings, error) {
    return nil, w.validate(ctx, n)
}
func (w *agentSessionWebhook) ValidateDelete(context.Context, *amv1.AgentSession) (admission.Warnings, error) { return nil, nil }

func (w *agentSessionWebhook) validate(ctx context.Context, s *amv1.AgentSession) error {
    // 1. agentRef exists in-namespace (the highest-value check — kills the Pending limbo).
    a := &amv1.Agent{}
    if err := w.client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.AgentRef}, a); err != nil {
        if apierrors.IsNotFound(err) {
            return fmt.Errorf("agentRef %q not found in namespace %q", s.Spec.AgentRef, s.Namespace)
        }
        return err
    }
    // 2. Cross-field rule (range checks are CRD markers): turn deadline ≤ retention.
    if s.Spec.TurnTimeoutSec() > s.Spec.RetentionSec() {
        return fmt.Errorf("turnDeliveryTimeoutSeconds (%d) must be ≤ turnRetentionSeconds (%d)", s.Spec.TurnTimeoutSec(), s.Spec.RetentionSec())
    }
    return nil
}
```

Register in `setup.go` (a new `SetupAgentSessionWebhook(mgr)`) and add the webhook entry to
`operator/config/webhook/webhooks.yaml`. The reconciler's `NotFound → Pending` path
(`agentsession_controller.go:75-78`) stays as belt-and-suspenders (the Agent can be deleted *after*
admission), but admission turns the *common* "typo in agentRef" case from a silent 15 s loop into an
immediate `kubectl apply` error.

Per-namespace quota (cap live `AgentSession` count or `Σ maxConcurrentTurns`) is enforced here too —
but it needs a `List` per admission; gate behind a Platform-level setting and treat as a follow-up
(see [`docs/specs/run-governance.md`](./run-governance.md) for the broader quota story).

---

## 7. Data / control flow

```
kubectl apply AgentSession ─▶ admission webhook (§6.7)
                                 ├─ agentRef exists?            (else REJECT)
                                 ├─ knob ranges (CRD markers)   (else REJECT)
                                 └─ turnTimeout ≤ retention      (else REJECT)
                                        │ admitted
                                        ▼
AgentSessionReconciler.Reconcile (agentsession_controller.go:66)
  ├─ resolveSandbox (fail-closed kata-fc)
  ├─ BuildRunSpecConfigMap / BuildBrokerConfigSecret / BuildAgentSessionEgressPolicy
  ├─ BuildAgentRunPod ─▶ ApplyRunSandbox ─▶ AttachSecretBroker
  ├─ cmd += --max-concurrent-turns / --turn-batch / --poll / --turn-timeout / --turn-history-limit   (§6.6)
  ├─ ApplyResources(pod, spec.resources)                                                              (§6.6)
  ├─ env += AGENTSESSION_NATS_URL / _KEY  (when --session-nats-url set)
  └─ ensureDeployment(1 replica)
        │
        ▼  pod runs:  /agent serve-session …
   SessionWorker.Run (session_worker.go:106)
     restore SessionState (AgentFS init container + SessionStore.Load)
     loop:
       processTurns  ──Poll──▶ QueueSource ──Consume(BatchSize)──▶ NATSQueue (stream AGENT_SESSIONS)
         fan-out under sem(MaxConcurrentTurns):
            handleTurn:  runTurn(turnCtx) [SLOW, no lock]
                         └─lock─ index-stamp + Append (+TotalTurns/+FailedTurns) + compact ─unlock─
                         Publish(result) ─▶ QueueSink ─▶ NATSQueue result.<turnID>
                         Ack
       store.Save(state)            (after wg.Wait — quiescent)
       write status-summary.json    (usage/totalTurns/failedTurns/updatedAt)
       park in RequiresAction when idle; exit on IdleTimeout

POST /v1/sessions/{ns}/{name}/turns ─▶ agentgateway (main.go:47)
  ├─ maxInputBytes lookup (GET AgentSession, cached)  (§6.5)
  ├─ LimitReader(min(perSession,10MiB)) + decode AgentRunSpec
  ├─ Queue.Publish ─▶ stream
  └─ ?wait>0: Queue.FetchResult ─▶ result.<turnID>

Controller requeue (~30s while Running) ─▶ read status-summary.json ─▶ writeStatus(usage/turns/…)
Gateway (or controller) reconcile ─▶ Queue.UpdateRetention(max RetentionSec across live sessions)
```

---

## 8. Security model

How the new surface composes with the existing containment (kata-fc sandbox + static egress
NetworkPolicy + secret broker + SPIFFE):

| New surface | Composition / mitigation |
|---|---|
| **Worker concurrency** | Same pod, same kata-fc microVM, same egress cage — concurrency is *intra-pod* goroutine fan-out, **no new network or kernel surface**. The `Budget`/`turnTimeout` per-turn deadline now actually cancels a wedged turn (improves DoS posture: one stuck harness call no longer pins a slot forever). The `state` mutex prevents a data race that could corrupt the durable checkpoint. |
| **Gateway → apiserver (new, §6.5)** | The gateway gains a k8s client + `get agentsessions` RBAC where it had none. Scope the SA to `get`/`list` on `agentsessions` only (no secrets, no pods, no write). Cache with a short TTL to bound apiserver load. The gateway already trusts the path `{ns}/{name}`; reading the same object adds no new authz decision, only a lookup. |
| **`maxTurnInputBytes`** | Tightens a DoS vector (oversized turn bodies) per-session; the hard `min(perSession, 10MiB)` ceiling means a tenant cannot raise its own limit unboundedly. Worker re-validates on decode (defence in depth). |
| **`turnRetentionSeconds` (cluster-wide max)** | A tenant requesting long retention raises the *shared* stream's `MaxAge`, so turns from *other* sessions also persist longer — a minor cross-tenant data-lifetime leak (storage, not content; subjects are per-session so no cross-read). Mitigation: cap `Maximum=2592000` (30 d) and document the shared-stream semantics; escalate to per-session streams (§3.3 option b) if a tenant needs *shorter* retention than a noisy neighbour forces. |
| **Worker→status patch (rejected option 1)** | Granting the worker `patch agentsession/status` would widen its blast radius; **option 2 (controller reads checkpoint)** keeps the worker with zero AgentSession RBAC — preferred precisely for this reason. |
| **Per-namespace quota (§6.7)** | Without it a tenant can pin unbounded resident kata-fc worker pods (each a microVM = real cost). Quota at admission is the mitigation; until shipped, document the unbounded-pod risk. |

No change to SPIFFE identity (the worker keeps the run-pod's `IdentitySpec` binding) or to the
broker UDS path. The new knobs are all *quantitative* (sizes/counts/timeouts), so they cannot widen
the *qualitative* trust boundary (sandbox class, egress allow-list, credential brokering) — those
remain governed by the existing fail-closed paths.

---

## 9. Phasing & effort

Each phase is independently shippable; defaults-equal-today means partial rollout is safe.

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **P1 — fields + flags, behaviour-preserving** | Add all spec fields + accessors (§3.1-3.2), CRD-YAML (§3.5), deepcopy (§3.6); render flags from the controller (§6.6 flag block); thread `--turn-batch`/`--poll`/`--turn-history-limit` into the worker; **`maxConcurrentTurns` accepted but clamped to 1** (no semaphore yet). Net: tunable batch/poll/history, zero concurrency risk. | **M** | — |
| **P2 — concurrency core** | Semaphore + `state` mutex + `handleTurn` lock discipline + `turnCtx` per-turn deadline + `compact` (§5). Flip `maxConcurrentTurns` to honour >1. The load-bearing, highest-risk change. | **L** | P1 |
| **P3 — status roll-up** | `SessionState.TotalTurns/FailedTurns` (§6.3); `status.usage/turns/failedTurns/lastTurnTime` (§3.4); deprecate `runs`; controller reads `status-summary.json` (§4 option 2). | **M** | P1 (fields); independent of P2 |
| **P4 — gateway per-session limits + retention** | Gateway k8s client + `maxTurnInputBytes` lookup + cache + RBAC (§6.5); `sessionqueue` `MaxAge` param + `UpdateRetention` (§6.4); retention reconciliation (gateway-owned). | **L** | P1 (fields) |
| **P5 — admission webhook + quota** | `agentsession_webhook.go` (agentRef existence + cross-field) + registration (§6.7); per-namespace quota behind a Platform flag. | **M** | P1 (fields); webhook infra already exists |
| **P6 — `spec.resources`** | Pure `ResourceRequirements` + `ApplyResources` builder helper (§3.1 option b) + controller wiring (§6.6). | **S** | P1 |

Cross-spec dependencies: none are hard blockers. The status roll-up overlaps the
[`response-richness.md`](./response-richness.md) (`Steps` size-budget) and
[`durable-session-architecture.md`](../design/durable-session-architecture.md) (the checkpoint it
reads from). Per-namespace quota shares machinery with
[`run-governance.md`](./run-governance.md). None of [`agentpolicy-enforcement.md`](./agentpolicy-enforcement.md)
or [`agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md) is required —
this spec is orthogonal to policy/egress enforcement.

---

## 10. Test plan

### Unit (both modules; `go test ./...` green is the gate)

| Test | Asserts | Pattern |
|---|---|---|
| `AgentSessionSpec` accessors | `0 → today's default`; in-range value passes through | new `agentsession_defaults_test.go`, mirroring `budget_test.go` |
| `processTurns` serial (width 1) | identical to today — same order, same checkpoint | extend `session_worker_test.go:37` (`TestSessionWorker_ProcessInbox`); inject `echoRun` (line 34) |
| `processTurns` concurrent (width N) | all turns folded exactly once; `CumulativeUsage` field-wise sum correct; **no duplicate `Index`**; `TotalTurns == #turns` | new `TestSessionWorker_ConcurrentTurns`; run with `-race` (the mutex is the point); use a `run` func that sleeps so goroutines actually overlap |
| `compact` | beyond `HistoryLimit` drops oldest; `CumulativeUsage`/`TotalTurns` unchanged | new test on `SessionState` + worker `compact` |
| `turnCtx` | `min(turnTimeout, budget)`; 0+0 → parent ctx; cancellation fires | table test |
| Resume after concurrency | checkpoint written post-`wg.Wait`; a second worker `Load`s and continues with correct `TotalTurns` | extend `TestSessionWorker_ResumeFromCheckpoint` (`session_worker_test.go:62`) |
| `NATSQueue.UpdateRetention` | `UpdateStream` called with new `MaxAge`; `MemQueue` no-op | extend `sessionqueue/queue_test.go` |
| status roll-up | `status.usage` == `CumulativeUsage` verbatim (NOT via `Usage.Add`); `status.turns` monotonic across compaction | controller unit (envtest or fake client) |
| admission | dangling `agentRef` → reject with message; valid → admit; `turnTimeout > retention` → reject | new `agentsession_webhook_test.go`, mirroring `agentnetwork_webhook_test.go` |
| deepcopy | round-trip a session with non-nil `Resources` + `LastTurnTime`; mutate copy, original unchanged | generated; add a focused assertion |

### E2E (the **cftest** single-node k0s box exists for live verification — see the cf-tunnel-deploy memory)

1. **Concurrency throughput.** Deploy an AgentSession with `maxConcurrentTurns: 4`, fire 8 turns via
   the gateway, assert wall-clock ≈ 2 batches not 8 serial; assert `status.turns == 8`,
   `status.usage.tokens` == sum of per-turn usage.
2. **Resume across pod kill.** Mid-session, `kubectl delete pod` the worker; assert the new pod's
   AgentFS init container restores state, `status.turns` does not regress, in-flight (unacked) turns
   redeliver and complete (at-least-once via NATS), no duplicate folds.
3. **Per-session body cap.** Set `maxTurnInputBytes: 4096`; POST a 5 KiB turn → `400`; POST a 2 KiB
   turn → accepted.
4. **Retention.** Two sessions with different `turnRetentionSeconds`; assert the shared stream's
   `MaxAge` reflects the max (or, if per-session streams chosen, each stream's own).
5. **Admission.** `kubectl apply` an AgentSession with a non-existent `agentRef` → immediate error
   (no `Pending` object created).
6. **Defaults regression.** An un-annotated AgentSession behaves exactly as v0.2.0 (serial, batch
   16, 2 s poll, 24 h retention) — the no-behaviour-change guarantee.

---

## 11. Risks & open decisions

**Risks**

- **Concurrency correctness (P2) is the real risk.** `SessionState` is single-writer today; the
  mutex + index-stamp-under-lock + post-`wg.Wait` checkpoint must be exactly right or the durable
  checkpoint corrupts. `-race` in CI is mandatory. Keeping `maxConcurrentTurns: 1` the default means
  the proven serial path ships unchanged and concurrency is opt-in.
- **Lost FIFO under concurrency** is inherent, not fixable — must be documented loudly (§5.3). A
  tenant that silently sets `maxConcurrentTurns: 8` and depends on ordering will get subtle bugs.
- **Gateway → apiserver coupling (P4)** turns the stateless gateway into a (lightly) apiserver-
  dependent service. The TTL cache bounds load, but a cache-miss storm on a cold gateway during a
  burst could add latency. Mitigate: cache-miss falls back to the global default (never blocks).
- **Cluster-wide retention max** is a coarse, slightly leaky model (§8). Acceptable for v1; flagged
  for per-session-stream escalation.
- **Status staleness (option 2)** — `status.usage` lags by up to one requeue (~30 s). Acceptable for
  observability; not a control signal.

**Open decisions (maintainer must choose)**

1. **`Resources` type — pure `ResourceRequirements` vs import `corev1` into the pure package**
   (§3.1). Recommendation: pure type + translation helper (keeps the in-pod binary's deps clean).
2. **Retention owner — gateway vs controller** calls `UpdateRetention` (§6.4/§6.6). Recommendation:
   gateway (it already lists sessions for the body-cap lookup, already holds a `Queue`; the
   controller stays NATS-client-free).
3. **Worker→status link — controller reads checkpoint (option 2) vs worker patches status
   (option 1)** (§4). Recommendation: option 2 (minimal worker blast radius).
4. **Retention granularity — shared stream + cluster-wide max (a) vs per-session streams (b)**
   (§3.3). Recommendation: (a) now, (b) only on a per-session retention SLA.
5. **Per-namespace quota — ship in P5 or defer to [`run-governance.md`](./run-governance.md)?** It
   shares machinery; deciding where it lives avoids duplication.
6. **Gateway singleton vs per-session Knative Service** (design doc §6). This spec assumes the
   **singleton** (Option A) — NATS is already the per-session fan-out buffer, so the stateless
   gateway needs no session affinity, and per-session Knative Services (revision + SKS + autoscaler
   accounting *per session*) defeat the gateway's whole point. Per-session isolation, if ever needed,
   is better bought with the per-namespace quota than by sharding the gateway. **Recommendation:
   keep the singleton; do not build per-session gateways.**

---

### See also

- [`docs/design/agent-session-scaling.md`](../design/agent-session-scaling.md) — the architecture this implements.
- [`docs/design/durable-session-architecture.md`](../design/durable-session-architecture.md) — the durable core being scaled.
- [`docs/design/agent-platform.md`](../design/agent-platform.md) — platform overview.
- [`docs/research/agent-runtime-fit-analysis-v0.2.0.md`](../research/agent-runtime-fit-analysis-v0.2.0.md) — the gap this closes.
- [`docs/specs/response-richness.md`](./response-richness.md) · [`docs/specs/run-governance.md`](./run-governance.md) · [`docs/specs/agentpolicy-enforcement.md`](./agentpolicy-enforcement.md) · [`docs/specs/agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md) — adjacent specs (orthogonal; cross-linked, not required).
