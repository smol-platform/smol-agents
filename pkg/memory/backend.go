package memory

import "context"

// Backend is the pluggable adapter interface between the retrieval worker and
// a concrete storage engine (pgvector, qdrant, agentfs, …).
//
// One adapter per MemoryStore kind is implemented; adding a new kind (graph,
// KV, event log) requires only a new Backend implementation — the gateway
// and the internal retrieval API are unaffected.
//
// All methods receive a context that may carry OTel trace spans and a deadline.
// Implementations MUST respect context cancellation.
//
// Tenant and namespace isolation MUST be enforced inside each method as a
// defence-in-depth second check. The worker re-validates these before calling
// Backend; the backend SHOULD also reject cross-tenant / cross-namespace reads.
//
// Implements R-MEM-WORK-1, R-MEM-WORK-2, R-MEM-SEC-1.
type Backend interface {
	// Retrieve performs a semantic (or keyword) search and returns the top-K
	// most relevant chunks. The filter carries the gateway-injected tenant and
	// namespace; results outside these boundaries MUST NOT be returned.
	//
	// topK is already clamped by the gateway; the backend MAY reduce it further
	// but MUST NOT increase it.
	Retrieve(ctx context.Context, query string, topK int, filter Filter) (RetrieveResult, error)

	// Write stores a document and returns its assigned ID + version. If the
	// document already exists (same ID) the backend SHOULD perform an upsert.
	// The document's Tenant and Namespace MUST be used as partition keys.
	Write(ctx context.Context, doc Document) (WriteResult, error)

	// Get fetches a single document by ID. The backend MUST verify that the
	// document's tenant and namespace match the request context (reject cross-tenant
	// access even for a direct ID lookup).
	Get(ctx context.Context, id string, filter Filter) (Document, error)

	// Delete removes the document with the given ID. Same tenant/namespace
	// ownership check as Get applies.
	Delete(ctx context.Context, id string, filter Filter) error

	// ListNamespaces returns the namespaces visible to the caller within their
	// tenant. The filter.Tenant field MUST constrain the result.
	ListNamespaces(ctx context.Context, filter Filter) ([]string, error)

	// Summarize produces a free-text summary of the documents matching the
	// query within the given filter scope. This is a P2 operation; backends
	// MAY return ErrNotSupported when LLM summarization is unavailable.
	Summarize(ctx context.Context, query string, filter Filter) (string, error)

	// ── Filesystem-only operations ─────────────────────────────────────────
	// These methods are only meaningful for kind=filesystem (agentfs) backends.
	// Non-filesystem adapters MUST return ErrNotSupported.

	// Branch forks baseBranch into a new branch named newBranch using
	// copy-on-write semantics. The fork is ephemeral until committed.
	Branch(ctx context.Context, baseBranch, newBranch string, filter Filter) (BranchInfo, error)

	// Snapshot captures a point-in-time snapshot of the named branch.
	// For agentfs this maps to a full+WAL S3 version via pkg/agentfs.Manager.
	Snapshot(ctx context.Context, branch string, filter Filter) (SnapshotInfo, error)

	// ListBranches returns the branches visible to the caller in the given
	// tenant/namespace scope.
	ListBranches(ctx context.Context, filter Filter) ([]BranchInfo, error)

	// Merge performs a fast-forward publish of srcBranch into dstBranch within
	// the tenant/namespace scope supplied by filter. All files from srcBranch
	// are applied onto dstBranch using copy-on-write semantics: files present
	// in srcBranch replace (or add to) the corresponding paths in dstBranch;
	// files only in dstBranch and absent from srcBranch are preserved.
	//
	// Filesystem-only — non-FS adapters MUST return
	//   &ErrNotSupported{Op:"Merge", Backend:"<name>"}.
	// Tenant and namespace isolation MUST be enforced; cross-tenant merge
	// attempts MUST return PermissionDenied.
	//
	// On success the updated dstBranch BranchInfo is returned.
	Merge(ctx context.Context, srcBranch, dstBranch string, filter Filter) (BranchInfo, error)
}

// ErrNotSupported is returned by Backend methods that the adapter does not
// implement (e.g. Branch on a vector backend).
type ErrNotSupported struct {
	Op      string
	Backend string
}

func (e *ErrNotSupported) Error() string {
	return "memory backend " + e.Backend + " does not support " + e.Op
}
