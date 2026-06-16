package v1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// WorkflowTemplateLabel links a per-event workflow instance back to the template
// AgentWorkflow it was cloned from (observability / a join key).
const WorkflowTemplateLabel = "runtime.agents.smol-agents.ai/workflow-template"

// AgentWorkflow is the K8s-native wrapper around the pure AgentWorkflowSpec — a
// declarative, result-routed DAG of agent steps (LangGraph StateGraph). The
// operator materializes each ready node as a child AgentRun and routes on the
// node's terminal output via the operator-evaluated predicate DSL.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=awf
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.spec.nodes[*]`,priority=1
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AgentWorkflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.AgentWorkflowSpec   `json:"spec"`
	Status pure.AgentWorkflowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentWorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentWorkflow `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentWorkflow{}, &AgentWorkflowList{})
}

// BuildWorkflowInstance clones a (paused) template AgentWorkflow into a fresh
// per-event instance for an event-driven workflow (v9h, docs/design/event-
// intake.md): one un-paused workflow instantiated PER inbound event (the
// AgentWorkflow analog of BuildCoordinatorRun for teams).
//
//   - name <workflow>-<eventID> so a redelivered event is an idempotent
//     (AlreadyExists) create, not a duplicate. eventID MUST be a DNS-1123 label.
//   - the spec is deep-copied and UN-paused (the instance runs; the template stays
//     dormant), and the event payload is injected as the entry node's input so the
//     event drives the workflow.
//   - ownerRef → the template (LITERAL uid) so a deleted template GCs its instances
//     (the workflow analog of the AgentTeam subtree GC).
//   - label WorkflowTemplateLabel=<template> ties the instance to its template.
func BuildWorkflowInstance(tmpl *AgentWorkflow, eventID string, data json.RawMessage) *AgentWorkflow {
	spec := *tmpl.Spec.DeepCopy()
	spec.Paused = false
	injectEntryInput(&spec, data)
	return &AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tmpl.Name + "-" + eventID,
			Namespace: tmpl.Namespace,
			Labels:    map[string]string{WorkflowTemplateLabel: tmpl.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         GroupVersion.String(),
				Kind:               "AgentWorkflow",
				Name:               tmpl.Name,
				UID:                tmpl.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: spec,
	}
}

// injectEntryInput sets the event payload as the input of the workflow's entry
// node (the node START points to), so a per-event instance is driven by the
// event. With no event data, or no single unambiguous START edge, the template's
// static node inputs are kept unchanged (don't guess which node gets the data).
func injectEntryInput(spec *pure.AgentWorkflowSpec, data json.RawMessage) {
	if len(data) == 0 {
		return
	}
	entry, count := "", 0
	for _, e := range spec.Edges {
		if e.From == pure.WorkflowStart {
			entry, count = e.To, count+1
		}
	}
	if count != 1 {
		return
	}
	for i := range spec.Nodes {
		if spec.Nodes[i].Name == entry {
			spec.Nodes[i].Input = data
			return
		}
	}
}
