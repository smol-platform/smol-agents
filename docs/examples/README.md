# Examples — a walkthrough

Six worked scenarios that take the platform from "hello, tool" to multi-agent
composition, durable sessions, human approval, and the multi-tenant containment
floor. Each `.yaml` is runnable as-is; this page explains the YAML field by field
and walks through what actually happens at runtime.

Every scenario here is exercised by the fullstack-e2e suite on real
kata-capable bare metal (`test/e2e/fullstack/` — the `R-E2E-SCN-*` scenarios),
so the behaviour described below is the behaviour that's tested, not aspirational.

```
docs/examples/
  00-prerequisites.yaml     namespace + provider secret + ModelProvider (apply first)
  01-quickstart-tool.yaml   M2  a loop agent that calls an HTTP tool
  02-agent-to-agent.yaml    M3  one agent delegates to another (A2A)
  03-durable-session.yaml   M4  a long-running, checkpointed session
  04-approval-gate.yaml     M5  human pre-run approval
  05-governance.yaml        M1  AgentPolicy allow-list + kata isolation
  06-secretless-egress.yaml      credential-blind egress via the broker
  07-event-driven-knative.yaml   a CloudEvent (via a Knative Trigger) spawns a
                                 per-event team coordinator (epic t0d)
```

> Example 07 needs **Knative Serving + Eventing** in the cluster (the agentgateway
> is a Knative Service; the example adds a Broker + Trigger). To try the routing
> without Knative Eventing, `POST` a CloudEvent straight to the gateway's
> `/v1/events/{ns}` ingress — the `EventBinding` still does the work.

## The model in one paragraph

You declare an **Agent** (an LLM + instructions + a budget + a list of **Tool**s,
referencing a **ModelProvider**). You trigger work with an **AgentRun** (one
execution) or an **AgentSession** (a long-lived multi-turn worker). The operator
reconciles each run into a **sandboxed pod** (a Firecracker microVM by default),
runs the plan-act-observe loop, and folds the result into `status`. Secrets are
**broker-leased** into the pod over a UDS — the agent process never reads a
provider key or an egress credential. A namespace **AgentPolicy** and an
**AgentNetwork** are the multi-tenant guardrails on top.

## Apply

```bash
# cluster-scoped, once (installed by the platform operator):
kubectl apply -f operator/config/samples/smolagentplatform.yaml
kubectl apply -f operator/config/samples/agentnodepool_kata_arm64.yaml   # for kata runs

# shared by every example:
kubectl apply -f docs/examples/00-prerequisites.yaml

# then any scenario, e.g.:
kubectl apply -f docs/examples/01-quickstart-tool.yaml
kubectl -n tenant-a get agentrun research-001 -w
```

`00-prerequisites.yaml` creates the `tenant-a` namespace, the provider API key
Secret, and the `openai` ModelProvider. Point `ModelProvider.endpoint` at any
OpenAI-compatible gateway (vLLM, LiteLLM, z.ai, Ollama) if you don't have an
OpenAI key.

---

## 01 — Quickstart: a loop agent that calls a tool (M2)

**What it shows:** the plan-act-observe loop with a real tool call.

**The YAML.** Three objects:

- a **`Tool`** `web-search` of `kind: http`. The `inputSchema`/`outputSchema`
  are JSON Schemas the LLM sees — they tell it what arguments to send and what
  shape comes back. `http.auth.secretName` is leased by the broker; the agent
  never reads it.
- an **`Agent`** `researcher` (`mode` defaults to `loop`). It points at the
  `openai` ModelProvider, lists the `web-search` tool, and sets a hard
  **`budget`** — the executor fail-closes the run if it exceeds any ceiling.
- an **`AgentRun`** `research-001` with the `input` payload.

**Runtime walkthrough.**
1. The Agent reconciler resolves the provider + each Tool ref and reports the
   Agent `Ready`.
2. The AgentRun controller renders a run pod: a kata-fc microVM, the `agent`
   image running `agent run`, a secret-broker sidecar, and a `tools.json` mount
   carrying the resolved Tool catalog.
3. Inside the pod, the executor loops: **Plan** (ask the LLM) → it returns a
   tool call → **invoke** the HTTP tool (the broker leases `tavily-key` for the
   call) → feed the **Observation** back → the LLM **finalizes**.
4. The result is folded into `status` (`state: Completed`, `output`, `usage`,
   and per-step `trace`); the pod exits.

> Without the resolved `tools.json`, the executor would reject the call with
> "tool not found in catalog" — shipping the catalog into the pod is what makes
> loop-mode tools work as a pod (the fix that landed with the A2A work).

---

## 02 — Agent-to-agent composition (M3)

**What it shows:** one agent invoking another, bounded and same-tenant.

**The YAML.** A child Agent `summarizer`; a **`Tool` of `kind: agent`**
(`delegate-summarize`) whose `spec.agent.ref.name` points at the child; and a
parent Agent `orchestrator` that lists that tool. Declaring a `kind=agent` tool
is the trigger that makes the operator grant the parent its A2A authority.

