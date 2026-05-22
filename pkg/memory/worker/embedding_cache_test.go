package worker_test

import (
	"context"
	"sync"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/memory/worker"
)

// ── basic cache hit / miss ────────────────────────────────────────────────────

func TestCachedEmbedder_CacheHit(t *testing.T) {
	inner, _ := worker.NewFakeEmbedder(16)
	cached := worker.NewCachedEmbedder(inner, 10)

	ctx := context.Background()
	v1, err := cached.Embed(ctx, "hello")
	if err != nil {
		t.Fatalf("first Embed: %v", err)
	}
	if cached.Len() != 1 {
		t.Errorf("cache len = %d, want 1", cached.Len())
	}
	v2, err := cached.Embed(ctx, "hello")
	if err != nil {
		t.Fatalf("second Embed: %v", err)
	}
	if len(v1) != len(v2) {
		t.Fatalf("vector length mismatch: %d vs %d", len(v1), len(v2))
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Errorf("cache returned different vector at index %d", i)
		}
	}
	// Cache should still have exactly 1 entry.
	if cached.Len() != 1 {
		t.Errorf("cache len = %d, want 1 after hit", cached.Len())
	}
}

func TestCachedEmbedder_DifferentTexts(t *testing.T) {
	inner, _ := worker.NewFakeEmbedder(16)
	cached := worker.NewCachedEmbedder(inner, 10)
	ctx := context.Background()

	for _, text := range []string{"alpha", "beta", "gamma"} {
		if _, err := cached.Embed(ctx, text); err != nil {
			t.Fatalf("Embed(%q): %v", text, err)
		}
	}
	if cached.Len() != 3 {
		t.Errorf("cache len = %d, want 3", cached.Len())
	}
}

// ── LRU eviction ─────────────────────────────────────────────────────────────

func TestCachedEmbedder_LRUEviction(t *testing.T) {
	inner, _ := worker.NewFakeEmbedder(16)
	// Capacity of 3 entries.
	cached := worker.NewCachedEmbedder(inner, 3)
	ctx := context.Background()

	texts := []string{"a", "b", "c", "d"}
	for _, txt := range texts {
		if _, err := cached.Embed(ctx, txt); err != nil {
			t.Fatalf("Embed: %v", err)
		}
	}
	// Cache should cap at 3.
	if cached.Len() > 3 {
		t.Errorf("cache len = %d, want <= 3 after eviction", cached.Len())
	}
}

// ── Disabled cache (size 0) ───────────────────────────────────────────────────

func TestCachedEmbedder_Disabled(t *testing.T) {
	inner, _ := worker.NewFakeEmbedder(16)
	cached := worker.NewCachedEmbedder(inner, 0)
	ctx := context.Background()

	_, _ = cached.Embed(ctx, "test")
	if cached.Len() != 0 {
		t.Errorf("disabled cache: Len = %d, want 0", cached.Len())
	}
}

// ── Dims pass-through ─────────────────────────────────────────────────────────

func TestCachedEmbedder_Dims(t *testing.T) {
	inner, _ := worker.NewFakeEmbedder(32)
	cached := worker.NewCachedEmbedder(inner, 10)
	if cached.Dims() != 32 {
		t.Errorf("Dims() = %d, want 32", cached.Dims())
	}
}

// ── Vector is a fresh copy each time (no aliasing) ───────────────────────────

func TestCachedEmbedder_VectorNotAliased(t *testing.T) {
	inner, _ := worker.NewFakeEmbedder(8)
	cached := worker.NewCachedEmbedder(inner, 10)
	ctx := context.Background()

	v1, _ := cached.Embed(ctx, "same text")
	v1[0] = 999 // mutate the returned slice
	v2, _ := cached.Embed(ctx, "same text")
	if v2[0] == 999 {
		t.Error("cache returned aliased slice; mutation of one copy affected the next")
	}
}

// ── Concurrent access ─────────────────────────────────────────────────────────

func TestCachedEmbedder_ConcurrentSafe(t *testing.T) {
	inner, _ := worker.NewFakeEmbedder(16)
	cached := worker.NewCachedEmbedder(inner, 100)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			text := "text"
			if n%2 == 0 {
				text = "other"
			}
			_, err := cached.Embed(ctx, text)
			if err != nil {
				t.Errorf("concurrent Embed: %v", err)
			}
		}(i)
	}
	wg.Wait()
}

// ── Worker.Summarize with FakeSummarizer ─────────────────────────────────────

// TestWorker_SummarizeWithFakeSummarizer verifies that when a Summarizer is
// attached, Worker.Summarize retrieves docs and calls it instead of returning
// ErrNotSupported.
func TestWorker_SummarizeWithFakeSummarizer(t *testing.T) {
	b := newWorkerBackend(t)
	w := newWorkerWithSummarizer(t, b, &worker.FakeSummarizer{})

	// Write a document so Retrieve has something.
	writeWorkerDoc(t, w, "tenant-x", "kb", "something about machine learning")

	req := workerSummarizeReq("tenant-x", "kb", "machine learning")
	resp, err := w.Summarize(context.Background(), &req)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if resp.Summary == "" {
		t.Error("expected non-empty summary from FakeSummarizer")
	}
}
