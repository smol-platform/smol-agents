package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
)

func eventsTestServer(t *testing.T, objs ...client.Object) (*httptest.Server, client.Client) {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	kc := ctrlfake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	g := &Gateway{Queue: sessionqueue.NewMemQueue(), K8s: kc}
	return httptest.NewServer(g.Handler()), kc
}

func makeTeam() *amv1.AgentTeam {
	team := &amv1.AgentTeam{ObjectMeta: metav1.ObjectMeta{Name: "squad", Namespace: "tenant-a", UID: types.UID("team-uid")}}
	team.Spec.Lead = "orchestrator"
	return team
}

// Binary-mode CloudEvent → 202 + a per-event coordinator run for the team's lead,
// carrying the event body as input + the team label + ownerRef + the ce-id annotation.
func TestPostTeamEvent_BinaryMode(t *testing.T) {
	srv, kc := eventsTestServer(t, makeTeam())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/teams/tenant-a/squad/events", strings.NewReader(`{"alert":"disk full"}`))
	req.Header.Set("Ce-Id", "pd/incident/9001")
	req.Header.Set("Ce-Type", "com.acme.incident.opened")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)

	var runs amv1.AgentRunList
	if err := kc.List(t.Context(), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("created %d runs, want 1", len(runs.Items))
	}
	run := runs.Items[0]
	if run.Spec.AgentRef != "orchestrator" {
		t.Errorf("agentRef = %q, want the lead", run.Spec.AgentRef)
	}
	if string(run.Spec.Input) != `{"alert":"disk full"}` {
		t.Errorf("input = %s, want the event body", run.Spec.Input)
	}
	if run.Labels[amv1.TeamLabel] != "squad" {
		t.Errorf("team label = %q, want squad", run.Labels[amv1.TeamLabel])
	}
	if run.Annotations[amv1.CloudEventIDAnnotation] != "pd/incident/9001" {
		t.Errorf("ce-id annotation = %q, want the raw id", run.Annotations[amv1.CloudEventIDAnnotation])
	}
	if len(run.OwnerReferences) != 1 || run.OwnerReferences[0].Kind != "AgentTeam" {
		t.Errorf("ownerRef = %+v, want the team", run.OwnerReferences)
	}
}

// Structured-mode CloudEvent (application/cloudevents+json) → 202.
func TestPostTeamEvent_StructuredMode(t *testing.T) {
	srv, kc := eventsTestServer(t, makeTeam())
	defer srv.Close()

	body := `{"specversion":"1.0","id":"abc-123","type":"t","source":"s","data":{"q":"hello"}}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/teams/tenant-a/squad/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/cloudevents+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var runs amv1.AgentRunList
	_ = kc.List(t.Context(), &runs)
	if len(runs.Items) != 1 || string(runs.Items[0].Spec.Input) != `{"q":"hello"}` {
		t.Fatalf("want 1 run with the structured data as input, got %+v", runs.Items)
	}
}

// A redelivered event (same id) is idempotent: second POST → 200 duplicate, still one run.
func TestPostTeamEvent_Idempotent(t *testing.T) {
	srv, kc := eventsTestServer(t, makeTeam())
	defer srv.Close()
	post := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/teams/tenant-a/squad/events", strings.NewReader(`{}`))
		req.Header.Set("Ce-Id", "dup-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if c := post(); c != http.StatusAccepted {
		t.Fatalf("first POST = %d, want 202", c)
	}
	if c := post(); c != http.StatusOK {
		t.Fatalf("redelivery = %d, want 200 (idempotent)", c)
	}
	var runs amv1.AgentRunList
	_ = kc.List(t.Context(), &runs)
	if len(runs.Items) != 1 {
		t.Fatalf("created %d runs for one event id, want 1 (idempotent)", len(runs.Items))
	}
}

func TestPostTeamEvent_TeamNotFound(t *testing.T) {
	srv, _ := eventsTestServer(t) // no team
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/teams/tenant-a/ghost/events", strings.NewReader(`{}`))
	req.Header.Set("Ce-Id", "x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPostTeamEvent_MissingID(t *testing.T) {
	srv, _ := eventsTestServer(t, makeTeam())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/teams/tenant-a/squad/events", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (no ce-id)", resp.StatusCode)
	}
}

func makeBinding(name, ns, filterType string, kind pure.EventTargetKind, target string) *amv1.EventBinding {
	return &amv1.EventBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: pure.EventBindingSpec{
			Filter: pure.EventFilter{Type: filterType},
			Target: pure.EventTarget{Kind: kind, Name: target},
		},
	}
}

func postCE(t *testing.T, url, id, ceType, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Ce-Id", id)
	if ceType != "" {
		req.Header.Set("Ce-Type", ceType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A matching EventBinding routes a CloudEvent to an AgentTeam → coordinator run.
func TestPostEvent_BindingRoutesToTeam(t *testing.T) {
	srv, kc := eventsTestServer(t, makeTeam(),
		makeBinding("incident-to-squad", "tenant-a", "com.acme.incident.opened", pure.EventTargetAgentTeam, "squad"))
	defer srv.Close()

	resp := postCE(t, srv.URL+"/v1/events/tenant-a", "ev-1", "com.acme.incident.opened", `{"sev":1}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var runs amv1.AgentRunList
	_ = kc.List(t.Context(), &runs)
	if len(runs.Items) != 1 || runs.Items[0].Spec.AgentRef != "orchestrator" {
		t.Fatalf("want 1 coordinator run for the team lead, got %+v", runs.Items)
	}
}

