# AgentSession Scaling & Turn-Queue Spec

> Scope: the concurrency / throughput / turn-handling surface for `AgentSession`. Specifies the new spec + status fields and the exact plumbing point each one drives (a serve-session flag, an env var, or a NATS `StreamConfig` update). Closes the P1 "AgentSessionSpec is skeletal" gap from [`docs/research/agent-runtime-fit-analysis-v0.2.0.md`](../research/agent-runtime-fit-analysis-v0.2.0.md). The durable-execution core it scales is specified in [`docs/design/durable-session-architecture.md`](./durable-session-architecture.md).

> **Status: DESIGN — proposed, not yet implemented.** Every field in the [Proposed spec fields](#3-proposed-spec-fields) and [Proposed status fields](#4-proposed-status-fields) tables is a *design proposal*. None of `maxConcurrentTurns`, `turnBatchSize`, `turnPollIntervalMs`, `turnDeliveryTimeoutSeconds`, `turnRetentionSeconds`, `maxTurnInputBytes`, `turnHistoryLimit`, `resources`, `usage`, `turns`, `failedTurns`, or `lastTurnTime` exists in the tree as of v0.2.0 (`grep` for `MaxConcurrentTurns`/`turnBatchSize`/`activeDeadlineSeconds` across `pkg/ operator/ cmd/ deploy/` returns nothing). The "today" values cited below are the live, **hard-coded** constants this spec proposes to lift into the CRD.

---

## 1. Problem statement

`AgentSessionSpec` is two fields. From `pkg/agentmodel/v1/types.go:282`:

```go
type AgentSessionSpec struct {
	AgentRef string `json:"agentRef"`

	// IdleTimeoutSeconds parks then exits the session worker after this idle
	// period so it can scale to zero; 0 (default) keeps it resident.
	// +optional
	IdleTimeoutSeconds int32 `json:"idleTimeoutSeconds,omitempty"`
}
```

`AgentRef` and `IdleTimeoutSeconds` are the *only* operator-facing knobs. Everything that governs how fast a session drains its turn queue, how many turns it runs at once, how big a turn may be, how long results and history live, and what resources the worker gets — is a constant compiled into a binary or baked into a deploy manifest. A tenant cannot tune any of it per session; an operator cannot tune it without rebuilding an image.

### What is hard-coded today (and where)

| Knob | Hard-coded value | Location |
|------|------------------|----------|
| Turn batch (max turns pulled per poll) | `Max: 16` on the `QueueSource` | `cmd/agent/serve_session.go:83` |
| Turn batch fallback (NATS `Consume`) | `if max <= 0 { max = 16 }` | `pkg/sessionqueue/nats.go:95-97` |
| Worker poll interval | `--poll` default `2*time.Second` | `cmd/agent/serve_session.go:33`; consumed at `pkg/agentruntime/session_worker.go:71-76` |
| NATS fetch wait per poll | `nats.MaxWait(500*time.Millisecond)` | `pkg/sessionqueue/nats.go:98` |
| Stream retention | `MaxAge: 24*time.Hour`, `Retention: nats.LimitsPolicy` | `pkg/sessionqueue/nats.go:46-52` |
| Max turn request body | `maxTurnBytes = 1 << 20` (1 MiB) | `cmd/agentgateway/main.go:30,49` |
| Synchronous wait cap | `--max-wait` default `60*time.Second` | `cmd/agentgateway/main.go:35,122`; clamped at `main.go:103-109` |
| Gateway autoscaling | `min-scale 0`, `max-scale 20`, `target 50`, `containerConcurrency 50` | `deploy/agentgateway/knative-service.yaml:22-26` |
| Worker concurrency | **1** — `processTurns` is a sequential `for` loop | `pkg/agentruntime/session_worker.go:178-185` |
| Worker pod resources (harness) | limits `1 CPU / 1Gi`, requests `100m / 256Mi` | `operator/internal/builders/agentrun.go:102-110` |
| Worker pod resources (loop) | limits `500m / 512Mi`, requests `100m / 128Mi` | `operator/internal/builders/agentrun.go:128-136` |
| Turn-history retention (in `SessionState`) | none — `state.Turns` grows unbounded | `pkg/agentruntime/session.go:34-53` (no compaction) |

Two structural facts shape the rest of this doc:

1. **The worker is single-threaded per session.** `SessionWorker.processTurns` (`session_worker.go:172-186`) pulls a batch and runs each turn one at a time via `handleTurn` → `runTurn`. The batch size (`Max: 16`) only controls how many are *fetched* per poll, not how many run concurrently; they still execute serially. There is no semaphore, no goroutine fan-out.
2. **`activeDeadlineSeconds` is never set.** The worker pod is a 1-replica `Deployment` (`agentsession_controller.go:224-238`) built from `BuildAgentRunPod` with `RestartPolicy: Always` patched on (`agentsession_controller.go:150`). Nothing bounds the *total* pod wall-clock — only the per-run `Budget.MaxWallClockSeconds` bounds an individual turn, and even that lives inside the executor, not on the pod. A wedged turn relies on `ctx` cancellation, not a Kubernetes deadline.

The worker pod inherits the run-pod sandbox: kata-fc `RuntimeClass` via `ApplyRunSandbox` and the egress `NetworkPolicy` via `BuildAgentSessionEgressPolicy` (`agentsession_controller.go:124-133`). There is **no per-tenant concurrency or quota** anywhere — a namespace can create unbounded `AgentSession` objects, each a resident pod.

> Out of scope (cross-link only): `AgentPolicy` budget caps and `AgentNetwork` egress are **not** enforced on the session datapath; see [`docs/design/agentnetwork-agentpolicy-interaction.md`](./agentnetwork-agentpolicy-interaction.md). Steps *are* folded for runs but not yet for session turns — tracked in [`docs/design/durable-session-architecture.md`](./durable-session-architecture.md).

---

## 2. Design goals

- **Lift the constants into the CRD** so a session author tunes throughput/latency/cost declaratively, and the operator plumbs each value to exactly one place.
- **Keep the durable core transport-agnostic.** `SessionWorker` already abstracts delivery behind `TurnSource`/`ResultSink` (`session_worker.go:38-39`); new knobs configure the *worker* and the *stream*, never the core's correctness contract.
- **Fail early, not at 15 s.** Reject a bad `agentRef` (or an out-of-range knob) at admission instead of looping in `Pending` (`agentsession_controller.go:76-78`).
- **No behavioural change at defaults.** Every default in §3 equals today's hard-coded value, so an un-annotated `AgentSession` behaves bit-for-bit as it does now.

---

## 3. Proposed spec fields

All added to `AgentSessionSpec` (`pkg/agentmodel/v1/types.go:282`). Defaults equal the current hard-coded value, so omitting a field is a no-op.

| Field | Go type | Default | Plumbed to |
|-------|---------|---------|------------|
| `maxConcurrentTurns` | `int32` | `1` | New `SessionWorker` semaphore (see §5). Passed as `--max-concurrent-turns=<n>` to `agent serve-session`; the controller appends it to `cmd` at `agentsession_controller.go:137-141`. `1` preserves today's serial loop. |
| `turnBatchSize` | `int32` | `16` | The `QueueSource.Max` field set at `serve_session.go:83`. Passed as `--turn-batch=<n>`; replaces the literal `16`. Also flows into `NATSQueue.Consume`'s `max` arg (`nats.go:90`), so the `max <= 0 → 16` fallback (`nats.go:95-97`) is only hit when unset. |
| `turnPollIntervalMs` | `int32` | `2000` | `SessionWorker.PollInterval` (`session_worker.go:30,71-76`). Passed as `--poll=<dur>` at `serve_session.go:33`; the controller renders `<n>ms`. Lower = snappier pickup, more idle NATS fetches. |
| `turnDeliveryTimeoutSeconds` | `int32` | `300` | A per-turn `context.WithTimeout` wrapping `runTurn` (new; see §5). Passed as `--turn-timeout=<dur>`. Caps a single turn's wall-clock independent of `Budget.MaxWallClockSeconds`; the effective per-turn deadline is `min(turnDeliveryTimeoutSeconds, Budget.MaxWallClockSeconds)`. |
| `turnRetentionSeconds` | `int32` | `86400` | The NATS `StreamConfig.MaxAge` at `nats.go:46-52` (today `24*time.Hour` = 86400 s). **This is a stream-level setting, not a per-worker flag** — see §3.1. |
| `maxTurnInputBytes` | `int32` | `1048576` | The gateway's `LimitReader` cap, today `maxTurnBytes = 1 << 20` at `main.go:30,49`. Enforced at the front door so an oversized turn is rejected with `400` before it ever reaches the stream. Plumbed as a per-session lookup (see §3.1). |
| `turnHistoryLimit` | `int32` | `10000` | New compaction bound in `SessionState.Append` (`session.go:45-53`): when `len(s.Turns)` exceeds the limit, the oldest turns are dropped (their `Usage` already folded into `CumulativeUsage`, so totals are preserved). Passed as `--turn-history-limit=<n>`. Caps the unbounded JSON growth flagged in §1. |
| `resources` | `*corev1.ResourceRequirements` | `nil` (use the built-in defaults at `agentrun.go:102-110` / `128-136`) | Overrides the worker container's `Resources`. The controller applies it to `pod.Spec.Containers[0].Resources` after `BuildAgentRunPod` (`agentsession_controller.go:132`), before wrapping in the `Deployment`. |

Validation bounds (CRD markers): `maxConcurrentTurns` ∈ [1, 64]; `turnBatchSize` ∈ [1, 256]; `turnPollIntervalMs` ∈ [100, 60000]; `turnDeliveryTimeoutSeconds` ∈ [1, 86400]; `turnRetentionSeconds` ∈ [60, 2592000]; `maxTurnInputBytes` ∈ [1024, 10485760]; `turnHistoryLimit` ∈ [10, 1000000].

### 3.1 The two knobs that are *not* worker flags

Most fields become a `serve-session` flag. Two cross a process boundary and need explicit wiring:

- **`turnRetentionSeconds` → NATS stream.** The stream is created once, lazily, in `NewNATSQueue` via `js.AddStream` (`nats.go:46-52`), and `AddStream` returns `ErrStreamNameAlreadyInUse` for an existing stream — it does **not** reconcile `MaxAge`. There is also exactly one shared stream (`defaultStream = "AGENT_SESSIONS"`, `nats.go:16`) for *all* sessions, so a single `MaxAge` cannot express a per-session retention. Two viable mechanisms:
  - **(a) Per-session subject + `UpdateStream`.** Keep the shared stream but have the operator (which already holds `NATSURL`, `agentsession_controller.go:47`) call `js.UpdateStream` with the **max** `turnRetentionSeconds` across live sessions. Coarse (cluster-wide max), but a one-call change.
  - **(b) Per-session stream.** Give each session its own stream `AGENT_SESSIONS_<key>` with its own `MaxAge`. Precise, but multiplies JetStream stream count by the number of sessions. **Recommended default: (a)**, escalate to (b) only if per-session retention SLAs are required. Either way `NewNATSQueue`/`sessionqueue` grows a `MaxAge time.Duration` parameter (today the value is the package constant at `nats.go:51`).
- **`maxTurnInputBytes` → gateway.** The gateway is stateless and shared (`cmd/agentgateway/main.go`), so it cannot bake a per-session limit at start. The cap at `main.go:49` (`io.LimitReader(r.Body, maxTurnBytes)`) becomes a per-request lookup: the gateway reads the `AgentSession` (by `{ns}/{name}` from the path, `main.go:48`) and uses its `maxTurnInputBytes`, falling back to the global `1<<20` default. This adds a `GET AgentSession` to the hot path; cache it with a short TTL keyed by session key. The worker also re-validates on decode as defence in depth (it already drops malformed turns at `session_queue_source.go:28-33`).

---

## 4. Proposed status fields

Added to `AgentSessionStatus` (`pkg/agentmodel/v1/types.go:291`). These surface the durable `SessionState` (`pkg/agentruntime/session.go:34`) — which the controller does not read today — up to `kubectl`.

| Field | Go type | Source |
|-------|---------|--------|
| `usage` | `v1.Usage` | Mirror of `SessionState.CumulativeUsage` (`session.go:38`), aggregated across all turns by `Append` (`session.go:48-51`). Carries `Steps`/`Tokens`/`ToolCalls`/`WallClockUsed`. |
| `turns` | `int32` | `len(SessionState.Turns)` (post-compaction count is fine for "turns completed" if reported as a monotonic counter rather than slice length — prefer a dedicated counter once `turnHistoryLimit` drops old entries). |
| `failedTurns` | `int32` | Count of turns where `SessionTurn.Phase == PhaseFailed` or `SessionTurn.Error != ""` (`session.go:21,24`; set in `handleTurn` at `session_worker.go:201-206`). |
| `lastTurnTime` | `*metav1.Time` | `SessionState.UpdatedAt` (`session.go:40`, advanced by `Append` at `session.go:52`). |

### Deprecate `runs []string`

```go
// types.go:301
// Runs are the names of AgentRuns this session aggregated (legacy field).
// +optional
Runs []string `json:"runs,omitempty"`
```

`runs` is dead: no reconciler writes it (the `AgentSessionReconciler` only ever sets `Phase` and `ObservedGeneration` via `writeStatus`, `agentsession_controller.go:210-219`), and a session does not aggregate `AgentRun` objects — turns are folded into `SessionState`, not into child `AgentRun` CRs. Mark it `// Deprecated: never populated; replaced by status.turns/usage.` and remove after one minor release. The replacement is `turns` (count) + `usage` (aggregate), which actually carry the information `runs` was meant to imply.

### Plumbing the worker→status link

Status today is operator-derived (Deployment availability → `Phase`, `agentsession_controller.go:157-168`); the worker's `SessionState` lives on the AgentFS volume and never reaches the controller. Two options to close that, both consistent with the existing architecture:

1. **Worker patches status directly.** Grant the worker SA `patch` on `agentsession/status` and have it `PATCH` `usage`/`turns`/`failedTurns`/`lastTurnTime` after each checkpoint (`session_worker.go:131`). Lowest latency; widens the worker's RBAC (it currently has none for AgentSession).
2. **Sidecar/controller reads the checkpoint.** The controller (or the AgentFS sidecar) reads `state.json` (`DefaultSessionStatePath`, `session.go:62`) and folds it into status on its existing requeue. No new RBAC on the worker, but eventually-consistent and adds I/O coupling.

**Recommended:** option 2 for now — it keeps the worker's blast radius minimal (matching the "long-running session pod + AgentFS checkpoint" architecture) and reuses the controller's requeue loop. Revisit option 1 if status staleness matters.

---

## 5. Concurrency model

### Semaphore in `processTurns`

`processTurns` (`session_worker.go:172-186`) runs the batch serially:

```go
for _, t := range turns {
	if ctx.Err() != nil { return handled, ctx.Err() }
	w.handleTurn(ctx, state, t)   // synchronous, one at a time
	handled++
}
```

When `maxConcurrentTurns > 1`, fan out under a buffered-channel semaphore of that width, running `handleTurn` in goroutines and joining before return:

```go
sem := make(chan struct{}, w.MaxConcurrentTurns) // 1 ⇒ today's serial behaviour
var wg sync.WaitGroup
for _, t := range turns {
	if ctx.Err() != nil { break }
	sem <- struct{}{}
	wg.Add(1)
	go func(t InboundTurn) {
		defer wg.Done(); defer func() { <-sem }()
		w.handleTurn(turnCtx(ctx, w), state, t)
	}(t)
}
wg.Wait()
```

**Critical constraint:** `handleTurn` mutates shared `*SessionState` (`state.Append`, `session.go:45`; `state.Phase`, `session_worker.go:189`) and the surrounding `Run` loop checkpoints `state` after each `processTurns` (`session_worker.go:131`). Concurrent `handleTurn` calls therefore need a mutex around `state.Append` / `state.Phase` writes (and the `st.Index = len(state.Turns)` stamp at `session_worker.go:209` must be inside the lock, since two turns could otherwise claim the same index). This is the load-bearing change — `SessionState` is currently single-writer by construction, and `maxConcurrentTurns > 1` breaks that assumption. Keep `maxConcurrentTurns: 1` as the default precisely so the proven serial path is the out-of-the-box behaviour.

> Ordering caveat: concurrent turns lose FIFO. The on-disk inbox relies on lexical filename order for FIFO (`session_worker.go:239`) and NATS gives per-consumer order; running a batch concurrently means turn *completion* order is non-deterministic. Sessions that need strict turn ordering must keep `maxConcurrentTurns: 1`. Document this as a hard trade-off, not a bug.

### Per-turn timeout

Derive a per-turn `ctx` from `Agent.Budget.MaxWallClockSeconds` (`pkg/agentmodel/v1/budget.go:27`) and the new `turnDeliveryTimeoutSeconds`:

```go
func turnCtx(parent context.Context, w *SessionWorker) (context.Context, context.CancelFunc) {
	d := time.Duration(w.Agent.Spec.Budget.MaxWallClockSeconds) * time.Second
	if w.TurnTimeout > 0 && (d == 0 || w.TurnTimeout < d) {
		d = w.TurnTimeout
	}
	return context.WithTimeout(parent, d)
}
```

Today there is **no** per-turn deadline at the worker layer: `runTurn` (`session_worker.go:96-101`) passes the parent `ctx` straight through, and only the executor's internal budget check (`Budget.AllowsStep`, `budget.go:67-81`) bounds wall-clock — which it evaluates *before each step*, so a single hung step (e.g. a stuck harness HTTP call) is not interrupted. A `context.WithTimeout` per turn makes the cancellation real at the worker boundary.

### Interplay with `activeDeadlineSeconds`

These cover different scopes; both should exist:

| Mechanism | Scope | Set where |
|-----------|-------|-----------|
| `Budget.MaxWallClockSeconds` | one turn, evaluated per-step inside the executor | Agent spec (`budget.go:27`) |
| `turnDeliveryTimeoutSeconds` (proposed) | one turn, hard `ctx` cancel at the worker | `AgentSessionSpec` → `--turn-timeout` |
| `activeDeadlineSeconds` | the **pod's** total lifetime | **Not set today** (`agentsession_controller.go` / `agentrun.go`) |

A session worker is meant to be long-lived (it parks in `RequiresAction` when idle, `session_worker.go:138-146`, and scales to zero via `IdleTimeoutSeconds`). So a pod-level `activeDeadlineSeconds` is generally **wrong** for a resident worker — it would kill a healthy idle session. Leave it unset for sessions; rely on `IdleTimeoutSeconds` for lifecycle and `turnDeliveryTimeoutSeconds` for per-turn bounding. (One-shot `AgentRun` pods are a separate datapath where a pod deadline *would* make sense — out of scope here.)

---

## 6. Gateway scaling

The gateway is stateless and already a Knative Service (`cmd/agentgateway/main.go:1-11`, `deploy/agentgateway/knative-service.yaml`). The autoscaling knobs are:

| Annotation / field | Today | Meaning |
|--------------------|-------|---------|
| `autoscaling.knative.dev/min-scale` | `0` | scale to zero when idle (`knative-service.yaml:22`) |
| `autoscaling.knative.dev/max-scale` | `20` | upper replica bound (`knative-service.yaml:23`) |
| `autoscaling.knative.dev/target` | `50` | target in-flight requests per pod (`knative-service.yaml:24`) |
| `containerConcurrency` | `50` | hard per-pod concurrency ceiling (`knative-service.yaml:26`) |

There is **one** gateway for the whole cluster. Two ways to scale it; both keep the knobs above.

### Option A — cluster-wide singleton gateway (recommended)

Keep the single shared gateway and make the session→gateway relationship explicit by adding a **required `GatewayRef`** to the cluster wiring (today there is no such field — `grep GatewayRef` is empty). The operator already knows the NATS URL (`agentsession_controller.go:47`); a `GatewayRef` (or a well-known gateway Service name) lets admission verify the gateway exists before admitting sessions that depend on it, and lets the operator surface "gateway unreachable" instead of silent `202`s piling up in NATS.

- **Pros:** one autoscaling pool absorbs bursts across all sessions; scale-to-zero amortised; NATS is already the per-session fan-out buffer, so the gateway needs no session affinity.
- **Cons:** one blast radius; `max-scale: 20 × containerConcurrency: 50 = 1000` concurrent in-flight turns cluster-wide — a noisy tenant can starve others (mitigate with the per-tenant quota in §7, not by sharding the gateway).
- **Tuning:** raise `max-scale` for higher cluster-wide turn ingest; raise `containerConcurrency`/`target` together to pack more turns per pod (cheaper, higher tail latency); lower both to shed load earlier.

### Option B — per-session Knative Service

Give each `AgentSession` its own gateway Service (named off the session) so its `min/max-scale` and `containerConcurrency` track that session alone.

- **Pros:** strict per-session isolation and independent autoscaling; a hot session can't crowd a cold one.
- **Cons:** a Knative Service (+ revision, +SKS, +autoscaler accounting) per session is heavy; loses the shared scale-to-zero amortisation; the gateway's whole point is statelessness, so per-session instances mostly duplicate cost. Reserve for tenants with hard isolation SLAs.

**Recommendation:** Option A plus a `GatewayRef` existence check, with per-tenant fairness handled by quota (§7). Option B is an escalation path, not the default.

---

## 7. Admission validation

Today a missing `agentRef` is discovered *at reconcile*: `r.Get` for the Agent returns `NotFound` and the controller writes `Pending` with a 15 s requeue (`agentsession_controller.go:75-78`). The session sits `Pending` indefinitely with no clear signal to the author.

Proposed validating admission webhook (or CEL on the CRD) for `AgentSession`:

1. **`spec.agentRef` exists in-namespace.** Reject `create`/`update` with a clear message (`agentRef "foo" not found in namespace "bar"`) instead of an indefinite `Pending` loop. This is the highest-value check — it turns a silent 15 s-requeue limbo into an immediate `kubectl apply` error.
2. **Knob ranges.** Enforce the §3 bounds (CRD `+kubebuilder:validation` markers cover most; a webhook covers cross-field rules like `turnDeliveryTimeoutSeconds ≤ turnRetentionSeconds`).
3. **Gateway reachability (Option A).** If `GatewayRef` is required, verify the referenced gateway Service exists at admission.

Per-namespace concurrency/quota (absent today) is enforced here too: cap live `AgentSession` count or aggregate `maxConcurrentTurns` per namespace via a `ResourceQuota`-style admission rule, so one tenant cannot pin unbounded resident worker pods.

---

## 8. Tuning examples

Defaults (un-annotated session) ≈ the 1-agent row. Numbers are illustrative shapes, not benchmarks.

| Deployment | `maxConcurrentTurns` | `turnBatchSize` | `turnPollIntervalMs` | Gateway `max-scale` / `containerConcurrency` | Worker `resources` | Profile |
|-----------|----------------------|-----------------|----------------------|----------------------------------------------|--------------------|---------|
| **1 agent** (dev / single user) | `1` | `16` | `2000` | `5` / `50` | default (`1 CPU / 1Gi` limit) | Lowest cost; serial turns; FIFO preserved; ~2 s pickup latency. |
| **10 agents** (team) | `4` | `32` | `1000` | `20` / `50` | `2 CPU / 2Gi` limit | Balanced; concurrent turns per session need the §5 `state` mutex; faster pickup. |
| **100 agents** (multi-tenant) | `8` | `64` | `500` | `20` / `100` | `4 CPU / 4Gi` limit, per-tenant quota | Throughput-first; gateway packs more per pod (higher tail latency, lower cost/turn); per-namespace quota (§7) is mandatory to bound resident worker pods. |

Reading the dials:

- **Latency** ← `turnPollIntervalMs` (worker pickup) + `containerConcurrency`/`target` (queueing at the gateway). Lower poll = snappier but more idle NATS fetches (`nats.go:98` is a 500 ms blocking fetch per poll).
- **Throughput** ← `maxConcurrentTurns` × worker count + gateway `max-scale`. Bounded ultimately by `turnBatchSize` per poll and the LLM/harness backend.
- **Cost** ← worker `resources` × resident sessions (the dominant term — each session is a kata-fc microVM pod) + gateway replicas. `IdleTimeoutSeconds` (existing) and gateway `min-scale: 0` recover cost when idle.

---

## 9. Summary of changes

- **`pkg/agentmodel/v1/types.go`** — 8 new `AgentSessionSpec` fields (§3); 4 new `AgentSessionStatus` fields + deprecate `runs` (§4).
- **`cmd/agent/serve_session.go`** — new flags (`--max-concurrent-turns`, `--turn-batch`, `--turn-timeout`, `--turn-history-limit`), replacing the literal `Max: 16` (`:83`) and threading the rest into `SessionWorker`.
- **`pkg/agentruntime/session_worker.go`** — `MaxConcurrentTurns`/`TurnTimeout` fields; semaphore + `state` mutex in `processTurns` (§5); per-turn `context.WithTimeout`.
- **`pkg/agentruntime/session.go`** — `turnHistoryLimit` compaction in `Append`.
- **`pkg/sessionqueue/nats.go`** — `MaxAge` parameter (replace constant at `:51`) + `UpdateStream` for `turnRetentionSeconds` (§3.1).
- **`cmd/agentgateway/main.go`** — per-session `maxTurnInputBytes` lookup (replace `maxTurnBytes` at `:30,49`).
- **`operator/internal/controllers/agentmodel/agentsession_controller.go`** — render new flags onto `cmd` (`:137-141`); apply `spec.resources` to the worker container (`:132`); fold `SessionState` into status (§4); optional `GatewayRef` check.
- **Admission webhook / CEL** — `agentRef` existence + knob bounds + per-namespace quota (§7).

See also: [`docs/design/durable-session-architecture.md`](./durable-session-architecture.md) (the durable-execution core), [`docs/design/agent-platform.md`](./agent-platform.md), and [`docs/research/agent-runtime-fit-analysis-v0.2.0.md`](../research/agent-runtime-fit-analysis-v0.2.0.md) (the gap this closes).
