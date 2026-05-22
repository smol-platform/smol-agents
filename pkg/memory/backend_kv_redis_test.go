package memory_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stigen/smol-agents/pkg/memory"
)

// newRedisBackend starts a miniredis server and connects a RedisBackend to it.
// The server is stopped when the test ends.
func newRedisBackend(t *testing.T) memory.Backend {
	t.Helper()
	mr := miniredis.RunT(t)
	b, err := memory.NewRedisBackend(context.Background(), memory.RedisConfig{
		Addr: mr.Addr(),
	})
	if err != nil {
		t.Fatalf("NewRedisBackend: %v", err)
	}
	return b
}

// ── Write / Get round-trip ────────────────────────────────────────────────────

func TestRedis_WriteGetRoundTrip(t *testing.T) {
	b := newRedisBackend(t)
	wr, err := b.Write(context.Background(), memory.Document{
		Tenant:    "t1",
		Namespace: "ns",
		Content:   []byte("hello redis"),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := b.Get(context.Background(), wr.ID, memory.Filter{Tenant: "t1", Namespace: "ns"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Content) != "hello redis" {
		t.Errorf("content = %q, want %q", got.Content, "hello redis")
	}
}

// ── Upsert overwrites existing document ──────────────────────────────────────

func TestRedis_WriteUpsert(t *testing.T) {
	b := newRedisBackend(t)
	ctx := context.Background()

	wr1, _ := b.Write(ctx, memory.Document{
		ID:        "stable-id",
		Tenant:    "t1",
		Namespace: "ns",
		Content:   []byte("v1"),
	})
	_, _ = b.Write(ctx, memory.Document{
		ID:        wr1.ID,
		Tenant:    "t1",
		Namespace: "ns",
		Content:   []byte("v2"),
	})
	got, err := b.Get(ctx, wr1.ID, memory.Filter{Tenant: "t1", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Content) != "v2" {
		t.Errorf("upsert: got %q, want v2", got.Content)
	}
}

// ── Delete ────────────────────────────────────────────────────────────────────

func TestRedis_Delete(t *testing.T) {
	b := newRedisBackend(t)
	ctx := context.Background()

	wr, _ := b.Write(ctx, memory.Document{Tenant: "t1", Namespace: "ns", Content: []byte("bye")})
	if err := b.Delete(ctx, wr.ID, memory.Filter{Tenant: "t1", Namespace: "ns"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := b.Get(ctx, wr.ID, memory.Filter{Tenant: "t1", Namespace: "ns"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("want NotFound after delete, got %v", err)
	}
}

func TestRedis_Delete_NotFound(t *testing.T) {
	b := newRedisBackend(t)
	err := b.Delete(context.Background(), "ghost", memory.Filter{Tenant: "t1", Namespace: "ns"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// ── Cross-tenant isolation ────────────────────────────────────────────────────

func TestRedis_CrossTenantGetNotFound(t *testing.T) {
	b := newRedisBackend(t)
	wr, _ := b.Write(context.Background(), memory.Document{
		Tenant:    "tenant-a",
		Namespace: "ns",
		Content:   []byte("secret"),
	})
	_, err := b.Get(context.Background(), wr.ID, memory.Filter{Tenant: "tenant-b", Namespace: "ns"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Fatalf("cross-tenant Get must be NotFound, got %v (kind=%s)", err, memory.KindOf(err))
	}
}

func TestRedis_CrossTenantRetrieve(t *testing.T) {
	b := newRedisBackend(t)

	_, _ = b.Write(context.Background(), memory.Document{
		Tenant:    "tenant-a",
		Namespace: "shared",
		Content:   []byte("tenant-a private"),
	})
	_, _ = b.Write(context.Background(), memory.Document{
		Tenant:    "tenant-b",
		Namespace: "shared",
		Content:   []byte("tenant-b document"),
	})

	res, err := b.Retrieve(context.Background(), "tenant-a", 10,
		memory.Filter{Tenant: "tenant-b", Namespace: "shared"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for _, sc := range res.Chunks {
		doc, getErr := b.Get(context.Background(), sc.Chunk.DocumentID,
			memory.Filter{Tenant: "tenant-b", Namespace: "shared"})
		if getErr != nil {
			t.Errorf("result chunk not owned by tenant-b: %v", getErr)
			continue
		}
		if doc.Tenant != "tenant-b" {
			t.Errorf("cross-tenant leak: chunk belongs to %q", doc.Tenant)
		}
	}
}

// ── Retrieve ──────────────────────────────────────────────────────────────────

func TestRedis_Retrieve_SubstringMatch(t *testing.T) {
	b := newRedisBackend(t)
	ctx := context.Background()

	_, _ = b.Write(ctx, memory.Document{Tenant: "t1", Namespace: "ns", Content: []byte("golang channels concurrency")})
	_, _ = b.Write(ctx, memory.Document{Tenant: "t1", Namespace: "ns", Content: []byte("python pandas dataframe")})

	res, err := b.Retrieve(ctx, "golang", 10, memory.Filter{Tenant: "t1", Namespace: "ns"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(res.Chunks))
	}
}

func TestRedis_Retrieve_TopK(t *testing.T) {
	b := newRedisBackend(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, _ = b.Write(ctx, memory.Document{
			Tenant:    "t1",
			Namespace: "ns",
			Content:   []byte(fmt.Sprintf("match me %d", i)),
		})
	}
	res, err := b.Retrieve(ctx, "match", 3, memory.Filter{Tenant: "t1", Namespace: "ns"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Chunks) > 3 {
		t.Errorf("topK=3 exceeded: got %d", len(res.Chunks))
	}
}

// ── ListNamespaces ────────────────────────────────────────────────────────────

func TestRedis_ListNamespaces(t *testing.T) {
	b := newRedisBackend(t)
	ctx := context.Background()

	_, _ = b.Write(ctx, memory.Document{Tenant: "t1", Namespace: "alpha", Content: []byte("a")})
	_, _ = b.Write(ctx, memory.Document{Tenant: "t1", Namespace: "beta", Content: []byte("b")})
	_, _ = b.Write(ctx, memory.Document{Tenant: "t2", Namespace: "gamma", Content: []byte("c")})

	nss, err := b.ListNamespaces(ctx, memory.Filter{Tenant: "t1"})
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
		t.Error("t2 namespace leaked to t1 ListNamespaces")
	}
}

// ── Summarize returns ErrNotSupported ─────────────────────────────────────────

func TestRedis_SummarizeNotSupported(t *testing.T) {
	b := newRedisBackend(t)
	_, err := b.Summarize(context.Background(), "x", memory.Filter{Tenant: "t1"})
	if memory.KindOf(err) != memory.KindNotSupported {
		t.Fatalf("want NotSupported, got %v", err)
	}
}

// ── FS ops return ErrNotSupported ─────────────────────────────────────────────

func TestRedis_BranchNotSupported(t *testing.T) {
	b := newRedisBackend(t)
	_, err := b.Branch(context.Background(), "main", "run-1", memory.Filter{Tenant: "t1", Namespace: "ns"})
	if memory.KindOf(err) != memory.KindNotSupported {
		t.Fatalf("want NotSupported, got %v", err)
	}
}

// ── Required field validation ─────────────────────────────────────────────────

func TestRedis_Write_MissingTenant(t *testing.T) {
	b := newRedisBackend(t)
	_, err := b.Write(context.Background(), memory.Document{Namespace: "ns", Content: []byte("x")})
	if memory.KindOf(err) != memory.KindInvalid {
		t.Fatalf("want Invalid, got %v", err)
	}
}

func TestRedis_Get_MissingTenant(t *testing.T) {
	b := newRedisBackend(t)
	_, err := b.Get(context.Background(), "id", memory.Filter{Namespace: "ns"})
	if memory.KindOf(err) != memory.KindInvalid {
		t.Fatalf("want Invalid, got %v", err)
	}
}

func TestRedis_Retrieve_RequiresTenant(t *testing.T) {
	b := newRedisBackend(t)
	_, err := b.Retrieve(context.Background(), "q", 5, memory.Filter{})
	if memory.KindOf(err) != memory.KindPermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

func TestRedis_Retrieve_RequiresNamespace(t *testing.T) {
	b := newRedisBackend(t)
	_, err := b.Retrieve(context.Background(), "q", 5, memory.Filter{Tenant: "t1"})
	if memory.KindOf(err) != memory.KindInvalid {
		t.Fatalf("want Invalid for missing namespace, got %v", err)
	}
}
