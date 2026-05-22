package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/memory"
	"github.com/stigen/smol-agents/pkg/memory/api"
	"github.com/stigen/smol-agents/pkg/memory/audit"
	"github.com/stigen/smol-agents/pkg/memory/policy"
	"github.com/stigen/smol-agents/pkg/memory/quota"
	"github.com/stigen/smol-agents/pkg/memory/store"
)

// Gateway is the memory-mcp business logic layer. It validates identity,
// enforces policy and quota, and forwards to the retrieval worker via the
// internal api.RetrievalService. It implements the mcp.Handler interface.
//
// Design: thin gateway — no embedding, chunking, ranking, or storage.
// Implements R-MEM-MCP-3, R-MEM-AUTH-1, R-MEM-AUTH-2, R-MEM-QUOTA-1,
// R-MEM-AUDIT-1, R-MEM-SEC-1.
type Gateway struct {
	// Auth configures JWT-SVID validation.
	Auth AuthConfig

	// Retrievers resolves retrieverRef → MemoryRetriever config + worker URL.
	Retrievers store.RetrieverStore

	// WorkerFactory builds a RetrievalService for a worker base URL.
	// Default: api.NewHTTPClient(url, nil).
	WorkerFactory func(url string) api.RetrievalService

	// Quota enforces per-identity request rate limits.
	Quota *quota.Enforcer

	// Policy is the stateless policy checker.
	Policy policy.Checker

	// AuditLog receives one Record per call.
	AuditLog audit.Logger
}

// HandleToolCall implements Handler. It is the central dispatch point for all
// MCP tools/call requests. Every code-path through this function emits an
// audit record and fails closed on any authz/tenant/quota/policy error.
func (g *Gateway) HandleToolCall(r *http.Request, toolName string, rawArgs json.RawMessage) (ToolCallResult, *RPCError) {
	start := time.Now()

	// ── Step 1: authenticate ───────────────────────────────────────────────
	caller, err := g.Auth.ExtractIdentity(r)
	if err != nil {
		g.logDeny(r, audit.Record{
			Op:        toolName,
			Decision:  audit.DecisionDeny,
			ErrorKind: string(memory.KindUnauthenticated),
			LatencyMs: msElapsed(start),
		})
		return errResult(CodeUnauthenticated, "unauthenticated: "+err.Error())
	}

	// ── Step 2: parse common args ──────────────────────────────────────────
	var args map[string]json.RawMessage
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return g.deny(r, caller, toolName, "", "", start, CodeInvalidParams,
			"decode arguments: "+err.Error(), memory.KindInvalid)
	}

	retrieverRef := jsonStr(args["retrieverRef"])
	namespace := jsonStr(args["namespace"])

	// ── Step 3: resolve retriever ──────────────────────────────────────────
	info, err := g.Retrievers.Get(r.Context(), retrieverRef)
	if err != nil {
		code := memKindToRPCCode(memory.KindOf(err))
		return g.deny(r, caller, toolName, retrieverRef, namespace, start, code,
			"resolve retriever: "+err.Error(), memory.KindOf(err))
	}

	// ── Step 4: validate tenant ────────────────────────────────────────────
	// The namespace in the request is a memory namespace (not a k8s namespace).
	// If the retriever has a tenant scope and the caller's tenant doesn't match,
	// reject. Never trust caller-supplied tenant fields.
	if info.Spec.Tenant != "" && info.Spec.Tenant != caller.Tenant {
		return g.deny(r, caller, toolName, retrieverRef, namespace, start,
			CodePermissionDenied,
			fmt.Sprintf("retriever tenant %q does not match caller tenant %q",
				info.Spec.Tenant, caller.Tenant),
			memory.KindPermissionDenied)
	}

	// ── Step 5: rate check ─────────────────────────────────────────────────
	if err := g.Quota.CheckRate(caller.SPIFFEID, info.Spec.Quota); err != nil {
		return g.deny(r, caller, toolName, retrieverRef, namespace, start,
			CodeQuotaExceeded, err.Error(), memory.KindQuotaExceeded)
	}

	// ── Step 6: route by tool name ─────────────────────────────────────────
	worker := g.workerFor(info.WorkerURL)
	identity := api.RequestIdentity{
		Tenant:         caller.Tenant,
		Namespace:      namespace,
		CallerSPIFFEID: caller.SPIFFEID,
		RetrieverRef:   retrieverRef,
	}

	switch toolName {
	case "retrieve_memory":
		return g.doRetrieve(r, caller, info, identity, args, start)
	case "write_memory":
		return g.doWrite(r, caller, info, worker, identity, args, start)
	case "list_memory_namespaces":
		return g.doListNamespaces(r, caller, info, worker, identity, start)
	case "get_memory":
		return g.doGet(r, caller, info, worker, identity, args, start)
	case "delete_memory":
		return g.doDelete(r, caller, info, worker, identity, args, start)
	case "summarize_memory":
		return g.doSummarize(r, caller, info, worker, identity, args, start)
	default:
		return errResult(CodeMethodNotFound, "unknown tool: "+toolName)
	}
}

