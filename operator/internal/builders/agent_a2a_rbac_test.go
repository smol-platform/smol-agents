package builders

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

func TestAgentA2ARole_NamespacedAndScoped(t *testing.T) {
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "tenant-a"}}

	role := AgentA2ARole(agent)
	if role.Namespace != "tenant-a" || role.Name != "parent-a2a" {
		t.Fatalf("role name/ns = %s/%s", role.Namespace, role.Name)
	}
	// Must grant agentruns create/get/list/watch + status get, and NOT delete
	// (children GC via OwnerReference) — verify verbs per resource.
	verbs := map[string][]string{}
	for _, r := range role.Rules {
		for _, res := range r.Resources {
			verbs[res] = r.Verbs
		}
	}
	want := map[string]bool{"create": true, "get": true, "list": true, "watch": true}
	got := map[string]bool{}
	for _, v := range verbs["agentruns"] {
		got[v] = true
		if v == "delete" {
			t.Errorf("agentruns must NOT include delete (rely on OwnerReference GC)")
		}
	}
	for v := range want {
		if !got[v] {
			t.Errorf("agentruns missing verb %q", v)
		}
	}
	if len(verbs["agentruns/status"]) != 1 || verbs["agentruns/status"][0] != "get" {
		t.Errorf("agentruns/status verbs = %v, want [get]", verbs["agentruns/status"])
	}
}

func TestAgentA2ARoleBinding_BindsRunSA(t *testing.T) {
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "tenant-a"}}
	rb := AgentA2ARoleBinding(agent)
	if rb.RoleRef.Name != "parent-a2a" || rb.RoleRef.Kind != "Role" {
		t.Errorf("roleRef = %+v, want Role/parent-a2a", rb.RoleRef)
	}
	if len(rb.Subjects) != 1 {
		t.Fatalf("subjects = %d", len(rb.Subjects))
	}
	s := rb.Subjects[0]
	if s.Kind != "ServiceAccount" || s.Name != AgentSAName("parent") || s.Namespace != "tenant-a" {
		t.Errorf("subject = %+v, want the agent run SA in tenant-a", s)
	}
}
