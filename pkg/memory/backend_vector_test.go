package memory_test

import (
	"context"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/memory"
)

// ── write + get ───────────────────────────────────────────────────────────────

func TestVectorBackend_WriteAndGet(t *testing.T) {
	b := memory.NewVectorBackend()
	ctx := context.Background()

	doc := memory.Document{
		Tenant:    "t1",
		Namespace: "ns1",
		Content:   []byte("hello world"),
		Metadata:  map[string]string{"source": "test"},
	}
	res, err := b.Write(ctx, doc)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.ID == "" {
		t.Fatal("Write returned empty ID")
	}

	got, err := b.Get(ctx, res.ID, memory.Filter{Tenant: "t1", Namespace: "ns1"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Content) != "hello world" {
		t.Errorf("content = %q, want %q", got.Content, "hello world")
	}
	if got.Tenant != "t1" {
		t.Errorf("tenant = %q, want t1", got.Tenant)
	}
}

func TestVectorBackend_GetNotFound(t *testing.T) {
	b := memory.NewVectorBackend()
	_, err := b.Get(context.Background(), "nonexistent", memory.Filter{Tenant: "t1", Namespace: "ns1"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("want NotFound, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

// ── cross-tenant isolation (direct Get) ───────────────────────────────────────

func TestVectorBackend_CrossTenantGetReturnsNotFound(t *testing.T) {
	b := memory.NewVectorBackend()
	ctx := context.Background()

	res, _ := b.Write(ctx, memory.Document{
		Tenant:    "tenant-a",
		Namespace: "sec",
		Content:   []byte("secret"),
	})

	// Another tenant tries to Get by the same ID.
	_, err := b.Get(ctx, res.ID, memory.Filter{Tenant: "tenant-b", Namespace: "sec"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("cross-tenant Get must return NotFound, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

// ── delete ────────────────────────────────────────────────────────────────────

func TestVectorBackend_Delete(t *testing.T) {
	b := memory.NewVectorBackend()
	ctx := context.Background()

	res, _ := b.Write(ctx, memory.Document{
		Tenant:    "t1",
		Namespace: "ns1",
		Content:   []byte("to be deleted"),
	})

	if err := b.Delete(ctx, res.ID, memory.Filter{Tenant: "t1", Namespace: "ns1"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := b.Get(ctx, res.ID, memory.Filter{Tenant: "t1", Namespace: "ns1"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("after Delete: want NotFound, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

func TestVectorBackend_DeleteCrossTenantReturnsNotFound(t *testing.T) {
	b := memory.NewVectorBackend()
	ctx := context.Background()

	res, _ := b.Write(ctx, memory.Document{
		Tenant:    "tenant-a",
		Namespace: "ns1",
		Content:   []byte("belongs to a"),
	})

	err := b.Delete(ctx, res.ID, memory.Filter{Tenant: "tenant-b", Namespace: "ns1"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("cross-tenant Delete must return NotFound, got %v (kind=%s)", err, memory.KindOf(err))
	}

	// Document must still exist for the real owner.
	_, err = b.Get(ctx, res.ID, memory.Filter{Tenant: "tenant-a", Namespace: "ns1"})
	if err != nil {
		t.Errorf("original doc should still exist after cross-tenant delete attempt: %v", err)
	}
}

// ── retrieve (text scoring) ───────────────────────────────────────────────────

func TestVectorBackend_RetrieveRanksCorrectly(t *testing.T) {
	b := memory.NewVectorBackend()
	ctx := context.Background()

	docs := []struct {
		content string
	}{
		{"golang concurrency channels goroutines"},
		{"python machine learning tensorflow keras"},
		{"golang channels select goroutines concurrency"},
	}
	for _, d := range docs {
		_, err := b.Write(ctx, memory.Document{
			Tenant:    "t1",
			Namespace: "code",
			Content:   []byte(d.content),
			Embedding: make([]float32, 0), // no embedding; use text scoring
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	res, err := b.Retrieve(ctx, "golang channels", 2, memory.Filter{Tenant: "t1", Namespace: "code"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if res.Total < 2 {
		t.Errorf("total = %d, want >= 2", res.Total)
	}
	if len(res.Chunks) == 0 {
		t.Fatal("no chunks returned")
	}
	// Chunks must be in descending score order.
	for i := 1; i < len(res.Chunks); i++ {
		if res.Chunks[i].Score > res.Chunks[i-1].Score {
			t.Errorf("chunks[%d].Score %v > chunks[%d].Score %v", i, res.Chunks[i].Score, i-1, res.Chunks[i-1].Score)
		}
	}
}

func TestVectorBackend_RetrieveTenantIsolation(t *testing.T) {
	b := memory.NewVectorBackend()
	ctx := context.Background()

	// Write docs for two tenants with same namespace name.
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		_, err := b.Write(ctx, memory.Document{
			Tenant:    tenant,
			Namespace: "shared",
			Content:   []byte("document for " + tenant),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	res, err := b.Retrieve(ctx, "document", 10, memory.Filter{Tenant: "tenant-a", Namespace: "shared"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for _, sc := range res.Chunks {
		// Look up the doc to verify ownership.
		doc, getErr := b.Get(ctx, sc.Chunk.DocumentID, memory.Filter{Tenant: "tenant-a", Namespace: "shared"})
		if getErr != nil {
			t.Errorf("result chunk %q not owned by tenant-a", sc.Chunk.DocumentID)
			continue
		}
		if doc.Tenant != "tenant-a" {
			t.Errorf("cross-tenant leak: chunk belongs to %q", doc.Tenant)
		}
	}
}

func TestVectorBackend_RetrieveRequiresTenant(t *testing.T) {
	b := memory.NewVectorBackend()
	_, err := b.Retrieve(context.Background(), "x", 5, memory.Filter{})
	if memory.KindOf(err) != memory.KindPermissionDenied {
		t.Fatalf("want PermissionDenied when no tenant, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

// ── retrieve with embedding vectors ──────────────────────────────────────────

func TestVectorBackend_RetrieveWithEmbedding(t *testing.T) {
	b := memory.NewVectorBackend()
	ctx := context.Background()

	// Two documents with embeddings: [1,0] and [0,1].
	_, _ = b.Write(ctx, memory.Document{
		ID:        "doc-horizontal",
		Tenant:    "t1",
		Namespace: "ns",
		Content:   []byte("horizontal document"),
		Embedding: []float32{1, 0},
	})
	_, _ = b.Write(ctx, memory.Document{
		ID:        "doc-vertical",
		Tenant:    "t1",
		Namespace: "ns",
		Content:   []byte("vertical document"),
		Embedding: []float32{0, 1},
	})

	// Query embedding [1,0] should rank doc-horizontal first.
	res, err := b.RetrieveWithEmbedding(ctx, []float32{1, 0}, 2, memory.Filter{Tenant: "t1", Namespace: "ns"})
	if err != nil {
		t.Fatalf("RetrieveWithEmbedding: %v", err)
	}
	if len(res.Chunks) < 2 {
		t.Fatalf("want 2 chunks, got %d", len(res.Chunks))
	}
	if res.Chunks[0].Chunk.DocumentID != "doc-horizontal" {
		t.Errorf("first result = %q, want doc-horizontal", res.Chunks[0].Chunk.DocumentID)
	}
}

// ── list namespaces ───────────────────────────────────────────────────────────

func TestVectorBackend_ListNamespaces(t *testing.T) {
	b := memory.NewVectorBackend()
	ctx := context.Background()

	for _, ns := range []string{"alpha", "beta", "gamma"} {
		_, _ = b.Write(ctx, memory.Document{Tenant: "t1", Namespace: ns, Content: []byte("x")})
	}
	// Different tenant.
	_, _ = b.Write(ctx, memory.Document{Tenant: "t2", Namespace: "alpha", Content: []byte("x")})

	nss, err := b.ListNamespaces(ctx, memory.Filter{Tenant: "t1"})
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(nss) != 3 {
		t.Errorf("want 3 namespaces, got %d: %v", len(nss), nss)
	}
}

func TestVectorBackend_ListNamespacesRequiresTenant(t *testing.T) {
	b := memory.NewVectorBackend()
	_, err := b.ListNamespaces(context.Background(), memory.Filter{})
	if memory.KindOf(err) != memory.KindPermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

// ── upsert semantics ──────────────────────────────────────────────────────────

func TestVectorBackend_UpsertReplaces(t *testing.T) {
	b := memory.NewVectorBackend()
	ctx := context.Background()

	res, _ := b.Write(ctx, memory.Document{
		ID:        "fixed-id",
		Tenant:    "t1",
		Namespace: "ns",
		Content:   []byte("original"),
	})

	// Overwrite with same ID.
	_, _ = b.Write(ctx, memory.Document{
		ID:        res.ID,
		Tenant:    "t1",
		Namespace: "ns",
		Content:   []byte("updated"),
	})

	got, err := b.Get(ctx, res.ID, memory.Filter{Tenant: "t1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Content) != "updated" {
		t.Errorf("upsert: content = %q, want %q", got.Content, "updated")
	}
}

// ── write requires tenant + namespace ─────────────────────────────────────────

func TestVectorBackend_WriteMissingTenantRejected(t *testing.T) {
	b := memory.NewVectorBackend()
	_, err := b.Write(context.Background(), memory.Document{Namespace: "ns", Content: []byte("x")})
	if memory.KindOf(err) != memory.KindInvalid {
		t.Fatalf("want Invalid, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

// ── FS ops not supported ──────────────────────────────────────────────────────

func TestVectorBackend_BranchNotSupported(t *testing.T) {
	b := memory.NewVectorBackend()
	_, err := b.Branch(context.Background(), "main", "run-1", memory.Filter{Tenant: "t1", Namespace: "ns"})
	if memory.KindOf(err) != memory.KindNotSupported {
		t.Fatalf("want NotSupported, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

func TestVectorBackend_SnapshotNotSupported(t *testing.T) {
	b := memory.NewVectorBackend()
	_, err := b.Snapshot(context.Background(), "main", memory.Filter{Tenant: "t1", Namespace: "ns"})
	if memory.KindOf(err) != memory.KindNotSupported {
		t.Fatalf("want NotSupported, got %v (kind=%s)", err, memory.KindOf(err))
	}
}