// HandleResourceRead implements Handler. Resources pass through the same
// auth/tenant/policy/audit path as tools. R-MEM-MCP-2.
func (g *Gateway) HandleResourceRead(r *http.Request, uri string) (ResourceReadResult, *RPCError) {
	start := time.Now()

	caller, err := g.Auth.ExtractIdentity(r)
	if err != nil {
		g.logDeny(r, audit.Record{
			Op:        "resource_read:" + uri,
			Decision:  audit.DecisionDeny,
			ErrorKind: string(memory.KindUnauthenticated),
			LatencyMs: msElapsed(start),
		})
		_, rpcErr := errResult(CodeUnauthenticated, "unauthenticated: "+err.Error())
		return ResourceReadResult{}, rpcErr
	}

	scheme, path, ok := parseResourceURI(uri)
	if !ok || scheme != "memory" {
		_, rpcErr := errResult(CodeInvalidParams, "invalid resource URI: "+uri)
		return ResourceReadResult{}, rpcErr
	}

	// Route by path prefix.
	switch {
	case strings.HasPrefix(path, "documents/"):
		return g.readDocument(r, caller, path[len("documents/"):], start)

	case strings.HasPrefix(path, "namespaces/"):
		return g.readNamespace(r, caller, path[len("namespaces/"):], start)

	case strings.HasPrefix(path, "retrievers/"):
		return g.readRetriever(r, caller, path[len("retrievers/"):], start)

	case strings.HasPrefix(path, "episodes/"):
		return g.readEpisodes(r, caller, path[len("episodes/"):], start)

	default:
		_, rpcErr := errResult(CodeNotFound, "unknown resource path: "+path)
		return ResourceReadResult{}, rpcErr
	}
}

// ── Tool handlers ──────────────────────────────────────────────────────────

func (g *Gateway) doRetrieve(
	r *http.Request,
	caller CallerIdentity,
	info store.RetrieverInfo,
	identity api.RequestIdentity,
	args map[string]json.RawMessage,
	start time.Time,
) (ToolCallResult, *RPCError) {
	// Policy check: read.
	if err := g.Policy.Allow(caller.SPIFFEID, v1.MemoryOpRead, identity.Namespace, info.Spec.Policy); err != nil {
		return g.deny(r, caller, "retrieve_memory", identity.RetrieverRef, identity.Namespace, start,
			CodePermissionDenied, err.Error(), memory.KindPermissionDenied)
	}

	query := jsonStr(args["query"])
	if query == "" {
		return g.deny(r, caller, "retrieve_memory", identity.RetrieverRef, identity.Namespace, start,
			CodeInvalidParams, "query is required", memory.KindInvalid)
	}

	var requestedTopK int32 = info.Spec.TopK
	if raw, ok := args["topK"]; ok {
		var k int32
		if err := json.Unmarshal(raw, &k); err == nil && k > 0 {
			requestedTopK = k
		}
	}
	effectiveTopK, err := quota.ClampTopK(requestedTopK, info.Spec.Quota)
	if err != nil {
		return g.deny(r, caller, "retrieve_memory", identity.RetrieverRef, identity.Namespace, start,
			CodeQuotaExceeded, err.Error(), memory.KindQuotaExceeded)
	}

	worker := g.workerFor(info.WorkerURL)
	req := &api.RetrieveRequest{
		Identity: identity,
		Query:    query,
		TopK:     effectiveTopK,
		Filters: memory.Filter{
			Tenant:    caller.Tenant,
			Namespace: identity.Namespace,
			Metadata:  jsonMetadata(args["filters"]),
		},
	}
	resp, err := worker.Retrieve(r.Context(), req)
	if err != nil {
		return g.denyWorkerError(r, caller, "retrieve_memory", identity.RetrieverRef, identity.Namespace, start, err)
	}

	g.logAllow(r, caller, "retrieve_memory", identity.RetrieverRef, identity.Namespace,
		filterSummaryFromMeta(req.Filters.Metadata), len(resp.Result.Chunks), start)

	out, _ := json.Marshal(resp.Result)
	return okResult(string(out))
}

