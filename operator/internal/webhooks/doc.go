// Package webhooks holds the operator's admission webhooks for the
// agent-model CRD family (AgentNetwork, AgentPolicy, AgentSession,
// AgentTeam, AgentWorkflow, Tool, ...).
//
// Implementations are pure functions with no client dependency, plus
// thin sigs.k8s.io/controller-runtime adapters that call them. This
// keeps the rule set unit-testable with no envtest.
package webhooks
