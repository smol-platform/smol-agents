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
		ObjectMeta: metav1.ObjectMeta{Name: "gh-token", Namespace: "t", Labels: map[string]string{TenantSecretLabel: "true"}},
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

// checkTenantSecret: the tenant-boundary opt-in label must be present and
// exactly "true" — missing, empty, and "false" are all rejected fail-closed.
func TestCheckTenantSecret(t *testing.T) {
	cases := []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		{name: "labeled true", labels: map[string]string{TenantSecretLabel: "true"}, wantErr: false},
		{name: "missing label", labels: nil, wantErr: true},
		{name: "empty value", labels: map[string]string{TenantSecretLabel: ""}, wantErr: true},
		{name: "false value", labels: map[string]string{TenantSecretLabel: "false"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "t", Labels: tc.labels},
			}
			err := checkTenantSecret(sec)
			if tc.wantErr && err == nil {
				t.Fatalf("checkTenantSecret() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkTenantSecret() = %v, want nil", err)
			}
		})
	}
}

// readSecretKey rejects a Secret missing the tenant-boundary label, and
// accepts one that carries it.
func TestReadSecretKey_TenantBoundary(t *testing.T) {
	labeled := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "labeled", Namespace: "t", Labels: map[string]string{TenantSecretLabel: "true"}},
		Data:       map[string][]byte{"k": []byte("v")},
	}
	unlabeled := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "unlabeled", Namespace: "t"},
		Data:       map[string][]byte{"k": []byte("v")},
	}
	c := fake.NewClientBuilder().WithScheme(secretsScheme(t)).WithObjects(labeled, unlabeled).Build()

	if _, err := readSecretKey(context.Background(), c, "t", "unlabeled", "k"); err == nil {
		t.Fatalf("readSecretKey(unlabeled) = nil error, want tenant-boundary rejection")
	}
	got, err := readSecretKey(context.Background(), c, "t", "labeled", "k")
	if err != nil {
		t.Fatalf("readSecretKey(labeled): %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("readSecretKey(labeled) = %q, want %q", got, "v")
	}
}

// The cross-namespace escalation this bead closes: a Tool in another
// namespace pointing its http Auth.SecretName at a Secret that isn't labeled
// tenant-owned must fail gatherRunSecrets, not silently serve it to the
// broker.
func TestGatherRunSecrets_ToolAuthCrossNamespaceUnlabeled(t *testing.T) {
	tool := &amv1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "other-ns"},
		Spec: pure.ToolSpec{
			Kind: pure.ToolHTTP,
			HTTP: &pure.HTTPSpec{URL: "https://api.example", Auth: &pure.AuthRef{SecretName: "gh-token"}},
		},
	}
	// Deliberately unlabeled — the escalation this bead closes.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-token", Namespace: "other-ns"},
		Data:       map[string][]byte{"token": []byte("abc123")},
	}
	c := fake.NewClientBuilder().WithScheme(secretsScheme(t)).WithObjects(tool, sec).Build()

	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	agent.Spec.Tools = []pure.ToolRef{{Name: "gh", Namespace: "other-ns"}}

	if _, _, err := gatherRunSecrets(context.Background(), c, agent, "t", nil); err == nil {
		t.Fatalf("gatherRunSecrets() = nil error, want rejection of unlabeled cross-namespace tool auth secret")
	}
}

// storageAgent builds an Agent declaring durable AgentFS storage with an S3
// backup credentialsRef pointing at secretName.
func storageAgent(secretName string) *amv1.Agent {
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	agent.Spec.Storage = &pure.StorageSpec{
		Kind: pure.StorageAgentFS,
		AgentFS: &pure.AgentFSSpec{
			SizeGiB: 1,
			Backup: &pure.BackupPolicy{
				S3: &pure.S3BackupSpec{
					Bucket:         "bucket",
					CredentialsRef: &pure.AuthRef{SecretName: secretName},
				},
			},
		},
	}
	return agent
}

// The operator never reads the AgentFS S3 backup credentials Secret's value
// (storage_mount.go projects it via secretKeyRef) but must still refuse to
// wire it into the pod when it isn't labeled tenant-owned.
func TestGatherRunSecrets_StorageS3CredsUnlabeled(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: "t"}} // no label
	c := fake.NewClientBuilder().WithScheme(secretsScheme(t)).WithObjects(sec).Build()

	if _, _, err := gatherRunSecrets(context.Background(), c, storageAgent("s3-creds"), "t", nil); err == nil {
		t.Fatalf("gatherRunSecrets() = nil error, want rejection of unlabeled AgentFS S3 credentialsRef secret")
	}
}

// The same credentialsRef succeeds once the Secret carries the label.
func TestGatherRunSecrets_StorageS3CredsLabeled(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: "t", Labels: map[string]string{TenantSecretLabel: "true"}},
	}
	c := fake.NewClientBuilder().WithScheme(secretsScheme(t)).WithObjects(sec).Build()

	if _, _, err := gatherRunSecrets(context.Background(), c, storageAgent("s3-creds"), "t", nil); err != nil {
		t.Fatalf("gatherRunSecrets() = %v, want nil for labeled AgentFS S3 credentialsRef secret", err)
	}
}