**Runtime walkthrough.**
1. Because `orchestrator` has a `kind=agent` tool, its reconcile creates a
   namespaced **Role + RoleBinding** (`orchestrator-a2a`) bound to the parent's
   run ServiceAccount — granting exactly `create/get/list/watch` on `agentruns`
   **in this namespace only**.
2. The parent run pod is given an in-cluster client + its own identity
   (downward-API `POD_NAMESPACE`/`RUN_NAME`/`A2A_DEPTH`).
3. When the LLM calls `delegate-summarize`, the in-pod **AgentRunInvoker**
   creates a **child `AgentRun`** for `summarizer` (labelled
   `agents.smol-agents.ai/parent-run=compose-001`), blocks while polling it to a
   terminal state, and returns the child's `output` as the tool **Observation**.
4. The parent's budget rolls up the child's tokens/tool-calls; the parent
   finalizes.

**Trust & safety (D1).** The child is created in the **same namespace** only;
each agent gets its own sandbox, budget, broker config, and SPIFFE id; recursion
is bounded (depth-1 by default, fail-closed). Watch it:

```bash
kubectl -n tenant-a get agentruns -l agents.smol-agents.ai/parent-run=compose-001
```

---

## 03 — A durable, long-running session (M4)

**What it shows:** a resident worker that keeps conversation state.

**The YAML.** An Agent `assistant` that **must** declare `storage.agentfs`
(serve-session needs a workspace), and an **`AgentSession`** `chat` referencing
it with `idleTimeoutSeconds`, `maxConcurrentTurns`, and `turnHistoryLimit`.

**Runtime walkthrough.**
1. The AgentSession controller resolves the Agent, the sandbox, and the secret,
   then renders a **1-replica Deployment** running `agent serve-session` on a
   kata node — with an AgentFS restore **init** container + a serving **sidecar**
   and the secret broker.
2. The worker opens its `/workspace`, processes turns from its inbox (NATS via
   the gateway, or an on-disk inbox in the gateway-less default), and
   **checkpoints** turns + usage after each — so a crash/restart resumes.
3. `status.phase` goes `Pending → Running`; `status.reason` carries fail-closed
   causes when it can't start (`NoKVMCapacity`, `SecretMissing`, …). On idle it
   parks and scales to zero; the next turn revives it.

For a stateless chat the ephemeral workspace is fine; uncomment
`storage.agentfs.backup.s3` for cross-restart durability (kopia-backed
content-addressed snapshots).

---

## 04 — Human-in-the-loop pre-run approval (M5)

**What it shows:** a run that won't start until a human says yes.

**The YAML.** The Agent `deploy-bot` carries an `approval` policy
(`requireApprovalBeforeRun: true`, `approvalTimeoutSeconds`). The AgentRun can
override it per-run (`spec.requireApprovalBeforeRun`), and the human verdict is
patched into `spec.decision` **later** — not set up front.

**Runtime walkthrough.**
1. The controller mints a one-time **token**, sets `status.state:
   RequiresAction` and `status.pendingAction.token`, and creates **no pod**.
2. A human reads the token and patches `spec.decision` with it:
   - `approve: true` → the gate releases and the run proceeds normally;
   - `approve: false` → terminal `Cancelled` (`terminationReason` carries
     `decision:denied`), still no pod;
   - no decision before the timeout → `Expired`, still no pod.
3. The token must match — a stale/forged decision is ignored.

```bash
TOKEN=$(kubectl -n tenant-a get agentrun deploy-bot-001 -o jsonpath='{.status.pendingAction.token}')
kubectl -n tenant-a patch agentrun deploy-bot-001 --type=merge \
  -p "{\"spec\":{\"decision\":{\"token\":\"$TOKEN\",\"approve\":true,\"reason\":\"LGTM\"}}}"
```

---

## 05 — Governance & containment floor (M1)

**What it shows:** the multi-tenant guardrails that are *mandatory* under the
platform's strict/fail-closed posture (D1/D3).

**The YAML.** An **`AgentPolicy`** `default` for the namespace
(`allowedProviders`, `allowedTools`, a per-run `budget` ceiling) and an Agent
pinned to `sandbox.runtimeClass: kata-fc`.

**Runtime walkthrough.**
- **AgentPolicy is enforced twice:** an **admission webhook** rejects an Agent
  (or AgentRun) referencing a provider/tool outside the allow-list at apply
  time; a **reconcile backstop** re-checks on every reconcile, so *tightening*
  the policy flips already-admitted Agents to `Failed/PolicyViolation`. Try it:
  point the Agent at a provider not in `allowedProviders` and apply — admission
  refuses it.
- **kata-fc isolation:** the run executes in a Firecracker microVM with its own
  kernel, scheduled onto an **AgentNodePool** advertising that isolation. With
  no matching pool the run holds at `Pending/NoKVMCapacity` — it **never** falls
  back to an unisolated runtime.
- **default-deny egress** is the floor for every run pod (DNS + in-cluster +
  HTTP(S)-only public, metadata/link-local blocked). Examples 02 and 06 layer an
  allow-list on top via an AgentNetwork.

---

## 06 — Secretless egress (credential-blind agents)

