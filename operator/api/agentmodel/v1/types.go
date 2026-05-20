package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// Agent is the K8s-native wrapper around the pure agent spec. The CR
// declares model, instructions, tools, and a non-optional Budget.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ag
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.name`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.model.providerRef`
// +kubebuilder:printcolumn:name="Tools",type=integer,JSONPath=`.spec.tools`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.AgentSpec   `json:"spec"`
	Status pure.AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

// Tool is a reusable callable capability (mcp/http/agent/function).
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=atool
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Tool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec pure.ToolSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type ToolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tool `json:"items"`
}

// ModelProvider records LLM provider config + a secret-broker reference.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=mp
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`
type ModelProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec pure.ModelProviderSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type ModelProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelProvider `json:"items"`
}

// AgentRun is a single bounded execution of an Agent against an input.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=arun
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Steps",type=integer,JSONPath=`.status.usage.steps`
// +kubebuilder:printcolumn:name="Tokens",type=integer,JSONPath=`.status.usage.tokens`
// +kubebuilder:printcolumn:name="Started",type=date,JSONPath=`.status.startedAt`
type AgentRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.AgentRunSpec `json:"spec"`
	Status pure.RunStatus    `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRun `json:"items"`
}

// AgentSession aggregates Runs that share memory.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=asess
type AgentSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.AgentSessionSpec   `json:"spec"`
	Status pure.AgentSessionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSession `json:"items"`
}

// AgentPolicy declares cluster- or namespace-wide guards.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=apol
type AgentPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec pure.AgentPolicySpec `json:"spec"`
}

// +kubebuilder:object:root=true
type AgentPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&Agent{}, &AgentList{},
		&Tool{}, &ToolList{},
		&ModelProvider{}, &ModelProviderList{},
		&AgentRun{}, &AgentRunList{},
		&AgentSession{}, &AgentSessionList{},
		&AgentPolicy{}, &AgentPolicyList{},
	)
}
