package v1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// Team labels mark a run/session as part of an AgentTeam. The coordinator (or the
// event ingress) stamps them when it spawns a member; the run-pod builder reads
// TeamLabel to inject the team NATS context (so kind=task/teammate/teambus
// invokers activate), and the AgentTeam reconciler reads them to map an owned
// run/session back to its member. Canonical here (the API package both the
// operator and the agentgateway import).
const (
	// TeamLabel names the owning AgentTeam (set alongside the OwnerReference).
	TeamLabel = "runtime.agents.smol-agents.ai/team"
	// TeamMemberLabel marks a run/session as a named team member's worker.
	TeamMemberLabel = "runtime.agents.smol-agents.ai/team-member"
	// CloudEventIDAnnotation records the source CloudEvent id a per-event
	// coordinator run was created from (observability; the run name carries a
	// DNS-safe derivation of it).
	CloudEventIDAnnotation = "runtime.agents.smol-agents.ai/cloudevent-id"
)

// BuildCoordinatorRun builds the per-event coordinator AgentRun for an event-
// driven AgentTeam (rv3.1, docs/design/agentteam-event-driven.md): one run of
// the team's lead agent, instantiated PER inbound event (Knative-function style).
//
//   - spec.agentRef = team.spec.lead (the coordinator agent, loop mode)
//   - spec.input    = the event payload (the coordinator's objective)
//   - label TeamLabel=<team> so BuildAgentRunPod injects the team NATS context and
//     the coordinator's kind=task/kind=teammate/kind=agent invokers activate
//   - ownerRef → the AgentTeam (LITERAL uid — a downward-API metadata.uid would be
//     the wrong object, the A2A child-GC bug) so the run + its delegated member
//     subtree GC with the team
//   - name <team>-<eventID> so a redelivered event is an idempotent (AlreadyExists)
//     create, not a duplicate coordinator. eventID MUST be a DNS-1123 label
//     segment (the ingress derives one from the raw CloudEvent id).
func BuildCoordinatorRun(team *AgentTeam, eventID string, data json.RawMessage) *AgentRun {
	return &AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      team.Name + "-" + eventID,
			Namespace: team.Namespace,
			Labels:    map[string]string{TeamLabel: team.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         GroupVersion.String(),
				Kind:               "AgentTeam",
				Name:               team.Name,
				UID:                team.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: pure.AgentRunSpec{AgentRef: team.Spec.Lead, Input: data},
	}
}
