// Package worker — LRU embedding cache.
//
// CachedEmbedder wraps any Embedder and caches its results in a fixed-size
// LRU. Cache hits are O(1) and skip the embedding endpoint entirely, reducing
// latency and API cost for repeated queries.
//
// The cache is keyed by the exact text string. Cache entries are immutable
// (embeddings are deterministic for a given text+model pair). The cache is
// safe for concurrent use.
//
// Cache size is bounded by maxEntries; eviction is LRU (least-recently-used).
// A maxEntries of 0 disables caching (every call passes through).
//
// Implements worker.Embedder.
package worker

import (
	"container/list"
	"context"
	"sync"
)

// CachedEmbedder wraps an Embedder with a fixed-size LRU cache.
type CachedEmbedder struct {
	inner      Embedder
	maxEntries int

	mu    sync.Mutex
	cache map[string]*list.Element
	lru   *list.List
}

type cacheEntry struct {
	text      string
	embedding []float32
}

// NewCachedEmbedder constructs a CachedEmbedder with the given capacity.
// When maxEntries <= 0 the cache is disabled and every call is forwarded
// directly to inner.
func NewCachedEmbedder(inner Embedder, maxEntries int) *CachedEmbedder {
	return &CachedEmbedder{
		inner:      inner,
		maxEntries: maxEntries,
		cache:      make(map[string]*list.Element),
		lru:        list.New(),
	}
}

// Dims delegates to the inner Embedder.
func (c *CachedEmbedder) Dims() int { return c.inner.Dims() }

// Embed returns a cached embedding when available, or calls the inner Embedder
// and caches the result.
func (c *CachedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if c.maxEntries <= 0 {
		return c.inner.Embed(ctx, text)
	}

	// Fast path: cache hit.
	c.mu.Lock()
	if elem, ok := c.cache[text]; ok {
		c.lru.MoveToFront(elem)
		vec := cloneVec(elem.Value.(*cacheEntry).embedding)
		c.mu.Unlock()
		return vec, nil
	}
	c.mu.Unlock()

	// Cache miss: call inner.
	vec, err := c.inner.Embed(ctx, text)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring lock (another goroutine may have inserted).
	if elem, ok := c.cache[text]; ok {
		c.lru.MoveToFront(elem)
		return cloneVec(elem.Value.(*cacheEntry).embedding), nil
	}

	// Insert.
	entry := &cacheEntry{text: text, embedding: cloneVec(vec)}
	elem := c.lru.PushFront(entry)
	c.cache[text] = elem

	// Evict LRU entry if over capacity.
	for c.lru.Len() > c.maxEntries {
		back := c.lru.Back()
		if back == nil {
			break
		}
		c.lru.Remove(back)
		delete(c.cache, back.Value.(*cacheEntry).text)
	}

	return vec, nil
}

// Len returns the current number of cached entries. Safe for concurrent use.
func (c *CachedEmbedder) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// cloneVec returns a copy of v so cache entries are not aliased with caller
// slices.
func cloneVec(v []float32) []float32 {
	if v == nil {
		return nil
	}
	out := make([]float32, len(v))
	copy(out, v)
	return out
}

// compile-time assertion: CachedEmbedder satisfies the Embedder interface.
var _ Embedder = (*CachedEmbedder)(nil)
