# Product Overview — smol-agents-agent-model

## Product Purpose

This spec defines **what an "agent" is** on the smol-agents platform
and **how a tenant declares one**, so that the platform can take a YAML
description of an agent's identity, model, tools, memory, and budget,
and produce verifiable Kubernetes-native executions.

It is a peer to (not derived from) `smol-agents-platform` and
`smol-agents-operator`: those describe the *substrate*; this spec
describes the *workload model* that runs on the substrate.

## Target Users

- **Application developers** who want to ship an agent (an LLM-driven
  function with tools and memory) to a Kubernetes cluster as
  declaratively as they ship a `Deployment`.
- **Platform engineers** who need an opinionated, schema-driven layer
  so tenants cannot deploy ill-formed agents.
- **Compliance / safety reviewers** who require the runtime to *prove*
  an agent's budget, tool allow-list, and identity guarantees, not
  just document them.

## Key Features

1. **Six CRDs** (`Agent`, `Tool`, `ModelProvider`, `AgentRun`,
   `AgentSession`, `AgentPolicy`) covering declaration, invocation,
   identity, and policy.
2. **Industry-aligned vocabulary** — Run states match OpenAI's
   Assistants API; Tools speak MCP; Identity uses SPIFFE / DID; OTel
   emission uses `gen_ai.*` semconv.
3. **Hard budget enforcement** — `MaxSteps`, `MaxTokens`,
   `MaxWallClockSeconds`, `MaxToolCalls` are evaluated before every
   step; the formal model proves the cap is never exceeded.
4. **Tool typing** — every Tool ships an input/output JSON Schema;
   the runtime rejects malformed calls before they reach the tool.
5. **Determinism + replay** — every Run captures a step log; given a
   seed, the runtime can replay a Run to bit-identical output.
6. **Per-agent SPIFFE identity** — every Run Pod gets one SPIFFE ID;
   LLM-provider keys and tool credentials enter only via the
   secret-broker (R-SEC) using that identity.
7. **Sandbox by default** — every Run Pod inherits the platform's
   gVisor RuntimeClass (R-SBX-1).
8. **Verifiable contract** — Go types in `pkg/agentmodel`,
   property-tested with rapid; runtime in `pkg/agentruntime` with
   safety invariants modelled in Quint.

## Business Objectives

- Cut the time-to-first-running-agent from "stand up a custom service"
  to "submit one CR" — measured in minutes, not days.
- Enable safety reviews to be **mechanical**: every claim about budget,
  tool allow-list, and credential isolation is checkable against the
  formal model and the property tests, not against a wiki page.
- Provide a single Kubernetes-native interface that converges with
  the AGNTCY / kagent posture so future migrations stay cheap.

## Success Metrics

- **First-agent-up time**: ≤ 5 minutes from `kubectl apply -f
  agent.yaml` on a cluster with the platform installed.
- **Run cold-start P95**: ≤ 1.5 s on commodity hardware (Knative
  scale-to-zero).
- **Budget violation rate**: zero in the property suite over
  1 000+ runs with random budgets and random tool latencies.
- **Tool schema validation**: 100% of calls validated against
  declared schemas; zero "free-form arg" fallbacks.
- **OTel coverage**: every Run emits a parent span plus one child
  span per Step, with `gen_ai.*` attributes set.

## Product Principles

1. **Declarative over imperative** — what the agent *is*, not how
   it runs. The runtime is an implementation detail.
2. **Bounded by construction** — budgets are non-optional; defaults
   are conservative; the runtime enforces; the proof confirms.
3. **Tools are typed contracts** — schemas are declared in the CR,
   not discovered at first call.
4. **One identity per agent, no shared keys** — every Run inherits
   a fresh SPIFFE ID; secret material moves through the broker.
5. **Industry vocabulary** — Run states (Pending / Running /
   RequiresAction / Completed / Failed / Cancelled / Expired) match
   OpenAI's Assistants API to lower onboarding cost.
6. **Verifiable** — every safety claim is checkable in CI.

## Monitoring & Visibility

- `kubectl get agents -A -o wide` printer columns: `MODEL`,
  `PROVIDER`, `TOOLS`, `READY`, `RUNS`, `AGE`.
- `kubectl get agentruns -A -o wide` columns: `AGENT`, `STATE`,
  `STEPS`, `TOKENS`, `COST`, `STARTED`, `DURATION`.
- OTel traces with `gen_ai.*` attributes per Step; Prometheus
  metrics: `agent_run_total{agent,state}`,
  `agent_run_duration_seconds`, `agent_run_steps`,
  `agent_run_budget_exceeded_total`.

## Future Vision

- **Multi-agent handoffs** as a first-class tool kind.
- **Federated SPIFFE** so an agent can call tools across trust
  domains.
- **Replay UI** — a Web UI that visualises a Run's step log.
- **Marketplace** — an in-cluster registry of `Tool`s and
  `Agent`s that tenants can subscribe to.
