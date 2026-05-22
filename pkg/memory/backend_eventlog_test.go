package memory_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/memory"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func writeEventDoc(t *testing.T, b memory.Backend, tenant, ns, content string) string {
	t.Helper()
	wr, err := b.Write(context.Background(), memory.Document{
		Tenant:    tenant,
		Namespace: ns,
		Content:   []byte(content),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return wr.ID
}

// ── Write / Get round-trip ────────────────────────────────────────────────────

func TestEventLog_WriteGetRoundTrip(t *testing.T) {
	b := memory.NewEventLogBackend()
	id := writeEventDoc(t, b, "tenant-a", "ns", "hello world")

	got, err := b.Get(context.Background(), id, memory.Filter{Tenant: "tenant-a", Namespace: "ns"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Content) != "hello world" {
		t.Errorf("content = %q, want %q", got.Content, "hello world")
	}
}

// ── Upsert: latest Write wins on Get ─────────────────────────────────────────

func TestEventLog_Write_Upsert(t *testing.T) {
	b := memory.NewEventLogBackend()
	ctx := context.Background()

	// Write v1.
	wr1, err := b.Write(ctx, memory.Document{
		ID:        "fixed-id",
		Tenant:    "t1",
		Namespace: "ns",
		Content:   []byte("v1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Write v2 with same ID.
	_, err = b.Write(ctx, memory.Document{
		ID:        wr1.ID,
		Tenant:    "t1",
		Namespace: "ns",
		Content:   []byte("v2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ctx, wr1.ID, memory.Filter{Tenant: "t1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Content) != "v2" {
		t.Errorf("upsert: content = %q, want v2", got.Content)
	}
}

// ── Delete + tombstone ────────────────────────────────────────────────────────

func TestEventLog_Delete(t *testing.T) {
	b := memory.NewEventLogBackend()
	id := writeEventDoc(t, b, "t1", "ns", "delete me")

	if err := b.Delete(context.Background(), id, memory.Filter{Tenant: "t1", Namespace: "ns"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := b.Get(context.Background(), id, memory.Filter{Tenant: "t1", Namespace: "ns"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("want NotFound after delete, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

func TestEventLog_Delete_NotFound(t *testing.T) {
	b := memory.NewEventLogBackend()
	err := b.Delete(context.Background(), "nonexistent", memory.Filter{Tenant: "t1", Namespace: "ns"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// ── Cross-tenant isolation ────────────────────────────────────────────────────

func TestEventLog_CrossTenantGetReturnsNotFound(t *testing.T) {
	b := memory.NewEventLogBackend()
	id := writeEventDoc(t, b, "tenant-a", "ns", "secret")

	_, err := b.Get(context.Background(), id, memory.Filter{Tenant: "tenant-b", Namespace: "ns"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("cross-tenant Get must be NotFound, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

func TestEventLog_CrossTenantRetrieve(t *testing.T) {
	b := memory.NewEventLogBackend()
	writeEventDoc(t, b, "tenant-a", "shared", "tenant-a secret data")
	writeEventDoc(t, b, "tenant-b", "shared", "tenant-b public data")

	res, err := b.Retrieve(context.Background(), "data", 10,
		memory.Filter{Tenant: "tenant-b", Namespace: "shared"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for _, sc := range res.Chunks {
		doc, getErr := b.Get(context.Background(), sc.Chunk.DocumentID,
			memory.Filter{Tenant: "tenant-b", Namespace: "shared"})
		if getErr != nil {
			t.Errorf("result chunk not readable by tenant-b: %v", getErr)
			continue
		}
		if doc.Tenant != "tenant-b" {
			t.Errorf("cross-tenant leak: got tenant %q", doc.Tenant)
		}
	}
}

// ── Retrieve ──────────────────────────────────────────────────────────────────

func TestEventLog_Retrieve_SubstringMatch(t *testing.T) {
	b := memory.NewEventLogBackend()
	writeEventDoc(t, b, "t1", "ns", "golang concurrency channels")
	writeEventDoc(t, b, "t1", "ns", "python machine learning")
	writeEventDoc(t, b, "t1", "ns", "rust memory safety")

	res, err := b.Retrieve(context.Background(), "concurrency", 10,
		memory.Filter{Tenant: "t1", Namespace: "ns"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(res.Chunks))
	}
}

func TestEventLog_Retrieve_TopK(t *testing.T) {
	b := memory.NewEventLogBackend()
	for i := 0; i < 5; i++ {
		writeEventDoc(t, b, "t1", "ns", fmt.Sprintf("match me doc%d", i))
	}
	res, err := b.Retrieve(context.Background(), "match", 3,
		memory.Filter{Tenant: "t1", Namespace: "ns"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Chunks) != 3 {
		t.Errorf("topK=3: got %d chunks", len(res.Chunks))
	}
	if res.Total != 5 {
		t.Errorf("total=%d, want 5", res.Total)
	}
}

// ── ListNamespaces ────────────────────────────────────────────────────────────

func TestEventLog_ListNamespaces(t *testing.T) {
	b := memory.NewEventLogBackend()
	writeEventDoc(t, b, "t1", "alpha", "a")
	writeEventDoc(t, b, "t1", "beta", "b")
	writeEventDoc(t, b, "t2", "gamma", "c")

	nss, err := b.ListNamespaces(context.Background(), memory.Filter{Tenant: "t1"})
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	got := map[string]bool{}
	for _, ns := range nss {
		got[ns] = true
	}
	if !got["alpha"] || !got["beta"] {
		t.Errorf("expected alpha+beta, got %v", nss)
	}
	if got["gamma"] {
		t.Error("t2 namespace leaked to t1")
	}
}

// ── Summarize returns ErrNotSupported ─────────────────────────────────────────

func TestEventLog_SummarizeNotSupported(t *testing.T) {
	b := memory.NewEventLogBackend()
	_, err := b.Summarize(context.Background(), "anything", memory.Filter{Tenant: "t1"})
	if memory.KindOf(err) != memory.KindNotSupported {
		t.Fatalf("want NotSupported, got %v", err)
	}
}

// ── FS ops not supported ──────────────────────────────────────────────────────

func TestEventLog_BranchNotSupported(t *testing.T) {
	b := memory.NewEventLogBackend()
	_, err := b.Branch(context.Background(), "main", "run-1", memory.Filter{Tenant: "t1", Namespace: "ns"})
	if memory.KindOf(err) != memory.KindNotSupported {
		t.Fatalf("want NotSupported, got %v", err)
	}
}

// ── Required fields ───────────────────────────────────────────────────────────

func TestEventLog_Write_MissingTenant(t *testing.T) {
	b := memory.NewEventLogBackend()
	_, err := b.Write(context.Background(), memory.Document{Namespace: "ns", Content: []byte("x")})
	if memory.KindOf(err) != memory.KindInvalid {
		t.Fatalf("want Invalid for missing Tenant, got %v", err)
	}
}

func TestEventLog_Get_MissingTenant(t *testing.T) {
	b := memory.NewEventLogBackend()
	_, err := b.Get(context.Background(), "any-id", memory.Filter{Namespace: "ns"})
	if memory.KindOf(err) != memory.KindInvalid {
		t.Fatalf("want Invalid for missing Tenant, got %v", err)
	}
}

func TestEventLog_Retrieve_MissingTenant(t *testing.T) {
	b := memory.NewEventLogBackend()
	_, err := b.Retrieve(context.Background(), "q", 5, memory.Filter{})
	if memory.KindOf(err) != memory.KindPermissionDenied {
		t.Fatalf("want PermissionDenied for missing Tenant, got %v", err)
	}
}
