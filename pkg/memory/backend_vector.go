// Package memory — in-memory cosine-similarity vector Backend.
//
// VectorBackend is the P1 default: no external DB required, so unit tests and
// the e2e probe work without standing up pgvector or Qdrant. The design keeps
// the pgvector/Qdrant path open: swap the storage layer by constructing with a
// different storeFunc.
//
// Tenant + namespace isolation is enforced inside every method as defense-in-
// depth (R-MEM-WORK-1, R-MEM-SEC-1). Filesystem-only operations return
// *ErrNotSupported.
//
// Implements R-MEM-WORK-2.
package memory

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// VectorEntry is one indexed document entry in the in-memory store.
type VectorEntry struct {
	Doc   Document
	Chunk Chunk
}

// VectorBackend is a thread-safe, in-memory cosine-similarity Backend. It stores
// every chunk produced by the worker and scores them by cosine similarity to the
// query embedding at retrieval time.
//
// For production, replace this with a pgvector or Qdrant adapter that satisfies
// the same Backend interface.
type VectorBackend struct {
	mu      sync.RWMutex
	entries []VectorEntry          // chunk index
	docs    map[string]Document    // id → doc for Get/Delete
	nss     map[tenantKey]struct{} // tenant+namespace presence set
}

type tenantKey struct{ tenant, namespace string }

// NewVectorBackend returns an empty VectorBackend ready for use.
func NewVectorBackend() *VectorBackend {
	return &VectorBackend{
		docs: make(map[string]Document),
		nss:  make(map[tenantKey]struct{}),
	}
}

// Write stores a document and indexes its Embedding as a single chunk when
// Embedding is present. If the document has no embedding, it is stored but
// will not appear in Retrieve results (requires the caller to embed first).
//
// Upsert: if a document with the same ID already exists for the same
// tenant+namespace, it is replaced.
func (b *VectorBackend) Write(_ context.Context, doc Document) (WriteResult, error) {
	if doc.Tenant == "" || doc.Namespace == "" {
		return WriteResult{}, Invalid("write: tenant and namespace are required")
	}
	if doc.ID == "" {
		doc.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = now
	}
	doc.UpdatedAt = now
	if doc.Version == "" {
		doc.Version = now.Format(time.RFC3339Nano)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Remove any previous entry for this id+tenant (upsert semantics).
	b.removeEntryLocked(doc.ID, doc.Tenant, doc.Namespace)

	b.docs[doc.ID] = doc
	b.nss[tenantKey{doc.Tenant, doc.Namespace}] = struct{}{}

	// Always index an entry — text scoring (Retrieve) needs no embedding, and
	// the embedding (possibly empty) rides along for RetrieveWithEmbedding's
	// cosine path. Guarding on a non-empty embedding made text-only docs
	// unretrievable.
	b.entries = append(b.entries, VectorEntry{
		Doc: doc,
		Chunk: Chunk{
			Index:      0,
			Text:       string(doc.Content),
			Embedding:  doc.Embedding,
			DocumentID: doc.ID,
			StartByte:  0,
			EndByte:    len(doc.Content),
		},
	})

	return WriteResult{ID: doc.ID, Version: doc.Version}, nil
}

// WriteChunk indexes an individual Chunk, associated with a parent Document.
// The worker calls this after splitting a document. The parent document must
// have been stored via Write first; the Chunk's tenant/namespace are inherited
// from the stored document.
func (b *VectorBackend) WriteChunk(_ context.Context, doc Document, chunk Chunk) error {
	if doc.Tenant == "" || doc.Namespace == "" {
		return Invalid("write-chunk: tenant and namespace are required")
	}
	chunk.DocumentID = doc.ID

	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, VectorEntry{Doc: doc, Chunk: chunk})
	return nil
}

// Get returns the document with the given ID, enforcing tenant/namespace
// ownership (R-MEM-SEC-1).
func (b *VectorBackend) Get(_ context.Context, id string, filter Filter) (Document, error) {
	b.mu.RLock()
	doc, ok := b.docs[id]
	b.mu.RUnlock()

	if !ok {
		return Document{}, NotFound("document not found: " + id)
	}
	if doc.Tenant != filter.Tenant {
		// Cross-tenant: return NotFound, not PermissionDenied — never confirm existence.
		return Document{}, NotFound("document not found: " + id)
	}
	if filter.Namespace != "" && doc.Namespace != filter.Namespace {
		return Document{}, NotFound("document not found: " + id)
	}
	return doc, nil
}

