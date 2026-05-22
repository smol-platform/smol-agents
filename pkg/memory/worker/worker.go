// Package worker implements the retrieval worker (data plane).
//
// Worker is a concrete api.RetrievalService that:
//  1. Validates every RequestIdentity (reject empty Tenant/Namespace/CallerSPIFFEID)
//     as a defense-in-depth re-check (R-MEM-WORK-1).
//  2. Selects a memory.Backend per StoreRef/config.
//  3. Embeds query/document text via the bound Embedder.
//  4. Chunks documents via the configured ChunkSpec.
//  5. Dispatches to the Backend, returning typed memory.* errors.
//
// The Worker owns no MCP, gateway, or auth-derivation logic — those belong to
// the gateway. It re-validates identity as defense-in-depth only.
package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/stigen/smol-agents/pkg/memory"
	"github.com/stigen/smol-agents/pkg/memory/api"
)

// Config holds the runtime configuration for a Worker.
type Config struct {
	// Chunk controls how documents are split before embedding.
	Chunk ChunkSpec

	// AllowedTenants, when non-empty, lists the tenants this worker is
	// authorised to serve. An empty slice means all tenants are accepted
	// (single-tenant deployment).
	AllowedTenants []string
}

// BackendSelector returns the Backend for a given storeRef. If storeRef is
// empty, the default backend should be returned. Returns memory.NotFound if
// no backend is configured for the ref.
type BackendSelector func(storeRef string) (memory.Backend, error)

// StaticSelector returns a BackendSelector that always returns the given backend,
// ignoring storeRef. Use this for single-backend deployments.
func StaticSelector(b memory.Backend) BackendSelector {
	return func(_ string) (memory.Backend, error) { return b, nil }
}

// MapSelector returns a BackendSelector backed by a map. The empty-string key
// is the default (returned when storeRef is "").
func MapSelector(m map[string]memory.Backend) BackendSelector {
	return func(storeRef string) (memory.Backend, error) {
		if b, ok := m[storeRef]; ok {
			return b, nil
		}
		if storeRef == "" {
			return nil, memory.BackendUnavailable("no default backend configured")
		}
		return nil, memory.NotFound("no backend for storeRef " + storeRef)
	}
}

// Worker is a concrete api.RetrievalService.
type Worker struct {
	cfg        Config
	selector   BackendSelector
	embedder   Embedder
	summarizer Summarizer // optional; nil = backend.Summarize only
	logger     *slog.Logger
}

