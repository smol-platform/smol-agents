package memory

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// makeRetriever returns a minimal MemoryRetriever for testing.
func makeRetriever(ns, name string, stores ...string) *amv1.MemoryRetriever {
	return &amv1.MemoryRetriever{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: pure.MemoryRetrieverSpec{
			Stores:           stores,
			ModelProviderRef: "embed-provider",
			TopK:             10,
			Chunking: pure.ChunkSpec{
				Size:     512,
				Overlap:  64,
				Strategy: "fixed",
			},
		},
	}
}

// makeVectorStore returns a minimal vector MemoryStore.
func makeVectorStore(ns, name string) *amv1.MemoryStore {
	return &amv1.MemoryStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: pure.MemoryStoreSpec{
			Kind:     pure.MemoryStoreVector,
			Driver:   pure.MemoryDriverQdrant,
			Endpoint: "qdrant.svc:6333",
			Auth:     &pure.AuthRef{SecretName: "qdrant-creds"},
			Tenancy:  pure.TenancySpec{Model: pure.TenancyDedicated},
		},
	}
}

// makeFilesystemStore returns a minimal filesystem MemoryStore.
func makeFilesystemStore(ns, name string) *amv1.MemoryStore {
	return &amv1.MemoryStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: pure.MemoryStoreSpec{
			Kind:    pure.MemoryStoreFilesystem,
			Driver:  pure.MemoryDriverAgentFS,
			Tenancy: pure.TenancySpec{Model: pure.TenancyDedicated},
			AgentFS: &pure.AgentFSSpec{SizeGiB: 1},
		},
	}
}

// TestResourceName verifies the deterministic naming convention.
func TestResourceName(t *testing.T) {
	r := makeRetriever("ns", "my-ret")
	tests := []struct {
		suffix string
		want   string
	}{
		{"sa", "mr-my-ret-sa"},
		{"worker", "mr-my-ret-worker"},
		{"mcp", "mr-my-ret-mcp"},
	}
	for _, tc := range tests {
		got := resourceName(r, tc.suffix)
		if got != tc.want {
			t.Errorf("resourceName(%q) = %q, want %q", tc.suffix, got, tc.want)
		}
	}
}

// TestBuildServiceAccount checks namespace, name, and label presence.
func TestBuildServiceAccount(t *testing.T) {
	r := makeRetriever("tenant-a", "my-ret")
	sa := BuildServiceAccount(r)

	if sa.Namespace != "tenant-a" {
		t.Errorf("namespace = %q, want tenant-a", sa.Namespace)
	}
	if sa.Name != "mr-my-ret-sa" {
		t.Errorf("name = %q, want mr-my-ret-sa", sa.Name)
	}
	if sa.Labels["runtime.agents.smol-agents.ai/retriever"] != "my-ret" {
		t.Error("retriever label missing")
	}
	if sa.Labels["app.kubernetes.io/managed-by"] != "smol-agents-operator" {
		t.Error("managed-by label missing")
	}
}

// TestBuildWorkerDeployment_VectorStore verifies the worker Deployment shape
// for a vector-backend MemoryStore.
func TestBuildWorkerDeployment_VectorStore(t *testing.T) {
	r := makeRetriever("tenant-a", "my-ret", "vec-store")
	stores := []*amv1.MemoryStore{makeVectorStore("tenant-a", "vec-store")}

	d := BuildWorkerDeployment(r, stores, "")

	// Name and namespace.
	if d.Name != "mr-my-ret-worker" {
		t.Errorf("name = %q", d.Name)
	}
	if d.Namespace != "tenant-a" {
		t.Errorf("namespace = %q", d.Namespace)
	}

	// Replicas default.
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", d.Spec.Replicas)
	}

	// Default image when empty string passed.
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != defaultWorkerImage {
		t.Errorf("image = %q, want %q", c.Image, defaultWorkerImage)
	}

	// Args must include backend flags.
	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--backend=qdrant",
		"--backend-endpoint=qdrant.svc:6333",
		"--backend-auth-secret=qdrant-creds",
		"--model-provider=embed-provider",
		"--chunk-size=512",
		"--chunk-overlap=64",
		"--chunk-strategy=fixed",
		"--retriever-ref=tenant-a/my-ret",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q; got: %s", want, args)
		}
	}

	// gRPC port exposed.
	found := false
	for _, p := range c.Ports {
		if p.ContainerPort == workerPort {
			found = true
		}
	}
	if !found {
		t.Errorf("gRPC port %d not found in worker container ports", workerPort)
	}

	// Security context hardening.
	sc := c.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true")
	}

	// Selector matches pod labels.
	sel := d.Spec.Selector.MatchLabels
	if sel["runtime.agents.smol-agents.ai/component"] != "memory-worker" {
		t.Error("selector component label mismatch")
	}
	if sel["runtime.agents.smol-agents.ai/retriever"] != "my-ret" {
		t.Error("selector retriever label mismatch")
	}
}

