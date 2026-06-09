package memory

import (
	"context"
	"path/filepath"
	"testing"
)

// TestPersistentVectorBackend_DurableAcrossRestart proves the embedded vector
// store survives a "restart" (a fresh backend from the same snapshot file),
// preserves tenant isolation, and keeps cosine ranking — all pure-Go, no DB.
func TestPersistentVectorBackend_DurableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.gob")

	b1, err := NewPersistentVectorBackend(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	docs := []Document{
		{Tenant: "t-a", Namespace: "ns1", ID: "d1", Content: []byte("hello alpha"), Embedding: []float32{1, 0, 0}},
		{Tenant: "t-a", Namespace: "ns1", ID: "d2", Content: []byte("hello beta"), Embedding: []float32{0, 1, 0}},
		{Tenant: "t-b", Namespace: "ns1", ID: "d3", Content: []byte("hello other"), Embedding: []float32{0, 0, 1}},
	}
	for _, d := range docs {
		if _, err := b1.Write(ctx, d); err != nil {
			t.Fatalf("write %s: %v", d.ID, err)
		}
	}

	// Simulate a restart: a fresh backend from the same snapshot file.
	b2, err := NewPersistentVectorBackend(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Durability: tenant-a sees its 2 docs after reload.
	res, err := b2.Retrieve(ctx, "hello", 10, Filter{Tenant: "t-a", Namespace: "ns1"})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if res.Total != 2 {
		t.Fatalf("after reload: total=%d, want 2 (tenant-a docs)", res.Total)
	}
	// Tenant isolation preserved through persistence.
	for _, c := range res.Chunks {
		if c.Chunk.DocumentID == "d3" {
			t.Fatal("tenant isolation breached: t-a saw t-b's doc after reload")
		}
	}

	// Cosine ranking works after reload: a query aligned with d1 ranks it first.
	cres, err := b2.RetrieveWithEmbedding(ctx, []float32{1, 0, 0}, 1, Filter{Tenant: "t-a", Namespace: "ns1"})
	if err != nil {
		t.Fatalf("retrieve-embed: %v", err)
	}
	if len(cres.Chunks) != 1 || cres.Chunks[0].Chunk.DocumentID != "d1" {
		t.Fatalf("cosine top result after reload = %+v, want d1", cres.Chunks)
	}

	// Ownership enforced after reload: t-a cannot Get t-b's doc.
	if _, err := b2.Get(ctx, "d3", Filter{Tenant: "t-a"}); err == nil {
		t.Fatal("Get should not return t-b's doc to t-a after reload")
	}
}

// TestPersistentVectorBackend_DeletePersists proves a delete survives a restart.
func TestPersistentVectorBackend_DeletePersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.gob")

	b1, err := NewPersistentVectorBackend(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := b1.Write(ctx, Document{Tenant: "t", Namespace: "n", ID: "x", Content: []byte("doc"), Embedding: []float32{1, 2, 3}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b1.Delete(ctx, "x", Filter{Tenant: "t", Namespace: "n"}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	b2, err := NewPersistentVectorBackend(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := b2.Get(ctx, "x", Filter{Tenant: "t", Namespace: "n"}); err == nil {
		t.Fatal("deleted doc resurrected after reload")
	}
}
