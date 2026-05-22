// Package memory defines the interfaces, types, and internal API for the
// smol-agents memory subsystem.
//
// Three-plane split:
//   - Control plane  — operator reconciles MemoryStore/MemoryRetriever CRDs.
//   - Data plane     — retrieval workers implement the Backend interface.
//   - Interface      — memory-mcp gateway translates MCP calls to the internal API.
//
// This package is the shared contract between the planes; it has no dependency
// on Kubernetes, MCP, or any backend driver.
package memory

import "time"

// Document is the atomic unit of storage across all backend kinds.
// For vector stores a Document holds the text and its embedding vector.
// For filesystem stores it represents a file at a path inside a branch.
type Document struct {
	// ID is the backend-assigned or caller-supplied stable identifier.
	ID string

	// Namespace partitions documents within a single tenant's store.
	Namespace string

	// Tenant is the owning tenant; set by the gateway from the caller's
	// attested SPIFFE identity and re-checked by the worker.
	Tenant string

	// Content is the raw text or bytes for this document.
	// For filesystem documents this is the file body.
	Content []byte

	// Path is non-empty for filesystem documents; it is the file path
	// relative to the branch root. Empty for non-filesystem kinds.
	Path string

	// Metadata is driver-opaque key/value pairs stored alongside the content.
	Metadata map[string]string

	// Embedding is the pre-computed embedding vector, if available.
	// Workers populate this after calling the ModelProvider.
	Embedding []float32

	// Version is an opaque revision marker (e.g. S3 version ID or WAL offset).
	Version string

	// CreatedAt and UpdatedAt record wall-clock timestamps.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Chunk is a sub-document produced by the chunking stage in the worker.
// Chunks are indexed individually; retrieval results reference their parent
// Document.
type Chunk struct {
	// Index is the zero-based position of this chunk in the source document.
	Index int

	// Text is the chunk content.
	Text string

	// Embedding is the embedding vector for this chunk.
	Embedding []float32

	// DocumentID is the parent document's ID.
	DocumentID string

	// StartByte / EndByte are byte offsets into the parent document content.
	StartByte int
	EndByte   int
}

// ScoredChunk pairs a Chunk with its relevance score from the backend.
type ScoredChunk struct {
	Chunk Chunk

	// Score is the relevance score; higher is more relevant.
	// Scale depends on the backend (cosine similarity, BM25, etc.).
	Score float32
}

// RetrieveResult is the result of a Retrieve call.
type RetrieveResult struct {
	// Chunks are the ranked results, in descending relevance order.
	Chunks []ScoredChunk

	// Total is the total number of matching chunks before topK truncation.
	// May be approximate for approximate-nearest-neighbour backends.
	Total int64
}

// WriteResult is the result of a Write call.
type WriteResult struct {
	// ID is the stable identifier assigned to the stored document.
	ID string

	// Version is the opaque revision marker after the write.
	Version string
}

// BranchInfo describes one branch in a filesystem store.
type BranchInfo struct {
	// Name is the branch identifier (e.g. "run-abc123").
	Name string

	// Base is the branch name this was forked from. Empty for root branches.
	Base string

	// CreatedAt is when the branch was created.
	CreatedAt time.Time

	// CommittedAt is non-zero when the branch has been published/committed.
	CommittedAt time.Time

	// DiscardedAt is non-zero when the branch was rolled back.
	DiscardedAt time.Time
}

// SnapshotInfo describes one point-in-time snapshot of a filesystem branch.
type SnapshotInfo struct {
	// ID is the backend-assigned snapshot identifier (e.g. S3 version ID).
	ID string

	// Branch is the branch this snapshot was taken from.
	Branch string

	// CreatedAt is when the snapshot was taken.
	CreatedAt time.Time

	// SizeBytes is the approximate size of the snapshot.
	SizeBytes int64
}

// ── Merge types ─────────────────────────────────────────────────────────────

// ConflictPolicy controls how the 3-way merge handles conflicting file edits.
// The default ("fail") is the safe choice: collect conflicts and commit nothing.
type ConflictPolicy string

const (
	// MergeFail collects conflicts and commits nothing (atomic rollback).
	// This is the default; the caller must explicitly choose a resolution strategy.
	MergeFail ConflictPolicy = "fail"

	// MergeOurs keeps the destination (ours) copy on any conflict.
	MergeOurs ConflictPolicy = "ours"

	// MergeTheirs overwrites with the source (theirs) copy on any conflict.
	MergeTheirs ConflictPolicy = "theirs"

	// MergeMarkers writes git-style conflict markers into conflicting files
	// and commits the result (Phase 2). Reported conflicts tell the agent
	// which paths need human/LLM resolution.
	MergeMarkers ConflictPolicy = "markers"

	// MergeUnion keeps both sides of a text conflict (append/deduplicate).
	// For binary files falls back to MergeOurs (Phase 2).
	MergeUnion ConflictPolicy = "union"
)

// MergeOptions controls 3-way merge behaviour.
type MergeOptions struct {
	// OnConflict determines what happens when a file-level conflict is detected.
	// Empty string is treated as MergeFail (safe default).
	OnConflict ConflictPolicy

	// DryRun computes the merge plan (conflicts + counts) but commits nothing.
	// Committed is always false when DryRun is true.
	DryRun bool
}

// ConflictInfo describes one conflicting file in a merge.
type ConflictInfo struct {
	// Path is the relative file path inside the branch.
	Path string

	// Kind classifies the conflict:
	//   "edit/edit"   — both ours and theirs modified the file relative to base.
	//   "edit/delete" — ours edited the file but theirs deleted it (or vice versa).
	//   "add/add"     — both sides added a file at the same path with different content.
	Kind string
}

// MergeResult is returned by Backend.Merge.
type MergeResult struct {
	// Branch is the updated destination BranchInfo on a committed merge.
	// Zero-value when Committed is false.
	Branch BranchInfo

	// Conflicts holds the conflicting files. Non-empty when OnConflict=fail
	// and there were conflicts, or in DryRun mode when conflicts exist.
	// Also non-empty for markers/union policies after commit (informational).
	Conflicts []ConflictInfo

	// Committed is true when the merge was fully applied to dstBranch.
	// False when DryRun=true or when OnConflict=fail and conflicts were found.
	Committed bool

	// Merged is the count of files updated in dstBranch from srcBranch
	// (excludes adds and deletes).
	Merged int

	// Added is the count of new files copied from srcBranch into dstBranch.
	Added int

	// Deleted is the count of files removed from dstBranch because srcBranch
	// deleted them relative to the fork base.
	Deleted int
}

// Filter carries optional predicate pushdown hints for a Retrieve call.
// Backends SHOULD apply all non-zero filters; they MUST NOT return results
// that violate Namespace or Tenant.
type Filter struct {
	// Namespace restricts results to documents in this namespace.
	// Injected and validated by the gateway; the caller's value is ignored.
	Namespace string

	// Tenant restricts results to documents owned by this tenant.
	// Injected by the gateway from the caller's SPIFFE identity.
	Tenant string

	// Metadata is an optional map of key/value predicates. Backends support
	// subset match (all key/value pairs must match).
	Metadata map[string]string
}