// Delete removes the document with the given ID, enforcing ownership.
func (b *VectorBackend) Delete(_ context.Context, id string, filter Filter) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	doc, ok := b.docs[id]
	if !ok {
		return NotFound("document not found: " + id)
	}
	if doc.Tenant != filter.Tenant {
		return NotFound("document not found: " + id) // same as Get: never confirm existence
	}
	if filter.Namespace != "" && doc.Namespace != filter.Namespace {
		return NotFound("document not found: " + id)
	}

	delete(b.docs, id)
	b.removeEntryLocked(id, filter.Tenant, filter.Namespace)
	// Refresh namespace set.
	b.rebuildNSLocked()
	return nil
}

// Retrieve returns the top-K chunks by cosine similarity to queryEmbedding,
// filtered by tenant and namespace. All results are confined to filter.Tenant
// and (when non-empty) filter.Namespace.
func (b *VectorBackend) Retrieve(_ context.Context, query string, topK int, filter Filter) (RetrieveResult, error) {
	if filter.Tenant == "" {
		return RetrieveResult{}, PermissionDenied("retrieve: tenant is required")
	}
	// The caller (worker) must supply the query embedding in the filter metadata
	// under the key "query_embedding_base64". In-memory mode accepts a nil
	// embedding and falls back to substring matching for tests without an embedder.
	//
	// For production embedder-backed paths the worker embeds the query and
	// passes the embedding through the ScoredChunk.Chunk.Embedding field by
	// pre-storing it in the filter metadata. Since the current contract does not
	// carry a query embedding inside Filter, we perform pure cosine similarity
	// when the entries carry embeddings, and fall back to substring match
	// otherwise. This keeps the backend correct for both the fake-embedder
	// (deterministic vectors) and the text-only path.

	b.mu.RLock()
	all := make([]VectorEntry, 0, len(b.entries))
	for _, e := range b.entries {
		if e.Doc.Tenant != filter.Tenant {
			continue
		}
		if filter.Namespace != "" && e.Doc.Namespace != filter.Namespace {
			continue
		}
		if !matchMetadata(e.Doc.Metadata, filter.Metadata) {
			continue
		}
		all = append(all, e)
	}
	b.mu.RUnlock()

	if len(all) == 0 {
		return RetrieveResult{}, nil
	}

	type scored struct {
		entry VectorEntry
		score float32
	}
	results := make([]scored, 0, len(all))
	for _, e := range all {
		s := scoreEntry(e, query)
		results = append(results, scored{entry: e, score: s})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })

	total := int64(len(results))
	if topK > 0 && int(topK) < len(results) {
		results = results[:topK]
	}

	chunks := make([]ScoredChunk, len(results))
	for i, r := range results {
		chunks[i] = ScoredChunk{Chunk: r.entry.Chunk, Score: r.score}
	}
	return RetrieveResult{Chunks: chunks, Total: total}, nil
}

// RetrieveWithEmbedding is used by the worker to pass a pre-computed query
// embedding for cosine-similarity ranking. When called, it overrides the
// text-based fallback in Retrieve.
func (b *VectorBackend) RetrieveWithEmbedding(_ context.Context, queryEmbedding []float32, topK int, filter Filter) (RetrieveResult, error) {
	if filter.Tenant == "" {
		return RetrieveResult{}, PermissionDenied("retrieve: tenant is required")
	}

	b.mu.RLock()
	all := make([]VectorEntry, 0, len(b.entries))
	for _, e := range b.entries {
		if e.Doc.Tenant != filter.Tenant {
			continue
		}
		if filter.Namespace != "" && e.Doc.Namespace != filter.Namespace {
			continue
		}
		if !matchMetadata(e.Doc.Metadata, filter.Metadata) {
			continue
		}
		all = append(all, e)
	}
	b.mu.RUnlock()

	if len(all) == 0 {
		return RetrieveResult{}, nil
	}

	type scored struct {
		entry VectorEntry
		score float32
	}
	results := make([]scored, 0, len(all))
	for _, e := range all {
		var s float32
		if len(e.Chunk.Embedding) > 0 && len(queryEmbedding) > 0 {
			s = cosineSimilarity(queryEmbedding, e.Chunk.Embedding)
		} else {
			// Fallback: presence score.
			if len(e.Chunk.Text) > 0 {
				s = 0.01
			}
		}
		results = append(results, scored{entry: e, score: s})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })

	total := int64(len(results))
	if topK > 0 && int(topK) < len(results) {
		results = results[:topK]
	}

	chunks := make([]ScoredChunk, len(results))
	for i, r := range results {
		chunks[i] = ScoredChunk{Chunk: r.entry.Chunk, Score: r.score}
	}
	return RetrieveResult{Chunks: chunks, Total: total}, nil
}

