# Turn Model vs Runtime — Layer Split + Interactive Sessions (webterm / attach)

> **Status: DESIGN / PROPOSAL.** Grounded against v0.2.x source (2026-06-03). No code
> changed by this doc. Marks **[EXISTS]** vs **[PROPOSED]** throughout — much of the
> split is *formalizing a boundary that already exists implicitly*, plus giving
> interactive attach a home.
>
> **Reads with:** [durable-session-architecture.md](durable-session-architecture.md)
> (the current session machinery = the Turn-Model layer's implementation today),
> [harness-authoring.md](harness-authoring.md) (the Runtime layer),
> [../specs/terminal-exposure-http-ssh-tmux.md](../specs/terminal-exposure-http-ssh-tmux.md)
> (the attach detail), and the per-client turn mapping in
> [../research/agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md).

---

## 1. Problem: the turn model and the runtime are one blurred thing

Today a **turn** (the *unit*: an input → an output, plus how turns are sequenced,
where session state lives, and how memory carries across turns) and the **runtime**
(how a *single* turn is actually executed: the executor or a harness, in a pod, under
a budget) both flow through one implicit seam — `RunTurn` (`pkg/agentruntime/runonce.go:61`).
There is no explicit boundary between "what a turn is" and "how a turn runs".

Concretely **[EXISTS]**:

- `AgentRun` (one-shot) and `AgentSession` (durable, N turns) **both** bottom out in
  `RunTurn(agent, runSpec)` → `Executor.Run` → either the loop or `runHarness` (one
  `harness.Run(Request) → Response` call, `executor.go`).
- The session worker (`session_worker.go`) — clearly a *turn-model* concern (ordering,
  checkpoint, idle-park) — is wired directly to runtime details.
- **Cross-turn memory is ad-hoc and smuggled**, not modeled: Hermes carries an
  `X-Hermes-Session-Id` (gateway-side memory, `hermes.go`), CLI harnesses carry nothing
  but a durable AgentFS *workspace*, loop carries nothing at all. There's no layer that
  *owns* "what context does the next turn get".
- **Interactive attach (webterm/SSH/tmux) has nowhere to live** — there's no "session
  layer" abstraction it belongs to, so the [terminal-exposure spec](../specs/terminal-exposure-http-ssh-tmux.md)
  has no seam to plug into.

The fix is to name the two layers and pin the seam between them.

---

## 2. The split

Two layers, one seam. **The layers already exist in the code; this makes the boundary
explicit and dependency-directional (Turn Model → Runtime, never the reverse).**

```
            ┌─────────────────────────────────────────────────────────┐
 SURFACES   │  AgentRun (1 turn)        AgentSession (N turns)          │
            └───────────────┬───────────────────────┬─────────────────┘
                            │                        │
            ┌───────────────▼────────────────────────▼─────────────────┐
 TURN MODEL │  Turn · Session · ordering/delivery (gateway, NATS) ·     │
  (owns the │  state + checkpoint (SessionState/Store) · cross-turn     │   + ATTACH
   session) │  MEMORY POLICY · lifecycle (idle/park/scale) · ATTACH     │◄── side-channel
            └───────────────────────────┬─────────────────────────────┘
                                         │  TurnExecutor.Execute(Turn) → Result   ← THE SEAM
            ┌────────────────────────────▼────────────────────────────┐
 RUNTIME    │  executes exactly ONE turn: Executor (loop) | Harness    │
 (executes  │  (hermes/claude/codex/aider/goose/generic) · pod ·       │
  one turn) │  budget · Request/Response contract                       │
            └──────────────────────────────────────────────────────────┘
```

### 2.1 Runtime layer — *executes one turn* **[EXISTS, to be fenced]**

- **Owns:** the loop `Executor` + the `Harness` implementations, the run pod, budget
  enforcement, the `Request`/`Response` contract (`harness/iface.go`).
- **Is stateless w.r.t. sessions:** given a turn's inputs (Instructions, Input,
  WorkingDir, Env, Budget, Seed) it produces a `Result`. It must **not** know about
  sessions, ordering, prior turns, or attach. (`RunTurn` is already per-turn — this is
  ~true today; the change is to *forbid* it knowing more.)
- **Package:** `pkg/agentruntime` (executor, harness, the `RunTurn` body becomes the
  canonical `TurnExecutor`).

### 2.2 Turn Model layer — *owns turns + sessions* **[EXISTS as session_worker, to be named]**

- **Owns:** `Turn`, `Session`, ordering + delivery (`TurnSource`/`ResultSink`, the
  gateway, NATS — `session_queue_source.go`, `sessionqueue/`, `cmd/agentgateway`),
  state + checkpoint (`SessionState`/`SessionStore`, `session.go`), **cross-turn memory
  policy**, lifecycle (idle-park/scale-to-zero), and — new — **attach** (§4).
