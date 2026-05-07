// Package agentruntime is the deterministic plan-act-observe executor
// that interprets an Agent CR.
//
// Key properties:
//
//   - Deterministic: no goroutines for in-loop work, no time.Now() outside
//     the injected Clock, all RNG seeded from AgentRun.Spec.Seed.
//   - Bounded: Budget is pre-checked before every step, including the
//     hypothetical token cost of the next plan; the loop transitions to
//     Expired the instant any axis is over.
//   - Auditable: every Step is appended to the in-memory log (which the
//     controller persists into AgentRun.Status). Tool inputs and outputs
//     are validated against their declared JSON Schemas; failures are
//     recorded as Step entries, never silently dropped.
//   - Verifiable: see pkg/agentruntime/property_test.go and
//     spec/quint/agent_execution.qnt for the safety invariants.
package agentruntime
