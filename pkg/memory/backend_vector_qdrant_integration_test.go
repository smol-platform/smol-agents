//go:build integration

// Integration tests for QdrantBackend. Require a live Qdrant gRPC endpoint.
// Skipped when QDRANT_ADDR is unset.
//
// Usage:
//
//	QDRANT_ADDR="localhost:6334" \
//	  go test -tags integration ./pkg/memory/... -run TestQdrantIntegration
package memory_test

import (
	"context"
	"os"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/memory"
)

func TestQdrantIntegration_WriteGetDelete(t *testing.T) {
	addr := os.Getenv("QDRANT_ADDR")
	if addr == "" {
		t.Skip("QDRANT_ADDR not set; skipping Qdrant integration tests")
	}
	ctx := context.Background()
	b, err := memory.NewQdrantBackend(ctx, memory.QdrantConfig{
		Addr:          addr,
		Collection:    "integ-memory",
		EmbeddingDims: 4,
	})
	if err != nil {
		t.Fatalf("NewQdrantBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	if err := b.EnsureCollection(ctx); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}

	wr, err := b.Write(ctx, memory.Document{
		Tenant:    "qdrant-tenant",
		Namespace: "integ-ns",
		Content:   []byte("qdrant integration test"),
		Embedding: []float32{0.1, 0.2, 0.3, 0.4},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := b.Get(ctx, wr.ID, memory.Filter{Tenant: "qdrant-tenant", Namespace: "integ-ns"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Content) != "qdrant integration test" {
		t.Errorf("content = %q", got.Content)
	}

	// Cross-tenant isolation.
	_, err = b.Get(ctx, wr.ID, memory.Filter{Tenant: "other-tenant", Namespace: "integ-ns"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Errorf("cross-tenant Get must return NotFound, got %v", err)
	}

	if err := b.Delete(ctx, wr.ID, memory.Filter{Tenant: "qdrant-tenant", Namespace: "integ-ns"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestQdrantIntegration_RetrieveWithEmbedding(t *testing.T) {
	addr := os.Getenv("QDRANT_ADDR")
	if addr == "" {
		t.Skip("QDRANT_ADDR not set")
	}
	ctx := context.Background()
	b, err := memory.NewQdrantBackend(ctx, memory.QdrantConfig{
		Addr:          addr,
		Collection:    "integ-memory-vec",
		EmbeddingDims: 4,
	})
	if err != nil {
		t.Fatalf("NewQdrantBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	_ = b.EnsureCollection(ctx)

	_, _ = b.Write(ctx, memory.Document{
		Tenant:    "qt",
		Namespace: "ns",
		Content:   []byte("horizontal"),
		Embedding: []float32{1, 0, 0, 0},
	})
	_, _ = b.Write(ctx, memory.Document{
		Tenant:    "qt",
		Namespace: "ns",
		Content:   []byte("vertical"),
		Embedding: []float32{0, 1, 0, 0},
	})

	res, err := b.RetrieveWithEmbedding(ctx, []float32{1, 0, 0, 0}, 2,
		memory.Filter{Tenant: "qt", Namespace: "ns"})
	if err != nil {
		t.Fatalf("RetrieveWithEmbedding: %v", err)
	}
	if len(res.Chunks) == 0 {
		t.Fatal("expected at least one result")
	}
}