- **Public surfaces:** `AgentRun` is a *degenerate session of one turn*; `AgentSession`
  is N turns. Both become thin entrypoints over the same Turn-Model core.
- **Depends on the Runtime only through `TurnExecutor`.** Today the worker reaches into
  `RunTurn` directly; after the split it holds a `TurnExecutor`.
- **Package:** a `pkg/turnmodel` (or `pkg/agentruntime/session` promoted) holding
  `session.go`, `session_worker.go`, `session_queue_source.go`, `sessionqueue/`.

### 2.3 The seam **[PROPOSED]**

```go
// Runtime layer exports this; Turn Model layer consumes it. The ONLY coupling.
type TurnExecutor interface {
    // Execute runs exactly one turn to a terminal Result. ctx cancellation
    // MUST terminate it. No session awareness.
    Execute(ctx context.Context, t Turn) (Result, error)
}

type Turn struct {
    Instructions string          // resolved from the Agent
    Input        json.RawMessage // this turn's payload
    Budget       v1.Budget
    Seed         int64
    // Context carried IN by the Turn-Model layer per its memory policy (§2.4):
    Memory       TurnMemory      // e.g. {ProviderSessionID, PriorOutput, History}
}

type Result struct {            // = today's RunResult, unchanged
    Output            json.RawMessage
    Usage             v1.Usage
    Steps             []v1.Step
    TerminationReason string
    Phase             v1.Phase
}
```

`RunTurn` becomes the reference `TurnExecutor` (loop + harness dispatch behind it).
The loop `Executor` and the harness runner are the two implementations.

### 2.4 Cross-turn memory becomes an explicit, per-runtime *policy* **[PROPOSED]**

The Turn-Model layer decides what context the next `Turn` carries — turning today's
smuggled handling into a first-class strategy:

| Runtime | Memory strategy (Turn-Model picks) | Carries |
|---|---|---|
| Hermes (HTTP) | **provider session** | stable `X-Hermes-Session-Id` → gateway-side memory |
| loop (native) | **history replay** *(needs resume work)* | prior `Steps`/messages into `Turn.Memory.History` |
| CLI (claude/codex/…) | **workspace only** | nothing in `Turn`; the AgentFS mount persists files |

This is where the (stubbed) [human-in-the-loop](../specs/human-in-the-loop.md) resume,
the [agent-claude-code](../specs/agent-claude-code.md) `--resume` work, and A2A
session-tree threading all attach cleanly — they are **Turn-Model** features, not
runtime ones.

### 2.5 Why split

- **Attach gets a home** (the session layer — §4).
- **Memory is explicit + testable** (per-runtime strategy, not smuggled).
- **Testability:** Turn Model tests against a fake `TurnExecutor`; Runtime tests
  per-turn with no session scaffolding.
- The dormant `runtime/contract.go` step-wise protocol and the
  [response-richness](../specs/response-richness.md) wire are clarified by the seam.

---

## 3. Agent classification: one-shot vs *requires a session*

| | One-shot (sessionless) | **Requires a session** (resident) |
|---|---|---|
| **Needs a resident pod?** | no (pod == turn) | yes (pod outlives a turn) |
| **Cross-turn memory?** | no | usually |
| **Interactive attach?** | no | **optionally — this is who webterm/attach is for** |
| **Surface** | `AgentRun` | `AgentSession` / serving `SmolAgent` |
| **Examples** | Hermes single call, loop (chat), **batch** CLI coding runs | **openclaw** (daemon), **interactive** claude-code/codex/pi-mono, durable Hermes conversations |

"**Requires a session**" = long-lived **or** interactive **or** stateful-across-turns.
Decision inputs: (a) does the workload listen / never return (daemon)? (b) does a human
want to watch/steer a live shell (interactive)? (c) does it need memory across turns?
Any "yes" ⇒ a resident session pod; (b) ⇒ also an **attach** surface.

**Open decision:** is this a new CRD field (`spec.session.required` / a `requiresSession`
flag), inferred (`harness.sessionPolicy=persistent` ∨ `kind=openclaw` ∨ an `interactive`
flag), or the existing `AgentSession`-vs-`AgentRun` choice made explicit? (§7.)

---

## 4. Webterm + attach — a Turn-Model / session-layer capability **[PROPOSED]**

Attach is **not a turn.** Two distinct planes on a resident session pod:

- **Turn plane** *(exists)* — programmatic, ordered, checkpointed request→response:
  `POST /v1/sessions/{ns}/{name}/turns` → NATS → worker → `TurnExecutor.Execute` → result.
- **Attach plane** *(new)* — an interactive **PTY** into the *same live pod*: a human
  watches/steers. Side-channel, not recorded as a turn.

