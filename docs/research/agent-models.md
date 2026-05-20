# Market Research — How Industry Models an "Agent"

This is the synthesis we used to design `SmolAgents`'s agent CRD set.
It captures the dominant abstractions across nine ecosystems as of 2026
Q2 and condenses them into a small set of primitives that map cleanly
onto Kubernetes-native objects.

## 1. Frameworks surveyed

| Ecosystem                          | Primary primitives                                                        | Execution model                                       | Notes                                                                |
|------------------------------------|---------------------------------------------------------------------------|-------------------------------------------------------|----------------------------------------------------------------------|
| **OpenAI Agents SDK / Assistants** | `Assistant`, `Thread`, `Run`, `Step`, `Tool` (function / file_search / code_interpreter) | Cloud-managed event loop; client polls Runs           | Defines `Run.status` lifecycle (queued, in_progress, requires_action, completed, failed, cancelled, expired). |
| **Anthropic Claude Agent SDK**     | `Agent`, `Session`, `MCP server` (tool host), `Subagent`                  | In-process loop; tool calls via MCP; sub-agents recursive | MCP is the standard transport; tools advertise JSON schemas.          |
| **Model Context Protocol (MCP)**   | `Server`, `Tool`, `Resource`, `Prompt`, `Sampling`                         | JSON-RPC over stdio/HTTP/WS                           | Industry-standard tool transport; orthogonal to agent model.         |
| **Agent2Agent (A2A) Protocol**     | `Agent Card`, `Task`, `Message`, `Artifact`, `Session`                    | HTTP+JSON-RPC inter-agent calls                       | Google-led, focused on multi-agent communication.                    |
| **AGNTCY / Internet of Agents**    | `Agent Identity (DID)`, `Capability`, `Skill`, `Network`                  | Registry + discovery + comms                          | Cisco-led 2025 spec; emphasises identity (DID/SPIFFE) and discovery. |
| **LangGraph**                      | `Graph`, `Node`, `Edge`, `State`, `Checkpoint`                            | Stateful graph; nodes are functions or agents         | First-class checkpoint persistence; fan-in / fan-out.                |
| **AutoGen (Microsoft)**            | `Agent`, `GroupChat`, `Tool`, `WorkflowTermination`                       | Conversation-driven multi-agent                       | Roles + termination conditions.                                      |
| **CrewAI**                         | `Crew`, `Agent` (role/goal/backstory), `Task`, `Tool`, `Process`          | Sequential or hierarchical orchestration              | Pre-defined "process" plays out tasks across crew.                   |
| **kagent.dev (Solo.io)**           | K8s CRDs: `Agent`, `ToolServer`, `ModelConfig`, `AgentLogs`               | In-cluster, Kubernetes-native                         | Closest existing CRD prior art; uses MCP for tools.                  |
| **Temporal AI / Inngest AI**       | `Workflow`, `Step`, `Activity`, `Determinism`                             | Durable execution                                     | Treats an agent run as a workflow; resumable on crash.               |

