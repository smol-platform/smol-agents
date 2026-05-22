// Package api defines the internal retrieval API between the memory-mcp gateway
// and the retrieval workers.
//
// The canonical implementation is gRPC over mTLS (steering requirement).
// Proto generation (buf + protoc-gen-go + protoc-gen-go-grpc) is deferred
// until the repo has a buf workspace; the Go interface here is the binding
// contract. When protos are added, the generated service interface MUST be a
// strict superset of RetrievalService below — no fields removed.
//
// Proto file location (when added): pkg/memory/api/retrieval.proto
// buf.yaml + buf.gen.yaml location (when added): pkg/memory/api/
//
// Every request type carries Tenant, Namespace, and CallerSPIFFEID so the
// worker can perform a defense-in-depth re-check (R-MEM-WORK-1, R-MEM-AUTH-1).
package api

import (
	"context"

	"github.com/stigen/smol-agents/pkg/memory"
)

// RetrievalService is the internal gRPC service contract between the
// memory-mcp gateway (client) and the retrieval workers (server).
//
// Design invariants:
//   - Every request carries Tenant, Namespace, and CallerSPIFFEID.
//   - Workers re-validate these fields and MUST reject mismatches
//     (defense-in-depth per R-MEM-WORK-1).
//   - Workers MUST NOT trust caller-supplied fields over what the gateway
//     injected; the gateway is authoritative for identity derivation.
//   - All errors are returned as typed responses or Go errors; the gateway
//     maps them to MCP errors and audit records.
type RetrievalService interface {
	// Retrieve performs semantic/keyword search. The gateway has already
	// clamped topK to the retriever's quota ceiling.
	Retrieve(ctx context.Context, req *RetrieveRequest) (*RetrieveResponse, error)

	// Write stores a document. When the retriever sets MutationsTraT=true,
	// the gateway has already validated the TraT before issuing this call.
	Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error)

	// Get fetches a single document by ID. The worker MUST verify that the
	// document's tenant matches req.Tenant (no cross-tenant id lookup).
	Get(ctx context.Context, req *GetRequest) (*GetResponse, error)

	// Delete removes a document by ID. Same ownership check as Get.
	Delete(ctx context.Context, req *DeleteRequest) (*DeleteResponse, error)

	// ListNamespaces returns the namespaces accessible to the caller within
	// their tenant.
	ListNamespaces(ctx context.Context, req *ListNamespacesRequest) (*ListNamespacesResponse, error)

	// Summarize produces a free-text summary over the matching document set.
	// Workers MAY return an error if LLM summarization is unavailable (P2).
	Summarize(ctx context.Context, req *SummarizeRequest) (*SummarizeResponse, error)

	// BranchFS forks a filesystem branch (kind=filesystem stores only).
	BranchFS(ctx context.Context, req *BranchFSRequest) (*BranchFSResponse, error)

	// SnapshotFS creates a point-in-time snapshot of a filesystem branch.
	SnapshotFS(ctx context.Context, req *SnapshotFSRequest) (*SnapshotFSResponse, error)

	// ListBranches returns the filesystem branches visible to the caller.
	ListBranches(ctx context.Context, req *ListBranchesRequest) (*ListBranchesResponse, error)

	// MergeFS fast-forward publishes srcBranch into dstBranch (kind=filesystem
	// stores only). Non-filesystem backends return ErrNotSupported.
	MergeFS(ctx context.Context, req *MergeFSRequest) (*MergeFSResponse, error)
}

// ── Identity header (embedded in every request) ────────────────────────────

// RequestIdentity carries the gateway-derived identity fields that every
// worker request must include. The worker validates these before dispatching
// to the Backend.
type RequestIdentity struct {
	// Tenant is the tenant derived from the caller's SPIFFE identity by the
	// gateway. Must match the retriever's configured tenant scope.
	Tenant string

	// Namespace is the memory namespace the operation targets.
	Namespace string

	// CallerSPIFFEID is the full SPIFFE URI of the authenticated caller
	// (e.g. spiffe://trust-domain/ns/agents/sa/coder). Used by the worker
	// for policy re-checks and audit logging.
	CallerSPIFFEID string

	// RetrieverRef is the namespace-qualified name of the MemoryRetriever CR
	// (e.g. "team-alpha/prod-knowledge"). Used for policy and quota lookup.
	RetrieverRef string
}

// ── Retrieve ───────────────────────────────────────────────────────────────

