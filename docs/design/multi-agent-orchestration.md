# Multi-agent orchestration — agent teams on smol-agents

> Status: **design / proposal** (not built). Successor to
> [`agent-to-agent-invoker`](../specs/agent-to-agent-invoker.md), building on the
> turn-model/runtime split ([`turn-model-vs-runtime`](turn-model-vs-runtime.md))
> and durable sessions ([`durable-session-architecture`](durable-session-architecture.md)).
> Decisions referenced from [`decisions.md`](decisions.md).

## 1. What this is, and why

Today the platform runs **one** agent per execution (an `AgentRun`) or one
durable worker per conversation (an `AgentSession`), and a single cross-agent
edge: **A2A** — a `kind=agent` tool whose `AgentRunInvoker` spawns a *child*
`AgentRun`, polls it to terminal, folds its output + usage, and GC's it via an
OwnerReference subtree. That is exactly **one** of the five industry coordination
patterns (orchestrator→subagent), and only in the parent→child direction.

This doc maps the broader multi-agent design space onto our primitives and
proposes the minimum net-new machinery to support **teams of agents** — peers
that message each other, claim work from a shared list, build on shared state,
and converge — while keeping the platform's non-negotiables: per-member
isolation, **enforced** budgets, deny-by-default egress, secretless credentials,
and audit (D1/D3/D10).

Two reference points:

- **Claude Code "agent teams"** — a *lead* coordinates *teammates* (each its own
  context window) through a **shared task list** (claim + dependencies, file-lock
  on claim) and a **mailbox** (`SendMessage` by name, peer↔peer), with a
  **plan-approval** gate and `TeammateIdle`/`TaskCreated`/`TaskCompleted` hooks.
- **The five coordination patterns** — generator-verifier, orchestrator-subagent,
  agent-teams (shared queue), message-bus (pub/sub), shared-state (blackboard).

### The differentiator

Claude Code teams are **single-user, single-trust, local** (tmux panes, files
under `~/.claude/teams`). Our value is the opposite axis: a team here is a set of
**sandboxed, budget-capped, policy-scoped, audited** members that can even span
**trust levels** safely. The thing the Claude Code docs call the biggest team
risk — *"two teammates editing the same file leads to overwrites"* — is something
we can actually **solve** with branchable, 3-way-mergeable **AgentFS** instead of
telling users to partition files by hand.

## 2. The five patterns → our primitives

| Pattern | What it needs | Our substrate | Status |
|---|---|---|---|
| **Orchestrator → subagent** | lead plans, delegates bounded subtasks, synthesizes | **A2A `AgentRunInvoker`** (`kind=agent` tool, `MaxDepth`, per-call timeout, usage roll-up, ownerRef GC) | **Shipped** (M3) |
| **Generator → verifier** | gen→verify loop, criteria, max-iter, best-effort fallback | two agents + a **convergence controller** (loop with stop condition) | Net-new (small) |
| **Agent teams** (shared queue) | members claim from a shared task list, work multi-step, coordinator collects | `AgentSession` workers + **`TeamTask` shared list** + **peer mailbox** | Substrate exists; coordination net-new |
| **Message bus** (pub/sub) | agents publish/subscribe events through a router; emergent workflow | **NATS JetStream** (`pkg/sessionqueue`) + a **subject schema + router** | Transport exists; router net-new |
| **Shared state** (blackboard) | concurrent read/write to a shared store; convergence/termination | **AgentFS** (kopia/S3, branchable + 3-way merge) as a shared workspace | Substrate exists (the killer fit); sharing semantics net-new |

The recommended evolution from the blog — *"start with orchestrator-subagent…
evolve where it struggles"* — is the right ordering for us too: A2A is already
the orchestrator-subagent base; the team patterns layer on top of it.

## 3. Where this lives: the turn-model layer

A team is a **Turn-Model-layer** concern. M4.1 split the platform into:

- **Runtime layer** (`pkg/agentruntime`) — executes exactly one turn behind
  `TurnExecutor.Execute(Turn) → Result`.
- **Turn-Model layer** (`pkg/turnmodel`) — owns turns, sessions, delivery,
  cross-turn memory.

