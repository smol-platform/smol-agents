# Requirements — smol-agents-agent-model

Each requirement carries a stable ID and references the lower-level
platform requirements it depends on (`R-IDN-*`, `R-SEC-*`, `R-SBX-*`).

## Alignment with Product Vision

These requirements implement `product.md`'s six principles:
declarative > imperative, bounded by construction, typed tools, one
identity per agent, industry vocabulary, verifiable.

## Requirements

### R-AM-API: API Surface

#### R-AM-API-1 — `Agent` CR
**User Story:** As a developer, I want one CR that fully describes my
agent (model, instructions, tool refs, budget, identity).

**Acceptance Criteria:**
1. WHEN an `Agent` is submitted THE validating webhook SHALL reject
   it unless every required field is set: `spec.model.providerRef`,
   `spec.model.name`, `spec.instructions`, `spec.budget`.
2. THE `Agent` CR SHALL include printer columns `MODEL`, `PROVIDER`,
   `TOOLS`, `READY`, `RUNS`, `AGE`.
3. THE CR SHALL be served at `agents.smol-agents.ai/v1`.

#### R-AM-API-2 — `Tool` CR
**User Story:** As a developer, I want to declare a tool as a separate
CR so it can be reused across agents.

**Acceptance Criteria:**
1. THE `Tool` CR SHALL declare `kind` (one of `mcp`, `http`,
   `agent`, `function`), `inputSchema` (JSON Schema), `outputSchema`
   (JSON Schema), and a transport-specific `spec` block.
2. WHEN an `Agent` references a Tool that does not exist THEN the
   Agent's status SHALL be `Pending` with reason `ToolMissing`.

#### R-AM-API-3 — `ModelProvider` CR
**User Story:** As an admin, I want to register an LLM provider once
and reference it from many Agents.

**Acceptance Criteria:**
1. THE `ModelProvider` CR SHALL declare `kind` (one of `openai`,
   `anthropic`, `bedrock`, `vertex`, `local`), `endpoint`, and a
   reference to a secret in the broker (`secretRef` →
   `secrets.Backend` name).
2. THE provider's credentials SHALL NOT be inlined in the CR.

#### R-AM-API-4 — `AgentRun` CR
**User Story:** As a developer, I want one CR per agent invocation.

**Acceptance Criteria:**
1. THE `AgentRun` CR SHALL carry `agentRef`, `input`, `seed`
   (optional), and a snapshot of the budget at submission time.
2. THE `AgentRun.status.state` SHALL be exactly one of: `Pending`,
   `Running`, `RequiresAction`, `Completed`, `Failed`, `Cancelled`,
   `Expired`.
3. THE `AgentRun.status.steps[]` SHALL record every Step with
   `index`, `kind`, `tokensIn`, `tokensOut`, `toolCalls[]`,
   `startedAt`, `endedAt`.

#### R-AM-API-5 — `AgentSession` CR
**User Story:** As a developer, I want a session that aggregates
many Runs sharing memory.

**Acceptance Criteria:**
1. THE `AgentSession` SHALL carry an `agentRef` and zero or more
   completed `AgentRun` references in its status.
2. WHEN a new `AgentRun` is submitted with `sessionRef` set THEN
   the session SHALL be appended to.

#### R-AM-API-6 — `AgentPolicy` CR
**User Story:** As an admin, I want cluster- or namespace-wide guards.

**Acceptance Criteria:**
1. THE `AgentPolicy` CR SHALL be cluster- or namespace-scoped and
   declare: `allowedProviders[]`, `allowedTools[]` (label
   selector), `maxBudget`, `redaction.rules[]`.
2. WHEN an `Agent` violates an applicable policy THEN the validating
   webhook SHALL reject it.

### R-AM-BUD: Budget enforcement

#### R-AM-BUD-1 — Required fields
**User Story:** As a safety reviewer, I want budgets to be mandatory.

**Acceptance Criteria:**
1. AN `Agent` SHALL declare `spec.budget.maxSteps`,
   `spec.budget.maxTokens`, `spec.budget.maxWallClockSeconds`,
   `spec.budget.maxToolCalls`. All four SHALL be > 0.
2. THE validating webhook SHALL reject Agents missing any of the
   four budget fields.

#### R-AM-BUD-2 — Pre-step enforcement
**User Story:** As an SRE, I want budgets evaluated *before* each
step, not on best-effort accounting.

**Acceptance Criteria:**
1. BEFORE every Step the runtime SHALL check
   `(steps_so_far+1 ≤ maxSteps) ∧
    (tokens_so_far ≤ maxTokens) ∧
    (now-started ≤ maxWallClockSeconds) ∧
    (toolCalls_so_far ≤ maxToolCalls)`.
2. WHEN any inequality fails THE Run SHALL transition to `Expired`
   with the offending budget reported in `status.terminationReason`.

#### R-AM-BUD-3 — Audit trail
**User Story:** As a reviewer, I want budget telemetry per Run.

**Acceptance Criteria:**
1. THE `AgentRun.status.usage` SHALL contain the totals for each
   budget axis at termination.
2. A Prometheus counter `agent_run_budget_exceeded_total{axis}` SHALL
   increment once per `Expired` termination, labelled by axis.

### R-AM-TOOL: Tool typing & invocation

#### R-AM-TOOL-1 — Schemas mandatory
**User Story:** As a developer, I want every tool I invoke to have a
declared schema.

