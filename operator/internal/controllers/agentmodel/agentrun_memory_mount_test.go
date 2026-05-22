package agentmodel

// Tests for the memoryFSRetriever helper and the AttachMemoryFS wiring path
// in the AgentRun controller. Uses a table-driven stub client to avoid
// pulling in envtest.

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/stigen/smol-agents/operator/api/agentmodel/v1"
	"github.com/stigen/smol-agents/operator/internal/builders"
	pure "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// ── stub reader ───────────────────────────────────────────────────────────────

// memoryStubReader satisfies client.Reader (Get + List) used by
// memoryFSRetriever. Only Get is implemented; List panics.
type memoryStubReader struct {
	retriever *amv1.MemoryRetriever // nil = NotFound
	stores    map[string]*amv1.MemoryStore
}

func (s *memoryStubReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	switch o := obj.(type) {
	case *amv1.MemoryRetriever:
		if s.retriever == nil || s.retriever.Name != key.Name {
			return apierrors.NewNotFound(
				schema.GroupResource{Group: "runtime.agents.stigen.ai", Resource: "memoryretrievers"},
				key.Name,
			)
		}
		s.retriever.DeepCopyInto(o)
		return nil
	case *amv1.MemoryStore:
		st, ok := s.stores[key.Name]
		if !ok {
			return apierrors.NewNotFound(
				schema.GroupResource{Group: "runtime.agents.stigen.ai", Resource: "memorystores"},
				key.Name,
			)
		}
		st.DeepCopyInto(o)
		return nil
	default:
		panic("memoryStubReader.Get: unexpected type")
	}
}

func (s *memoryStubReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	panic("memoryStubReader.List: not implemented")
}

// compile-time assertion: stub satisfies client.Reader.
var _ client.Reader = (*memoryStubReader)(nil)

// ── helpers ───────────────────────────────────────────────────────────────────

func fsRetriever(ns, name, storeName string) *amv1.MemoryRetriever {
	return &amv1.MemoryRetriever{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: pure.MemoryRetrieverSpec{
			Stores: []string{storeName},
			TopK:   5,
			Mount:  &pure.MountSpec{Enabled: true, MountPath: "/var/mem-fs"},
		},
	}
}

func fsStore(ns, name string) *amv1.MemoryStore {
	return &amv1.MemoryStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: pure.MemoryStoreSpec{
			Kind:    pure.MemoryStoreFilesystem,
			Driver:  pure.MemoryDriverAgentFS,
			Tenancy: pure.TenancySpec{Model: pure.TenancyDedicated},
			AgentFS: &pure.AgentFSSpec{SizeGiB: 2, Image: "test/agentfs:v1"},
		},
	}
}

func runWithRetriever(retrieverRef string) *amv1.AgentRun {
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-001", Namespace: "tenant-a"}}
	r.Spec.AgentRef = "alice"
	r.Spec.Input = []byte(`{}`)
	r.Spec.MemoryRetrieverRef = retrieverRef
	return r
}

// ── unit tests for memoryFSRetriever ─────────────────────────────────────────