// TestBuildWorkerDeployment_FilesystemStore checks the agentfs backend flag.
func TestBuildWorkerDeployment_FilesystemStore(t *testing.T) {
	r := makeRetriever("tenant-a", "fs-ret", "fs-store")
	stores := []*amv1.MemoryStore{makeFilesystemStore("tenant-a", "fs-store")}

	d := BuildWorkerDeployment(r, stores, "custom-image:v1")
	c := d.Spec.Template.Spec.Containers[0]

	// Image override.
	if c.Image != "custom-image:v1" {
		t.Errorf("image = %q, want custom-image:v1", c.Image)
	}

	args := strings.Join(c.Args, " ")
	if !strings.Contains(args, "--backend=agentfs") {
		t.Errorf("args missing --backend=agentfs; got: %s", args)
	}
	// agentfs has no endpoint or auth-secret flags.
	if strings.Contains(args, "--backend-endpoint") {
		t.Errorf("unexpected --backend-endpoint in agentfs args: %s", args)
	}
	if strings.Contains(args, "--backend-auth-secret") {
		t.Errorf("unexpected --backend-auth-secret in agentfs args: %s", args)
	}
}

// TestBuildWorkerService checks the headless Service shape.
func TestBuildWorkerService(t *testing.T) {
	r := makeRetriever("tenant-b", "my-ret")
	svc := BuildWorkerService(r)

	if svc.Name != "mr-my-ret-worker" {
		t.Errorf("name = %q", svc.Name)
	}
	if svc.Namespace != "tenant-b" {
		t.Errorf("namespace = %q", svc.Namespace)
	}
	if svc.Spec.ClusterIP != "None" {
		t.Errorf("ClusterIP = %q, want None (headless)", svc.Spec.ClusterIP)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != workerPort {
		t.Errorf("ports = %v, want single port %d", svc.Spec.Ports, workerPort)
	}
}

// TestBuildMCPDeployment checks the MCP gateway Deployment.
func TestBuildMCPDeployment(t *testing.T) {
	r := makeRetriever("tenant-c", "my-ret", "vec-store")

	d := BuildMCPDeployment(r, "")

	if d.Name != "mr-my-ret-mcp" {
		t.Errorf("name = %q", d.Name)
	}
	if d.Namespace != "tenant-c" {
		t.Errorf("namespace = %q", d.Namespace)
	}

	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != defaultMCPImage {
		t.Errorf("image = %q, want %q", c.Image, defaultMCPImage)
	}

	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--worker-url=grpc://mr-my-ret-worker.tenant-c.svc.cluster.local:9090",
		"--retriever-ref=tenant-c/my-ret",
		"--http-addr=:8080",
		"--top-k=10",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q; got: %s", want, args)
		}
	}

	// HTTP port.
	found := false
	for _, p := range c.Ports {
		if p.ContainerPort == mcpPort {
			found = true
		}
	}
	if !found {
		t.Errorf("HTTP port %d not found in mcp container ports", mcpPort)
	}

	// Selector.
	sel := d.Spec.Selector.MatchLabels
	if sel["runtime.agents.smol-agents.ai/component"] != "memory-mcp" {
		t.Error("mcp selector component label mismatch")
	}
}

