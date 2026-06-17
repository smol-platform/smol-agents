package agentmodel

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func newDCB(name string) *amv1.DynamicCredentialBackend {
	return &amv1.DynamicCredentialBackend{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "platform-secrets", Generation: 1},
		Spec: pure.DynamicCredentialBackendSpec{
			CredentialName: "github",
			Provider:       "githubApp",
			GitHubApp: &pure.GitHubAppBackendSpec{
				AppID:            "123456",
				PrivateKeyRef:    pure.AuthRef{SecretName: "github-app-key", Key: "private-key.pem"},
				ScopePermissions: map[string]map[string]string{"github:repo:read": {"contents": "read"}},
			},
			Grants: []pure.CredentialGrantSpec{
				{Principal: "spiffe://smol-agents.ai/ns/t/sa/a", Scope: "github:repo:read"},
			},
		},
	}
}

func reconcileDCB(t *testing.T, objs ...client.Object) (*DynamicCredentialBackendReconciler, func(string) amv1.DynamicCredentialBackend) {
	t.Helper()
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = amv1.AddToScheme(sch)
	c := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&amv1.DynamicCredentialBackend{}).
		WithObjects(objs...).Build()
	r := &DynamicCredentialBackendReconciler{Client: c, Scheme: sch}
	get := func(name string) amv1.DynamicCredentialBackend {
		var got amv1.DynamicCredentialBackend
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "platform-secrets", Name: name}, &got); err != nil {
			t.Fatalf("get dcb: %v", err)
		}
		return got
	}
	return r, get
}

func reconcileOnce(t *testing.T, r *DynamicCredentialBackendReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "platform-secrets", Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// M1.21: a missing root secret holds the backend Pending/SecretMissing; creating
// the secret flips it Ready; grantCount tracks the spec.
func TestDCBReconcile_SecretMissingThenReady(t *testing.T) {
	r, get := reconcileDCB(t, newDCB("github"))

	reconcileOnce(t, r, "github")
	got := get("github")
	if got.Status.Phase != "Pending" || got.Status.Reason != "SecretMissing" {
		t.Fatalf("missing secret → want Pending/SecretMissing, got %s/%s", got.Status.Phase, got.Status.Reason)
	}
	if got.Status.GrantCount != 1 {
		t.Errorf("grantCount = %d, want 1", got.Status.GrantCount)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "github-app-key", Namespace: "platform-secrets"},
		Data:       map[string][]byte{"private-key.pem": []byte("KEY")},
	}
	if err := r.Create(context.Background(), secret); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	reconcileOnce(t, r, "github")
	if got := get("github"); got.Status.Phase != "Ready" {
		t.Errorf("with secret → want Ready, got %s/%s (%s)", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
}

// M1.21: a secret present but missing the named key stays Pending.
func TestDCBReconcile_SecretWrongKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "github-app-key", Namespace: "platform-secrets"},
		Data:       map[string][]byte{"wrong-key": []byte("x")},
	}
	r, get := reconcileDCB(t, newDCB("github"), secret)
	reconcileOnce(t, r, "github")
	if got := get("github"); got.Status.Phase != "Pending" || got.Status.Reason != "SecretMissing" {
		t.Errorf("wrong key → want Pending/SecretMissing, got %s/%s", got.Status.Phase, got.Status.Reason)
	}
}

// M1.21: an invalid spec folds to Failed/InvalidSpec without touching secrets.
func TestDCBReconcile_InvalidSpec(t *testing.T) {
	dcb := newDCB("github")
	dcb.Spec.CredentialName = ""
	r, get := reconcileDCB(t, dcb)
	reconcileOnce(t, r, "github")
	if got := get("github"); got.Status.Phase != "Failed" || got.Status.Reason != "InvalidSpec" {
		t.Errorf("invalid spec → want Failed/InvalidSpec, got %s/%s", got.Status.Phase, got.Status.Reason)
	}
}