func TestMemoryFSRetriever_NoRef(t *testing.T) {
	run := runWithRetriever("")
	cli := &memoryStubReader{}

	input, enabled, err := memoryFSRetriever(context.Background(), cli, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled {
		t.Error("expected enabled=false when no MemoryRetrieverRef set")
	}
	if input.MountEnabled() {
		t.Error("expected MountEnabled=false when no ref")
	}
}

func TestMemoryFSRetriever_RetrieverNotFound(t *testing.T) {
	run := runWithRetriever("missing-retriever")
	cli := &memoryStubReader{retriever: nil}

	_, enabled, err := memoryFSRetriever(context.Background(), cli, run)
	if err != nil {
		t.Fatalf("unexpected error on NotFound: %v", err)
	}
	if enabled {
		t.Error("expected enabled=false when retriever not found")
	}
}

func TestMemoryFSRetriever_MountDisabled(t *testing.T) {
	retriever := fsRetriever("tenant-a", "my-ret", "fs-store")
	retriever.Spec.Mount.Enabled = false

	run := runWithRetriever("my-ret")
	cli := &memoryStubReader{
		retriever: retriever,
		stores:    map[string]*amv1.MemoryStore{"fs-store": fsStore("tenant-a", "fs-store")},
	}

	_, enabled, err := memoryFSRetriever(context.Background(), cli, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled {
		t.Error("expected enabled=false when Mount.Enabled=false")
	}
}

func TestMemoryFSRetriever_NoFilesystemStore(t *testing.T) {
	// Retriever has mount enabled but its store is vector, not filesystem.
	retriever := fsRetriever("tenant-a", "my-ret", "vec-store")

	vecStore := &amv1.MemoryStore{
		ObjectMeta: metav1.ObjectMeta{Name: "vec-store", Namespace: "tenant-a"},
		Spec: pure.MemoryStoreSpec{
			Kind:     pure.MemoryStoreVector,
			Driver:   pure.MemoryDriverQdrant,
			Endpoint: "qdrant:6333",
			Auth:     &pure.AuthRef{SecretName: "q"},
			Tenancy:  pure.TenancySpec{Model: pure.TenancyDedicated},
		},
	}

	run := runWithRetriever("my-ret")
	cli := &memoryStubReader{
		retriever: retriever,
		stores:    map[string]*amv1.MemoryStore{"vec-store": vecStore},
	}

	_, enabled, err := memoryFSRetriever(context.Background(), cli, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled {
		t.Error("expected enabled=false when no filesystem store found")
	}
}

func TestMemoryFSRetriever_HappyPath(t *testing.T) {
	retriever := fsRetriever("tenant-a", "my-ret", "fs-store")
	store := fsStore("tenant-a", "fs-store")

	run := runWithRetriever("my-ret")
	cli := &memoryStubReader{
		retriever: retriever,
		stores:    map[string]*amv1.MemoryStore{"fs-store": store},
	}

	input, enabled, err := memoryFSRetriever(context.Background(), cli, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected enabled=true for filesystem retriever with Mount.Enabled=true")
	}
	if !input.MountEnabled() {
		t.Error("input.MountEnabled() should be true")
	}
	if input.AgentFS == nil {
		t.Fatal("AgentFS must not be nil in resolved input")
	}
	if input.AgentFS.SizeGiB != 2 {
		t.Errorf("AgentFS.SizeGiB = %d, want 2", input.AgentFS.SizeGiB)
	}
	if input.AgentFS.Image != "test/agentfs:v1" {
		t.Errorf("AgentFS.Image = %q, want test/agentfs:v1", input.AgentFS.Image)
	}
	if input.MountPath() != "/var/mem-fs" {
		t.Errorf("MountPath = %q, want /var/mem-fs", input.MountPath())
	}
}

// ── integration: memoryFSRetriever + AttachMemoryFS round-trip ───────────────

// TestMemoryMount_PodWiring verifies that after memoryFSRetriever resolves a
// filesystem retriever, calling AttachMemoryFS on a BuildAgentRunPod result
// produces the expected volume + sidecar shape (R-MEM-FS-2).
func TestMemoryMount_PodWiring(t *testing.T) {
	retriever := fsRetriever("tenant-a", "my-ret", "fs-store")
	store := fsStore("tenant-a", "fs-store")

	run := runWithRetriever("my-ret")
	agent := sampleAgent()

	cli := &memoryStubReader{
		retriever: retriever,
		stores:    map[string]*amv1.MemoryStore{"fs-store": store},
	}

	input, enabled, err := memoryFSRetriever(context.Background(), cli, run)
	if err != nil || !enabled {
		t.Fatalf("memoryFSRetriever: enabled=%v err=%v", enabled, err)
	}

	pod := builders.BuildAgentRunPod(run, agent)
	builders.AttachMemoryFS(pod, input)

	// Volume must be present with EmptyDir + SizeLimit.
	var gotVol bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == "memory-agentfs" {
			gotVol = true
			if v.EmptyDir == nil {
				t.Error("volume source is not EmptyDir")
			}
			if v.EmptyDir.SizeLimit == nil {
				t.Error("EmptyDir SizeLimit not set from AgentFSSpec.SizeGiB")
			}
		}
	}
	if !gotVol {
		t.Error("memory-agentfs volume missing after AttachMemoryFS")
	}

	// Init container must exist.
	var gotInit bool
	for _, c := range pod.Spec.InitContainers {
		if c.Name == "memory-agentfs-init" {
			gotInit = true
			if c.Image != "test/agentfs:v1" {
				t.Errorf("init container image = %q, want test/agentfs:v1", c.Image)
			}
		}
	}
	if !gotInit {
		t.Error("memory-agentfs-init init container missing")
	}

	// Sidecar must exist.
	var gotSidecar bool
	for _, c := range pod.Spec.Containers {
		if c.Name == "memory-agentfs-sidecar" {
			gotSidecar = true
			if c.Image != "test/agentfs:v1" {
				t.Errorf("sidecar image = %q, want test/agentfs:v1", c.Image)
			}
		}
	}
	if !gotSidecar {
		t.Error("memory-agentfs-sidecar container missing")
	}

	// The agent container must have the VolumeMount.
	agentContainer := pod.Spec.Containers[0]
	if agentContainer.Name != "agent" {
		t.Fatalf("expected agent at index 0, got %q", agentContainer.Name)
	}
	var gotMount bool
	for _, vm := range agentContainer.VolumeMounts {
		if vm.Name == "memory-agentfs" && vm.MountPath == "/var/mem-fs" {
			gotMount = true
		}
	}
	if !gotMount {
		t.Error("agent container missing memory-agentfs VolumeMount at /var/mem-fs")
	}
}