A **team** is a *coordination policy over a set of TurnExecutor-backed members*.
It belongs in `pkg/turnmodel` (a new `team` sub-package), and the **coordinator**
is itself a turn-model construct — either (a) a loop-mode *coordinator agent*
whose tools are `kind=agent` (delegate) + a new `kind=teammate` (message) +
task-list ops, or (b) an operator-driven controller. We propose **(a) the
coordinator-agent model** as primary (it reuses the loop + A2A + budget machinery
and keeps the coordinator itself observable/affordable), with the operator
providing the durable scaffolding (CRD, task list, mailbox subjects).

## 4. Proposed primitives (net-new)

### 4.1 `AgentTeam` CRD (`runtime.agents.smol-agents.ai`)

The durable team record + governance envelope.

```
AgentTeamSpec:
  lead:             AgentRef                 # the coordinator agent (loop mode)
  members:         []TeamMemberSpec          # { name, agentRef, role?, maxConcurrent? }
  pattern:         orchestrator | generator-verifier | team | bus | shared-state
  budget:          Budget                    # TEAM-WIDE ceiling (rolls up members)
  maxMembers:      int32                     # fan-out cap (extends A2A MaxDepth to width)
  sharedWorkspace: *SharedWorkspaceSpec      # optional shared AgentFS (§4.4)
  convergence:     *ConvergenceSpec          # maxIterations, stopCondition (§4.3)
AgentTeamStatus:
  phase, members[].phase, cumulativeUsage (field-wise, never Usage.Add),
  taskSummary{pending,inProgress,completed}, lastActivity
```

- **OwnerReference**: every member run/session + the task list + mailbox stream is
  owned by the `AgentTeam` (literal UID, like A2A's `AGENT_RUN_UID`), so deleting
  the team GC's the whole subtree — the team-scale generalization of the A2A
  subtree GC we already ship.
- **Namespaced**; a team's members are **same-namespace by default** (D1 — no
  cross-tenant team without an explicit, policy-gated grant).

### 4.2 Shared task list — `TeamTask`

The Claude Code "shared task list" (claim + dependencies + states). Two viable
backings; recommend **NATS JetStream KV** (we already run NATS for turns):

- `TeamTask{ id, title, state: pending|inProgress|completed, deps[], owner?, result? }`
  in a per-team KV bucket `team_<ns>_<team>`.
- **Claim = atomic** via KV revision CAS (the durable analog of Claude Code's
  file-lock-on-claim) — no two members win the same task.
- **Dependency unblock** — a completed task flips its dependents claimable; the
  coordinator (or a tiny controller) watches the bucket.
- Exposed to members as loop tools: `task.list`, `task.claim`, `task.complete`
  (a new `kind=task` invoker, fail-closed unless the team grants it).
- *(CRD alternative: a `TeamTask` CRD per task — heavier, but gets k8s RBAC +
  `kubectl get` for free. KV is lighter + matches the turn transport; pick at P0.)*

### 4.3 Peer mailbox — `kind=teammate` invoker + NATS subjects

A2A is parent→child only; teams need **peer↔peer** (Claude Code's `SendMessage`
by name). Add a mailbox over NATS subjects, scoped + ACL'd per team:

- Subjects: `agentteam.<ns>.<team>.mbox.<member>` (one inbox per member),
  delivered into that member's session control channel (reuse the `serve-session`
  worker's turn-source seam, a new "control turn" kind).
- A `kind=teammate` loop tool — `teammate.send(to, message)` — lets any member
  message any other **by name** (names from `AgentTeamStatus.members`).
- **ACL** (extends [`nats-tenant-acls`](nats-tenant-acls.md)): the operator mints
  each member a NATS user JWT scoped to **publish** `…mbox.*` within its team and
  **subscribe** only `…mbox.<self>` — a member cannot read another's inbox or
  reach another team. Cross-team/cross-tenant messaging is structurally impossible.