// RetrieveRequest is sent by the gateway for a retrieve_memory MCP call.
type RetrieveRequest struct {
	Identity RequestIdentity

	// Query is the natural-language search string.
	Query string

	// TopK is the (already quota-clamped) number of results to return.
	TopK int32

	// Filters is an optional predicate pushed down to the backend.
	// Tenant and Namespace MUST be sourced from Identity, not from caller input.
	Filters memory.Filter

	// StoreRef, when non-empty, restricts the search to a single MemoryStore
	// within the retriever's Stores list.
	StoreRef string
}

// RetrieveResponse carries ranked results.
type RetrieveResponse struct {
	Result memory.RetrieveResult
}

// ── Write ──────────────────────────────────────────────────────────────────

// WriteRequest is sent by the gateway for a write_memory MCP call.
type WriteRequest struct {
	Identity RequestIdentity

	// Document is the content to store. Tenant and Namespace MUST be
	// overwritten from Identity before the worker calls Backend.Write.
	Document memory.Document

	// TraT is the optional Transaction Token when MutationsTraT=true.
	// The gateway validates the TraT before issuing this request; the worker
	// MAY re-verify if it has access to the JWKS.
	TraT string
}

// WriteResponse carries the assigned document ID and version.
type WriteResponse struct {
	Result memory.WriteResult
}

// ── Get ────────────────────────────────────────────────────────────────────

// GetRequest is sent by the gateway for a get_memory MCP call or a
// memory://documents/{id} resource read.
type GetRequest struct {
	Identity RequestIdentity

	// ID is the stable document identifier.
	ID string
}

// GetResponse carries the fetched document. Not found is a typed error.
type GetResponse struct {
	Document memory.Document
}

// ── Delete ─────────────────────────────────────────────────────────────────

// DeleteRequest is sent by the gateway for a delete_memory MCP call.
type DeleteRequest struct {
	Identity RequestIdentity

	// ID is the document to remove.
	ID string

	// TraT is the optional Transaction Token when MutationsTraT=true.
	TraT string
}

// DeleteResponse is empty on success; errors are returned as Go errors.
type DeleteResponse struct{}

// ── ListNamespaces ─────────────────────────────────────────────────────────

// ListNamespacesRequest is sent by the gateway for a list_memory_namespaces
// MCP call or a memory://namespaces/{ns} resource read.
type ListNamespacesRequest struct {
	Identity RequestIdentity
}

// ListNamespacesResponse carries the accessible namespace names.
type ListNamespacesResponse struct {
	Namespaces []string
}

// ── Summarize ──────────────────────────────────────────────────────────────

// SummarizeRequest is sent by the gateway for a summarize_memory MCP call.
type SummarizeRequest struct {
	Identity RequestIdentity

	// Query scopes the summarization topic.
	Query string
}

// SummarizeResponse carries the LLM-generated summary.
type SummarizeResponse struct {
	Summary string
}

// ── BranchFS ───────────────────────────────────────────────────────────────

// BranchFSRequest is sent by the gateway for a branch_memory_fs MCP call.
type BranchFSRequest struct {
	Identity RequestIdentity

	// BaseBranch is the branch to fork from (e.g. "main").
	BaseBranch string

	// NewBranch is the name for the new ephemeral branch (e.g. "run-abc123").
	NewBranch string
}

// BranchFSResponse carries metadata about the newly created branch.
type BranchFSResponse struct {
	Branch memory.BranchInfo
}

// ── SnapshotFS ─────────────────────────────────────────────────────────────

// SnapshotFSRequest is sent by the gateway for a snapshot_memory_fs MCP call.
type SnapshotFSRequest struct {
	Identity RequestIdentity

	// Branch is the branch to snapshot.
	Branch string
}

// SnapshotFSResponse carries metadata about the created snapshot.
type SnapshotFSResponse struct {
	Snapshot memory.SnapshotInfo
}

// ── ListBranches ───────────────────────────────────────────────────────────

// ListBranchesRequest is sent by the gateway for a list_memory_branches MCP
// call or a memory://branches/{agentId} resource read.
type ListBranchesRequest struct {
	Identity RequestIdentity
}

// ListBranchesResponse carries the visible branches.
type ListBranchesResponse struct {
	Branches []memory.BranchInfo
}

// ── MergeFS ────────────────────────────────────────────────────────────────

// MergeFSRequest is sent by the gateway for a merge_memory_fs MCP call.
// It performs a fast-forward publish of SrcBranch into DstBranch.
type MergeFSRequest struct {
	Identity RequestIdentity

	// SrcBranch is the branch whose files are applied onto DstBranch.
	SrcBranch string

	// DstBranch is the branch that receives the merged files.
	DstBranch string
}

// MergeFSResponse carries metadata about the updated destination branch.
type MergeFSResponse struct {
	Branch memory.BranchInfo
}
