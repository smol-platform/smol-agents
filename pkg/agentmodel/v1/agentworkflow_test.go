package v1

import (
	"encoding/json"
	"testing"
)

func validWorkflow() AgentWorkflow {
	return AgentWorkflow{
		Name: "wf",
		Spec: AgentWorkflowSpec{
			Nodes: []WorkflowNode{
				{Name: "research", AgentRef: "researcher"},
				{Name: "review", AgentRef: "critic"},
			},
			Edges: []WorkflowEdge{
				{From: WorkflowStart, To: "research"},
				{From: "research", To: "review", When: "score >= 80"},
				{From: "research", To: WorkflowEnd, When: "score < 80"},
				{From: "review", To: WorkflowEnd},
			},
		},
	}
}

func TestValidateAgentWorkflow_OK(t *testing.T) {
	if err := ValidateAgentWorkflow(validWorkflow()); err != nil {
		t.Fatalf("valid workflow rejected: %v", err)
	}
}

func TestValidateAgentWorkflow_Rejections(t *testing.T) {
	cases := map[string]func(*AgentWorkflow){
		"no nodes":        func(w *AgentWorkflow) { w.Spec.Nodes = nil },
		"dup node name":   func(w *AgentWorkflow) { w.Spec.Nodes[1].Name = "research" },
		"reserved name":   func(w *AgentWorkflow) { w.Spec.Nodes[0].Name = "END" },
		"cross-ns ref":    func(w *AgentWorkflow) { w.Spec.Nodes[0].AgentRef = "other/researcher" },
		"edge to ghost":   func(w *AgentWorkflow) { w.Spec.Edges[1].To = "ghost" },
		"no START edge":   func(w *AgentWorkflow) { w.Spec.Edges[0].From = "research" },
		"bad predicate":   func(w *AgentWorkflow) { w.Spec.Edges[1].When = "score !! 80" },
		"usage predicate": func(w *AgentWorkflow) { w.Spec.Edges[1].When = "usage.tokens > 5" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			w := validWorkflow()
			mutate(&w)
			if err := ValidateAgentWorkflow(w); err == nil {
				t.Fatalf("%s: expected rejection", name)
			}
		})
	}
}

func TestValidateAgentWorkflow_Acyclic(t *testing.T) {
	w := validWorkflow()
	w.Spec.Edges = []WorkflowEdge{
		{From: WorkflowStart, To: "research"},
		{From: "research", To: "review"},
		{From: "review", To: "research"}, // cycle
	}
	if err := ValidateAgentWorkflow(w); err == nil {
		t.Fatal("a cycle must be rejected (workflow must be a DAG)")
	}
}

func TestPredicate_NumericCompare(t *testing.T) {
	p, err := CompilePredicate("score >= 80")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{`{"score":90}`, true},
		{`{"score":80}`, true},
		{`{"score":50}`, false},
		{`{"other":1}`, false},   // missing field → false (fail-closed)
		{`{"score":"x"}`, false}, // wrong type → false
	} {
		got, err := p.Eval(json.RawMessage(tc.out))
		if err != nil || got != tc.want {
			t.Fatalf("eval %s: got %v err=%v, want %v", tc.out, got, err, tc.want)
		}
	}
}

func TestPredicate_StringAndBoolAndNested(t *testing.T) {
	ps, _ := CompilePredicate(`result.status == "ok"`)
	if got, _ := ps.Eval(json.RawMessage(`{"result":{"status":"ok"}}`)); !got {
		t.Fatal("nested string eq should be true")
	}
	if got, _ := ps.Eval(json.RawMessage(`{"result":{"status":"err"}}`)); got {
		t.Fatal("nested string eq should be false")
	}
	pb, _ := CompilePredicate("done == true")
	if got, _ := pb.Eval(json.RawMessage(`{"done":true}`)); !got {
		t.Fatal("bool eq should be true")
	}
}

func TestPredicate_CompileErrors(t *testing.T) {
	for _, expr := range []string{
		"noop",           // no operator
		`name < "x"`,     // ordering on a string literal
		"score ~= 1",     // unknown operator
		"usage.cost > 0", // usage gating forbidden
	} {
		if _, err := CompilePredicate(expr); err == nil {
			t.Fatalf("CompilePredicate(%q) should error", expr)
		}
	}
}

func TestPredicate_NonJSONOutputErrors(t *testing.T) {
	p, _ := CompilePredicate("score > 1")
	if _, err := p.Eval(json.RawMessage("not json")); err == nil {
		t.Fatal("non-JSON output must error")
	}
}
