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

func newMP(name string) *amv1.ModelProvider {
	return &amv1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: pure.ModelProviderSpec{
			Kind:      "openai",
			SecretRef: pure.AuthRef{SecretName: "provider-key"},
		},
	}
}

func reconcileMP(t *testing.T, objs ...client.Object) (*ModelProviderReconciler, func(string) amv1.ModelProvider) {
	t.Helper()
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = amv1.AddToScheme(sch)
	c := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&amv1.ModelProvider{}).
		WithObjects(objs...).Build()
	r := &ModelProviderReconciler{Client: c, Scheme: sch}
	get := func(name string) amv1.ModelProvider {
		var got amv1.ModelProvider
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got); err != nil {
			t.Fatalf("get modelprovider: %v", err)
		}
		return got
	}
	return r, get
}

func reconcileMPOnce(t *testing.T, r *ModelProviderReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// A valid provider with a single-key secret goes Ready.
func TestMPReconcile_ReadyWithSingleKeySecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-key", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-x")},
	}
	r, get := reconcileMP(t, newMP("zai"), secret)
	reconcileMPOnce(t, r, "zai")
	got := get("zai")
	if got.Status.Phase != "Ready" || got.Status.Reason != "Reconciled" {
		t.Fatalf("want Ready/Reconciled, got %s/%s (%s)", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", got.Status.ObservedGeneration)
	}
}

// A missing secret holds the provider Pending/SecretMissing.
func TestMPReconcile_SecretMissing(t *testing.T) {
	r, get := reconcileMP(t, newMP("zai"))
	reconcileMPOnce(t, r, "zai")
	got := get("zai")
	if got.Status.Phase != "Pending" || got.Status.Reason != "SecretMissing" {
		t.Errorf("want Pending/SecretMissing, got %s/%s", got.Status.Phase, got.Status.Reason)
	}
}

// A secret with two keys and no spec.secretRef.key is ambiguous: Pending/SecretAmbiguous.
func TestMPReconcile_SecretAmbiguousNoKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-key", Namespace: "default"},
		Data:       map[string][]byte{"a": []byte("1"), "b": []byte("2")},
	}
	r, get := reconcileMP(t, newMP("zai"), secret)
	reconcileMPOnce(t, r, "zai")
	got := get("zai")
	if got.Status.Phase != "Pending" || got.Status.Reason != "SecretAmbiguous" {
		t.Fatalf("want Pending/SecretAmbiguous, got %s/%s", got.Status.Phase, got.Status.Reason)
	}
	if got.Status.Message == "" {
		t.Error("expected a message explaining the sole-key rule")
	}
}

// A secret with two keys but an explicit spec.secretRef.key present goes Ready.
func TestMPReconcile_ReadyWithExplicitKey(t *testing.T) {
	mp := newMP("zai")
	mp.Spec.SecretRef.Key = "a"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-key", Namespace: "default"},
		Data:       map[string][]byte{"a": []byte("1"), "b": []byte("2")},
	}
	r, get := reconcileMP(t, mp, secret)
	reconcileMPOnce(t, r, "zai")
	got := get("zai")
	if got.Status.Phase != "Ready" {
		t.Errorf("want Ready, got %s/%s (%s)", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
}

// An explicit key that isn't in the secret stays Pending/SecretMissing.
func TestMPReconcile_ExplicitKeyMissing(t *testing.T) {
	mp := newMP("zai")
	mp.Spec.SecretRef.Key = "missing"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-key", Namespace: "default"},
		Data:       map[string][]byte{"a": []byte("1")},
	}
	r, get := reconcileMP(t, mp, secret)
	reconcileMPOnce(t, r, "zai")
	got := get("zai")
	if got.Status.Phase != "Pending" || got.Status.Reason != "SecretMissing" {
		t.Errorf("want Pending/SecretMissing, got %s/%s", got.Status.Phase, got.Status.Reason)
	}
}

// An invalid kind folds to Failed/InvalidSpec without touching secrets.
func TestMPReconcile_InvalidKind(t *testing.T) {
	mp := newMP("zai")
	mp.Spec.Kind = "not-a-real-kind"
	r, get := reconcileMP(t, mp)
	reconcileMPOnce(t, r, "zai")
	got := get("zai")
	if got.Status.Phase != "Failed" || got.Status.Reason != "InvalidSpec" {
		t.Errorf("want Failed/InvalidSpec, got %s/%s", got.Status.Phase, got.Status.Reason)
	}
}

// kind=local without spec.endpoint is Failed/InvalidSpec.
func TestMPReconcile_LocalRequiresEndpoint(t *testing.T) {
	mp := newMP("zai")
	mp.Spec.Kind = "local"
	r, get := reconcileMP(t, mp)
	reconcileMPOnce(t, r, "zai")
	got := get("zai")
	if got.Status.Phase != "Failed" || got.Status.Reason != "InvalidSpec" {
		t.Errorf("want Failed/InvalidSpec, got %s/%s", got.Status.Phase, got.Status.Reason)
	}
}

// A non-absolute-URL endpoint is Failed/InvalidSpec, regardless of kind.
func TestMPReconcile_EndpointMustBeAbsoluteURL(t *testing.T) {
	mp := newMP("zai")
	mp.Spec.Endpoint = "not a url"
	r, get := reconcileMP(t, mp)
	reconcileMPOnce(t, r, "zai")
	got := get("zai")
	if got.Status.Phase != "Failed" || got.Status.Reason != "InvalidSpec" {
		t.Errorf("want Failed/InvalidSpec, got %s/%s", got.Status.Phase, got.Status.Reason)
	}
}

// An unchanged status does not issue a second status write.
func TestMPReconcile_UnchangedStatusSkipsWrite(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-key", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-x")},
	}
	r, get := reconcileMP(t, newMP("zai"), secret)
	reconcileMPOnce(t, r, "zai")
	firstGen := get("zai").ResourceVersion

	reconcileMPOnce(t, r, "zai")
	secondGen := get("zai").ResourceVersion
	if firstGen != secondGen {
		t.Errorf("resourceVersion changed on a no-op reconcile: %s -> %s", firstGen, secondGen)
	}
}
