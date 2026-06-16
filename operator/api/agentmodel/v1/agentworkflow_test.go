package v1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func wfTemplate() *AgentWorkflow {
	return &AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "triage", Namespace: "tenant-a", UID: types.UID("wf-uid-1")},
		Spec: pure.AgentWorkflowSpec{
			Paused: true,
			Nodes: []pure.WorkflowNode{
				{Name: "classify", AgentRef: "classifier", Input: json.RawMessage(`{"static":"default"}`)},
				{Name: "act", AgentRef: "actor"},
			},
			Edges: []pure.WorkflowEdge{
				{From: pure.WorkflowStart, To: "classify"},
				{From: "classify", To: "act"},
				{From: "act", To: pure.WorkflowEnd},
			},
		},
	}
}

// v9h: each inbound event clones the paused template into a fresh un-paused
// per-event instance named <workflow>-<eventID>, owned by the template (subtree
// GC), with the event payload injected as the entry node's input.
func TestBuildWorkflowInstance(t *testing.T) {
	tmpl := wfTemplate()
	data := json.RawMessage(`{"incident":9001}`)

	inst := BuildWorkflowInstance(tmpl, "evt-7", data)

	if inst.Name != "triage-evt-7" {
		t.Errorf("name = %q, want triage-evt-7 (per-event idempotency)", inst.Name)
	}
	if inst.Namespace != "tenant-a" {
		t.Errorf("namespace = %q, want tenant-a", inst.Namespace)
	}
	if inst.Spec.Paused {
		t.Error("the per-event instance must be UN-paused so it runs")
	}
	if inst.Labels[WorkflowTemplateLabel] != "triage" {
		t.Errorf("template label = %q, want triage", inst.Labels[WorkflowTemplateLabel])
	}
	// Entry node (START → classify) gets the event payload; non-entry node untouched.
	if got := string(inst.Spec.Nodes[0].Input); got != string(data) {
		t.Errorf("entry node input = %s, want the event payload %s", got, data)
	}
	if inst.Spec.Nodes[1].Input != nil {
		t.Errorf("non-entry node input = %s, want nil (untouched)", inst.Spec.Nodes[1].Input)
	}
	if len(inst.OwnerReferences) != 1 {
		t.Fatalf("ownerRefs = %d, want 1 (the template)", len(inst.OwnerReferences))
	}
	or := inst.OwnerReferences[0]
	if or.Kind != "AgentWorkflow" || or.Name != "triage" || or.UID != types.UID("wf-uid-1") {
		t.Errorf("ownerRef = %+v, want AgentWorkflow/triage with the template's LITERAL uid", or)
	}
	if or.Controller == nil || !*or.Controller {
		t.Error("ownerRef.Controller must be true (the template is the GC root)")
	}

	// The clone must NOT mutate the template (deep copy): template stays paused and
	// its entry node keeps its static input.
	if !tmpl.Spec.Paused {
		t.Error("BuildWorkflowInstance must not un-pause the template")
	}
	if string(tmpl.Spec.Nodes[0].Input) != `{"static":"default"}` {
		t.Errorf("template entry input mutated: %s", tmpl.Spec.Nodes[0].Input)
	}
}

// With no event data the template's static node inputs are preserved.
func TestBuildWorkflowInstance_NoData(t *testing.T) {
	inst := BuildWorkflowInstance(wfTemplate(), "evt-8", nil)
	if string(inst.Spec.Nodes[0].Input) != `{"static":"default"}` {
		t.Errorf("entry node input = %s, want the template's static input (no event data)", inst.Spec.Nodes[0].Input)
	}
	if inst.Spec.Paused {
		t.Error("instance must still be un-paused")
	}
}

// Ambiguous entry (two START edges) → don't guess; leave node inputs unchanged.
func TestBuildWorkflowInstance_AmbiguousEntry(t *testing.T) {
	tmpl := wfTemplate()
	tmpl.Spec.Edges = append(tmpl.Spec.Edges, pure.WorkflowEdge{From: pure.WorkflowStart, To: "act"})
	inst := BuildWorkflowInstance(tmpl, "evt-9", json.RawMessage(`{"x":1}`))
	if string(inst.Spec.Nodes[0].Input) != `{"static":"default"}` {
		t.Errorf("entry node input = %s, want unchanged (ambiguous entry)", inst.Spec.Nodes[0].Input)
	}
}