func (g *Gateway) doWrite(
	r *http.Request,
	caller CallerIdentity,
	info store.RetrieverInfo,
	worker api.RetrievalService,
	identity api.RequestIdentity,
	args map[string]json.RawMessage,
	start time.Time,
) (ToolCallResult, *RPCError) {
	if err := g.Policy.Allow(caller.SPIFFEID, v1.MemoryOpWrite, identity.Namespace, info.Spec.Policy); err != nil {
		return g.deny(r, caller, "write_memory", identity.RetrieverRef, identity.Namespace, start,
			CodePermissionDenied, err.Error(), memory.KindPermissionDenied)
	}

	content := jsonStr(args["content"])
	if content == "" {
		return g.deny(r, caller, "write_memory", identity.RetrieverRef, identity.Namespace, start,
			CodeInvalidParams, "content is required", memory.KindInvalid)
	}

	payload := []byte(content)
	if err := quota.CheckWriteSize(int64(len(payload)), info.Spec.Quota); err != nil {
		return g.deny(r, caller, "write_memory", identity.RetrieverRef, identity.Namespace, start,
			CodeQuotaExceeded, err.Error(), memory.KindQuotaExceeded)
	}

	doc := memory.Document{
		ID:        jsonStr(args["id"]),
		Namespace: caller.Tenant + "/" + identity.Namespace, // partition key
		Tenant:    caller.Tenant,
		Content:   payload,
		Metadata:  jsonMetadata(args["metadata"]),
	}
	// Override namespace and tenant from the gateway-derived identity.
	doc.Namespace = identity.Namespace
	doc.Tenant = caller.Tenant

	req := &api.WriteRequest{Identity: identity, Document: doc}
	resp, err := worker.Write(r.Context(), req)
	if err != nil {
		return g.denyWorkerError(r, caller, "write_memory", identity.RetrieverRef, identity.Namespace, start, err)
	}

	g.logAllow(r, caller, "write_memory", identity.RetrieverRef, identity.Namespace, "", 1, start)
	out, _ := json.Marshal(resp.Result)
	return okResult(string(out))
}

func (g *Gateway) doListNamespaces(
	r *http.Request,
	caller CallerIdentity,
	info store.RetrieverInfo,
	worker api.RetrievalService,
	identity api.RequestIdentity,
	start time.Time,
) (ToolCallResult, *RPCError) {
	// ListNamespaces is a read operation; require read grant on any namespace.
	if err := g.Policy.Allow(caller.SPIFFEID, v1.MemoryOpRead, "*", info.Spec.Policy); err != nil {
		return g.deny(r, caller, "list_memory_namespaces", identity.RetrieverRef, "*", start,
			CodePermissionDenied, err.Error(), memory.KindPermissionDenied)
	}

	req := &api.ListNamespacesRequest{Identity: identity}
	resp, err := worker.ListNamespaces(r.Context(), req)
	if err != nil {
		return g.denyWorkerError(r, caller, "list_memory_namespaces", identity.RetrieverRef, "*", start, err)
	}

	g.logAllow(r, caller, "list_memory_namespaces", identity.RetrieverRef, "*", "", len(resp.Namespaces), start)
	out, _ := json.Marshal(resp.Namespaces)
	return okResult(string(out))
}

