package agentmodel

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func secretsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("amv1 scheme: %v", err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	return sch
}

// A loop-mode tool's http/mcp Auth.SecretName must be served by the broker under
// that name, so the in-pod invoker can lease it (the bug this closes: auth'd
// HTTP/MCP tools previously failed their lease at runtime).
func TestGatherRunSecrets_LoopToolAuth(t *testing.T) {
	tool := &amv1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "t"},
		Spec: pure.ToolSpec{
			Kind: pure.ToolHTTP,
			HTTP: &pure.HTTPSpec{URL: "https://api.example", Auth: &pure.AuthRef{SecretName: "gh-token"}},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-token", Namespace: "t"},
		Data:       map[string][]byte{"token": []byte("abc123")},
	}
	c := fake.NewClientBuilder().WithScheme(secretsScheme(t)).WithObjects(tool, sec).Build()

	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	agent.Spec.Tools = []pure.ToolRef{{Name: "gh"}}

	_, values, err := gatherRunSecrets(context.Background(), c, agent, "t", nil)
	if err != nil {
		t.Fatalf("gatherRunSecrets: %v", err)
	}
	if got := string(values["gh-token"]); got != "abc123" {
		t.Fatalf("tool auth secret not served by broker under its lease name: got %q, values=%v", got, values)
	}
}

// A tool with no auth (or a harness agent) adds nothing for tools.
func TestGatherRunSecrets_NoToolAuth(t *testing.T) {
	tool := &amv1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "t"},
		Spec:       pure.ToolSpec{Kind: pure.ToolHTTP, HTTP: &pure.HTTPSpec{URL: "https://api.example"}},
	}
	c := fake.NewClientBuilder().WithScheme(secretsScheme(t)).WithObjects(tool).Build()
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	agent.Spec.Tools = []pure.ToolRef{{Name: "echo"}}

	_, values, err := gatherRunSecrets(context.Background(), c, agent, "t", nil)
	if err != nil {
		t.Fatalf("gatherRunSecrets: %v", err)
	}
	if len(values) != 0 {
		t.Fatalf("no-auth tool must contribute no broker secrets, got %v", values)
	}
}
