# Tasks — knative-agents-agent-model

## Phase 1 — API surface (`pkg/agentmodel/v1/`)

- [x] 1. Author canonical lifecycle, budget, and usage types
  - File: `pkg/agentmodel/v1/lifecycle.go`, `budget.go`, `usage.go`
  - _Requirements: R-AM-LIF-1, R-AM-BUD-1, R-AM-BUD-2_

- [x] 2. Author `Agent`, `AgentRun`, `AgentSession`, `Tool`,
  `ModelProvider`, `AgentPolicy` types
  - Files: `pkg/agentmodel/v1/types_*.go`
  - _Requirements: R-AM-API-1..6_

- [x] 3. JSON Schema validation helpers
  - File: `pkg/agentmodel/v1/schema.go`
  - _Requirements: R-AM-TOOL-1_

- [x] 4. Validation entrypoints (validating webhook bodies)
  - File: `pkg/agentmodel/v1/validation.go`,
    `pkg/agentmodel/v1/validation_test.go`
  - _Requirements: R-AM-API-1..6, R-AM-BUD-1_

- [x] 5. DeepCopy + scheme registration
  - File: `pkg/agentmodel/v1/zz_deepcopy.go`,
    `pkg/agentmodel/v1/register.go`
  - _Requirements: standard k8s_

## Phase 2 — Runtime contract (`pkg/agentmodel/runtime/`)

- [x] 6. Wire contract types: `StepRequest`, `StepResponse`,
  `ToolCall`, `Observation`, `FinalAnswer`, `StepAudit`
  - File: `pkg/agentmodel/runtime/contract.go`
  - _Requirements: R-AM-LIF-1, R-AM-OBS-1_

## Phase 3 — Executor (`pkg/agentruntime/`)

- [x] 7. `LLM`, `ToolInvoker`, `Clock` interfaces with fakes
  - File: `pkg/agentruntime/iface.go`, `fake.go`
  - _Requirements: R-AM-DET-1_

- [x] 8. Plan-act-observe executor with budget pre-check
  - File: `pkg/agentruntime/executor.go`
  - _Requirements: R-AM-BUD-2, R-AM-LIF-1_

- [x] 9. OTel GenAI emission
  - File: `pkg/agentruntime/otel.go`
  - _Requirements: R-AM-OBS-1_

- [x] 10. Property tests
  - File: `pkg/agentruntime/property_test.go`
  - _Requirements: R-AM-VRF-2_

## Phase 4 — Formal model

- [x] 11. Quint model
  - File: `spec/quint/agent_execution.qnt`
  - _Requirements: R-AM-VRF-1_

## Phase 5 — CRD wiring (deferred to operator)

- [x] 12. Operator reconciler that maps `Agent` to runtime Pod
  - File: under `operator/internal/controllers/features/agentmodel.go`
    when the operator is built (per `knative-agents-operator` spec)
  - _Requirements: R-AM-API-*_

## Validation Matrix

| Requirement      | Code reference                                       | Test reference                                         |
|------------------|------------------------------------------------------|--------------------------------------------------------|
| R-AM-API-1       | pkg/agentmodel/v1/types_agent.go                     | pkg/agentmodel/v1/validation_test.go                   |
| R-AM-API-2       | pkg/agentmodel/v1/types_tool.go                      | pkg/agentmodel/v1/validation_test.go                   |
| R-AM-API-3       | pkg/agentmodel/v1/types_provider.go                  | pkg/agentmodel/v1/validation_test.go                   |
| R-AM-API-4       | pkg/agentmodel/v1/types_run.go                       | pkg/agentmodel/v1/validation_test.go                   |
| R-AM-API-5       | pkg/agentmodel/v1/types_session.go                   | (covered by Run tests)                                 |
| R-AM-API-6       | pkg/agentmodel/v1/types_policy.go                    | pkg/agentmodel/v1/validation_test.go                   |
| R-AM-BUD-1       | pkg/agentmodel/v1/budget.go                          | pkg/agentmodel/v1/budget_test.go                       |
| R-AM-BUD-2       | pkg/agentruntime/executor.go                         | pkg/agentruntime/property_test.go::Property_BudgetNeverExceeded |
| R-AM-BUD-3       | pkg/agentruntime/executor.go (Usage tally)           | pkg/agentruntime/executor_test.go                      |
| R-AM-TOOL-1      | pkg/agentmodel/v1/schema.go                          | pkg/agentruntime/property_test.go::Property_OnlyAllowedTools |
| R-AM-TOOL-2      | secret-broker integration (existing pkg/secrets)     | pkg/secrets unit tests + integration                   |
| R-AM-TOOL-3      | pkg/agentruntime/iface.go (MCPInvoker interface)     | (transport-specific unit tests once implementation ships) |
| R-AM-LIF-1       | pkg/agentmodel/v1/lifecycle.go                       | pkg/agentmodel/v1/lifecycle_test.go                    |
| R-AM-LIF-2       | pkg/agentruntime/executor.go cancellation path       | pkg/agentruntime/executor_test.go                      |
| R-AM-DET-1       | pkg/agentruntime/executor.go (seed plumbing)         | pkg/agentruntime/property_test.go::Property_Determinism |
| R-AM-OBS-1       | pkg/agentruntime/otel.go                             | pkg/agentruntime/otel_test.go                          |
| R-AM-SEC-1       | identity binding via Operator Pod template           | covered in operator e2e (future)                       |
| R-AM-SEC-2       | sandbox propagation via Operator Pod template        | covered in operator e2e (future)                       |
| R-AM-VRF-1       | spec/quint/agent_execution.qnt                       | quint run --invariant=Safety                           |
| R-AM-VRF-2       | pkg/agentruntime/property_test.go                    | rapid suite                                            |