func (g *Gateway) doGet(
	r *http.Request,
	caller CallerIdentity,
	info store.RetrieverInfo,
	worker api.RetrievalService,
	identity api.RequestIdentity,
	args map[string]json.RawMessage,
	start time.Time,
) (ToolCallResult, *RPCError) {
	if err := g.Policy.Allow(caller.SPIFFEID, v1.MemoryOpRead, identity.Namespace, info.Spec.Policy); err != nil {
		return g.deny(r, caller, "get_memory", identity.RetrieverRef, identity.Namespace, start,
			CodePermissionDenied, err.Error(), memory.KindPermissionDenied)
	}

	id := jsonStr(args["id"])
	if id == "" {
		return g.deny(r, caller, "get_memory", identity.RetrieverRef, identity.Namespace, start,
			CodeInvalidParams, "id is required", memory.KindInvalid)
	}

	req := &api.GetRequest{Identity: identity, ID: id}
	resp, err := worker.Get(r.Context(), req)
	if err != nil {
		return g.denyWorkerError(r, caller, "get_memory", identity.RetrieverRef, identity.Namespace, start, err)
	}

	// Double-check: the worker must have validated tenant ownership, but the
	// gateway enforces it too (R-MEM-SEC-1: no cross-tenant document via direct id).
	if resp.Document.Tenant != "" && resp.Document.Tenant != caller.Tenant {
		return g.deny(r, caller, "get_memory", identity.RetrieverRef, identity.Namespace, start,
			CodePermissionDenied, "document tenant mismatch", memory.KindPermissionDenied)
	}

	g.logAllow(r, caller, "get_memory", identity.RetrieverRef, identity.Namespace, "id="+id, 1, start)
	out, _ := json.Marshal(resp.Document)
	return okResult(string(out))
}

func (g *Gateway) doDelete(
	r *http.Request,
	caller CallerIdentity,
	info store.RetrieverInfo,
	worker api.RetrievalService,
	identity api.RequestIdentity,
	args map[string]json.RawMessage,
	start time.Time,
) (ToolCallResult, *RPCError) {
	if err := g.Policy.Allow(caller.SPIFFEID, v1.MemoryOpDelete, identity.Namespace, info.Spec.Policy); err != nil {
		return g.deny(r, caller, "delete_memory", identity.RetrieverRef, identity.Namespace, start,
			CodePermissionDenied, err.Error(), memory.KindPermissionDenied)
	}

	id := jsonStr(args["id"])
	if id == "" {
		return g.deny(r, caller, "delete_memory", identity.RetrieverRef, identity.Namespace, start,
			CodeInvalidParams, "id is required", memory.KindInvalid)
	}

	req := &api.DeleteRequest{Identity: identity, ID: id}
	if _, err := worker.Delete(r.Context(), req); err != nil {
		return g.denyWorkerError(r, caller, "delete_memory", identity.RetrieverRef, identity.Namespace, start, err)
	}

	g.logAllow(r, caller, "delete_memory", identity.RetrieverRef, identity.Namespace, "id="+id, 0, start)
	return okResult(`{"deleted":true}`)
}