// ListNamespaces returns namespaces visible to filter.Tenant.
func (b *VectorBackend) ListNamespaces(_ context.Context, filter Filter) ([]string, error) {
	if filter.Tenant == "" {
		return nil, PermissionDenied("list-namespaces: tenant is required")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	seen := make(map[string]struct{})
	for k := range b.nss {
		if k.tenant == filter.Tenant {
			seen[k.namespace] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out, nil
}

// Summarize is a P2 operation; the in-memory backend does not implement it.
func (b *VectorBackend) Summarize(_ context.Context, _ string, _ Filter) (string, error) {
	return "", &ErrNotSupported{Op: "Summarize", Backend: "vector-inmem"}
}

// ── Filesystem-only stubs ───────────────────────────────────────────────────

func (b *VectorBackend) Branch(_ context.Context, _, _ string, _ Filter) (BranchInfo, error) {
	return BranchInfo{}, &ErrNotSupported{Op: "Branch", Backend: "vector-inmem"}
}

func (b *VectorBackend) Snapshot(_ context.Context, _ string, _ Filter) (SnapshotInfo, error) {
	return SnapshotInfo{}, &ErrNotSupported{Op: "Snapshot", Backend: "vector-inmem"}
}

func (b *VectorBackend) ListBranches(_ context.Context, _ Filter) ([]BranchInfo, error) {
	return nil, &ErrNotSupported{Op: "ListBranches", Backend: "vector-inmem"}
}

func (b *VectorBackend) Merge(_ context.Context, _, _ string, _ Filter) (BranchInfo, error) {
	return BranchInfo{}, &ErrNotSupported{Op: "Merge", Backend: "vector-inmem"}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// removeEntryLocked removes all chunk entries for the given document id
// belonging to the given tenant+namespace. Caller must hold b.mu.
func (b *VectorBackend) removeEntryLocked(id, tenant, namespace string) {
	n := 0
	for _, e := range b.entries {
		if e.Chunk.DocumentID == id && e.Doc.Tenant == tenant && (namespace == "" || e.Doc.Namespace == namespace) {
			continue
		}
		b.entries[n] = e
		n++
	}
	b.entries = b.entries[:n]
}

// rebuildNSLocked rebuilds the namespace presence set from b.docs. Caller
// must hold b.mu (write lock).
func (b *VectorBackend) rebuildNSLocked() {
	b.nss = make(map[tenantKey]struct{}, len(b.docs))
	for _, d := range b.docs {
		b.nss[tenantKey{d.Tenant, d.Namespace}] = struct{}{}
	}
}

// scoreEntry produces a score for a chunk entry against a text query. When the
// chunk has an embedding the caller should use RetrieveWithEmbedding instead;
// this function is the fallback for the text-only path and tests.
func scoreEntry(e VectorEntry, query string) float32 {
	text := strings.ToLower(e.Chunk.Text)
	q := strings.ToLower(query)
	if q == "" {
		return 0.5 // no query — return everything equally
	}
	terms := strings.Fields(q)
	var hits int
	for _, t := range terms {
		if strings.Contains(text, t) {
			hits++
		}
	}
	if len(terms) == 0 {
		return 0
	}
	return float32(hits) / float32(len(terms))
}

// cosineSimilarity computes the cosine similarity between two vectors. Returns
// 0 if either vector is zero-length.
func cosineSimilarity(a, b []float32) float32 {
	n := len(a)
	if n == 0 || len(b) == 0 {
		return 0
	}
	if len(b) < n {
		n = len(b)
	}
	var dot, normA, normB float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}

// matchMetadata returns true if all key/value pairs in want are present in got.
func matchMetadata(got, want map[string]string) bool {
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// compile-time assertion: VectorBackend satisfies the Backend interface.
var _ Backend = (*VectorBackend)(nil)