// New constructs a Worker. embedder may be nil when no real embedding is
// needed (text-only retrieval via the fallback scorer in VectorBackend).
// summarizer may be nil; when non-nil it is used by Summarize to produce
// LLM-generated summaries over retrieved top-K documents.
func New(cfg Config, selector BackendSelector, embedder Embedder, logger *slog.Logger) (*Worker, error) {
	if selector == nil {
		return nil, fmt.Errorf("worker: BackendSelector is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{cfg: cfg, selector: selector, embedder: embedder, logger: logger}, nil
}

// WithSummarizer attaches a Summarizer to the Worker, enabling the
// summarize_memory MCP tool to return LLM-generated summaries. When
// summarizer is nil the worker falls back to backend.Summarize (which
// returns ErrNotSupported for all current adapters).
func (w *Worker) WithSummarizer(s Summarizer) { w.summarizer = s }

// compile-time assertion.
var _ api.RetrievalService = (*Worker)(nil)

// ── identity validation ─────────────────────────────────────────────────────

// validateIdentity is the defense-in-depth re-check mandated by R-MEM-WORK-1.
// It rejects any request where the gateway-injected fields are empty (which
// would indicate a misconfigured or malicious caller bypassing the gateway).
func (w *Worker) validateIdentity(id api.RequestIdentity) error {
	if id.Tenant == "" {
		return memory.PermissionDenied("worker: tenant is required")
	}
	if id.Namespace == "" {
		return memory.PermissionDenied("worker: namespace is required")
	}
	if id.CallerSPIFFEID == "" {
		return memory.PermissionDenied("worker: CallerSPIFFEID is required")
	}
	if len(w.cfg.AllowedTenants) > 0 && !contains(w.cfg.AllowedTenants, id.Tenant) {
		return memory.PermissionDenied("worker: tenant not served by this worker: " + id.Tenant)
	}
	return nil
}

// filterFrom builds a memory.Filter from an identity, merging any additional
// caller-supplied filter fields. Tenant and Namespace are always sourced from
// the identity (never from the caller-supplied filter).
func filterFrom(id api.RequestIdentity, extra memory.Filter) memory.Filter {
	f := extra
	// Always overwrite from the authoritative identity fields.
	f.Tenant = id.Tenant
	f.Namespace = id.Namespace
	return f
}

// ── Retrieve ────────────────────────────────────────────────────────────────

func (w *Worker) Retrieve(ctx context.Context, req *api.RetrieveRequest) (*api.RetrieveResponse, error) {
	if err := w.validateIdentity(req.Identity); err != nil {
		return nil, err
	}

	filter := filterFrom(req.Identity, req.Filters)

	backend, err := w.selector(req.StoreRef)
	if err != nil {
		return nil, err
	}

	topK := int(req.TopK)
	if topK <= 0 {
		topK = 10
	}

	// Embed the query if an embedder is available; fall back to the backend's
	// text-scoring path (VectorBackend handles both).
	var result memory.RetrieveResult
	if w.embedder != nil && req.Query != "" {
		qvec, embedErr := w.embedder.Embed(ctx, req.Query)
		if embedErr != nil {
			w.logger.Warn("embed query failed, falling back to text retrieval",
				"err", embedErr,
				"tenant", req.Identity.Tenant,
				"namespace", req.Identity.Namespace)
			// Fall through to Backend.Retrieve (text-based).
			result, err = backend.Retrieve(ctx, req.Query, topK, filter)
		} else {
			// Use the vector backend's embedding-aware path when available.
			if vb, ok := backend.(*memory.VectorBackend); ok {
				result, err = vb.RetrieveWithEmbedding(ctx, qvec, topK, filter)
			} else {
				result, err = backend.Retrieve(ctx, req.Query, topK, filter)
			}
		}
	} else {
		result, err = backend.Retrieve(ctx, req.Query, topK, filter)
	}
	if err != nil {
		return nil, wrapBackend(err)
	}

	return &api.RetrieveResponse{Result: result}, nil
}

// ── Write ───────────────────────────────────────────────────────────────────

func (w *Worker) Write(ctx context.Context, req *api.WriteRequest) (*api.WriteResponse, error) {
	if err := w.validateIdentity(req.Identity); err != nil {
		return nil, err
	}

	// Overwrite doc fields from the authoritative identity (defense-in-depth).
	doc := req.Document
	doc.Tenant = req.Identity.Tenant
	doc.Namespace = req.Identity.Namespace

	backend, err := w.selector(req.Identity.RetrieverRef)
	if err != nil {
		return nil, err
	}

	// Embed the full document content.
	if w.embedder != nil && len(doc.Content) > 0 {
		vec, embedErr := w.embedder.Embed(ctx, string(doc.Content))
		if embedErr != nil {
			w.logger.Warn("embed document failed, storing without embedding",
				"err", embedErr,
				"docID", doc.ID,
				"tenant", doc.Tenant)
		} else {
			doc.Embedding = vec
		}
	}

	// Store the document (Write returns the assigned ID).
	result, err := backend.Write(ctx, doc)
	if err != nil {
		return nil, wrapBackend(err)
	}
	doc.ID = result.ID

	// Chunk and index the document.
	if vb, ok := backend.(*memory.VectorBackend); ok {
		chunks := Chunk(doc, w.cfg.Chunk)
		for _, c := range chunks {
			// Embed each chunk individually when it differs from the full doc.
			chunkEmbedding := doc.Embedding
			if len(chunks) > 1 && w.embedder != nil && c.Text != string(doc.Content) {
				if vec, embedErr := w.embedder.Embed(ctx, c.Text); embedErr == nil {
					chunkEmbedding = vec
				}
			}
			c.Embedding = chunkEmbedding
			c.DocumentID = doc.ID
			if storeErr := vb.WriteChunk(ctx, doc, c); storeErr != nil {
				w.logger.Warn("write chunk failed", "err", storeErr, "index", c.Index)
			}
		}
	}

	return &api.WriteResponse{Result: result}, nil
}

// ── Get ─────────────────────────────────────────────────────────────────────

func (w *Worker) Get(ctx context.Context, req *api.GetRequest) (*api.GetResponse, error) {
	if err := w.validateIdentity(req.Identity); err != nil {
		return nil, err
	}

	backend, err := w.selector(req.Identity.RetrieverRef)
	if err != nil {
		return nil, err
	}

	filter := filterFrom(req.Identity, memory.Filter{})
	doc, err := backend.Get(ctx, req.ID, filter)
	if err != nil {
		return nil, wrapBackend(err)
	}
	return &api.GetResponse{Document: doc}, nil
}

// ── Delete ──────────────────────────────────────────────────────────────────

func (w *Worker) Delete(ctx context.Context, req *api.DeleteRequest) (*api.DeleteResponse, error) {
	if err := w.validateIdentity(req.Identity); err != nil {
		return nil, err
	}

	backend, err := w.selector(req.Identity.RetrieverRef)
	if err != nil {
		return nil, err
	}

	filter := filterFrom(req.Identity, memory.Filter{})
	if err := backend.Delete(ctx, req.ID, filter); err != nil {
		return nil, wrapBackend(err)
	}
	return &api.DeleteResponse{}, nil
}

// ── ListNamespaces ──────────────────────────────────────────────────────────

func (w *Worker) ListNamespaces(ctx context.Context, req *api.ListNamespacesRequest) (*api.ListNamespacesResponse, error) {
	if err := w.validateIdentity(req.Identity); err != nil {
		return nil, err
	}

	backend, err := w.selector(req.Identity.RetrieverRef)
	if err != nil {
		return nil, err
	}

	filter := filterFrom(req.Identity, memory.Filter{})
	namespaces, err := backend.ListNamespaces(ctx, filter)
	if err != nil {
		return nil, wrapBackend(err)
	}
	return &api.ListNamespacesResponse{Namespaces: namespaces}, nil
}

// ── Summarize ───────────────────────────────────────────────────────────────

// Summarize retrieves the top-K documents matching the query and, when a
// Summarizer is wired, passes them to the LLM to produce a summary.
// When no Summarizer is configured it delegates to backend.Summarize (which
// returns ErrNotSupported for all current adapters).
func (w *Worker) Summarize(ctx context.Context, req *api.SummarizeRequest) (*api.SummarizeResponse, error) {
	if err := w.validateIdentity(req.Identity); err != nil {
		return nil, err
	}

	backend, err := w.selector(req.Identity.RetrieverRef)
	if err != nil {
		return nil, err
	}

	filter := filterFrom(req.Identity, memory.Filter{})

	// When no Summarizer is attached, fall through to the backend's own
	// Summarize method (typically ErrNotSupported).
	if w.summarizer == nil {
		summary, backendErr := backend.Summarize(ctx, req.Query, filter)
		if backendErr != nil {
			return nil, wrapBackend(backendErr)
		}
		return &api.SummarizeResponse{Summary: summary}, nil
	}

	// Retrieve top-K documents and build the document set for the LLM.
	topK := 10
	result, retrieveErr := backend.Retrieve(ctx, req.Query, topK, filter)
	if retrieveErr != nil {
		return nil, wrapBackend(retrieveErr)
	}

	texts := make([]string, 0, len(result.Chunks))
	for _, sc := range result.Chunks {
		if sc.Chunk.Text != "" {
			texts = append(texts, sc.Chunk.Text)
		}
	}

	summary, summErr := w.summarizer.Summarize(ctx, req.Query, texts)
	if summErr != nil {
		return nil, memory.BackendUnavailable("summarizer: " + summErr.Error())
	}
	return &api.SummarizeResponse{Summary: summary}, nil
}

// ── Filesystem stubs ────────────────────────────────────────────────────────
// BranchFS, SnapshotFS, ListBranches pass through to the backend. A vector
// backend will return ErrNotSupported; agentfs (P2) will implement them.

func (w *Worker) BranchFS(ctx context.Context, req *api.BranchFSRequest) (*api.BranchFSResponse, error) {
	if err := w.validateIdentity(req.Identity); err != nil {
		return nil, err
	}
	backend, err := w.selector(req.Identity.RetrieverRef)
	if err != nil {
		return nil, err
	}
	filter := filterFrom(req.Identity, memory.Filter{})
	info, err := backend.Branch(ctx, req.BaseBranch, req.NewBranch, filter)
	if err != nil {
		return nil, wrapBackend(err)
	}
	return &api.BranchFSResponse{Branch: info}, nil
}

func (w *Worker) SnapshotFS(ctx context.Context, req *api.SnapshotFSRequest) (*api.SnapshotFSResponse, error) {
	if err := w.validateIdentity(req.Identity); err != nil {
		return nil, err
	}
	backend, err := w.selector(req.Identity.RetrieverRef)
	if err != nil {
		return nil, err
	}
	filter := filterFrom(req.Identity, memory.Filter{})
	info, err := backend.Snapshot(ctx, req.Branch, filter)
	if err != nil {
		return nil, wrapBackend(err)
	}
	return &api.SnapshotFSResponse{Snapshot: info}, nil
}

func (w *Worker) ListBranches(ctx context.Context, req *api.ListBranchesRequest) (*api.ListBranchesResponse, error) {
	if err := w.validateIdentity(req.Identity); err != nil {
		return nil, err
	}
	backend, err := w.selector(req.Identity.RetrieverRef)
	if err != nil {
		return nil, err
	}
	filter := filterFrom(req.Identity, memory.Filter{})
	branches, err := backend.ListBranches(ctx, filter)
	if err != nil {
		return nil, wrapBackend(err)
	}
	return &api.ListBranchesResponse{Branches: branches}, nil
}

func (w *Worker) MergeFS(ctx context.Context, req *api.MergeFSRequest) (*api.MergeFSResponse, error) {
	if err := w.validateIdentity(req.Identity); err != nil {
		return nil, err
	}
	backend, err := w.selector(req.Identity.RetrieverRef)
	if err != nil {
		return nil, err
	}
	filter := filterFrom(req.Identity, memory.Filter{})
	opts := memory.MergeOptions{
		OnConflict: memory.ConflictPolicy(req.OnConflict),
		DryRun:     req.DryRun,
	}
	result, err := backend.Merge(ctx, req.SrcBranch, req.DstBranch, opts, filter)
	if err != nil {
		return nil, wrapBackend(err)
	}
	return &api.MergeFSResponse{
		Branch:    result.Branch,
		Conflicts: result.Conflicts,
		Committed: result.Committed,
		Merged:    result.Merged,
		Added:     result.Added,
		Deleted:   result.Deleted,
	}, nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

// wrapBackend ensures backend errors that are not already typed memory.Error
// or memory.ErrNotSupported are wrapped as BackendUnavailable.
func wrapBackend(err error) error {
	if err == nil {
		return nil
	}
	switch memory.KindOf(err) {
	case memory.KindInternal:
		return memory.BackendUnavailable(err.Error())
	default:
		return err
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
