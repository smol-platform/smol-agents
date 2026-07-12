// Package v1 wraps the pure pkg/agentmodel/v1 specs as Kubernetes
// CRDs (Agent, Tool, ModelProvider, AgentRun, AgentSession,
// AgentPolicy). Implements R-AM-API-1..6.
//
// The split is deliberate: pkg/agentmodel/v1 has no Kubernetes
// dependencies and can be imported by non-K8s consumers (the agent
// runtime image, third-party controllers). This package is the K8s
// adapter — adding TypeMeta, ObjectMeta, DeepCopy, and scheme
// registration on top.
//
// +kubebuilder:object:generate=true
// +groupName=runtime.agents.smol-agents.ai
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is `runtime.agents.smol-agents.ai/v1`. We use a different
// group from the operator's `agents.smol-agents.ai/v1` (AgentNodePool) so
// the two specs evolve independently.
var GroupVersion = schema.GroupVersion{Group: "runtime.agents.smol-agents.ai", Version: "v1"}

// SchemeBuilder is used to add go types to the scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the types in this group-version to the given scheme.
var AddToScheme = SchemeBuilder.AddToScheme
