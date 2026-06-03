# Spec: Human-in-the-loop approval gates — wire `RequiresAction`

> **Status: DESIGN / NOT BUILT (v0.2.0).** As of HEAD (2026-06-03) `Phase=RequiresAction` is a first-class lifecycle state in the enum (`pkg/agentmodel/v1/lifecycle.go:14`), the transition table (`lifecycle.go:72-81`), the Quint model (`spec/quint/agent_execution.qnt:13,207`), and the CRD `state` enum (`operator/config/crd/runtime.agents.smol-agents.ai_agentruns.yaml:108`) — **but it is emitted by no controller on the AgentRun datapath and consumed by no human-decision loop.** The step-wise resume contract (`StepRequest.History`, `pkg/agentmodel/runtime/contract.go:53-60`) exists but is used only in tests. This spec turns `RequiresAction` into a real approval valve in two increments: **(A)** a cheap harness-mode **pre-run** gate (block the pod until a human approves), and **(B)** the hard loop-mode **mid-run** gate (pause before a high-blast-radius tool/agent call, resume in a continuation pod). Every "exists today" claim cites `file:line`; every proposed change is marked **(proposed)**.

> Builds on (read first, do not duplicate): [`docs/design/framework-enhancements.md`](../design/framework-enhancements.md) **§O3** (the original proposal — this spec deepens it and corrects the load-bearing "executor already replays prior Steps" error). The persisted-Steps prerequisite is [`docs/specs/response-richness.md`](./response-richness.md) (future). The redaction prerequisite is the redaction half of [`docs/specs/agentpolicy-enforcement.md`](./agentpolicy-enforcement.md).

---

## 1. Summary

**Human-in-the-loop (HITL)** means an AgentRun can *pause* and require a human (or an external orchestrator) to make a decision before it proceeds — and then *resume* from where it paused. This is the operator-side realization of the dormant `Phase=RequiresAction` state. We ship it in two clearly-separated forms:

- **(A) Harness pre-run gate — cheap, self-contained.** An Agent (or run) declares `requireApprovalBeforeRun: true`. The controller holds the run in `RequiresAction` (no pod) until a human patches `spec.decision`. On `approve` it proceeds to the normal pod-create path; on deny it marks `Cancelled`. This is the *only* HITL form that works for `Mode=harness`, because the harness owns its plan-act-observe loop opaquely (`executor.go:377` `runHarness` makes exactly one bounded call) — we can gate *whether* it runs, never *what it does mid-loop*.

- **(B) Loop mid-run gate — hard, net-new resume.** A `Mode=loop` Agent declares which tool kinds/names require approval. When the executor is about to invoke a matching tool, it emits `StepKind=AwaitingApproval`, returns `Phase=RequiresAction` with `RunResult.PendingAction{Tool, Arguments}`, and exits the pod cleanly. The controller folds this into `Status` and stops. A human patches `spec.decision`. The controller spawns a **continuation pod** seeded with the prior Steps + the approval; a net-new executor entry point replays history, applies the decision, and continues the loop.

The outcome: a tenant can require sign-off before an agent runs at all (form A — usable today, low risk) or before specific dangerous actions inside a loop agent (form B — gated behind real resume plumbing). Form A is **M**; form B is **XL** and depends on persisted Steps ([response-richness](./response-richness.md), future) plus net-new executor resume, Quint actions, and a `RequiresAction` TTL. **Ship form A first.**

---

## 2. Current state

### 2.1 What exists (the dormant scaffolding)

