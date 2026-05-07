// Package v1 defines the agent-model CRD types and the runtime contract
// types used by the executor.
//
// The shape comes from the market research in
// docs/research/agent-models.md. Concretely:
//
//   - Agent          — declarative agent definition.
//   - Tool           — typed callable capability (mcp/http/agent/function).
//   - ModelProvider  — LLM provider config (credentials via secret-broker).
//   - AgentRun       — bounded execution; mirrors OpenAI Assistants Run states.
//   - AgentSession   — long-running aggregation of Runs.
//   - AgentPolicy    — namespace- or cluster-wide guards.
//
// All time-bounded fields use seconds (int32) on the wire so JSON Schema
// stays simple; conversions to time.Duration happen at the package
// boundary.
//
// DeepCopy: controller-gen generates most methods. The few types
// containing json.RawMessage (which controller-gen mis-handles)
// have hand-rolled methods in deepcopy.go.
//
// +kubebuilder:object:generate=true
package v1
