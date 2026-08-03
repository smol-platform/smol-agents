package agentmodel

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func policyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = amv1.AddToScheme(sch)
	return sch
}

// im4: default secret-shape patterns must always mask known credential
// shapes, even in a namespace with no AgentPolicy at all.
func TestCompileNamespaceRedaction_NoPolicy_StillMasksDefaults(t *testing.T) {
	sch := policyScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).Build()

	pats := compileNamespaceRedaction(context.Background(), c, "no-policy-ns", types.NamespacedName{})
	if len(pats) == 0 {
		t.Fatalf("compileNamespaceRedaction must never return an empty set (defaults are always on)")
	}

	out := pure.RedactJSON([]byte(`{"apiKey":"sk-FAKEabcdEFGH12345678","note":"hello"}`), pats)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("redacted output must be valid JSON: %v (%s)", err, out)
	}
	if m["apiKey"] != pure.RedactionMask {
		t.Errorf("fake key must be masked by default patterns with no AgentPolicy present: %v", m["apiKey"])
	}
	if m["note"] != "hello" {
		t.Errorf("non-secret value must survive untouched: %v", m["note"])
	}
}

// The namespace policy's own patterns extend, not replace, the defaults.
func TestCompileNamespaceRedaction_PolicyPatternsExtendDefaults(t *testing.T) {
	sch := policyScheme(t)
	pol := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "custom-ns"},
		Spec: pure.AgentPolicySpec{
			Redaction: &pure.RedactionPolicy{Patterns: []string{`custom-[0-9]+`}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pol).Build()

	pats := compileNamespaceRedaction(context.Background(), c, "custom-ns", types.NamespacedName{})

	out := pure.RedactJSON([]byte(`{"a":"sk-FAKEabcdEFGH12345678","b":"custom-42"}`), pats)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("redacted output must be valid JSON: %v (%s)", err, out)
	}
	if m["a"] != pure.RedactionMask {
		t.Errorf("default pattern must still mask a fake key: %v", m["a"])
	}
	if m["b"] != pure.RedactionMask {
		t.Errorf("namespace policy pattern must also mask: %v", m["b"])
	}
}
