package builders

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

// rv3.1: each inbound event instantiates one coordinator run of the team's lead,
// carrying the event payload + the team label + a team-owned ref so it (and its
// A2A subtree) GC with the team. Name is <team>-<eventID> for idempotency.
func TestBuildCoordinatorRun(t *testing.T) {
	team := &amv1.AgentTeam{ObjectMeta: metav1.ObjectMeta{Name: "squad", Namespace: "tenant-a", UID: types.UID("team-uid-1")}}
	team.Spec.Lead = "orchestrator"
	data := json.RawMessage(`{"prompt":"summarize the incident"}`)

	run := BuildCoordinatorRun(team, "evt-42", data)

	if run.Name != "squad-evt-42" {
		t.Errorf("name = %q, want squad-evt-42 (per-event idempotency)", run.Name)
	}
	if run.Namespace != "tenant-a" {
		t.Errorf("namespace = %q, want tenant-a", run.Namespace)
	}
	if run.Spec.AgentRef != "orchestrator" {
		t.Errorf("agentRef = %q, want the team lead 'orchestrator'", run.Spec.AgentRef)
	}
	if string(run.Spec.Input) != string(data) {
		t.Errorf("input = %s, want the event payload %s", run.Spec.Input, data)
	}
	if run.Labels[TeamLabel] != "squad" {
		t.Errorf("team label = %q, want squad (so BuildAgentRunPod injects team env)", run.Labels[TeamLabel])
	}
	if len(run.OwnerReferences) != 1 {
		t.Fatalf("ownerRefs = %d, want 1 (the team)", len(run.OwnerReferences))
	}
	or := run.OwnerReferences[0]
	if or.Kind != "AgentTeam" || or.Name != "squad" || or.UID != types.UID("team-uid-1") {
		t.Errorf("ownerRef = %+v, want AgentTeam/squad with the team's LITERAL uid", or)
	}
	if or.Controller == nil || !*or.Controller {
		t.Error("ownerRef.Controller must be true (the team is the controlling owner / GC root)")
	}
}