- **Convergence/termination** (the blog's #1 failure mode — *"termination as an
  afterthought… cycle indefinitely"*): `ConvergenceSpec{maxIterations,
  stopCondition, timeBudget}` is **mandatory** for the `generator-verifier`,
  `bus`, and `shared-state` patterns; the coordinator enforces it and the
  team-wide `Budget` is the hard backstop (a team cannot out-spend its ceiling).

### 4.4 Shared state — shared AgentFS (the conflict story)

For the shared-state / blackboard pattern and for teams that touch shared files:

- A `SharedWorkspaceSpec` mounts one **shared AgentFS** volume across members
  (read/write), distinct from each member's private workspace.
- **Conflict avoidance is our advantage**: AgentFS is branchable + 3-way
  mergeable. Each member works a **branch**; the coordinator (or a merge step)
  3-way-merges branches at task-completion — turning the Claude Code
  "don't let two teammates edit the same file" warning into an enforced merge,
  not a manual partition. (Plain shared-RW is the simple default; branch-merge is
  the strong mode.)
- Termination for blackboard loops: a `stopCondition` + `timeBudget` (per §4.3),
  never open-ended.

### 4.5 Plan-approval + hooks (reuse what we have)

- **Plan approval** (Claude Code: teammate plans read-only, lead approves) maps
  **directly** onto **M5's pre-run gate**: a member run held in `RequiresAction`
  with a `PendingAction`, approved/denied via `spec.decision` — except the
  approver is the **coordinator** (or a human), not only a human. Reuse M5
  wholesale; add a coordinator-issued decision path.
- **Hooks** (`TeammateIdle`/`TaskCreated`/`TaskCompleted`) map onto **AgentPolicy
  gates + admission webhooks**: a team can attach policy that vetoes a task
  creation/completion or re-queues an idle member — the same fail-closed,
  operator-enforced gate model we already use for tools/egress.

## 5. Governance (non-negotiable, the platform's reason to exist)

Everything below is **enforced by the operator/broker**, not advisory — this is
what a local agent-team tool cannot do:

- **Team budget** — `AgentTeamSpec.budget` is a hard ceiling; member usage rolls
  up **field-wise** (`Steps/Tokens/ToolCalls/WallClock`, never `Usage.Add`),
  exactly as A2A child usage rolls up today. A team **cannot** exceed it; the
  coordinator's budget context fires first, the pod `ActiveDeadlineSeconds` is the
  backstop. Cost stays **milli-USD, observability-only** (never a gate).
- **Per-member isolation** — every member is still a normal run/session: kata
  microVM in prod (D3), default-deny egress floor, secretless broker per member,
  hardened PSA. A team adds **no** weaker posture.
- **Mailbox/task ACLs** — per-member NATS JWT (§4.3) — a member can't read peers'
  inboxes, other teams, or other tenants.
- **Subtree GC** — team OwnerReference owns members + task KV + mailbox stream.
- **Fan-out ceilings** — `maxMembers` + per-member `maxConcurrent` generalize the
  A2A `MaxDepth` recursion guard to **width**; admission queue + per-tenant
  concurrency caps (D10) still apply to the member runs.
- **Audit** — every claim, message, handoff, plan-approval, and merge is an audit
  event (same rail as the attach audit in `cmd/agentterminal`).
- **Mixed-trust teams** — because members are independently sandboxed + ACL'd, a
  team may include a lower-trust member (e.g. an untrusted "devil's advocate")
  whose egress/tools are tighter than the lead's — a capability local teams lack.

## 6. Pattern realizations (what each looks like, concretely)

- **Orchestrator-subagent** — *today.* Coordinator agent with `kind=agent` tools;
  children fan out, results + usage fold back. (Add `maxMembers` width to A2A's
  depth.)
- **Generator-verifier** — coordinator loops: delegate to `generator` (A2A),
  delegate output to `verifier` (A2A) with explicit criteria, repeat until the
  verifier accepts or `convergence.maxIterations`; return best attempt on
  non-convergence. *The verifier is only as good as its criteria* → criteria are a
  required field, not free text.
- **Agent teams** — members are `AgentSession` workers; they `task.claim` from the
  shared KV, `teammate.send` findings, and the coordinator collects on completion.
  Shared files via shared AgentFS branches.
- **Message bus** — members publish/subscribe team subjects; a **router**
  (subject schema + a tiny dispatch controller, or a Knative event source)
  delivers by topic so new members subscribe without rewiring. Tracing via the
  audit rail (the blog flags bus tracing as the hard part).
- **Shared state** — shared AgentFS blackboard; members read/write concurrently;
  the coordinator owns the `stopCondition`.

## 7. Phased build plan

Each phase reuses the prior; each is independently shippable + governed.

- **P0 — `AgentTeam` CRD + team budget roll-up + subtree GC.** Pure types +
  operator wrapper + hand-written deepcopy + hand-edited CRD (CRD-drift rule);
  team-wide usage roll-up reusing the A2A field-wise fold; team OwnerReference.
  *No coordination yet* — just the governed envelope. (Smallest, highest-leverage.)
- **P1 — Shared task list (`TeamTask` over NATS KV) + `kind=task` invoker.**
  Atomic claim (KV CAS), dependency unblock, fail-closed grant.
- **P2 — Peer mailbox (`kind=teammate` + per-member NATS ACL) + member
  addressing.** The `SendMessage` analog; control-turn delivery into the session
  worker.
- **P3 — Coordination controllers.** Generator-verifier convergence loop +
  orchestrator width (`maxMembers`); mandatory `ConvergenceSpec` + team-budget
  backstop. (Orchestrator-subagent is already live via A2A.)
- **P4 — Shared AgentFS workspace + branch-merge conflict resolution.** The
  blackboard + the "two teammates, one file" solution.
- **P5 — Message-bus router + plan-approval (reuse M5) + team hooks (AgentPolicy
  gates).** Emergent pub/sub workflows + governance gates.

## 8. Reuse vs build (summary)

| Capability | Reuse (shipped) | Build (net-new) |
|---|---|---|
| Delegation (orch→subagent) | A2A `AgentRunInvoker`, ownerRef GC, usage roll-up | width cap (`maxMembers`) |
| Durable members | `AgentSession`, `serve-session`, checkpoint/resume | control-turn channel |
| Turn transport | `pkg/sessionqueue` NATS JetStream | task-KV + mailbox subjects + router |
| Shared state | AgentFS (kopia/S3, branch + 3-way merge) | shared-workspace mount + merge step |
| Governance | budgets, AgentPolicy, egress floor, secretless broker, NATS ACLs, kata | team budget envelope, per-member mailbox ACL |
| Approval | M5 pre-run gate (`RequiresAction`/`PendingAction`/`spec.decision`) | coordinator-issued decision path |
| Layering | `pkg/turnmodel` + `TurnExecutor` seam | `pkg/turnmodel/team` coordinator |

## 9. Open questions (decide before P0)

1. **Coordinator = agent or controller?** Recommend a **loop-mode coordinator
   agent** (reuses budget/loop/A2A, stays observable) over an opaque operator
   controller. The operator still owns the durable CRD/KV/ACL scaffolding.
2. **Task list backing — NATS KV vs `TeamTask` CRD?** KV is lighter + matches the
   turn transport; CRD gives RBAC + `kubectl`. Lean KV; revisit if tenants need
   to inspect tasks via the API.
3. **Member trust span.** Same-namespace only at GA (D1). Cross-tenant teams need
   a new policy-gated grant (defer).
4. **Termination authority.** Who owns `stopCondition` — the coordinator agent or
   an operator watchdog? Both: agent decides, team budget + deadline backstop.
5. **Conflict model default.** Shared-RW AgentFS (simple) vs branch-per-member +
   merge (strong). Ship shared-RW first; branch-merge as the opt-in strong mode.
6. **Relationship to webterm/attach.** A human can attach (M4 `AttachGrant`) to
   the **coordinator** to steer a live team — a natural compose, not new work.

## 10. Non-goals (for now)

- Nested teams (a member spawning its own team) — mirrors Claude Code's "no
  nested teams"; the `MaxDepth`/`maxMembers` ceilings forbid it by default.
- Promoting a member to lead / transferring leadership.
- Cross-tenant teams (needs a trust-grant design first).