**Acceptance Criteria:**
1. AN `Agent.spec.tools[]` entry MUST reference a `Tool` whose
   `inputSchema` and `outputSchema` validate as JSON Schema 2020-12.
2. THE runtime SHALL reject any tool call whose arguments fail
   `inputSchema` validation; the Run's Step SHALL record
   `kind=ToolCallRejected`.

#### R-AM-TOOL-2 — Identity-bound credentials
**User Story:** As a security engineer, I want tool calls to use the
agent's SPIFFE identity.

**Acceptance Criteria:**
1. WHEN the runtime calls a tool that needs credentials THE runtime
   SHALL acquire a short-lived lease from the secret-broker (R-SEC)
   using the Agent's SPIFFE ID.
2. NO long-lived API key SHALL be stored in the Run Pod.

#### R-AM-TOOL-3 — MCP transport
**User Story:** As a developer, I want my MCP tools to plug in.

**Acceptance Criteria:**
1. WHEN `Tool.spec.kind == mcp` THE runtime SHALL invoke the tool via
   MCP (stdio or HTTP) using the URL declared in `spec.url`.

### R-AM-LIF: Run lifecycle

#### R-AM-LIF-1 — State machine
**User Story:** As a tenant, I want Runs to follow a stable state
machine.

**Acceptance Criteria:**
1. THE runtime SHALL transition `AgentRun.status.state` only along
   the edges:
   - `Pending → Running`
   - `Running → RequiresAction → Running`
   - `Running → Completed | Failed | Cancelled | Expired`
   - `RequiresAction → Cancelled | Expired`
2. ANY other transition SHALL be a controller error and SHALL leave
   the previous state unchanged.

#### R-AM-LIF-2 — Cancellation
**User Story:** As an operator, I want to cancel a stuck Run.

**Acceptance Criteria:**
1. SETTING `AgentRun.spec.cancel: true` SHALL cause the runtime to
   transition the Run to `Cancelled` and stop emitting Steps within
   the Agent's `gracefulCancelTimeout` (default 5 s).

### R-AM-DET: Determinism

#### R-AM-DET-1 — Seedable runs
**User Story:** As a debugger, I want bit-exact replay.

**Acceptance Criteria:**
1. WHEN `AgentRun.spec.seed` is set THE runtime SHALL pass the seed
   to the LLM provider (where supported) and to all internal RNGs.
2. RE-running an `AgentRun` with the same seed AND the same Agent
   spec AND the same Tool versions SHALL yield identical step logs
   when tool side effects are pure.

### R-AM-OBS: Observability

#### R-AM-OBS-1 — OTel GenAI semconv
**User Story:** As an SRE, I want standard tracing.

**Acceptance Criteria:**
1. EACH Run SHALL emit a parent span with attributes
   `gen_ai.operation.name=invoke_agent`, `gen_ai.agent.name`,
   `gen_ai.agent.version`, `gen_ai.provider.name`,
   `gen_ai.request.model`.
2. EACH Step SHALL emit a child span with `gen_ai.operation.name`
   in {`chat`, `tool_call`, `embeddings`}, plus token counters.

### R-AM-SEC: Identity & isolation

#### R-AM-SEC-1 — Per-agent SPIFFE
**User Story:** As a security engineer, I want every Run Pod issued
its own SPIFFE ID.

**Acceptance Criteria:**
1. WHEN the runtime starts a Run Pod THE Pod SHALL be matched by a
   `ClusterSPIFFEID` whose path includes the Agent's name and
   namespace.
2. NO shared SPIFFE ID SHALL serve two distinct Agents.

#### R-AM-SEC-2 — Sandbox inheritance
**User Story:** As a security engineer, I want every Run sandboxed.

**Acceptance Criteria:**
1. THE Run Pod SHALL inherit the platform's
   `sandbox.runtimeClass` (R-SBX-1).

### R-AM-VRF: Verification

#### R-AM-VRF-1 — Quint model
**User Story:** As a reviewer, I want safety properties proven.

**Acceptance Criteria:**
1. `spec/quint/agent_execution.qnt` SHALL include invariants
   `BudgetNeverExceeded`, `OnlyAllowedToolsCalled`,
   `LifecycleProgresses`, `RunsTerminate`, all checkable via
   `quint run --invariant=Safety`.

#### R-AM-VRF-2 — Property tests
**User Story:** As a developer, I want runtime invariants under
load.

**Acceptance Criteria:**
1. `pkg/agentruntime` SHALL include rapid-driven property tests
   that:
   - never exceed `maxSteps`,
   - never call a Tool not in the agent's allow-list,
   - always terminate within `maxWallClockSeconds + ε`,
   - always validate tool inputs against `inputSchema`.

## Non-Functional Requirements

### Performance
- Runtime per-step overhead ≤ 5 ms (excluding LLM and tool latency).
- Schema validation per call ≤ 1 ms for typical schemas.

### Reliability
- The runtime SHALL be deterministic: given a seed and pure tools,
  identical inputs produce identical outputs.

### Compatibility
- Tool transport MUST support MCP 2025-06.
- LLM providers MUST follow OpenAI-compatible message format OR
  Anthropic Messages API.
- All telemetry MUST follow OTel GenAI semconv 1.34+.

### Security
- Zero plaintext provider keys in pod env or filesystem.
- All Run Pods inherit gVisor RuntimeClass.
- Tool calls cross trust boundaries only via the secret-broker.
