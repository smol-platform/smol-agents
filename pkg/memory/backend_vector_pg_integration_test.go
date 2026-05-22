//go:build integration

// Integration tests for PgvectorBackend. These tests require a live PostgreSQL
// server with the pgvector extension and are skipped when PGVECTOR_DSN is unset.
//
// Usage:
//
//	PGVECTOR_DSN="postgres://user:pass@localhost:5432/testdb?sslmode=disable" \
//	  go test -tags integration ./pkg/memory/... -run TestPgvectorIntegration
package memory_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stigen/smol-agents/pkg/memory"
)

func TestPgvectorIntegration_WriteGetDelete(t *testing.T) {
	dsn := os.Getenv("PGVECTOR_DSN")
	if dsn == "" {
		t.Skip("PGVECTOR_DSN not set; skipping pgvector integration tests")
	}
	ctx := context.Background()
	b, err := memory.NewPgvectorBackend(ctx, memory.PgvectorConfig{
		DSN:           dsn,
		EmbeddingDims: 4,
	})
	if err != nil {
		t.Fatalf("NewPgvectorBackend: %v", err)
	}
	defer b.Close()

	if err := b.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	// Write.
	wr, err := b.Write(ctx, memory.Document{
		Tenant:    "integ-tenant",
		Namespace: "integ-ns",
		Content:   []byte("pgvector integration test document"),
		Embedding: []float32{0.1, 0.2, 0.3, 0.4},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	t.Logf("Written document ID: %s", wr.ID)

	// Get.
	got, err := b.Get(ctx, wr.ID, memory.Filter{Tenant: "integ-tenant", Namespace: "integ-ns"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Content) != "pgvector integration test document" {
		t.Errorf("content = %q", got.Content)
	}

	// Cross-tenant isolation.
	_, err = b.Get(ctx, wr.ID, memory.Filter{Tenant: "other-tenant", Namespace: "integ-ns"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Errorf("cross-tenant Get must return NotFound, got %v", err)
	}

	// Delete.
	if err := b.Delete(ctx, wr.ID, memory.Filter{Tenant: "integ-tenant", Namespace: "integ-ns"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = b.Get(ctx, wr.ID, memory.Filter{Tenant: "integ-tenant", Namespace: "integ-ns"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Errorf("want NotFound after delete, got %v", err)
	}
}

func TestPgvectorIntegration_RetrieveWithEmbedding(t *testing.T) {
	dsn := os.Getenv("PGVECTOR_DSN")
	if dsn == "" {
		t.Skip("PGVECTOR_DSN not set")
	}
	ctx := context.Background()
	b, err := memory.NewPgvectorBackend(ctx, memory.PgvectorConfig{
		DSN:           dsn,
		EmbeddingDims: 4,
	})
	if err != nil {
		t.Fatalf("NewPgvectorBackend: %v", err)
	}
	defer b.Close()
	_ = b.EnsureSchema(ctx)

	docs := []struct {
		content   string
		embedding []float32
	}{
		{"doc-horizontal", []float32{1, 0, 0, 0}},
		{"doc-vertical", []float32{0, 1, 0, 0}},
	}
	for _, d := range docs {
		_, err := b.Write(ctx, memory.Document{
			Tenant:    "integ-tenant",
			Namespace: "vec-ns",
			Content:   []byte(d.content),
			Embedding: d.embedding,
		})
		if err != nil {
			t.Fatalf("Write %q: %v", d.content, err)
		}
	}

	res, err := b.RetrieveWithEmbedding(ctx, []float32{1, 0, 0, 0}, 2,
		memory.Filter{Tenant: "integ-tenant", Namespace: "vec-ns"})
	if err != nil {
		t.Fatalf("RetrieveWithEmbedding: %v", err)
	}
	if len(res.Chunks) == 0 {
		t.Fatal("expected at least one result")
	}
	// The first result should be doc-horizontal (closest to [1,0,0,0]).
	if res.Chunks[0].Score <= 0 {
		t.Errorf("expected positive cosine score, got %v", res.Chunks[0].Score)
	}
	t.Logf("Top result: %s score=%.4f", res.Chunks[0].Chunk.DocumentID, res.Chunks[0].Score)
}

func TestPgvectorIntegration_ListNamespaces(t *testing.T) {
	dsn := os.Getenv("PGVECTOR_DSN")
	if dsn == "" {
		t.Skip("PGVECTOR_DSN not set")
	}
	ctx := context.Background()
	b, err := memory.NewPgvectorBackend(ctx, memory.PgvectorConfig{
		DSN:           dsn,
		EmbeddingDims: 4,
	})
	if err != nil {
		t.Fatalf("NewPgvectorBackend: %v", err)
	}
	defer b.Close()
	_ = b.EnsureSchema(ctx)

	tenant := fmt.Sprintf("ns-integ-tenant-%d", os.Getpid())
	for _, ns := range []string{"alpha", "beta"} {
		_, _ = b.Write(ctx, memory.Document{
			Tenant:    tenant,
			Namespace: ns,
			Content:   []byte("x"),
		})
	}

	nss, err := b.ListNamespaces(ctx, memory.Filter{Tenant: tenant})
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
}