| Thing | Where | State |
|---|---|---|
| `PhaseRequiresAction` constant | `pkg/agentmodel/v1/lifecycle.go:14` | Declared; **not terminal** (`Terminal()` excludes it, `lifecycle.go:22-28`) |
| Transition edges into/out of `RequiresAction` | `lifecycle.go:72-81` | `Running→RequiresAction`, `RequiresAction→{Running,Cancelled,Expired,Failed}` — **already legal**; no code drives them on the run datapath |
| Quint phase + invariant | `spec/quint/agent_execution.qnt:13,202,207` | `RequiresAction` is a `Phase` value and `LifecycleConsistent` admits it, but **no Quint action transitions into it** (the proposal's gap; verified — `step` at `:171` never reaches it) |
| CRD `state` enum | `operator/config/crd/...agentruns.yaml:106,108` | Lists `RequiresAction` with description "awaiting external input" |
| Step-wise resume contract | `pkg/agentmodel/runtime/contract.go:53-60` | `StepRequest{Run, AgentSpec, History []v1.Step, Now, BudgetLeft, Cancel}` + `StepResponse` — a complete controller-drives-each-step protocol, **used only in tests** |
| `StepKind` taxonomy | `pkg/agentmodel/v1/types.go:296-301` | 6 kinds (`Plan`/`ToolCall`/`ToolCallRejected`/`Observation`/`ObservationRejected`/`Final`); **no `AwaitingApproval`** |
| `Step` carries tool args | `types.go:280-311` (`Step.ToolCalls[].Arguments json.RawMessage`) | The mechanism to record a pending call's arguments already exists |

### 2.2 What is stubbed / missing — the gap this spec closes

| Gap | Evidence (`file:line`) |
|---|---|
| **Nothing emits `RequiresAction`** on the AgentRun datapath | `agentrun_controller.go` maps pod phase → run phase at `:233-244` with cases for `PodPending`/`PodRunning`/`PodSucceeded`/`PodFailed` only; `markRunning`/`markPending`/`markTerminal` never set `RequiresAction` |
| **No `Decision` field** on `AgentRunSpec` | `AgentRunSpec` (`types.go:219-256`) has `Cancel bool` but no approval-decision input |
| **No `PendingAction`** on the wire or in Status | `RunResult` (`runonce.go:27-38`) has no pending-action field; `RunStatus` (`types.go:313-322`) has no `PendingAction` |
| **No `ApprovalPolicy`** on the Agent | `AgentSpec` has no approval knobs (grep: zero `RequireApproval`/`ApprovalPolicy` symbols repo-wide) |
| **Executor cannot resume** | `Executor.Run` signature is `Run(ctx, agent, input, seed)` (`executor.go:79`) — **no prior-Steps parameter**; line 95 hard-inits `steps := []v1.Step{}` empty. Loop resume is net-new (corrects O3's "executor already replays prior Steps") |
| **`RequiresAction` has no TTL** | nothing expires a paused run; the legal `RequiresAction→Expired` edge (`lifecycle.go:80`) is never driven → a paused run hangs forever |
| **`PendingAction.Arguments` redaction** | `foldRunResult` copies `rr.Output`/`rr.Steps` verbatim (`agentrun_controller.go:403-404`); no redaction (same gap as [agentpolicy-enforcement](./agentpolicy-enforcement.md)) |

> **CAUTION — `RequiresAction` is already overloaded.** The durable **session worker** sets `state.Phase = v1.PhaseRequiresAction` to mean *"idle, parked, awaiting the next turn"* (`pkg/agentruntime/session_worker.go:139-141`), and `AgentSessionStatus.Phase` mirrors that. That is a *different semantics* (idle, not blocked-on-a-human) on a *different object* (the session checkpoint, not `AgentRun.Status.State`). **This spec only touches `AgentRun.Status.State`.** Do not conflate them; do not let an AgentRun HITL gate reuse the session-idle meaning. See §10 open decision 6.

---

## 3. External interface research

**N/A — internal-only.** HITL is a first-party control over our own CRDs and runtime. The `spec.decision` shape is modeled loosely on OpenAI's `submit_tool_outputs` request (the Assistants `requires_action` → human-supplies-output pattern) for vocabulary familiarity only — there is no external wire we must track. (Section retained for template parity per the canonical spec structure.)

---

## 4. Design

### 4.1 Two gates, one decision channel

Both forms share **one input channel** — `AgentRunSpec.Decision` — and **one output channel** — `RunStatus.PendingAction` + `Status.State=RequiresAction`. They differ only in *where the pause happens*:

```
Form A (harness pre-run gate) — controller-side, NO pod runs while paused:

  AgentRun created
        │
        ▼
  requireApprovalBeforeRun?  ──no──►  normal pod-create path (unchanged)
        │ yes
        ▼
  Status.State = RequiresAction
  Status.PendingAction = {Reason: "pre-run approval", ...}     ◄── no pod, no cost
        │
        ▼  human patches spec.decision
   approve? ──no──► markTerminal(Cancelled, "decision:denied")
        │ yes
        ▼
  proceed to pod-create (clear PendingAction, markRunning)


Form B (loop mid-run gate) — pod runs, pauses, exits; continuation pod resumes:

  run pod (loop) plans → about to invoke a gated tool
        │
        ▼
  emit Step{Kind: AwaitingApproval}; Phase = RequiresAction
  RunResult.PendingAction = {Tool, Arguments, StepIndex}
  pod exits 0  ──fold──►  Status.State = RequiresAction + PendingAction + Steps
        │
        ▼  human patches spec.decision{Approve, Reason}
   approve? ──no──► CONTINUATION pod runs, executor records the denial as an
        │ yes          Observation{error:"denied by <user>"} and continues/ends
        ▼
  CONTINUATION pod runs: Executor.Resume(prior Steps + decision + carried Usage)
        │                replays history, applies decision, continues the loop
        ▼
  Completed | RequiresAction-again | Expired | Failed
```

### 4.2 The hard part of form B: stateful resume across a pod boundary

The run pod is `RestartPolicy:Never` and stateless; a paused loop has **nowhere in-pod to keep history**. Resume therefore requires three things that do not exist today, in dependency order:

1. **Persisted Steps on the cluster.** The continuation pod must be re-seeded with the prior `[]v1.Step`. Steps are folded into `Status.Steps` *today* (`agentrun_controller.go:404`) — but **bounded by the ~4 KiB termination-message cap**, which `clampForTerminationMessage` (`cmd/agent/run.go:102-115`) elides under pressure (drops tool-call arg/result bodies, then the whole trace). A resume that re-seeds *elided* history would lose the arguments the loop needs. → **Hard dependency on [response-richness](./response-richness.md) (future)**: a size-budgeted overflow store (Steps to AgentFS/S3, referenced from Status) so the full prior trace survives the pause. Until that lands, form B is only safe for runs whose trace fits 3 KiB.

2. **A net-new executor entry point.** `Executor.Run` cannot accept prior Steps. **(proposed)** add `Executor.Resume(ctx, agent, input, seed, prior []v1.Step, decision *v1.Decision, carried v1.Usage) (Result, error)` that seeds `steps := prior`, `usage := carried`, and — critically — **does not re-invoke the gated tool blindly**: it consumes `decision` for the *first* pending action, then re-enters the loop.

3. **Carrying `Usage` across the pause.** This is the **highest correctness risk** and the reason form B is XL. The Quint `BudgetNeverExceeded` invariant (`agent_execution.qnt:189-193`) must hold *across the pause boundary*. A naive resume that restarts `usage := v1.Usage{}` would let a paused-then-resumed run consume `2 × MaxTokens`. The continuation must re-seed `usage` from the parent's folded `Status.Usage` and the budget pre-checks (`executor.go:112,162,182,246`) must run against the *cumulative* total. **Wall-clock is the subtle one**: the parent's `WallClockUsed` accrued only while its pod ran; the human-think-time gap must **not** count against the wall budget (or every gated run trivially expires). → Resume re-bases the wall clock to "time spent executing", carrying token + tool-call + step counts but **re-anchoring `startedAt = now`** for the continuation's wall accounting, while still summing prior `WallClockUsed` into the budget check. See §6.3 and §10 decision 2.

### 4.3 Why not adopt `contract.go` step-wise now?

`runtime/contract.go` (`StepRequest.History`) is the "right" primitive for durable resumable runs — the controller drives *each* step and owns history, so a pause is just "stop calling". But adopting it is an **architecture inversion** (pod-runs-the-whole-loop → controller-orchestrates-each-step) that touches the entire run datapath, and it is shared with the async-fan-out work ([framework-enhancements §A4-Part-B](../design/framework-enhancements.md)). **This spec does NOT adopt it.** Form B reuses the existing monolithic in-pod executor and re-seeds it via a continuation pod — strictly less invasive. Choosing the engine for *all* durable/resumable runs (HITL, async fan-out, sessions) is §10 decision 7 and is explicitly deferred. The continuation-pod approach is forward-compatible: if we later adopt step-wise, the `Decision` + `PendingAction` CRD surface is unchanged.

---

## 5. Concrete changes

### 5.1 CRD / Go type additions

All pure types live in `pkg/agentmodel/v1`; the operator API embeds them directly, so fields flow to CRDs via `make -C operator deepcopy` (CRD YAML is still hand-edited — CRD-generation drift per project memory). Marked **(proposed)** throughout.

**(proposed)** `pkg/agentmodel/v1/types.go` — new approval-policy on `AgentSpec`:

```go
// ApprovalPolicy gates an Agent's execution behind a human decision.
// Pre-run gating works for any Mode; mid-run tool gating is Mode=loop only.
type ApprovalPolicy struct {
    // RequireApprovalBeforeRun holds the run in RequiresAction (no pod) until a
    // human patches spec.decision. The only HITL form a harness Agent supports.
    // +optional
    RequireApprovalBeforeRun bool `json:"requireApprovalBeforeRun,omitempty"`

    // RequireApprovalForKinds pauses a Mode=loop run before invoking a tool of
    // any listed ToolKind (e.g. ["agent","http"]). Ignored in harness mode.
    // +optional
    RequireApprovalForKinds []ToolKind `json:"requireApprovalForKinds,omitempty"`

    // RequireApprovalForTools pauses before invoking any tool with a listed
    // name. Union'd with RequireApprovalForKinds. Mode=loop only.
    // +optional
    RequireApprovalForTools []string `json:"requireApprovalForTools,omitempty"`

    // ApprovalTimeoutSeconds expires a run left in RequiresAction this long
    // (RequiresAction→Expired). 0 = use the operator default
    // (--default-approval-timeout, see §5.4); a paused run never hangs forever.
    // +optional
    ApprovalTimeoutSeconds int32 `json:"approvalTimeoutSeconds,omitempty"`
}
```
Add `Approval *ApprovalPolicy` to `AgentSpec` (`+optional`).

**(proposed)** `AgentRunSpec` (`types.go:219-256`) — the decision input + a per-run pre-run override:

```go
// Decision answers a RequiresAction pause. The controller acts on it exactly
// once per pause (matched by PendingAction.Token), then resumes or terminates.
// +optional
Decision *Decision `json:"decision,omitempty"`

// RequireApprovalBeforeRun overrides the Agent's pre-run gate for THIS run
// (e.g. a one-off run that must be approved even if the Agent doesn't require
// it). nil = inherit the Agent's ApprovalPolicy. Mirrors BudgetOverride.
// +optional
RequireApprovalBeforeRun *bool `json:"requireApprovalBeforeRun,omitempty"`
```

```go
// Decision is a human's answer to a pending approval.
type Decision struct {
    // Token MUST equal Status.PendingAction.Token — guards against approving a
    // stale/superseded pause (the run advanced between read and patch).
    Token string `json:"token"`
    // Approve true = proceed (run the pod / invoke the gated tool); false = deny.
    Approve bool `json:"approve"`
    // Reason is a free-text audit note (who/why), recorded in Status + Steps.
    // +optional
    Reason string `json:"reason,omitempty"`
    // DecidedBy identifies the approver (informational; real authz is RBAC on
    // patching AgentRun/spec — see §7). +optional
    DecidedBy string `json:"decidedBy,omitempty"`
}
```

**(proposed)** `RunStatus` (`types.go:313-322`) — the pending-action output:

```go
// PendingAction is set iff State==RequiresAction: what the run is waiting for.
// Cleared when a Decision is applied. +optional
PendingAction *PendingAction `json:"pendingAction,omitempty"`
```

```go
// PendingAction describes why a run is paused in RequiresAction.
type PendingAction struct {
    // Kind: "pre-run" (form A) | "tool-call" (form B).
    Kind string `json:"kind"`
    // Token is a fresh, opaque id per pause; a Decision must echo it.
    Token string `json:"token"`
    // RequestedAt is when the pause began (drives ApprovalTimeoutSeconds).
    RequestedAt metav1.Time `json:"requestedAt"`
    // Tool / Arguments / StepIndex are set only for Kind=="tool-call".
    // Arguments may be redacted to "<redacted>" per §7. +optional
    Tool      string          `json:"tool,omitempty"`
    Arguments json.RawMessage `json:"arguments,omitempty"`
    StepIndex int32           `json:"stepIndex,omitempty"`
    // Reason is a human-readable summary of what approval is for. +optional
    Reason string `json:"reason,omitempty"`
}
```

**(proposed)** `StepKind` (`types.go:296-301`) — add:
```go
StepAwaitingApproval StepKind = "AwaitingApproval" // loop paused before a gated tool
```

**(proposed)** `RunResult` (`pkg/agentruntime/runonce.go:27-38`) — carry the pending action over the wire:
```go
// PendingAction is set when Phase==RequiresAction (loop mid-run gate): the
// gated tool the loop paused before. Folded into Status.PendingAction.
PendingAction *v1.PendingAction `json:"pendingAction,omitempty"`
```
And copy it in `ResultToWire` (`runonce.go:88-103`).

### 5.2 CRD YAML edits (hand-edited)

`operator/config/crd/...agentruns.yaml`:
- **spec.decision** object: `token` (string, required), `approve` (bool, required), `reason` (string), `decidedBy` (string).
- **spec.requireApprovalBeforeRun** (boolean).
- **status.pendingAction** object: `kind`, `token`, `requestedAt` (date-time), `tool`, `arguments` (`x-kubernetes-preserve-unknown-fields: true`), `stepIndex`, `reason`.
- **additionalPrinterColumns** (`...agentruns.yaml:19`): add a `PENDING` column `jsonPath: .status.pendingAction.kind` so `kubectl get agentrun` shows what's blocked. The `STATE` column already surfaces `RequiresAction`.

`operator/config/crd/...agents.yaml` (Agent CRD): **spec.approval** object with `requireApprovalBeforeRun` (bool), `requireApprovalForKinds` (array of string, enum mirrors `ToolKind`), `requireApprovalForTools` (array of string), `approvalTimeoutSeconds` (integer, minimum 0).

### 5.3 Validation

`pkg/agentmodel/v1/validation.go`:
- `ValidateAgentRun` (`:125`): if `Spec.Decision != nil`, require `Decision.Token != ""`.
- `ValidateAgent` (`:18`): if `Spec.Approval` set — `ApprovalTimeoutSeconds >= 0`; each `RequireApprovalForKinds` entry is a known `ToolKind`; `RequireApprovalForKinds`/`RequireApprovalForTools` are non-empty only meaningful for `Mode==loop` (warn, not reject — a harness Agent ignores them, documented).

### 5.4 Controller wiring — form A (pre-run gate)

`operator/internal/controllers/agentmodel/agentrun_controller.go`, in `Reconcile` (`:118`), **before** the `Get(pod)` / pod-create block at `:145`:

```
effectiveGate := agent.Spec.Approval.RequireApprovalBeforeRun
if run.Spec.RequireApprovalBeforeRun != nil { effectiveGate = *run.Spec.RequireApprovalBeforeRun }

if effectiveGate && !run.Status.State.Terminal() && !run.Status.preRunApprovalSatisfied() {
    switch {
    case run.Spec.Decision == nil:
        // First time: enter RequiresAction, mint a Token, requeue on the TTL.
        r.markRequiresAction(run, &PendingAction{Kind: "pre-run", Token: newToken(), RequestedAt: now, Reason: "approval required before run"})
        return r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: ttlRemaining(run, defaultTTL)})
    case run.Spec.Decision.Token != run.Status.PendingAction.Token:
        // Stale decision — ignore; keep waiting.
        return r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: ttlRemaining(...)})
    case !run.Spec.Decision.Approve:
        r.markTerminal(run, pure.PhaseCancelled, "decision:denied:"+run.Spec.Decision.Reason)
        return r.updateRunStatus(ctx, run, ctrl.Result{})
    default: // approved
        // Clear PendingAction, record approval, fall through to pod-create.
        run.Status.PendingAction = nil
    }
}
```

- **`markRequiresAction(run, pa)`** (proposed helper, sibling to `markPending`/`markRunning`/`markTerminal` at `:271-297`): `State=RequiresAction`, `PendingAction=pa`. `RequiresAction` is **non-terminal**, so existing `!Terminal()` guards (`:139`, `:248`) keep requeuing — no change to the cancel path (cancel still wins: `:139-143` runs first).
- **TTL → Expired**: when `now - PendingAction.RequestedAt >= effectiveTimeout`, `markTerminal(run, PhaseExpired, "approval:timeout")`. The 5s/TTL requeue (`:226,249`) drives the check; **(proposed)** operator flag `--default-approval-timeout` (default e.g. 1h) on `AgentRunReconciler`, mirroring `--default-run-runtime-class` (`main.go`).
- **Decision delivery is a `spec` patch**, so it changes generation → `GenerationChangedPredicate` (`:110`) wakes the reconcile. (Verified: the controller filters on generation; a status-only field would not wake it, which is why `decision` rides `spec`, not `status`.)

### 5.5 Controller wiring — form B (loop mid-run gate)

Two new behaviors in `agentrun_controller.go`:

1. **Fold the pause.** `foldRunResult` (`:398`) already sets `Status.Steps`/`Usage` and, when `rr.Phase != ""`, `Status.State = rr.Phase` (`:412-414`). Since the run pod will emit `Phase=RequiresAction`, that line **already** stamps `RequiresAction` — we add: `if rr.PendingAction != nil { run.Status.PendingAction = rr.PendingAction }`. **But** the pod-phase switch (`:233-244`) runs *first* and `PodSucceeded` calls `markTerminal(...Completed...)`. A clean pod exit on a pause is `PodSucceeded` (exit 0), so `markTerminal` would wrongly stamp `Completed` before `foldRunResult` re-stamps `RequiresAction`. **Order is load-bearing**: `foldRunResult` must run and `rr.Phase` must win. It does today (fold runs after markTerminal at `:240`, and `:412` overwrites State) — but `markTerminal` also sets `EndedAt` (`:292`); on a pause we must **clear `EndedAt`** (the run is not ended). → **(proposed)** in `foldRunResult`: `if rr.Phase == RequiresAction { run.Status.EndedAt = nil }`.

2. **Spawn the continuation pod on approval.** When `State==RequiresAction && PendingAction.Kind=="tool-call" && Spec.Decision` is present & token-matched:
   - deny → spawn a continuation pod anyway (so the executor records the denial as an `Observation{error}` and continues/ends cleanly), **or** short-circuit to `markTerminal(Cancelled)` if the Agent opts into "deny ends the run". Default: continuation pod (lets the loop react). See §10 decision 4.
   - approve → spawn the **continuation pod**: a *new* pod (the prior pod is gone — `RestartPolicy:Never`, already exited) whose run ConfigMap carries `prior Steps + Decision + carried Usage`. The controller writes these via an extended `BuildRunSpecConfigMap`.

`operator/internal/builders/runspec.go` (`BuildRunSpecConfigMap`, `:49`): today it marshals `run.Spec` whole into `run.json` (`:54`) — so **`spec.decision` already rides into the pod for free**. The continuation also needs prior Steps + carried Usage; **(proposed)** write a `resume.json` (prior `[]v1.Step` + `v1.Usage`) alongside `agent.json`/`run.json`, sourced from `run.Status.Steps`/`Status.Usage` (or the overflow store from [response-richness](./response-richness.md) when the trace was elided). `cmd/agent/run.go` loads `resume.json` when present and calls `Executor.Resume` instead of `Run`.

> **Continuation-pod naming.** The prior pod's name was `run.Name` (`agentrun_controller.go:147,186`). The continuation pod cannot reuse it (it may still be terminating / GC-pending). **(proposed)** name continuation pods `<run.Name>-c<N>` where N is an attempt counter on `Status`, and broaden `runResultFromPod` (`:420`) to match the continuation pod, not just `run.Name`. This is real plumbing — see §10 risk.

### 5.6 Runtime — executor pause + resume

`pkg/agentruntime/executor.go`:

- **(proposed)** `gatedTool(spec ApprovalPolicy, tool v1.Tool) bool` — pure: name in `RequireApprovalForTools` OR kind in `RequireApprovalForKinds`.
- **Pause emission**: in `Run`'s loop, **after** the allow-list/schema/budget pre-checks but **before** the `invoker.Invoke` call (`executor.go:270`), if `gatedTool(...)` and this pending action has not already been approved by a carried `Decision`:
  ```go
  steps = append(steps, v1.Step{Index: ..., Kind: v1.StepAwaitingApproval,
      ToolCalls: []v1.ToolCallRecord{{Tool: tc.Tool, Arguments: tc.Arguments}}})
  return Result{Phase: v1.PhaseRequiresAction, Steps: steps, Usage: usage,
      PendingAction: &v1.PendingAction{Kind: "tool-call", Tool: tc.Tool,
          Arguments: tc.Arguments, StepIndex: int32(len(steps)-1), Token: <seed-derived>}}, nil
  ```
  Add `PendingAction *v1.PendingAction` to `Result` (`executor.go:62-69`).
- **(proposed)** `Executor.Resume(ctx, agent, input, seed, prior []v1.Step, decision *v1.Decision, carried v1.Usage)`: seeds `steps := prior`, `usage := carried`, re-anchors the wall clock (§6.3), then:
  - if `decision.Approve`: re-derive the pending tool from the last `AwaitingApproval` step, run the *same* allow-list/schema/budget checks, `invoker.Invoke`, append the `Observation`, and **continue the normal loop** (`Run`'s body, factored into a shared `runLoop` so `Run` and `Resume` don't fork).
  - if `!decision.Approve`: append an `Observation`-shaped step with `Error: "denied by " + decision.DecidedBy` (no invoke) and continue — the LLM sees the denial and re-plans or finalizes.
- **Factor the loop body** out of `Run` (`:103-309`) into `runLoop(ctx, agent, input, seed, steps, usage, startedAt)` so `Run` (empty seed) and `Resume` (seeded) share one verified loop. This is the bulk of the executor diff and where the budget-carry correctness must be unit-tested hard.

`pkg/agentruntime/runonce.go`:
- `RunTurn` (`:61`) gains a resume path: load `resume.json` if present (proposed `ResumeFile = "resume.json"`); call `Executor.Resume`. `ResultToWire` (`:88`) copies `res.PendingAction` into `RunResult.PendingAction`.
- **Note — session worker interaction**: `RunTurn` is also the session worker's per-turn core (`session_worker.go:100`). A session turn that pauses in `RequiresAction` is a *real* possibility once form B lands; for v1, **gate form B to `RunOnce` (single-shot) runs only** and document that sessions don't yet support mid-turn HITL (the session's own `RequiresAction`-means-idle semantics collide — §10 decision 6).

`cmd/agent/run.go`:
- `clampForTerminationMessage` (`:102`) must **preserve `PendingAction`** under size pressure (it is small and load-bearing for resume) — add it to the "always keep" set alongside phase/usage/reason. The elision order (output → step bodies → steps) is unchanged; `PendingAction.Arguments` is the one arg body we must *not* drop (or resume can't re-derive the call) — but it is also the redaction target (§7), so the controller, not the pod, redacts it for Status.

### 5.7 Quint — new actions + invariant

`spec/quint/agent_execution.qnt`: the model has the `RequiresAction` *state* but **no action reaches it** (`:171-185`). **(proposed)** add:
```
action awaitApproval = all {           // Running → RequiresAction
  phase == Running,
  phase' = RequiresAction,
  // counters frozen — no tokens/tools consumed by pausing
  steps' = steps, tokens' = tokens, toolCalls' = toolCalls, wall' = wall, ...
}
action approve = all {                 // RequiresAction → Running (resume)
  phase == RequiresAction, phase' = Running, ...frozen...
}
action denyOrExpire = all {            // RequiresAction → Cancelled|Expired|Failed
  phase == RequiresAction,
  nondet p = oneOf(Set(Cancelled, Expired, Failed)),
  phase' = p, ...frozen...
}
```
Add to the `step` disjunction (`:174`). **New invariant** `ApprovalFreezesBudget`: across `awaitApproval`/`approve`, the four counters are unchanged — the formal guarantee that pausing/resuming **cannot** be used to exceed budget (the §4.2 #3 carry-correctness risk, proven at the model level). Verify `BudgetNeverExceeded` still holds with the new edges via `quint run --invariant=Safety`.

### 5.8 New / touched files summary

| File | Change |
|---|---|
| `pkg/agentmodel/v1/types.go` | **(proposed)** `ApprovalPolicy`, `Decision`, `PendingAction`; `AgentSpec.Approval`; `AgentRunSpec.Decision`+`RequireApprovalBeforeRun`; `RunStatus.PendingAction`; `StepAwaitingApproval` |
| `pkg/agentmodel/v1/validation.go` | **(proposed)** decision/approval validation |
| `pkg/agentmodel/v1/zz_generated.deepcopy.go` | regen via `make -C operator deepcopy` |
| `pkg/agentruntime/executor.go` | **(proposed)** `gatedTool`, pause emission, `Result.PendingAction`, `Executor.Resume`, factor `runLoop` |
| `pkg/agentruntime/runonce.go` | **(proposed)** `RunResult.PendingAction`, `ResumeFile`, resume path in `RunTurn`, copy in `ResultToWire` |
| `cmd/agent/run.go` | **(proposed)** load `resume.json`; keep `PendingAction` in clamp |
| `operator/internal/controllers/agentmodel/agentrun_controller.go` | **(proposed)** `markRequiresAction`, pre-run gate, TTL→Expired, continuation-pod spawn, fold `PendingAction`, clear `EndedAt` on pause, redact args |
| `operator/internal/builders/runspec.go` | **(proposed)** write `resume.json` for continuation pods |
| `operator/cmd/manager/main.go` | **(proposed)** `--default-approval-timeout` flag |
| `operator/config/crd/...agentruns.yaml`, `...agents.yaml` | **(proposed)** hand-edited CRD fields + printcolumn |
| `spec/quint/agent_execution.qnt` | **(proposed)** approval actions + `ApprovalFreezesBudget` invariant |

---

## 6. Data / control flow

### 6.1 Form A end-to-end (harness pre-run)

```
1. User creates AgentRun (Agent has approval.requireApprovalBeforeRun=true).
2. Reconcile: gate hits BEFORE pod-create → markRequiresAction(pre-run, Token=T1).
   Status.State=RequiresAction, PendingAction={kind:pre-run, token:T1}. No pod.
3. kubectl get agentrun → STATE=RequiresAction, PENDING=pre-run.
4. Human: kubectl patch agentrun X --type=merge -p \
     '{"spec":{"decision":{"token":"T1","approve":true,"decidedBy":"alice"}}}'
5. Generation bumps → reconcile. Decision.Token==T1, Approve=true →
   clear PendingAction, fall through to the EXISTING pod-create path (sandbox
   resolve, prepareRun, ensureRunSpec, broker, egress, BuildAgentRunPod).
6. Normal run; folds to Completed as today. (Deny at step 4 → Cancelled.)
7. If no decision within ApprovalTimeoutSeconds → markTerminal(Expired,"approval:timeout").
```

### 6.2 Form B end-to-end (loop mid-run)

```
1. Loop run pod runs; LLM plans a call to a gated tool (e.g. ToolKind=agent).
2. Executor: gatedTool() true, no carried Decision → emit Step{AwaitingApproval},
   return Phase=RequiresAction + PendingAction{tool, arguments, stepIndex}.
3. agent run writes RunResult (incl. PendingAction, prior Steps) to termination
   message (clamp keeps PendingAction; full Steps in pod logs / overflow store).
   Pod exits 0 → PodSucceeded.
4. foldRunResult: rr.Phase=RequiresAction wins over the Completed markTerminal set,
   EndedAt cleared, Status.Steps + PendingAction folded. Controller redacts
   PendingAction.Arguments per AgentPolicy (§7).
5. Human inspects Status.pendingAction (tool + redacted args), patches spec.decision.
6. Reconcile: token-matched approve → write resume.json (prior Steps + carried
   Usage + Decision) into a continuation ConfigMap, create pod <run>-c1.
7. Continuation pod: RunTurn loads resume.json → Executor.Resume seeds Steps+Usage,
   invokes the approved tool, records Observation, CONTINUES the loop.
8. Loop ends Completed (or pauses AGAIN on the next gated call → repeat 2-7,
   continuation pods -c2, -c3, …), or Expired on cumulative budget.
```

### 6.3 Budget carry (the correctness core)

| Axis | Carried across pause? | Mechanism |
|---|---|---|
| `Tokens` | **Yes** (cumulative) | `Resume` seeds `usage.Tokens = carried.Tokens`; pre-checks (`executor.go:162`) compare cumulative |
| `ToolCalls` | **Yes** (cumulative) | `usage.ToolCalls = carried.ToolCalls`; the gated call counts once, on the resume invoke |
| `Steps` | **Yes** (cumulative) | `steps := prior`; the `AwaitingApproval` step **does not** consume a budget step (it's a pause marker), but it IS in the trace |
| `WallClockUsed` | **Partially** | Budget check sums `carried.WallClockUsed + (now - resumeStart)`; human-think-time is **excluded** (re-anchor `startedAt=now` for the continuation, add prior wall to the *check* only). Otherwise every gated run trivially Expires. |

This split is exactly what makes the Quint `ApprovalFreezesBudget` invariant meaningful: pausing freezes all four counters; resuming continues from the frozen totals; only *executing* advances them.

---

## 7. Security model

HITL composes with the existing run-datapath containment without weakening it; it adds **one new disclosure surface** (PendingAction in Status) and **one new authority** (who may patch a Decision).

- **kata-fc sandbox + egress floor are unchanged.** Form A pauses *before* any pod exists (zero attack surface while paused). Form B's continuation pod gets the **same** `resolveRunSandbox` fail-closed path (`agentrun_controller.go:153`, default kata-fc) and the **same** default-deny egress NetworkPolicy (`run_sandbox.go:60`, `BuildAgentRunEgressPolicy`). A continuation pod is just another run pod. **(proposed)** the continuation must re-run `ensureRunEgressPolicy`/`ApplyRunSandbox` — it is **not** exempt because it "continues" a prior approved run.
- **Broker / SPIFFE invariant holds.** Secrets are minted only when a pod runs (`AttachSecretBroker`, `agentrun_controller.go:208`). A paused run holds no leased secret. The continuation pod gets a fresh broker config + SPIFFE id like any run — pausing does not extend a credential lease across the human-think-time gap. (Verified: leases are per-pod via the broker sidecar, not persisted in Status.)
- **NEW disclosure surface — `PendingAction.Arguments`.** Form B writes the gated call's arguments into broadly-readable `Status` (file paths, command strings, child-agent inputs). This is the **same class of leak** as the unredacted `Status.Output`/`Steps` today. **Mitigation**: the controller applies the namespace's `AgentPolicy.redaction.patterns` to `PendingAction.Arguments` on the fold — **but that redaction engine is itself unbuilt** ([agentpolicy-enforcement](./agentpolicy-enforcement.md) §7). Until it lands, the honest stance is: **arguments are disclosed at the same level as Output already is.** Optionally support a per-policy `redactPendingArguments: true` that stores `"<redacted; see pod logs>"` and forces the approver to read pod logs (analogous to the termination-message overflow). See §10 decision 3.
- **NEW authority — who approves.** A `Decision` is a `spec` patch on the AgentRun. **Authz is plain Kubernetes RBAC** on `patch agentruns` (or, tighter, a future `agentruns/decision` subresource — §10 decision 5). This spec does **not** add a bespoke approver-identity check; `DecidedBy` is informational. The maintainer must scope the approver Role narrowly — anyone who can patch an AgentRun spec can approve a gated action, i.e. authorize a high-blast-radius tool call. **This is a real privilege boundary; treat the patch-AgentRun permission as the approval authority.**
- **Stale-decision guard.** `Decision.Token` must equal `PendingAction.Token`; a mismatched token is ignored. This prevents a replayed/auto-approving patch from green-lighting a *different* pending action than the human reviewed (e.g. the run advanced to a new gate between read and patch).
- **No new egress.** Pausing/resuming uses only the apiserver (controller-side) + the existing run-pod datapath. No agent-to-agent or external call is introduced by HITL itself (form B *gating* an `agent`-kind tool is governed by [agent-to-agent-invoker](./agent-to-agent-invoker.md) (future), not by this spec).

**Net new attack surface:** (1) argument disclosure in Status — mitigated by redaction (unbuilt) or read-logs-only mode; (2) approval authority = patch-AgentRun RBAC — mitigated by scoping the Role; (3) a malicious/auto approver could rubber-stamp gates — out of scope (it's an RBAC + process control, not a code control).

---

## 8. Phasing & effort

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **P1 — Pre-run gate (form A)** | `ApprovalPolicy.RequireApprovalBeforeRun` + per-run override, `Decision`, `PendingAction{kind:pre-run}`, `markRequiresAction`, pre-run gate in `Reconcile`, TTL→Expired, `--default-approval-timeout`, printcolumn, validation, CRD edits. **No executor/runtime change.** | **M** | none (works for harness **and** loop — it gates the pod, not the loop) |
| **P2 — Quint + lifecycle proof** | Approval actions + `ApprovalFreezesBudget` invariant; confirm `Safety` holds. | **S** | P1 (shape settled) |
| **P3 — Loop pause emission (form B, half 1)** | `StepAwaitingApproval`, `gatedTool`, `Result.PendingAction`, executor pause-and-exit, `RunResult.PendingAction`, fold + clear `EndedAt`, keep in clamp. **Pauses but cannot resume yet** (a paused loop run is a manual dead-end until P4). | **L** | P1; [response-richness](./response-richness.md) (future) for traces >3 KiB |
| **P4 — Resume (form B, half 2)** | `Executor.Resume` + `runLoop` refactor, budget-carry (§6.3), `resume.json`, continuation-pod spawn + naming + `runResultFromPod` broadening, deny-handling. **The XL core.** | **XL** | P3; persisted/overflow Steps ([response-richness](./response-richness.md), future); §10 decisions 2,4,7 |
| **P5 — Redaction of PendingAction.Arguments** | Apply `AgentPolicy.redaction` to args on fold (or read-logs-only mode). | **S** | the redaction engine from [agentpolicy-enforcement](./agentpolicy-enforcement.md) |

**Cross-spec dependencies:** P3/P4 hard-depend on [response-richness](./response-richness.md) (full Steps must survive the pause to re-seed resume). [agent-to-agent-invoker](./agent-to-agent-invoker.md) and [loop-mode-tools-and-invokers](./loop-mode-tools-and-invokers.md) (both future) are the obvious *consumers* of form B (gating an `agent`/`http`/`mcp` call) — but form B's *gating mechanism* does not require them: it can gate any kind, and a kind with no production invoker simply never reaches the gate today. P5 depends on the redaction half of [agentpolicy-enforcement](./agentpolicy-enforcement.md).

**Ship order is P1 → P2 → P3 → P4 → P5.** P1 delivers a usable, low-risk valve immediately (correcting framework-enhancements §O3, which proposed the expensive loop half first).

---

## 9. Test plan

### Unit
- **lifecycle** (`pkg/agentmodel/v1/lifecycle_test.go`): assert `CanTransition(Running, RequiresAction)`, `(RequiresAction, Running)`, `(RequiresAction, Expired/Cancelled/Failed)` all legal; `RequiresAction` non-terminal.
- **executor pause** (`pkg/agentruntime/executor_test.go`): a fake LLM that plans a gated tool → assert `Phase=RequiresAction`, a trailing `StepAwaitingApproval`, `Result.PendingAction.{Tool,Arguments,StepIndex}` set, **no invoke happened** (invoker call count 0).
- **executor resume — budget carry (the critical test)**: seed `prior` with usage near `MaxTokens`; `Resume(approve)`; assert the resumed invoke + subsequent plan **cannot** push cumulative tokens past `MaxTokens` (Expired fires on the cumulative total, not a fresh budget). Separately assert wall-clock human-think-time is **excluded** (a 10-minute decision gap on a 60s wall budget still runs).
- **executor resume — deny**: `Resume(deny)` records an `Observation{error}` and continues without invoking; a finalizing LLM then completes.
- **token guard** (controller, table test): mismatched `Decision.Token` is ignored (run stays `RequiresAction`); matched + approve proceeds; matched + deny → Cancelled (form A) / continuation (form B).
- **clamp** (`cmd/agent/run.go` test): a large run that pauses still emits a parseable termination message with `PendingAction` intact after `clampForTerminationMessage`.
- **controller pre-run gate** (envtest, `agentrun_controller_test.go`): Agent with `requireApprovalBeforeRun` → run enters `RequiresAction` with **no pod created**; patch approve → pod appears; patch deny → Cancelled; no decision past TTL → Expired.
- **session non-interference**: a session-worker turn does **not** trip the AgentRun pre-run gate (form B disabled for sessions in v1); the session's idle `RequiresAction` semantics are untouched.

### Quint (`spec/quint/agent_execution.qnt`)
- `quint run --invariant=Safety` passes with the new `awaitApproval`/`approve`/`denyOrExpire` actions; add a `humanInTheLoop` scenario run (`init → begin → plan → awaitApproval → approve → toolGood → complete`) and assert `ApprovalFreezesBudget` + `BudgetNeverExceeded` hold throughout.

### E2E
- The **cftest single-node k0s box** (project memory: live-verified Hetzner arm64, Hermes + z.ai) is the live target. **Form A e2e**: deploy a harness Agent with `requireApprovalBeforeRun`; create a run; assert `kubectl get agentrun` shows `RequiresAction` and **no pod**; `kubectl patch` the decision; assert the pod runs and the run completes. This exercises the real generation-bump → reconcile → pod-create path end-to-end and is the highest-confidence verification for P1.
- **Form B e2e** is gated on P4 + persisted Steps; defer until [response-richness](./response-richness.md) lands. A loop Agent with `requireApprovalForKinds:[function]` and a real in-process tool, asserting pause → patch → continuation pod → completion. (No loop-mode tool invoker ships in production today — this e2e also depends on [loop-mode-tools-and-invokers](./loop-mode-tools-and-invokers.md), future.)

---

## 10. Risks & open decisions

1. **Form B is genuinely XL and chained behind two future specs.** Resume needs (a) full prior Steps surviving the pause (the 4 KiB clamp elides them — [response-richness](./response-richness.md), future) and (b) a production loop-mode tool invoker for the gated kind to be reachable at all ([loop-mode-tools-and-invokers](./loop-mode-tools-and-invokers.md), future). **Recommendation: ship P1 (form A) standalone; treat P3/P4 as a later milestone** once both backbones land. Do not block the cheap valve on the expensive one.

2. **Wall-clock carry semantics (decision needed).** Does human-think-time count against `MaxWallClockSeconds`? This spec recommends **no** (re-anchor; sum prior wall into the budget check only) — otherwise gated runs trivially Expire. The alternative ("total elapsed including think-time") makes the budget a hard SLA on approval latency. **Maintainer must pick.** The Quint `ApprovalFreezesBudget` invariant assumes the recommended split.

3. **Redaction of `PendingAction.Arguments` (decision needed).** The redaction engine is unbuilt ([agentpolicy-enforcement](./agentpolicy-enforcement.md) §7). Options: (a) disclose args at the same level Output already is (ship now, redact later via P5); (b) read-logs-only mode (`"<redacted>"` in Status, full args in pod logs) — but that defeats the point of an approver reviewing *in* `kubectl`. Recommendation: (a) now, P5 later.

4. **Deny semantics for form B (decision needed).** On deny, do we (a) spawn a continuation pod that records the denial as an `Observation{error}` and lets the loop re-plan/finalize (richer, default), or (b) short-circuit the whole run to `Cancelled`? (a) is more flexible but costs a continuation pod for a "no"; (b) is cheaper but blunt. Consider a per-policy `denyEndsRun bool`.

5. **Approval authz model (decision needed).** Plain RBAC on `patch agentruns` (this spec's default) vs a dedicated `agentruns/decision` subresource (lets you grant "may approve" without "may edit spec"). The subresource is cleaner for least-privilege but is extra plumbing (new subresource + RBAC markers + webhook to reject other spec edits via the subresource). Recommendation: start with patch-RBAC; promote to a subresource if approver/editor separation is needed.

6. **`RequiresAction` is overloaded between AgentRun (blocked-on-human) and AgentSession (idle-parked).** `session_worker.go:139` and `AgentSessionStatus.Phase` use `RequiresAction` to mean "idle". This spec scopes HITL to `AgentRun.Status.State` and **disables form B for session turns in v1**. If sessions later need mid-turn HITL, the two meanings must be disambiguated (e.g. a separate `SessionPhase` or a `PendingAction` discriminator). **Flagged, not resolved.**

7. **Resume engine (shared with async fan-out).** This spec reuses the monolithic in-pod executor via continuation pods rather than adopting `runtime/contract.go`'s step-wise protocol. The step-wise engine is cleaner for *all* durable/resumable runs (HITL, async fan-out per [framework-enhancements §A4-B](../design/framework-enhancements.md), sessions) but is an architecture inversion. **The maintainer should decide the durable-execution engine once, before building P4** — committing to continuation-pods here is the low-risk default but may be re-litigated.

8. **Continuation-pod identity & idempotency.** Naming continuation pods `<run>-c<N>` and broadening `runResultFromPod` (`agentrun_controller.go:420`, currently matches `run.Name` only) is real, fiddly plumbing with retry/idempotency edges (what if the controller crashes after writing `resume.json` but before pod-create? after pod-create but before status?). The attempt counter must be on `Status` and the spawn must be idempotent (get-or-create the named pod). Needs careful envtest coverage.

---

**Bottom line.** `RequiresAction` is dead scaffolding that two increments can bring to life. **Form A (pre-run gate)** is a self-contained, low-risk **M** that delivers a real approval valve for *any* Agent by gating the pod — ship it first. **Form B (loop mid-run gate + resume)** is an honest **XL**: the pause emission is straightforward, but stateful resume across a stateless `RestartPolicy:Never` pod requires net-new executor entry points, careful budget-carry (the Quint-provable correctness core), continuation-pod plumbing, and — load-bearing — full prior Steps surviving the 4 KiB termination cap, which is itself a separate spec ([response-richness](./response-richness.md), future). Sequence form B behind that backbone and behind the wall-clock-carry and resume-engine decisions.