func (g *Gateway) doSummarize(
	r *http.Request,
	caller CallerIdentity,
	info store.RetrieverInfo,
	worker api.RetrievalService,
	identity api.RequestIdentity,
	args map[string]json.RawMessage,
	start time.Time,
) (ToolCallResult, *RPCError) {
	if err := g.Policy.Allow(caller.SPIFFEID, v1.MemoryOpRead, identity.Namespace, info.Spec.Policy); err != nil {
		return g.deny(r, caller, "summarize_memory", identity.RetrieverRef, identity.Namespace, start,
			CodePermissionDenied, err.Error(), memory.KindPermissionDenied)
	}

	query := jsonStr(args["query"])
	if query == "" {
		return g.deny(r, caller, "summarize_memory", identity.RetrieverRef, identity.Namespace, start,
			CodeInvalidParams, "query is required", memory.KindInvalid)
	}

	req := &api.SummarizeRequest{Identity: identity, Query: query}
	resp, err := worker.Summarize(r.Context(), req)
	if err != nil {
		return g.denyWorkerError(r, caller, "summarize_memory", identity.RetrieverRef, identity.Namespace, start, err)
	}

	g.logAllow(r, caller, "summarize_memory", identity.RetrieverRef, identity.Namespace, "", 1, start)
	out, _ := json.Marshal(map[string]string{"summary": resp.Summary})
	return okResult(string(out))
}

// ── Resource handlers ──────────────────────────────────────────────────────

func (g *Gateway) readDocument(r *http.Request, caller CallerIdentity, id string, start time.Time) (ResourceReadResult, *RPCError) {
	// Re-use the get_memory path: derive retriever from a default or require
	// a retrieverRef query param. For simplicity, resources require the caller
	// to embed retrieverRef in the URI query part or use tools/call instead.
	// The MCP resource model doesn't natively carry authz params; we deny
	// resource reads that can't be scoped. R-MEM-SEC-1.
	retrieverRef := r.URL.Query().Get("retrieverRef")
	namespace := r.URL.Query().Get("namespace")

	info, err := g.Retrievers.Get(r.Context(), retrieverRef)
	if err != nil {
		g.logDeny(r, audit.Record{
			CallerSPIFFEID: caller.SPIFFEID, Tenant: caller.Tenant,
			Op:        "resource:documents/" + id,
			Decision:  audit.DecisionDeny,
			ErrorKind: string(memory.KindOf(err)),
			LatencyMs: msElapsed(start),
		})
		return ResourceReadResult{}, &RPCError{Code: CodeNotFound, Message: "retriever not found: " + retrieverRef}
	}

	identity := api.RequestIdentity{
		Tenant:         caller.Tenant,
		Namespace:      namespace,
		CallerSPIFFEID: caller.SPIFFEID,
		RetrieverRef:   retrieverRef,
	}

	if err := g.Policy.Allow(caller.SPIFFEID, v1.MemoryOpRead, namespace, info.Spec.Policy); err != nil {
		g.logDeny(r, audit.Record{
			CallerSPIFFEID: caller.SPIFFEID, Tenant: caller.Tenant,
			RetrieverRef: retrieverRef, Namespace: namespace,
			Op:        "resource:documents/" + id,
			Decision:  audit.DecisionDeny,
			ErrorKind: string(memory.KindPermissionDenied),
			LatencyMs: msElapsed(start),
		})
		return ResourceReadResult{}, &RPCError{Code: CodePermissionDenied, Message: err.Error()}
	}

	worker := g.workerFor(info.WorkerURL)
	resp, err := worker.Get(r.Context(), &api.GetRequest{Identity: identity, ID: id})
	if err != nil {
		g.logDeny(r, audit.Record{
			CallerSPIFFEID: caller.SPIFFEID, Tenant: caller.Tenant,
			RetrieverRef: retrieverRef, Namespace: namespace,
			Op:        "resource:documents/" + id,
			Decision:  audit.DecisionDeny,
			ErrorKind: string(memory.KindOf(err)),
			LatencyMs: msElapsed(start),
		})
		return ResourceReadResult{}, &RPCError{Code: memKindToRPCCode(memory.KindOf(err)), Message: err.Error()}
	}

	if resp.Document.Tenant != "" && resp.Document.Tenant != caller.Tenant {
		g.logDeny(r, audit.Record{
			CallerSPIFFEID: caller.SPIFFEID, Tenant: caller.Tenant,
			RetrieverRef: retrieverRef, Namespace: namespace,
			Op:        "resource:documents/" + id,
			Decision:  audit.DecisionDeny,
			ErrorKind: string(memory.KindPermissionDenied),
			LatencyMs: msElapsed(start),
		})
		return ResourceReadResult{}, &RPCError{Code: CodePermissionDenied, Message: "document tenant mismatch"}
	}

	g.logAllow(r, caller, "resource:documents/"+id, retrieverRef, namespace, "id="+id, 1, start)
	out, _ := json.Marshal(resp.Document)
	return ResourceReadResult{
		Contents: []ResourceContent{{
			URI:      "memory://documents/" + id,
			MIMEType: "application/json",
			Text:     string(out),
		}},
	}, nil
}