// A binding to an Agent target creates a plain AgentRun named <agent>-<token>.
func TestPostEvent_BindingRoutesToAgent(t *testing.T) {
	srv, kc := eventsTestServer(t, makeBinding("b", "tenant-a", "t", pure.EventTargetAgent, "summarizer"))
	defer srv.Close()
	resp := postCE(t, srv.URL+"/v1/events/tenant-a", "ev-2", "t", `{"text":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var runs amv1.AgentRunList
	_ = kc.List(t.Context(), &runs)
	if len(runs.Items) != 1 || runs.Items[0].Spec.AgentRef != "summarizer" {
		t.Fatalf("want 1 AgentRun for the agent target, got %+v", runs.Items)
	}
	if string(runs.Items[0].Spec.Input) != `{"text":"hi"}` {
		t.Errorf("input = %s, want the event data", runs.Items[0].Spec.Input)
	}
}

func makeWorkflow() *amv1.AgentWorkflow {
	return &amv1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "triage", Namespace: "tenant-a", UID: types.UID("wf-uid")},
		Spec: pure.AgentWorkflowSpec{
			Paused: true,
			Nodes:  []pure.WorkflowNode{{Name: "classify", AgentRef: "classifier"}},
			Edges:  []pure.WorkflowEdge{{From: pure.WorkflowStart, To: "classify"}, {From: "classify", To: pure.WorkflowEnd}},
		},
	}
}

// A binding to an AgentWorkflow target clones a fresh un-paused per-event instance
// (v9h), idempotent on the CloudEvent id.
func TestPostEvent_BindingRoutesToWorkflow(t *testing.T) {
	srv, kc := eventsTestServer(t, makeWorkflow(),
		makeBinding("triage-bind", "tenant-a", "com.acme.alert", pure.EventTargetAgentWorkflow, "triage"))
	defer srv.Close()

	resp := postCE(t, srv.URL+"/v1/events/tenant-a", "ev-wf-1", "com.acme.alert", `{"sev":2}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	var wfs amv1.AgentWorkflowList
	_ = kc.List(t.Context(), &wfs)
	var inst *amv1.AgentWorkflow
	for i := range wfs.Items {
		if wfs.Items[i].Name != "triage" { // the template stays; find the instance
			inst = &wfs.Items[i]
		}
	}
	if inst == nil {
		t.Fatalf("want a per-event workflow instance, got %d workflows", len(wfs.Items))
	}
	if inst.Spec.Paused {
		t.Error("the per-event instance must be un-paused so it runs")
	}
	if inst.Labels[amv1.WorkflowTemplateLabel] != "triage" {
		t.Errorf("instance template label = %q, want triage", inst.Labels[amv1.WorkflowTemplateLabel])
	}

	// Redelivering the same CloudEvent id is idempotent — no duplicate instance.
	resp2 := postCE(t, srv.URL+"/v1/events/tenant-a", "ev-wf-1", "com.acme.alert", `{"sev":2}`)
	resp2.Body.Close()
	var wfs2 amv1.AgentWorkflowList
	_ = kc.List(t.Context(), &wfs2)
	if len(wfs2.Items) != len(wfs.Items) {
		t.Errorf("redelivery created a duplicate: %d → %d workflows", len(wfs.Items), len(wfs2.Items))
	}
}

// An event with no matching binding (filter mismatch) → 404, nothing created.
func TestPostEvent_NoMatch(t *testing.T) {
	srv, kc := eventsTestServer(t, makeTeam(),
		makeBinding("only-incidents", "tenant-a", "com.acme.incident.opened", pure.EventTargetAgentTeam, "squad"))
	defer srv.Close()
	resp := postCE(t, srv.URL+"/v1/events/tenant-a", "ev-3", "com.other.thing", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no binding matches)", resp.StatusCode)
	}
	var runs amv1.AgentRunList
	_ = kc.List(t.Context(), &runs)
	if len(runs.Items) != 0 {
		t.Errorf("created %d runs for an unmatched event, want 0", len(runs.Items))
	}
}

func TestPostTeamEvent_NoClient(t *testing.T) {
	srv := httptest.NewServer((&Gateway{Queue: sessionqueue.NewMemQueue()}).Handler()) // K8s nil
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/teams/tenant-a/squad/events", strings.NewReader(`{}`))
	req.Header.Set("Ce-Id", "x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no cluster client)", resp.StatusCode)
	}
}