**What it shows:** the agent calls an external API with a real credential it
never possesses.

**The YAML.** An **`AgentNetwork`** of `kind: identityProxy` selecting the
tenant's agent pods. Its `identityProxy.resources[].credential` declares the
intent (`github:repo:read`); the agent's Tool points at the **local sidecar
port** (`127.0.0.1:9200`), not GitHub.

**Runtime walkthrough.**
1. The agent makes a plain request to the in-pod sidecar.
2. The sidecar mints a **Transaction-Token** (RFC 8693) via the TTS, scoped to
   the declared intent and audienced at this trust domain — never sent upstream.
3. It hands the TraT to the **secret broker**, which verifies signature,
   audience, expiry, and that the caller's attested SPIFFE id matches — then
   mints a **short-lived GitHub App token** for the requested repo.
4. The sidecar injects `Authorization: Bearer <token>` and forwards to
   `api.github.com`. **eBPF drops** any egress not on the allow-list, so the
   credential can't be exfiltrated to another host.

The agent code holds **no token**; the credential is bounded to one
transaction's intent and a few minutes of validity.

---

## What you'll see in `status`

| field | meaning |
|---|---|
| `AgentRun.status.state` | `Pending` / `RequiresAction` / `Running` / `Completed` / `Failed` / `Cancelled` / `Expired` |
| `AgentRun.status.output` | the folded final answer (JSON) |
| `AgentRun.status.usage` | steps / tokens / toolCalls / wallClock (cost is observability-only) |
| `AgentRun.status.terminationReason` | the most specific cause (`budget:tokens`, `decision:denied`, …) |
| `Agent.status.phase` / `.reason` | `Ready`, or a fail-closed cause (`ToolKindUnsupported`, `PolicyViolation`, …) |
| `AgentSession.status.phase` / `.reason` | `Pending`/`Running`, or `NoKVMCapacity`/`SecretMissing`/… |

## Notes & gotchas

- **`mode: harness`** swaps the native loop for a CLI agent (claude-code, codex,
  pi, …) — see `operator/config/samples/agent_*.yaml`. Tools and the A2A invoker
  are loop-mode features.
- **Danger flags** (`--dangerously-skip-permissions`, `danger-full-access`) are
  opt-in *and* admission-refused unless the resolved sandbox is a kata microVM.
- **Cross-tenant** anything (A2A target, AgentNetwork selection) is refused by
  design; everything is namespace-scoped.

## Running these for real with z.ai (live-verified 2026-06-06)

These were exercised end-to-end on real **z.ai glm-4.6** on a local kind cluster
(multi-run, session-per-request, idle pause/resume, file-state recovery,
config/settings injection). Things you'll hit when pointing them at z.ai:

- **Provider secret.** Create `zai-key` with a **single** key `ZAI_API_KEY`
  (`kubectl -n <ns> create secret generic zai-key --from-literal=ZAI_API_KEY=<key>`).
  The `ModelProvider.spec.secretRef` reads the secret's *sole* key — its CRD has no
  `key` field today, so don't add a second key.
- **z.ai Coding Plan vs pay-as-you-go.** The pay-as-you-go OpenAI endpoint
  `https://api.z.ai/api/paas/v4/...` returns `429 code 1113 "insufficient balance"`. A
  Coding-Plan account uses `https://api.z.ai/api/coding/paas/v4/...` (OpenAI-compatible,
  for `mode: loop`) and `https://api.z.ai/api/anthropic` (for `harness: claude-code`).
- **Loop path bridge.** The loop client posts to `<endpoint>/v1/chat/completions`, but
  z.ai's path is `/api/coding/paas/v4/chat/completions`. Point `ModelProvider.endpoint`
  at a tiny in-cluster proxy that rewrites `/v1/chat/completions` → the z.ai coding path.
- **Kataless cluster (kind).** Start the operator with
  `--default-run-runtime-class=runc --allow-host-runtime`, else every run holds at
  `Pending/NoKVMCapacity` (the default class is `kata-fc`).
- **claude-code file writes need kata.** On `runc`, claude-code can't create files
  headlessly — `approvalMode: acceptEdits` / `cli.allowedTools` are *not* enough, and the
  only flag that works (`--dangerously-skip-permissions`) is D3-refused unless the runtime
  is a kata microVM. So examples 03/04 (claude-code writing files) need a kata cluster;
  AgentFS *recovery* (delete the worker pod → fresh pod restores from minio) demonstrates
  file-state durability on `runc` without writing via the CLI.
- **Sessions (03).** AgentSession workers run the **loop** (not a CLI harness), need NATS +
  `agentgateway` (operator `--session-nats-url`), and on the `0.2.1` images require
  `storage.agentfs.backup.s3` with `sizeGiB > 0` (ephemeral workspaces are fixed only on
  HEAD). Submit turns via `POST /v1/sessions/{ns}/{name}/turns`.
- **Token accounting.** A claude-code agent reports `usage.tokens=0` unless you set
  `cli.outputFormat: json`.

The full local bring-up recipe (operator flags, minio, NATS/gateway, the z.ai proxy) is in
the repo `CLAUDE.md`.