```
 human ──ws/ssh──►  agentterminal gateway  ──(authz: AttachGrant)──►  session pod
                    (cmd/agentterminal)                               ├─ ttyd sidecar (loopback PTY)
                    record→AgentFS                                    ├─ tmux (persistent/shared)
                                                                      └─ agent process / shell
```

Design (folds in [terminal-exposure-http-ssh-tmux.md](../specs/terminal-exposure-http-ssh-tmux.md)):

- A **`ttyd` sidecar** (loopback-only) in the session/serving pod exposes a PTY into the
  agent's workspace; **tmux** gives persistent + multi-viewer attach.
- A **`cmd/agentterminal`** gateway proxies the user's websocket (and later SSH) to the
  sidecar, gated by an **`AttachGrant`** (SPIFFE/broker-scoped), **read-only by default**,
  *driver* mode behind an elevated grant; sessions **recorded** (asciinema → AgentFS).
- **Turns + attach coexist:** turns keep flowing while a human is attached; for a daemon
  (openclaw) attach is its console/canvas, for interactive claude-code it's the live TUI.
- **Security:** attach is the highest-privilege surface (interactive shell inside the
  sandbox), so kata + default-deny egress still bound it, recording is mandatory, and a
  **human identity source (OIDC) is an undesigned prerequisite** for driver-mode (§7).

Only **session-requiring** agents (§3) get an attach plane; one-shot `AgentRun`s never do.

---

## 5. Phased refactor + build plan

| Phase | What | Risk | Maps to |
|---|---|---|---|
| **P1** | Extract the `TurnExecutor` seam; split packages into `turnmodel` vs `runtime`. **Behavior-preserving** (build/vet/tests stay green). | low | this doc §2 |
| **P2** | Make cross-turn memory an explicit Turn-Model strategy (Hermes session-id / workspace / history). | med | §2.4, [agent-claude-code](../specs/agent-claude-code.md) |
| **P3** | First-class "requires a session": resident-pod lifecycle + the classification (CRD field or inferred). | med | §3, [agentsession-scaling-impl](../specs/agentsession-scaling-impl.md) |
| **P4** | **Webterm slice**: `ttyd` loopback sidecar on the serving path + gateway ws proxy, read-only. | med | §4, terminal-exposure |
| **P5** | Attach authz + SSH/tmux + recording + `AttachGrant` + human OIDC. | high | §4, terminal-exposure |

P1 is the "design + extract the seam" option from this conversation; P4 is the "webterm
slice". Each phase ships independently.

---

## 6. What this relates to / does not change

- **No public API break in P1** — `AgentRun`/`AgentSession` CRDs unchanged; the split is
  internal package + interface structure.
- The **harness contract** ([harness-authoring.md](harness-authoring.md)) is the Runtime
  layer's public face and is unchanged.
- The **existing session machinery** ([durable-session-architecture.md](durable-session-architecture.md))
  *is* the Turn-Model layer's current implementation — this names it, doesn't rewrite it.

---

## 7. Decisions (RESOLVED 2026-06-03 — see [decisions.md](decisions.md))

The maintainer interview settled every open item here:

1. **"Requires a session" representation → explicit CRD field.** Add
   `spec.session { required: bool, interactive: bool }` to the Agent (D4). `required` ⇒
   resident pod; `interactive` ⇒ attach plane.
2. **Human identity for attach → build it now, bundled OIDC.** Driver-mode (not just
   observe) ships in v1 (D5), authenticating against a **bundled self-hosted OIDC (Dex or
   Keycloak)** (D9) — SPIFFE stays the machine rail, this adds the human rail. So the
   OIDC IdP + `AttachGrant` are committed work (M4/M5), not a deferred unknown.
3. **Package boundary → `pkg/turnmodel`** (sibling to `pkg/agentruntime`); the runtime
   exports only `TurnExecutor`.
4. **Interactive claude-code → batch-turn now, resident-interactive later.** Cross-turn
   memory is provider-session (Hermes) + AgentFS workspace (CLI); **loop-mode resume +
   HITL continuation are deferred post-GA** (D6). The resident claude TUI variant follows
   the resumable-session work.
5. **Webterm tech → `ttyd` loopback sidecar** + tmux (confirmed).

Additional context from the interview that shapes this doc: **multi-tenant/untrusted**
(D1 — governance is mandatory), **both batch + interactive first-class** (D2 — build the
full split), **strict/fail-closed default** (D3), and **mid-scale ~100s concurrent** (D10
— per-tenant concurrency caps + queue at the Turn-Model layer). stdio MCP tools are
allowed only from a cluster allow-list (D11); dynamic creds via a `DynamicCredentialPolicy`
CRD (D8).
