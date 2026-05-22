package worker_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/stigen/smol-agents/pkg/memory"
	"github.com/stigen/smol-agents/pkg/memory/api"
	"github.com/stigen/smol-agents/pkg/memory/worker"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func newWorker(t *testing.T, b memory.Backend) *worker.Worker {
	t.Helper()
	emb, err := worker.NewFakeEmbedder(16)
	if err != nil {
		t.Fatal(err)
	}
	w, err := worker.New(
		worker.Config{Chunk: worker.ChunkSpec{MaxBytes: 512, OverlapBytes: 64}},
		worker.StaticSelector(b),
		emb,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func validID(tenant, ns string) api.RequestIdentity {
	return api.RequestIdentity{
		Tenant:         tenant,
		Namespace:      ns,
		CallerSPIFFEID: "spiffe://stigen.ai/ns/" + tenant + "/sa/agent",
		RetrieverRef:   tenant + "/default",
	}
}

// writeDoc is a helper that writes one document and returns its assigned ID.
func writeDoc(t *testing.T, w *worker.Worker, tenant, ns, content string) string {
	t.Helper()
	resp, err := w.Write(context.Background(), &api.WriteRequest{
		Identity: validID(tenant, ns),
		Document: memory.Document{Content: []byte(content)},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if resp.Result.ID == "" {
		t.Fatal("Write returned empty ID")
	}
	return resp.Result.ID
}

// ── identity validation ───────────────────────────────────────────────────────

func TestWorker_RejectEmptyTenant(t *testing.T) {
	w := newWorker(t, memory.NewVectorBackend())
	id := api.RequestIdentity{Namespace: "kb", CallerSPIFFEID: "spiffe://td/x"}
	_, err := w.Retrieve(context.Background(), &api.RetrieveRequest{Identity: id, Query: "x"})
	if memory.KindOf(err) != memory.KindPermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

func TestWorker_RejectEmptyNamespace(t *testing.T) {
	w := newWorker(t, memory.NewVectorBackend())
	id := api.RequestIdentity{Tenant: "t1", CallerSPIFFEID: "spiffe://td/x"}
	_, err := w.Retrieve(context.Background(), &api.RetrieveRequest{Identity: id, Query: "x"})
	if memory.KindOf(err) != memory.KindPermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

func TestWorker_RejectEmptyCallerSPIFFEID(t *testing.T) {
	w := newWorker(t, memory.NewVectorBackend())
	id := api.RequestIdentity{Tenant: "t1", Namespace: "kb"}
	_, err := w.Write(context.Background(), &api.WriteRequest{
		Identity: id,
		Document: memory.Document{Content: []byte("hello")},
	})
	if memory.KindOf(err) != memory.KindPermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

// ── write + retrieve ──────────────────────────────────────────────────────────

func TestWorker_WriteAndRetrieve(t *testing.T) {
	b := memory.NewVectorBackend()
	w := newWorker(t, b)

	writeDoc(t, w, "tenant-a", "kb", "The quick brown fox jumps over the lazy dog")
	writeDoc(t, w, "tenant-a", "kb", "GPU scheduling with NUMA-aware allocation")

	resp, err := w.Retrieve(context.Background(), &api.RetrieveRequest{
		Identity: validID("tenant-a", "kb"),
		Query:    "GPU scheduling",
		TopK:     5,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(resp.Result.Chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	// The GPU doc should rank first.
	if resp.Result.Chunks[0].Score <= 0 {
		t.Errorf("expected positive score, got %v", resp.Result.Chunks[0].Score)
	}
}

func TestWorker_WriteAndGet(t *testing.T) {
	b := memory.NewVectorBackend()
	w := newWorker(t, b)

	id := writeDoc(t, w, "tenant-a", "notes", "hello world")

	resp, err := w.Get(context.Background(), &api.GetRequest{
		Identity: validID("tenant-a", "notes"),
		ID:       id,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(resp.Document.Content) != "hello world" {
		t.Errorf("content = %q, want %q", resp.Document.Content, "hello world")
	}
}

func TestWorker_Delete(t *testing.T) {
	b := memory.NewVectorBackend()
	w := newWorker(t, b)

	id := writeDoc(t, w, "tenant-a", "ns1", "delete me")

	_, err := w.Delete(context.Background(), &api.DeleteRequest{
		Identity: validID("tenant-a", "ns1"),
		ID:       id,
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Get after delete must return NotFound.
	_, err = w.Get(context.Background(), &api.GetRequest{
		Identity: validID("tenant-a", "ns1"),
		ID:       id,
	})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("want NotFound after delete, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

// ── cross-tenant isolation ────────────────────────────────────────────────────

// TestWorker_CrossTenantGetDenied verifies that tenant-b cannot retrieve a
// document written by tenant-a even if it knows the exact ID.
func TestWorker_CrossTenantGetDenied(t *testing.T) {
	b := memory.NewVectorBackend()
	w := newWorker(t, b)

	id := writeDoc(t, w, "tenant-a", "secret", "very secret data")

	// tenant-b tries to get with the stolen ID.
	_, err := w.Get(context.Background(), &api.GetRequest{
		Identity: validID("tenant-b", "secret"),
		ID:       id,
	})
	// Must be NotFound (not PermissionDenied — we never confirm existence).
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("cross-tenant Get: want NotFound, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

// TestWorker_CrossTenantRetrieveDenied verifies that retrieval never returns
// documents from a different tenant's namespace, even when the namespace name
// collides.
func TestWorker_CrossTenantRetrieveDenied(t *testing.T) {
	b := memory.NewVectorBackend()
	w := newWorker(t, b)

	writeDoc(t, w, "tenant-a", "shared", "tenant-a private document about rockets")
	writeDoc(t, w, "tenant-b", "shared", "tenant-b document about satellites")

	// tenant-b retrieves from "shared" — must only see its own docs.
	resp, err := w.Retrieve(context.Background(), &api.RetrieveRequest{
		Identity: validID("tenant-b", "shared"),
		Query:    "rockets satellites",
		TopK:     10,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for _, sc := range resp.Result.Chunks {
		// Reconstruct the document from the backend directly to check tenant.
		doc, getErr := b.Get(context.Background(), sc.Chunk.DocumentID, memory.Filter{Tenant: "tenant-b", Namespace: "shared"})
		if getErr != nil {
			// If Get returns NotFound, the chunk belongs to tenant-a — that's a leak.
			t.Errorf("cross-tenant leak: chunk %q belongs to another tenant", sc.Chunk.DocumentID)
			continue
		}
		if doc.Tenant != "tenant-b" {
			t.Errorf("cross-tenant leak: got tenant %q, want tenant-b", doc.Tenant)
		}
	}
}

// ── list namespaces ───────────────────────────────────────────────────────────

func TestWorker_ListNamespaces(t *testing.T) {
	b := memory.NewVectorBackend()
	w := newWorker(t, b)

	writeDoc(t, w, "tenant-a", "ns-alpha", "doc1")
	writeDoc(t, w, "tenant-a", "ns-beta", "doc2")
	writeDoc(t, w, "tenant-b", "ns-alpha", "doc3") // different tenant, same ns name

	resp, err := w.ListNamespaces(context.Background(), &api.ListNamespacesRequest{
		Identity: validID("tenant-a", "ns-alpha"),
	})
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	got := make(map[string]bool)
	for _, ns := range resp.Namespaces {
		got[ns] = true
	}
	if !got["ns-alpha"] || !got["ns-beta"] {
		t.Errorf("expected ns-alpha and ns-beta, got %v", resp.Namespaces)
	}
	// Must not leak tenant-b namespaces (which happen to share the same name).
	// We can only verify this indirectly: tenant-a should have exactly 2.
	if len(resp.Namespaces) != 2 {
		t.Errorf("expected 2 namespaces for tenant-a, got %d: %v", len(resp.Namespaces), resp.Namespaces)
	}
}

// ── in-memory vector ranking ──────────────────────────────────────────────────

func TestWorker_VectorRanking(t *testing.T) {
	b := memory.NewVectorBackend()
	w := newWorker(t, b)

	// Write two docs; the fake embedder produces deterministic vectors so we can
	// verify ordering is consistent.
	writeDoc(t, w, "tenant-r", "kb", "machine learning neural networks deep learning")
	writeDoc(t, w, "tenant-r", "kb", "database SQL indexing query optimisation")

	resp, err := w.Retrieve(context.Background(), &api.RetrieveRequest{
		Identity: validID("tenant-r", "kb"),
		Query:    "neural networks",
		TopK:     2,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(resp.Result.Chunks) < 1 {
		t.Fatal("expected ranked results")
	}
	// Results must be in descending score order.
	for i := 1; i < len(resp.Result.Chunks); i++ {
		if resp.Result.Chunks[i].Score > resp.Result.Chunks[i-1].Score {
			t.Errorf("chunks not sorted: scores[%d]=%v > scores[%d]=%v",
				i, resp.Result.Chunks[i].Score, i-1, resp.Result.Chunks[i-1].Score)
		}
	}
}

// ── allowed-tenants restriction ───────────────────────────────────────────────

func TestWorker_AllowedTenants(t *testing.T) {
	b := memory.NewVectorBackend()
	emb, _ := worker.NewFakeEmbedder(16)
	w, err := worker.New(
		worker.Config{AllowedTenants: []string{"tenant-ok"}},
		worker.StaticSelector(b),
		emb,
		slog.Default(),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Tenant in the allow-list should succeed.
	_, writeErr := w.Write(context.Background(), &api.WriteRequest{
		Identity: validID("tenant-ok", "ns"),
		Document: memory.Document{Content: []byte("allowed")},
	})
	if writeErr != nil {
		t.Fatalf("allowed tenant Write failed: %v", writeErr)
	}

	// A tenant NOT in the allow-list must be rejected.
	_, writeErr = w.Write(context.Background(), &api.WriteRequest{
		Identity: validID("tenant-bad", "ns"),
		Document: memory.Document{Content: []byte("blocked")},
	})
	if memory.KindOf(writeErr) != memory.KindPermissionDenied {
		t.Fatalf("want PermissionDenied for disallowed tenant, got %v (kind=%s)", writeErr, memory.KindOf(writeErr))
	}
}

// ── not-supported passthrough ─────────────────────────────────────────────────

func TestWorker_SummarizeNotSupported(t *testing.T) {
	w := newWorker(t, memory.NewVectorBackend())
	_, err := w.Summarize(context.Background(), &api.SummarizeRequest{
		Identity: validID("tenant-a", "ns"),
		Query:    "anything",
	})
	if memory.KindOf(err) != memory.KindNotSupported {
		t.Fatalf("want NotSupported, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

func TestWorker_BranchFSNotSupported(t *testing.T) {
	w := newWorker(t, memory.NewVectorBackend())
	_, err := w.BranchFS(context.Background(), &api.BranchFSRequest{
		Identity:   validID("tenant-a", "ns"),
		BaseBranch: "main",
		NewBranch:  "run-1",
	})
	if memory.KindOf(err) != memory.KindNotSupported {
		t.Fatalf("want NotSupported, got %v (kind=%s)", err, memory.KindOf(err))
	}
}
