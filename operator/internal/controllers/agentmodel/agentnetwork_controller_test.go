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

func TestAgentNetworkSetStatus_RecordsAllFields(t *testing.T) {
	r := &AgentNetworkReconciler{}
	an := &amv1.AgentNetwork{}
	an.Generation = 7
	r.setStatus(an, "Pending", "SecretMissing", "wg-private not found")
	if an.Status.Phase != "Pending" {
		t.Errorf("phase = %q", an.Status.Phase)
	}
	if an.Status.Reason != "SecretMissing" {
		t.Errorf("reason = %q", an.Status.Reason)
	}
	if an.Status.Message != "wg-private not found" {
		t.Errorf("message = %q", an.Status.Message)
	}
	if an.Status.ObservedGeneration != 7 {
		t.Errorf("gen = %d", an.Status.ObservedGeneration)
	}
}

func TestAgentNetworkDeepCopy_PreservesContents(t *testing.T) {
	an := &amv1.AgentNetwork{}
	an.Spec.Kind = pure.NetworkIdentityProxy
	an.Spec.IdentityProxy = &pure.IdentityProxySpec{
		Resources: []pure.ResourceTarget{{
			Name: "db", Kind: "tcp", LocalAddr: "127.0.0.1:5432",
			Gateway: "pg.svc:8443", Authorize: []string{"spiffe://x"},
		}},
		Egress: pure.EgressPolicy{
			Enforcement:   "ebpfBoth",
			RedirectCIDRs: []string{"10.0.0.0/16"},
			Allow:         []pure.EgressRule{{CIDR: "10.0.0.5/32", Protocol: "tcp", Ports: []int32{443}}},
		},
	}
	cp := an.DeepCopy()
	if cp.Spec.IdentityProxy == nil {
		t.Fatal("identityProxy not copied")
	}
	cp.Spec.IdentityProxy.Resources[0].Name = "mutated"
	fresh := an.DeepCopy()
	if fresh.Spec.IdentityProxy.Resources[0].Name == "mutated" {
		t.Error("deepcopy shared the resources slice")
	}
}

// --- knative-agents-13s: cross-object credential alignment ---

// credentialAN builds a valid identityProxy AgentNetwork whose single http
// resource injects a credential with the given name+scope.
func credentialAN(credName, credScope string) *amv1.AgentNetwork {
	return &amv1.AgentNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "egress", Namespace: "tenant-a", Generation: 1},
		Spec: pure.AgentNetworkSpec{
			Kind: pure.NetworkIdentityProxy,
			IdentityProxy: &pure.IdentityProxySpec{
				TTS: &pure.TTSRef{URL: "https://tts.svc/token", JWKSURL: "https://tts.svc/jwks"},
				Resources: []pure.ResourceTarget{{
					Name: "gh", Kind: "http", LocalPort: 8080,
					Gateway: "https://api.github.com", JWTAudience: "github",
					Credential: &pure.CredentialInjection{Name: credName, Scope: credScope},
				}},
			},
		},
	}
}

// backendNamed builds a githubApp DCB in tenant-a mapping a single scope.
func backendNamed(credName, scope string) *amv1.DynamicCredentialBackend {
	return &amv1.DynamicCredentialBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-backend", Namespace: "tenant-a", Generation: 1},
		Spec: pure.DynamicCredentialBackendSpec{
			CredentialName: credName,
			Provider:       "githubApp",
			GitHubApp: &pure.GitHubAppBackendSpec{
				AppID:            "123",
				PrivateKeyRef:    pure.AuthRef{SecretName: "gh-key"},
				ScopePermissions: map[string]map[string]string{scope: {"contents": "read"}},
			},
		},
	}
}

func reconcileAN(t *testing.T, objs ...client.Object) amv1.AgentNetwork {
	t.Helper()
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = amv1.AddToScheme(sch)
	c := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&amv1.AgentNetwork{}).
		WithObjects(objs...).Build()
	r := &AgentNetworkReconciler{Client: c, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "egress"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got amv1.AgentNetwork
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "egress"}, &got); err != nil {
		t.Fatalf("get agentnetwork: %v", err)
	}
	return got
}

// A credential referencing a non-existent backend holds Pending/BackendMissing
// (self-heals when the backend appears, like SecretMissing).
func TestAgentNetworkReconcile_CredentialBackendMissing(t *testing.T) {
	got := reconcileAN(t, credentialAN("github", "github:repo:read"))
	if got.Status.Phase != "Pending" || got.Status.Reason != "BackendMissing" {
		t.Fatalf("want Pending/BackendMissing, got %s/%s (%s)", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
}

// A credential scope the existing backend does not map fails closed —
// the same misalignment c5r.22 rejected intra-object, now cross-object.
func TestAgentNetworkReconcile_CredentialScopeUnmapped(t *testing.T) {
	got := reconcileAN(t,
		credentialAN("github", "github:repo:write"),
		backendNamed("github", "github:repo:read"),
	)
	if got.Status.Phase != "Failed" || got.Status.Reason != "InvalidSpec" {
		t.Fatalf("want Failed/InvalidSpec, got %s/%s (%s)", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
}

// A credential whose name+scope align with a backend reconciles Ready.
func TestAgentNetworkReconcile_CredentialAligned(t *testing.T) {
	got := reconcileAN(t,
		credentialAN("github", "github:repo:read"),
		backendNamed("github", "github:repo:read"),
	)
	if got.Status.Phase != "Ready" {
		t.Fatalf("want Ready, got %s/%s (%s)", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
}

// The DCB watch requeues only the AgentNetworks that reference the changed
// backend's credentialName.
func TestAgentNetworkBackendToAgentNetworks_RequeuesConsumers(t *testing.T) {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = amv1.AddToScheme(sch)
	consumer := credentialAN("github", "github:repo:read")
	other := credentialAN("gitlab", "gitlab:repo:read")
	other.Name = "other"
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(consumer, other).Build()
	r := &AgentNetworkReconciler{Client: c, Scheme: sch}

	reqs := r.backendToAgentNetworks(context.Background(), backendNamed("github", "github:repo:read"))
	if len(reqs) != 1 || reqs[0].Name != "egress" {
		t.Fatalf("want only the github consumer requeued, got %+v", reqs)
	}
}

func TestAgentNetworkDeepCopy_WireGuardBranch(t *testing.T) {
	an := &amv1.AgentNetwork{}
	an.Spec.Kind = pure.NetworkWireGuardMesh
	an.Spec.WireGuardMesh = &pure.WireGuardSpec{
		Mode:          "client",
		PrivateKeyRef: pure.AuthRef{SecretName: "wg-priv"},
		Peers: []pure.WGPeer{{
			Name:       "hub",
			PublicKey:  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			AllowedIPs: []string{"10.0.0.0/16"},
		}},
	}
	cp := an.DeepCopy()
	if cp.Spec.WireGuardMesh == nil {
		t.Fatal("wireguardMesh not copied")
	}
	cp.Spec.WireGuardMesh.Peers[0].Name = "mutated"
	fresh := an.DeepCopy()
	if fresh.Spec.WireGuardMesh.Peers[0].Name == "mutated" {
		t.Error("deepcopy shared the peers slice")
	}
}