func (g *Gateway) readNamespace(r *http.Request, caller CallerIdentity, ns string, start time.Time) (ResourceReadResult, *RPCError) {
	retrieverRef := r.URL.Query().Get("retrieverRef")
	info, err := g.Retrievers.Get(r.Context(), retrieverRef)
	if err != nil {
		return ResourceReadResult{}, &RPCError{Code: CodeNotFound, Message: "retriever not found"}
	}
	if err := g.Policy.Allow(caller.SPIFFEID, v1.MemoryOpRead, ns, info.Spec.Policy); err != nil {
		return ResourceReadResult{}, &RPCError{Code: CodePermissionDenied, Message: err.Error()}
	}

	identity := api.RequestIdentity{
		Tenant:         caller.Tenant,
		Namespace:      ns,
		CallerSPIFFEID: caller.SPIFFEID,
		RetrieverRef:   retrieverRef,
	}
	worker := g.workerFor(info.WorkerURL)
	resp, err := worker.ListNamespaces(r.Context(), &api.ListNamespacesRequest{Identity: identity})
	if err != nil {
		return ResourceReadResult{}, &RPCError{Code: memKindToRPCCode(memory.KindOf(err)), Message: err.Error()}
	}

	g.logAllow(r, caller, "resource:namespaces/"+ns, retrieverRef, ns, "", len(resp.Namespaces), start)
	out, _ := json.Marshal(map[string]any{"namespaces": resp.Namespaces, "namespace": ns})
	return ResourceReadResult{Contents: []ResourceContent{{
		URI: "memory://namespaces/" + ns, MIMEType: "application/json", Text: string(out),
	}}}, nil
}

func (g *Gateway) readRetriever(_ *http.Request, caller CallerIdentity, ref string, start time.Time) (ResourceReadResult, *RPCError) {
	info, err := g.Retrievers.Get(nil, ref) //nolint:staticcheck // nil ctx is fine for fake
	if err != nil {
		return ResourceReadResult{}, &RPCError{Code: CodeNotFound, Message: "retriever not found: " + ref}
	}
	// Tenant gate: if the retriever has a tenant, only that tenant can see it.
	if info.Spec.Tenant != "" && info.Spec.Tenant != caller.Tenant {
		return ResourceReadResult{}, &RPCError{Code: CodePermissionDenied, Message: "retriever not visible to caller"}
	}
	// Expose only non-sensitive metadata (policy grants are sensitive config).
	summary := map[string]any{
		"retrieverRef": ref,
		"stores":       info.Spec.Stores,
		"topK":         info.Spec.TopK,
		"namespaces":   info.Spec.Namespaces,
		"tenant":       info.Spec.Tenant,
	}
	_ = start // latency recorded by caller
	out, _ := json.Marshal(summary)
	return ResourceReadResult{Contents: []ResourceContent{{
		URI: "memory://retrievers/" + ref, MIMEType: "application/json", Text: string(out),
	}}}, nil
}

func (g *Gateway) readEpisodes(_ *http.Request, caller CallerIdentity, agentID string, start time.Time) (ResourceReadResult, *RPCError) {
	// Episodes are a P2 feature. We return a not-supported error rather than
	// silently returning empty results. R-MEM-SEC-1: fail-closed, not silent.
	_ = caller
	_ = agentID
	_ = start
	return ResourceReadResult{}, &RPCError{
		Code:    CodeNotSupported,
		Message: "memory://episodes/{agentId} is not yet implemented (P2)",
	}
}

