package v1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelRef points at a `ModelProvider` plus a model name on it.
type ModelRef struct {
	// ProviderRef is the namespace-local name of a ModelProvider.
	ProviderRef string `json:"providerRef"`

	// Name is the provider-specific model identifier.
	Name string `json:"name"`

	// Parameters that are forwarded to the provider verbatim.
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens *int32   `json:"maxOutputTokens,omitempty"`
}

// ToolRef references a Tool by name. The Agent's allow-list of tools.
type ToolRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"` // empty = same as Agent
}

// MemoryRef points at a MemoryStore.
type MemoryRef struct {
	Name string `json:"name"`
}

// IdentitySpec configures the SPIFFE binding for the Run Pod. R-AM-SEC-1.
type IdentitySpec struct {
	// SPIFFEIDPrefix lets tenants share a sub-tree (e.g. group runs by team).
	// The platform appends `/agent/<name>/run/<runid>`.
	SPIFFEIDPrefix string `json:"spiffeIDPrefix,omitempty"`
}

// SandboxSpec inherits the platform default unless overridden. R-AM-SEC-2.
type SandboxSpec struct {
	RuntimeClass string `json:"runtimeClass,omitempty"`
}

// AgentSpec — what the agent IS. Implements R-AM-API-1.
type AgentSpec struct {
	// Mode selects the execution path: "loop" (default) or "harness".
	// +optional
	Mode AgentMode `json:"mode,omitempty"`

	// Model is required when Mode==loop and ignored when Mode==harness.
	Model ModelRef `json:"model,omitempty"`

	// Harness is required when Mode==harness and ignored otherwise.
	// +optional
	Harness *HarnessSpec `json:"harness,omitempty"`

	Instructions string     `json:"instructions"`
	Tools        []ToolRef  `json:"tools,omitempty"`
	Memory       *MemoryRef `json:"memory,omitempty"`

	// Storage attaches persistent state (AgentFS today). Required for
	// SessionPersistent harnesses.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`

	Budget                       Budget       `json:"budget"`
	Identity                     IdentitySpec `json:"identity,omitempty"`
	Sandbox                      SandboxSpec  `json:"sandbox,omitempty"`
	GracefulCancelTimeoutSeconds int32        `json:"gracefulCancelTimeoutSeconds,omitempty"`
}

// AgentStatus is reported by the controller.
type AgentStatus struct {
	Phase              string   `json:"phase,omitempty"` // "Pending" | "Ready" | "Failed"
	ObservedGeneration int64    `json:"observedGeneration,omitempty"`
	ResolvedTools      []string `json:"resolvedTools,omitempty"`
	ResolvedProvider   string   `json:"resolvedProvider,omitempty"`
	Reason             string   `json:"reason,omitempty"`
	Message            string   `json:"message,omitempty"`
}

// Agent is the namespaced top-level CR.
type Agent struct {
	Spec   AgentSpec   `json:"spec"`
	Status AgentStatus `json:"status,omitempty"`
}

// ToolKind enumerates the transports the runtime knows how to drive.
// R-AM-API-2.
type ToolKind string

const (
	ToolMCP      ToolKind = "mcp"
	ToolHTTP     ToolKind = "http"
	ToolAgent    ToolKind = "agent"
	ToolFunction ToolKind = "function"
)

// Valid returns true if k is a known ToolKind.
func (k ToolKind) Valid() bool {
	switch k {
	case ToolMCP, ToolHTTP, ToolAgent, ToolFunction:
		return true
	}
	return false
}

// MCPSpec describes an MCP transport target. R-AM-TOOL-3.
type MCPSpec struct {
	URL  string   `json:"url"`            // mcp://… or http(s)://…/mcp
	Auth *AuthRef `json:"auth,omitempty"` // optional secret broker reference
}

