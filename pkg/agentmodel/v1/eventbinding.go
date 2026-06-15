package v1

import (
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EventBindingSpec routes a matched CloudEvent to a platform work target — the
// platform's namespaced analog of a Knative Trigger (docs/design/event-intake.md).
// The CloudEvents ingress (agentgateway) consults bindings to decide which
// Agent/AgentTeam/AgentSession/AgentWorkflow an inbound event instantiates work
// on. The target is same-namespace (D1 — no cross-tenant routing).
type EventBindingSpec struct {
	// Filter selects which CloudEvents this binding routes. Every SET attribute
	// must match (exact); an empty filter matches any event delivered to the
	// binding's namespace.
	// +optional
	Filter EventFilter `json:"filter,omitempty"`

	// Target is the work object an event instantiates work on.
	Target EventTarget `json:"target"`
}

// EventFilter matches on CloudEvents context attributes (exact match; empty = any).
type EventFilter struct {
	// Type matches the CloudEvent `type` (e.g. com.acme.incident.opened). +optional
	Type string `json:"type,omitempty"`
	// Source matches the CloudEvent `source`. +optional
	Source string `json:"source,omitempty"`
	// Subject matches the CloudEvent `subject`. +optional
	Subject string `json:"subject,omitempty"`
}

// EventTargetKind is the kind of platform work object an event drives.
type EventTargetKind string

const (
	EventTargetAgent         EventTargetKind = "Agent"
	EventTargetAgentTeam     EventTargetKind = "AgentTeam"
	EventTargetAgentSession  EventTargetKind = "AgentSession"
	EventTargetAgentWorkflow EventTargetKind = "AgentWorkflow"
)

// EventTarget names the same-namespace object an event instantiates work on.
type EventTarget struct {
	// Kind is the target object kind.
	// +kubebuilder:validation:Enum=Agent;AgentTeam;AgentSession;AgentWorkflow
	Kind EventTargetKind `json:"kind"`
	// Name is the target object in the binding's namespace (D1).
	Name string `json:"name"`
}

// EventBindingStatus is observability over the binding's dispatch activity.
type EventBindingStatus struct {
	// Phase is Ready (the target resolves) or Degraded (target missing). +optional
	Phase string `json:"phase,omitempty"`
	// LastEventID is the most recent CloudEvent id this binding dispatched. +optional
	LastEventID string `json:"lastEventID,omitempty"`
	// LastEventTime is when that event was dispatched. +optional
	LastEventTime *metav1.Time `json:"lastEventTime,omitempty"`
	// Dispatched / Failed are cumulative counters (observability, never gate). +optional
	Dispatched int64 `json:"dispatched,omitempty"`
	Failed     int64 `json:"failed,omitempty"`
}

// ValidateEventBinding fail-closes a malformed binding (the reconcile + admission
// backstop). A target kind/name is mandatory; the filter is optional.
func ValidateEventBinding(spec EventBindingSpec) error {
	if spec.Target.Name == "" {
		return errors.New("eventbinding: spec.target.name is required")
	}
	switch spec.Target.Kind {
	case EventTargetAgent, EventTargetAgentTeam, EventTargetAgentSession, EventTargetAgentWorkflow:
		return nil
	case "":
		return errors.New("eventbinding: spec.target.kind is required")
	default:
		return fmt.Errorf("eventbinding: spec.target.kind %q is invalid (Agent|AgentTeam|AgentSession|AgentWorkflow)", spec.Target.Kind)
	}
}

// Matches reports whether a CloudEvent (by its context attributes) is routed by
// this filter: every SET attribute must equal the event's; unset = wildcard.
func (f EventFilter) Matches(ceType, ceSource, ceSubject string) bool {
	if f.Type != "" && f.Type != ceType {
		return false
	}
	if f.Source != "" && f.Source != ceSource {
		return false
	}
	if f.Subject != "" && f.Subject != ceSubject {
		return false
	}
	return true
}