### Sources
- OpenAI Assistants API ([platform.openai.com/docs/assistants](https://platform.openai.com/docs/assistants))
- Anthropic Agent SDK + MCP spec ([modelcontextprotocol.io](https://modelcontextprotocol.io), `docs.anthropic.com/agent-sdk`)
- A2A protocol announcement (Google 2025)
- AGNTCY / Internet of Agents (Cisco / Outshift Labs 2025)
- LangGraph docs (`langchain-ai.github.io/langgraph`)
- AutoGen ([microsoft.github.io/autogen](https://microsoft.github.io/autogen))
- CrewAI ([docs.crewai.com](https://docs.crewai.com))
- kagent.dev project (Solo.io 2024-)
- OpenTelemetry GenAI semantic conventions
  ([opentelemetry.io/docs/specs/semconv/gen-ai/](https://opentelemetry.io/docs/specs/semconv/gen-ai/))

## 2. Convergent primitives

Across these nine ecosystems, the same conceptual primitives keep appearing
under different names. Normalising to the names we adopt:

| Concept              | Definition                                                             | Lives where             |
|----------------------|------------------------------------------------------------------------|-------------------------|
| **Agent**            | Declarative "what the agent is": model, instructions, allowed tools, allowed providers, identity. | `Agent` CRD             |
| **Tool**             | A callable capability with a typed input/output schema. May be a local function, an MCP server, an HTTP endpoint, or another `Agent`. | `Tool` CRD              |
| **Model provider**   | Credentials + endpoint for an LLM service.                             | `ModelProvider` CRD     |
| **Run**              | A single bounded execution: input → loop → output, with budget caps.   | `AgentRun` CRD          |
| **Session**          | A long-running conversation referencing many Runs sharing memory.      | `AgentSession` CRD      |
| **Memory**           | Pluggable backend for episodic / semantic / KV memory.                 | `MemoryStore` CRD       |
| **Policy**           | Guardrails: allowed tools, allowed providers, budget, redaction rules. | `AgentPolicy` CRD       |
| **Plan / Step**      | One iteration of decide-act-observe. Implementation detail of the loop. | not a CRD; runtime concept |

## 3. Execution model — what every framework agrees on

Every surveyed framework boils its execution down to a variant of
**plan-act-observe**:

```
state = initial(input, system_prompt, memory)
while not done:
    decision = LLM(state, tools_schema)            # plan
    if decision.is_final_answer:
        emit_output(decision.answer); break
    obs = invoke_tool(decision.tool_call)          # act
    state = state.append(decision, obs)            # observe
```

Differences are in how the loop is *bounded* and *audited*:

- **Bounded**: max steps, wall-clock timeout, token budget, cost budget.
- **Audited**: every Step is recorded with timing, tokens, tool args, and
  tool result fingerprint.
- **Resumable**: checkpoints between steps so a crash mid-run does not
  re-run side-effecting tools (LangGraph, Temporal AI).

We adopt all three and make them part of the `AgentRun` contract.

## 4. Identity, isolation, and side effects

A common gap across the cloud-managed ecosystems (OpenAI Assistants,
LangGraph Cloud) is **per-agent identity for tool calls**. They typically
ship one shared API key per tenant. AGNTCY, kagent, and the SPIFFE-aware
ecosystem treat each agent as a workload with its own SPIFFE ID; tool
calls happen *under that identity* with the secret-broker pattern we
already implement.

Our CRD aligns with the AGNTCY / kagent posture: the `Agent` CR's pod is
issued a SPIFFE ID, and the runtime calls tools (and the LLM provider!)
through the secret-broker so an agent never holds long-lived API keys.

## 5. Observability — settle on OTel GenAI semconv

The OpenTelemetry community has stabilised semantic conventions for
generative AI (`gen_ai.*` attributes: `gen_ai.agent.name`,
`gen_ai.agent.version`, `gen_ai.operation.name`,
`gen_ai.provider.name`, `gen_ai.request.model`, plus per-tool function
attributes). We emit only these. No bespoke schema.

## 6. Decisions for our CRD set

1. **Six CRDs**: `Agent`, `Tool`, `ModelProvider`, `AgentRun`,
   `AgentSession`, `AgentPolicy`. (Memory is an opaque reference;
   we don't introduce a CRD for it in v1.)
2. **`AgentRun` is the unit of work**, like `Pod`/`Job` — a run is a
   bounded execution of an `Agent` against a single input.
   `AgentSession` aggregates runs.
3. **Budgets are first-class**: `MaxSteps`, `MaxTokens`,
   `MaxWallClock`, `MaxToolCalls`. The runtime enforces; the formal
   model proves the cap is never exceeded.
4. **Tools are typed**: every `Tool` carries a JSON Schema for input
   and output. The runtime rejects calls that don't validate.
5. **No new tool transport**: we use MCP. A `Tool` CR can reference
   either an MCP server (URL) or an in-cluster Service.
6. **One agent ↔ one workload identity**: every Agent's runtime Pod
   gets a SPIFFE ID. Tool calls and LLM provider calls are
   credential-brokered.
7. **Determinism for replay**: `AgentRun.spec.seed` plus the durable
   step log gives bit-exact replay (LangGraph-style checkpoints).
8. **Run lifecycle states** mirror OpenAI Assistants: `Pending`,
   `Running`, `RequiresAction`, `Completed`, `Failed`, `Cancelled`,
   `Expired`. Reusing this vocabulary cuts onboarding cost for
   anyone coming from the major ecosystems.

## 7. What we explicitly do NOT model in v1

- **No graph / workflow CR** (LangGraph). The plan-act-observe loop
  covers most agents; users wanting graph orchestration can chain
  `AgentRun`s via the runtime's "handoff" tool.
- **No GroupChat CR** (AutoGen). Multi-agent collaboration is just
  one agent calling another via the `agent` tool kind.
- **No checkpoint CR**. Step logs in the `AgentRun.status` are
  enough; deeper durability lands later if needed.
- **No vector DB shape**. `MemoryStore` carries a URI + auth ref;
  the runtime negotiates the protocol (Pinecone, Qdrant, pgvector).

## 8. Comparative cheat-sheet

| Capability                           | OpenAI Assistants | Claude Agent SDK | LangGraph | CrewAI | kagent | **Ours** |
|--------------------------------------|-------------------|------------------|-----------|--------|--------|----------|
| Declarative agent definition          | ✓                 | partial          | ✓ (graph) | ✓     | ✓ CRD  | ✓ CRD     |
| Typed tools                           | ✓                 | MCP              | ✓        | ✓     | ✓     | ✓ JSONSchema |
| Run as first-class object             | ✓                 | partial          | ✓ Thread | partial | partial | ✓ CRD     |
| Hard budget enforcement               | partial           | ✗               | ✗        | ✗     | ✗     | ✓ formal   |
| Per-agent SPIFFE identity             | ✗                 | ✗               | ✗        | ✗     | partial| ✓ default |
| Secret-broker for LLM key             | ✗                 | ✗               | ✗        | ✗     | ✗     | ✓        |
| OTel GenAI semconv emission           | ✓ (cloud)         | ✓               | ✓        | partial| partial| ✓        |
| Verifiable lifecycle invariants       | ✗                 | ✗               | ✗        | ✗     | ✗     | ✓ Quint   |
| Sandbox containment (gVisor)          | ✗                 | ✗               | ✗        | ✗     | ✗     | ✓ default |

The combination on the right column is, as of 2026 Q2, unique to our
stack: OpenAI's surface area, AGNTCY-style identity, kagent's native
posture, and the formal-method discipline of safety-critical software.

## 9. Implementation outline

This research drives:

- `.spec-workflow/specs/smol-agents-agent-model/` — the spec.
- `pkg/agentmodel/` — the typed Go contract for all six CRDs plus the
  in-cluster runtime contract (`StepRequest`, `StepResponse`,
  `ToolCall`, `Observation`, `Budget`, `Lifecycle`).
- `pkg/agentruntime/` — the deterministic plan-act-observe executor
  with budget enforcement, OTel emission, and a `BudgetEnforced`
  property test that hammers the runtime with rapid generators.
- `spec/quint/agent_execution.qnt` — the formal model with safety
  invariants (`BudgetNeverExceeded`, `OnlyAllowedToolsCalled`,
  `RunsTerminate`, `LifecycleProgresses`).