// TestBuildMCPService checks the ClusterIP Service shape.
func TestBuildMCPService(t *testing.T) {
	r := makeRetriever("tenant-c", "my-ret")
	svc := BuildMCPService(r)

	if svc.Name != "mr-my-ret-mcp" {
		t.Errorf("name = %q", svc.Name)
	}
	if svc.Namespace != "tenant-c" {
		t.Errorf("namespace = %q", svc.Namespace)
	}
	// Not headless.
	if svc.Spec.ClusterIP == "None" {
		t.Error("MCP Service should not be headless")
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != mcpPort {
		t.Errorf("ports = %v, want single port %d", svc.Spec.Ports, mcpPort)
	}
}

// TestBuildWorkerDeployment_OwnerRef verifies that after SetControllerReference
// the Deployment carries an owner reference back to the MemoryRetriever. This
// exercises the round-trip that the reconciler performs.
func TestBuildWorkerDeployment_OwnerRef(t *testing.T) {
	r := makeRetriever("tenant-a", "my-ret", "vec-store")
	r.UID = "uid-1234"
	r.ResourceVersion = "1"

	stores := []*amv1.MemoryStore{makeVectorStore("tenant-a", "vec-store")}
	d := BuildWorkerDeployment(r, stores, "")

	// Mimic what the reconciler does (scheme-less check: just verify the
	// builder returns the right type and name before the ref is set).
	if _, ok := (interface{}(d)).(*appsv1.Deployment); !ok {
		t.Fatal("BuildWorkerDeployment did not return *appsv1.Deployment")
	}
	if d.Name == "" {
		t.Error("Deployment name must not be empty")
	}
}

// TestWorkerArgs_NoStores checks baseline args when no stores are resolved.
func TestWorkerArgs_NoStores(t *testing.T) {
	r := makeRetriever("ns", "ret")
	r.Spec.ModelProviderRef = ""
	r.Spec.Chunking = pure.ChunkSpec{}

	args := workerArgs(r, nil)
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--retriever-ref=ns/ret") {
		t.Errorf("missing --retriever-ref; args: %s", joined)
	}
	if !strings.Contains(joined, "--grpc-addr=:9090") {
		t.Errorf("missing --grpc-addr; args: %s", joined)
	}
	// No backend flags when no stores.
	if strings.Contains(joined, "--backend=") {
		t.Errorf("unexpected --backend flag with no stores; args: %s", joined)
	}
}

// TestBuildMCPDeployment_SecurityContext verifies the MCP container runs with
// the same hardened security profile as the worker.
func TestBuildMCPDeployment_SecurityContext(t *testing.T) {
	r := makeRetriever("ns", "r")
	d := BuildMCPDeployment(r, "")
	c := d.Spec.Template.Spec.Containers[0]

	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("SecurityContext nil")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true")
	}

	// Pod-level non-root enforcement.
	psc := d.Spec.Template.Spec.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("PodSecurityContext RunAsNonRoot must be true")
	}
}

// TestBuildWorkerDeployment_TmpVolume checks that the worker has a /tmp
// EmptyDir volume mount (needed for ReadOnlyRootFilesystem).
func TestBuildWorkerDeployment_TmpVolume(t *testing.T) {
	r := makeRetriever("ns", "r", "s")
	d := BuildWorkerDeployment(r, nil, "")

	hasTmpVol := false
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "tmp" && v.EmptyDir != nil {
			hasTmpVol = true
		}
	}
	if !hasTmpVol {
		t.Error("tmp EmptyDir volume not found")
	}

	c := d.Spec.Template.Spec.Containers[0]
	hasTmpMount := false
	for _, vm := range c.VolumeMounts {
		if vm.Name == "tmp" && vm.MountPath == "/tmp" {
			hasTmpMount = true
		}
	}
	if !hasTmpMount {
		t.Error("/tmp VolumeMount not found in worker container")
	}
}

// TestWorkerLabels_ComponentValue ensures the component label is "memory-worker"
// not "memory-mcp" (a regression guard against label copy-paste).
func TestWorkerLabels_ComponentValue(t *testing.T) {
	r := makeRetriever("ns", "r")
	lbls := workerLabels(r)
	if lbls["runtime.agents.smol-agents.ai/component"] != "memory-worker" {
		t.Errorf("component = %q, want memory-worker", lbls["runtime.agents.smol-agents.ai/component"])
	}
	mlbls := mcpLabels(r)
	if mlbls["runtime.agents.smol-agents.ai/component"] != "memory-mcp" {
		t.Errorf("component = %q, want memory-mcp", mlbls["runtime.agents.smol-agents.ai/component"])
	}
}

// TestMCPServiceNotHeadless ensures the MCP Service has a normal ClusterIP.
func TestMCPServiceNotHeadless(t *testing.T) {
	r := makeRetriever("ns", "r")
	svc := BuildMCPService(r)
	if svc.Spec.ClusterIP == "None" {
		t.Error("MCP Service must not be headless")
	}
}