// ── Audit helpers ──────────────────────────────────────────────────────────

func (g *Gateway) logAllow(r *http.Request, caller CallerIdentity, op, ref, ns, filterSummary string, count int, start time.Time) {
	if g.AuditLog == nil {
		return
	}
	g.AuditLog.Log(r.Context(), audit.Record{
		CallerSPIFFEID: caller.SPIFFEID,
		Tenant:         caller.Tenant,
		RetrieverRef:   ref,
		Op:             op,
		Namespace:      ns,
		FilterSummary:  filterSummary,
		ResultCount:    count,
		Decision:       audit.DecisionAllow,
		LatencyMs:      msElapsed(start),
		Timestamp:      time.Now(),
	})
}

func (g *Gateway) logDeny(r *http.Request, rec audit.Record) {
	if g.AuditLog == nil {
		return
	}
	rec.Decision = audit.DecisionDeny
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now()
	}
	ctx := r.Context()
	g.AuditLog.Log(ctx, rec)
}

// deny emits a structured audit record and returns an MCP error result.
// It is the single exit for all access-control and quota failures.
func (g *Gateway) deny(r *http.Request, caller CallerIdentity, op, ref, ns string, start time.Time, code int, msg string, k memory.Kind) (ToolCallResult, *RPCError) {
	g.logDeny(r, audit.Record{
		CallerSPIFFEID: caller.SPIFFEID,
		Tenant:         caller.Tenant,
		RetrieverRef:   ref,
		Op:             op,
		Namespace:      ns,
		Decision:       audit.DecisionDeny,
		ErrorKind:      string(k),
		LatencyMs:      msElapsed(start),
		Timestamp:      time.Now(),
	})
	return errResult(code, msg)
}

// denyWorkerError wraps a worker error as a deny record + MCP error.
func (g *Gateway) denyWorkerError(r *http.Request, caller CallerIdentity, op, ref, ns string, start time.Time, err error) (ToolCallResult, *RPCError) {
	k := memory.KindOf(err)
	return g.deny(r, caller, op, ref, ns, start, memKindToRPCCode(k), err.Error(), k)
}

// workerFor returns a RetrievalService for the given base URL.
func (g *Gateway) workerFor(url string) api.RetrievalService {
	if g.WorkerFactory != nil {
		return g.WorkerFactory(url)
	}
	return api.NewHTTPClient(url, nil)
}

// ── Small helpers ──────────────────────────────────────────────────────────

func okResult(text string) (ToolCallResult, *RPCError) {
	return ToolCallResult{Content: []ContentBlock{{Type: "text", Text: text}}}, nil
}

func errResult(code int, msg string) (ToolCallResult, *RPCError) {
	return ToolCallResult{}, &RPCError{Code: code, Message: msg}
}

func jsonStr(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func jsonMetadata(raw json.RawMessage) map[string]string {
	if raw == nil {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

func filterSummaryFromMeta(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return "metadata_keys=[" + strings.Join(keys, ",") + "]"
}

func msElapsed(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

func memKindToRPCCode(k memory.Kind) int {
	switch k {
	case memory.KindUnauthenticated:
		return CodeUnauthenticated
	case memory.KindPermissionDenied:
		return CodePermissionDenied
	case memory.KindQuotaExceeded:
		return CodeQuotaExceeded
	case memory.KindNotFound:
		return CodeNotFound
	case memory.KindNotSupported:
		return CodeNotSupported
	case memory.KindInvalid:
		return CodeInvalidParams
	default:
		return CodeBackendError
	}
}

// parseResourceURI splits "memory://namespaces/foo" into ("memory", "namespaces/foo", true).
func parseResourceURI(uri string) (scheme, path string, ok bool) {
	const sep = "://"
	idx := strings.Index(uri, sep)
	if idx < 0 {
		return "", "", false
	}
	scheme = uri[:idx]
	rest := uri[idx+len(sep):]
	// rest may be "host/path" or just "path"; memory:// URIs use the first
	// segment as the resource type.
	return scheme, rest, true
}