// HTTPSpec describes a generic HTTP+JSON tool target.
type HTTPSpec struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"` // default POST
	Auth    *AuthRef          `json:"auth,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// AgentTargetSpec lets one Agent invoke another.
type AgentTargetSpec struct {
	Ref ToolRef `json:"ref"`
}

// FunctionSpec is for in-process functions (test only).
type FunctionSpec struct {
	Name string `json:"name"`
}

// AuthRef points at a secret in the broker.
type AuthRef struct {
	SecretName string `json:"secretName"`

	// Key is the key within the referenced Secret that holds the value. When
	// empty and the Secret has exactly one key, that key is used. Consumed by
	// the broker config generator (R-SEC) to populate the static backend.
	// +optional
	Key string `json:"key,omitempty"`
}

// ToolSpec is a discriminated union by Kind.
type ToolSpec struct {
	Kind         ToolKind         `json:"kind"`
	Description  string           `json:"description,omitempty"`
	InputSchema  json.RawMessage  `json:"inputSchema"`
	OutputSchema json.RawMessage  `json:"outputSchema"`
	MCP          *MCPSpec         `json:"mcp,omitempty"`
	HTTP         *HTTPSpec        `json:"http,omitempty"`
	Agent        *AgentTargetSpec `json:"agent,omitempty"`
	Function     *FunctionSpec    `json:"function,omitempty"`
}

// Tool is a namespaced reusable capability.
type Tool struct {
	Name string   `json:"name"`
	Spec ToolSpec `json:"spec"`
}

// ModelProvider — credentials + endpoint. R-AM-API-3.
type ModelProvider struct {
	Name string            `json:"name"`
	Spec ModelProviderSpec `json:"spec"`
}

type ModelProviderSpec struct {
	Kind      string  `json:"kind"`               // openai | anthropic | bedrock | vertex | local
	Endpoint  string  `json:"endpoint,omitempty"` // override default
	SecretRef AuthRef `json:"secretRef"`
}

// AgentRun — single bounded execution. R-AM-API-4.
type AgentRun struct {
	Name   string       `json:"name"`
	Spec   AgentRunSpec `json:"spec"`
	Status RunStatus    `json:"status,omitempty"`
}

type AgentRunSpec struct {
	AgentRef       string          `json:"agentRef"`
	SessionRef     string          `json:"sessionRef,omitempty"`
	Input          json.RawMessage `json:"input"`
	Seed           int64           `json:"seed,omitempty"`
	BudgetOverride *Budget         `json:"budgetOverride,omitempty"`
	Cancel         bool            `json:"cancel,omitempty"`

	// MemoryRetrieverRef is the namespace-local name of a MemoryRetriever CR
	// whose filesystem store should be mounted into the agent pod when
	// MemoryRetriever.spec.mount.enabled is true (R-MEM-FS-2).
	// The run controller resolves this to a MemoryRetriever and calls
	// builders.AttachMemoryFS on the pod before creating it.
	// Empty means no memory filesystem is mounted.
	// +optional
	MemoryRetrieverRef string `json:"memoryRetrieverRef,omitempty"`
}

// Step is a single plan-act-observe iteration.
type Step struct {
	Index     int32            `json:"index"`
	Kind      StepKind         `json:"kind"`
	StartedAt metav1.Time      `json:"startedAt"`
	EndedAt   metav1.Time      `json:"endedAt,omitempty"`
	TokensIn  int64            `json:"tokensIn"`
	TokensOut int64            `json:"tokensOut"`
	ToolCalls []ToolCallRecord `json:"toolCalls,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// StepKind discriminates Step types.
type StepKind string

const (
	StepPlan                StepKind = "Plan"                // LLM decision
	StepToolCall            StepKind = "ToolCall"            // tool invocation
	StepToolCallRejected    StepKind = "ToolCallRejected"    // failed input validation
	StepObservation         StepKind = "Observation"         // tool result accepted
	StepObservationRejected StepKind = "ObservationRejected" // tool result failed schema
	StepFinal               StepKind = "Final"               // emitted final answer
)

// ToolCallRecord captures a single tool invocation inside a Step.
type ToolCallRecord struct {
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	DurationMs int64           `json:"durationMs"`
}

// RunStatus is the live state of an AgentRun.
type RunStatus struct {
	State             Phase           `json:"state"`
	StartedAt         *metav1.Time    `json:"startedAt,omitempty"`
	EndedAt           *metav1.Time    `json:"endedAt,omitempty"`
	Steps             []Step          `json:"steps,omitempty"`
	Usage             Usage           `json:"usage"`
	TerminationReason string          `json:"terminationReason,omitempty"`
	Output            json.RawMessage `json:"output,omitempty"`
}

// AgentSession — long-running aggregation. R-AM-API-5.
type AgentSession struct {
	Name   string             `json:"name"`
	Spec   AgentSessionSpec   `json:"spec"`
	Status AgentSessionStatus `json:"status,omitempty"`
}

type AgentSessionSpec struct {
	AgentRef string `json:"agentRef"`
}

type AgentSessionStatus struct {
	Runs []string `json:"runs,omitempty"` // names of completed AgentRuns
}

// AgentPolicy — guardrails. R-AM-API-6.
type AgentPolicy struct {
	Name string          `json:"name"`
	Spec AgentPolicySpec `json:"spec"`
}

type AgentPolicySpec struct {
	AllowedProviders []string         `json:"allowedProviders,omitempty"`
	AllowedTools     []string         `json:"allowedTools,omitempty"` // tool names
	MaxBudget        *Budget          `json:"maxBudget,omitempty"`
	Redaction        *RedactionPolicy `json:"redaction,omitempty"`
}

type RedactionPolicy struct {
	Patterns []string `json:"patterns,omitempty"` // regex patterns
}
