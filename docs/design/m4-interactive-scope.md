# M4 — interactive + long-running daemons: scope decision

> Status: **the durable-session core is done + live-verified; the interactive /
> attach / daemon plane is post-GA.** Recorded 2026-06-06 after the milestone
> audit, as the explicit "M4 scope decision" (the alternative — half-building ~15
> net-new tasks — is worse than a clear phased plan).

## What is done (and live-verified)

The substrate every M4 feature rides on is built and, as of 2026-06-06, verified
end-to-end on real glm-4.6 (kind, `.claude/live-zai/`):

- `AgentSession` controller → resident 1-replica worker (`agent serve-session`)
  on its sandbox class, with the AgentFS restore init + secret broker.
- Turn transport: on-disk inbox **and** NATS JetStream via `agentgateway`
  (`POST /v1/sessions/{ns}/{name}/turns`).
- Durable execution: checkpoint after each turn; **idle-pause** (worker exits at
  `idleTimeoutSeconds`) + **resume** (restart reloads the turn log); **AgentFS
  cross-pod recovery** from kopia/S3 (delete the pod → fresh pod restores → turn
  index continuity).
- `spec.session{required,interactive}` field + Go validation; `pi` rename
  (`inflection-pi`); generic run/turn deadline.

This is the foundation `framework-enhancements.md` / `turn-model-vs-runtime.md`
called the *session layer*. It is real and tested.

## What is deferred to post-GA (and why)

These are a single coherent **net-new** surface, not isolated fixes, and one of
them (human OIDC for attach) is an undesigned prerequisite. Building them safely
is multi-phase; none should be half-landed:

| Phase | Tasks | What it adds | Hard prereq |
|---|---|---|---|
| **P1** | M4.1, M4.2 | extract `pkg/turnmodel` + a `TurnExecutor` seam; cross-turn memory (provider session-id / prior-output replay) | none (internal refactor) |
| **P2** | M4.7–M4.13 | `terminal` sidecar (ttyd/tmux) + recording + PTY broker policy + in-pod sshd | P1 |
| **P3** | M4.5, M4.6, M4.10 | bundled OIDC (Dex/Keycloak) + `AttachGrant` CRD + signed-token minter + `cmd/agentterminal` attach gateway | **human OIDC is undesigned** (D5/D9) |
| **P4** | M4.15–M4.19 | `pi-mono` harness + `cmd/pi-bridge` + `harness-pi-mono` image | P1 |
| **P5** | M4.20–M4.24 | OpenClaw daemon harness + WS canvas/terminal route through the gateway | P2, P3 |

### Worker scale-to-zero

The controller ships a plain 1-replica `Deployment` (`agentsession_controller.go`
still notes `// Phase 4 ... Knative Service for scale-to-zero`). True
request-driven scale-to-zero for a **stateful** worker does not fit Knative's
stateless model and needs a turn-arrival signal the controller does not yet
observe (it would have to watch NATS stream depth and scale the Deployment
0↔1). The current **idle-exit + checkpoint + restart-on-next-turn** already
gives most of the benefit (the pod parks; AgentFS state survives). Promoting it
to true 0↔1 is tracked with P1.

### Small partials gated on the above

`M4.3` (webhook arm `interactive ⇒ non-knative`) and `M4.7` (webhook arm
`terminal.enabled ⇒ reject scale-to-zero`) can't be closed meaningfully until a
Knative/scale-to-zero path for the worker exists to reject — they are deferred
with that work, not independently.

## Decision

Ship GA on the **one-shot run + durable-session** surface (M1/M2/M3-A2A/M4-core/M5),
which is implemented and verified. The interactive **attach/terminal/daemon**
plane (P1–P5 above) is an explicit post-GA track; HermesGateway/attach already
appear on the README roadmap. Revisit P1 (turn-model split) first — it is the
only piece with no external prerequisite and it de-risks all of P2–P5.
