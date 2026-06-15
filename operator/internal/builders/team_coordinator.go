package builders

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// BuildCoordinatorRun builds the per-event coordinator AgentRun for an event-
// driven AgentTeam (rv3.1, docs/design/agentteam-event-driven.md): one run of
// the team's lead agent, instantiated PER inbound event (Knative-function style).
//
//   - spec.agentRef = team.spec.lead (the coordinator agent, loop mode)
//   - spec.input    = the event payload (the coordinator's objective)
//   - label team=<team> so BuildAgentRunPod injects the team NATS context and the
//     coordinator's kind=task/kind=teammate/kind=agent invokers activate (slice 1)
//   - ownerRef → the AgentTeam (LITERAL uid — a downward-API metadata.uid would be
//     the wrong object, the A2A child-GC bug) so the run + its delegated member
//     subtree GC with the team
//   - name <team>-<eventID> so a redelivered event is an idempotent (AlreadyExists)
//     create, not a duplicate coordinator
func BuildCoordinatorRun(team *amv1.AgentTeam, eventID string, data json.RawMessage) *amv1.AgentRun {
	return &amv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      team.Name + "-" + eventID,
			Namespace: team.Namespace,
			Labels:    map[string]string{TeamLabel: team.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         amv1.GroupVersion.String(),
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
