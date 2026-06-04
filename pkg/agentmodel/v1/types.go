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

// SandboxSpec configures the pod RuntimeClass (R-AM-SEC-2). Empty inherits the
// operator's --default-run-runtime-class (default kata-fc); runc is rejected
// fail-closed unless the operator runs with --allow-host-runtime. AgentRun has
// no per-run override — it inherits the referenced Agent's sandbox.
type SandboxSpec struct {
	// RuntimeClass overrides the pod RuntimeClassName. Empty = operator default.
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

	Instructions string `json:"instructions"`

	// Tools is the Agent's allow-list of Tool references. NOTE: loop-mode tool
	// invocation is NOT wired end-to-end as of v0.2.0 — the operator resolves
	// these names but never ships the specs to the pod and the executor has no
	// invokers, so a loop agent's tool call is rejected at runtime. Only
	// harness-mode agents act on tools (via embedded harness logic). See
	// docs/design/tool-kinds-roadmap.md.
	Tools []ToolRef `json:"tools,omitempty"`

	// Memory is DEAD on the Agent as of v0.2.0 — no controller reads spec.memory
	// and no builder mounts it. Per-run memory is attached via
	// AgentRunSpec.MemoryRetrieverRef; durable state via spec.storage.
	Memory *MemoryRef `json:"memory,omitempty"`

	// Storage attaches persistent state (AgentFS today). Required for
	// SessionPersistent harnesses.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`

	Budget   Budget       `json:"budget"`
	Identity IdentitySpec `json:"identity,omitempty"`
	Sandbox  SandboxSpec  `json:"sandbox,omitempty"`

	// GracefulCancelTimeoutSeconds is DEPRECATED/UNUSED as of v0.2.0 — read by no
	// controller; cancel deletes the pod immediately. Slated for removal.
	GracefulCancelTimeoutSeconds int32 `json:"gracefulCancelTimeoutSeconds,omitempty"`
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
	ToolMCP  ToolKind = "mcp"  // external MCP server (no production invoker yet)
	ToolHTTP ToolKind = "http" // generic HTTP+JSON endpoint (no production invoker yet)
	// ToolAgent (agent-to-agent) and ToolFunction (in-process) are RESERVED: no
	// production invoker exists and loop-mode tool invocation is unimplemented as
	// of v0.2.0 (ToolFunction is test-only). See docs/design/tool-kinds-roadmap.md.
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

// AuthRef points at a Kubernetes Secret resolved by the operator at pod-build
// time — into the broker config (harness env) or a secretKeyRef (AgentFS backup
// creds). The raw value is never written into the pod spec.
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

// ModelProviderSpec holds the provider family + brokered credential. Kind drives
// credential brokering (operator/internal/controllers/agentmodel/secrets.go) and
// endpoint defaults. As of v0.2.0 the only loop-mode LLM client is the
// OpenAI-compatible one (pkg/agentruntime/openaillm); bedrock/vertex have no
// dedicated client, and anthropic is reachable only via an OpenAI/Anthropic-
// compatible endpoint or a harness (e.g. claude-code).
type ModelProviderSpec struct {
	// Kind: openai | anthropic | bedrock | vertex | local. Only openai-compatible
	// (and "local") have a working loop-mode client today.
	Kind string `json:"kind"`
	// Endpoint overrides the kind's default base URL; required for local.
	Endpoint string `json:"endpoint,omitempty"`
	// SecretRef is the API-key Secret, leased by the broker at runtime — never
	// embedded in the pod spec.
	SecretRef AuthRef `json:"secretRef"`
}

// AgentRun — single bounded execution. R-AM-API-4.
type AgentRun struct {
	Name   string       `json:"name"`
	Spec   AgentRunSpec `json:"spec"`
	Status RunStatus    `json:"status,omitempty"`
}

// AgentRunSpec is a single bounded execution of the referenced Agent. The pod
// sandbox (RuntimeClass) and egress policy are inherited from the Agent — there
// is no per-run sandbox override.
type AgentRunSpec struct {
	AgentRef string `json:"agentRef"`

	// SessionRef optionally associates this run with an AgentSession.
	SessionRef string `json:"sessionRef,omitempty"`

	// Input is the run's JSON payload. It rides the run spec, which the operator
	// marshals into a ~1 MiB ConfigMap — use Inputs[].secretRef for large or
	// secret payloads instead of inlining them here.
	Input json.RawMessage `json:"input"`

	Seed int64 `json:"seed,omitempty"`

	// BudgetOverride escalates the Agent's budget for THIS run. In harness mode
	// only maxTokens/maxWallClockSeconds bind at runtime; maxSteps and
	// maxToolCalls are computed post-hoc from the harness result.
	BudgetOverride *Budget `json:"budgetOverride,omitempty"`

	Cancel bool `json:"cancel,omitempty"`

	// PlacementFallback controls what happens when the run's sandbox class is a
	// microVM (kata) but no AgentNodePool provides that isolation. "Pending"
	// (default, fail-closed) holds the run until capacity appears; "Schedule"
	// lets it schedule without node affinity (a dev/unlabelled-cluster escape
	// hatch — the RuntimeClass is still pinned).
	// +kubebuilder:validation:Enum=Pending;Schedule
	// +optional
	PlacementFallback string `json:"placementFallback,omitempty"`

	// MemoryRetrieverRef is the namespace-local name of a MemoryRetriever CR
	// whose filesystem store should be mounted into the agent pod when
	// MemoryRetriever.spec.mount.enabled is true (R-MEM-FS-2).
	// The run controller resolves this to a MemoryRetriever and calls
	// builders.AttachMemoryFS on the pod before creating it.
	// Empty means no memory filesystem is mounted.
	// +optional
	MemoryRetrieverRef string `json:"memoryRetrieverRef,omitempty"`

	// Inputs are files materialized into the agent's workspace before the run
	// executes, so a harness/loop can work on "the files I gave you". Requires a
	// workspace (the Agent's storage.agentfs mount or harness.cli.workingDir);
	// a run with inputs but no workspace fails loud. Large or secret payloads
	// should use secretRef rather than inline (inputs ride the run spec, which
	// the operator marshals into a ~1 MiB ConfigMap).
	// +optional
	Inputs []RunInputFile `json:"inputs,omitempty"`
}

// RunInputFile is a single file seeded into the run workspace. Exactly one
// source must be set: Inline (UTF-8 text), InlineBase64 (binary), or SecretRef
// (leased from the broker at runtime, never written into the AgentRun spec).
type RunInputFile struct {
	// Path is the destination relative to the workspace root. Must be relative
	// with no ".." segment.
	Path string `json:"path"`

	// Inline is literal UTF-8 file content.
	// +optional
	Inline string `json:"inline,omitempty"`

	// InlineBase64 is base64-encoded file content (for binary payloads).
	// +optional
	InlineBase64 string `json:"inlineBase64,omitempty"`

	// SecretRef leases the file content from the broker at runtime, so the value
	// never sits in the AgentRun spec.
	// +optional
	SecretRef *AuthRef `json:"secretRef,omitempty"`
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

	// IdleTimeoutSeconds parks then exits the session worker after this idle
	// period so it can scale to zero; 0 (default) keeps it resident.
	// +optional
	IdleTimeoutSeconds int32 `json:"idleTimeoutSeconds,omitempty"`
}

type AgentSessionStatus struct {
	// Phase mirrors the session worker's lifecycle as observed by the operator
	// (Pending until the worker is available, then Running).
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// ObservedGeneration is the spec generation the controller last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Runs are the names of AgentRuns this session aggregated (legacy field).
	// +optional
	Runs []string `json:"runs,omitempty"`
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