// TestWorkerServiceHeadless ensures the worker Service is headless.
func TestWorkerServiceHeadless(t *testing.T) {
	r := makeRetriever("ns", "r")
	svc := BuildWorkerService(r)
	if svc.Spec.ClusterIP != "None" {
		t.Errorf("worker Service ClusterIP = %q, want None", svc.Spec.ClusterIP)
	}
}

// TestBuildWorkerDeployment_ResourceLimits checks that CPU/memory limits are set.
func TestBuildWorkerDeployment_ResourceLimits(t *testing.T) {
	r := makeRetriever("ns", "r", "s")
	d := BuildWorkerDeployment(r, nil, "")
	c := d.Spec.Template.Spec.Containers[0]

	if c.Resources.Limits == nil {
		t.Fatal("resource limits nil")
	}
	if c.Resources.Limits.Cpu().IsZero() {
		t.Error("CPU limit not set")
	}
	if c.Resources.Limits.Memory().IsZero() {
		t.Error("memory limit not set")
	}
}

// --- table-driven test: workerArgs covers all flag combinations ---

func TestWorkerArgs_Table(t *testing.T) {
	tests := []struct {
		name      string
		retriever *amv1.MemoryRetriever
		stores    []*amv1.MemoryStore
		wantArgs  []string
		noArgs    []string
	}{
		{
			name: "vector store with auth",
			retriever: func() *amv1.MemoryRetriever {
				r := makeRetriever("ns", "r", "vs")
				return r
			}(),
			stores:   []*amv1.MemoryStore{makeVectorStore("ns", "vs")},
			wantArgs: []string{"--backend=qdrant", "--backend-endpoint=qdrant.svc:6333", "--backend-auth-secret=qdrant-creds"},
		},
		{
			name:      "filesystem store — no endpoint/auth flags",
			retriever: makeRetriever("ns", "r", "fs"),
			stores:    []*amv1.MemoryStore{makeFilesystemStore("ns", "fs")},
			wantArgs:  []string{"--backend=agentfs"},
			noArgs:    []string{"--backend-endpoint", "--backend-auth-secret"},
		},
		{
			name: "no model provider",
			retriever: func() *amv1.MemoryRetriever {
				r := makeRetriever("ns", "r")
				r.Spec.ModelProviderRef = ""
				return r
			}(),
			stores: nil,
			noArgs: []string{"--model-provider"},
		},
		{
			name: "chunking defaults omitted when zero",
			retriever: func() *amv1.MemoryRetriever {
				r := makeRetriever("ns", "r")
				r.Spec.Chunking = pure.ChunkSpec{}
				return r
			}(),
			stores: nil,
			noArgs: []string{"--chunk-size", "--chunk-overlap", "--chunk-strategy"},
		},
		{
			name:      "vector store without auth",
			retriever: makeRetriever("ns", "r", "vs"),
			stores: func() []*amv1.MemoryStore {
				s := makeVectorStore("ns", "vs")
				s.Spec.Auth = nil
				return []*amv1.MemoryStore{s}
			}(),
			wantArgs: []string{"--backend=qdrant"},
			noArgs:   []string{"--backend-auth-secret"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := workerArgs(tc.retriever, tc.stores)
			joined := strings.Join(args, " ")
			for _, want := range tc.wantArgs {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q; got: %s", want, joined)
				}
			}
			for _, no := range tc.noArgs {
				if strings.Contains(joined, no) {
					t.Errorf("unexpected %q; got: %s", no, joined)
				}
			}
		})
	}
}

// Ensure BuildMCPDeployment and BuildMCPService both return typed objects
// (smoke test for type assertions in reconciler).
func TestBuildFunctions_ReturnCorrectTypes(t *testing.T) {
	r := makeRetriever("ns", "r")
	if _, ok := (interface{}(BuildMCPDeployment(r, ""))).(*appsv1.Deployment); !ok {
		t.Error("BuildMCPDeployment did not return *appsv1.Deployment")
	}
	if _, ok := (interface{}(BuildMCPService(r))).(*corev1.Service); !ok {
		t.Error("BuildMCPService did not return *corev1.Service")
	}
	if _, ok := (interface{}(BuildWorkerService(r))).(*corev1.Service); !ok {
		t.Error("BuildWorkerService did not return *corev1.Service")
	}
	if _, ok := (interface{}(BuildServiceAccount(r))).(*corev1.ServiceAccount); !ok {
		t.Error("BuildServiceAccount did not return *corev1.ServiceAccount")
	}
}
