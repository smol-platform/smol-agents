package api

// Internal transport for the RetrievalService contract.
//
// The canonical transport is gRPC over mTLS (see api.go); until a buf workspace
// exists, P1 uses HTTP+JSON over the same mTLS channel — a thin, swappable wire
// format that wraps the RetrievalService interface verbatim. The gateway uses
// NewHTTPClient; the worker serves NewHTTPServer. The typed memory.Kind survives
// the hop via HTTP status + a JSON error envelope (R-MEM-SEC-1 fail-closed).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/stigen/smol-agents/pkg/memory"
)

const (
	pathRetrieve       = "/v1/retrieve"
	pathWrite          = "/v1/write"
	pathGet            = "/v1/get"
	pathDelete         = "/v1/delete"
	pathListNamespaces = "/v1/list-namespaces"
	pathSummarize      = "/v1/summarize"
	pathBranchFS       = "/v1/branch-fs"
	pathSnapshotFS     = "/v1/snapshot-fs"
	pathListBranches   = "/v1/list-branches"
	pathMergeFS        = "/v1/merge-fs"
)

// errorEnvelope is the JSON body of a non-2xx response.
type errorEnvelope struct {
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
}

func statusForKind(k memory.Kind) int {
	switch k {
	case memory.KindUnauthenticated:
		return http.StatusUnauthorized
	case memory.KindPermissionDenied:
		return http.StatusForbidden
	case memory.KindQuotaExceeded:
		return http.StatusTooManyRequests
	case memory.KindNotFound:
		return http.StatusNotFound
	case memory.KindNotSupported:
		return http.StatusNotImplemented
	case memory.KindInvalid:
		return http.StatusBadRequest
	case memory.KindBackendUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func kindForStatus(code int) memory.Kind {
	switch code {
	case http.StatusUnauthorized:
		return memory.KindUnauthenticated
	case http.StatusForbidden:
		return memory.KindPermissionDenied
	case http.StatusTooManyRequests:
		return memory.KindQuotaExceeded
	case http.StatusNotFound:
		return memory.KindNotFound
	case http.StatusNotImplemented:
		return memory.KindNotSupported
	case http.StatusBadRequest:
		return memory.KindInvalid
	case http.StatusServiceUnavailable:
		return memory.KindBackendUnavailable
	default:
		return memory.KindInternal
	}
}

// ── Server (worker side) ────────────────────────────────────────────────────

// NewHTTPServer wraps a RetrievalService as an http.Handler. Mount it behind an
// mTLS listener in cmd/memory-worker.
func NewHTTPServer(svc RetrievalService) http.Handler {
	mux := http.NewServeMux()
	route(mux, pathRetrieve, svc.Retrieve)
	route(mux, pathWrite, svc.Write)
	route(mux, pathGet, svc.Get)
	route(mux, pathDelete, svc.Delete)
	route(mux, pathListNamespaces, svc.ListNamespaces)
	route(mux, pathSummarize, svc.Summarize)
	route(mux, pathBranchFS, svc.BranchFS)
	route(mux, pathSnapshotFS, svc.SnapshotFS)
	route(mux, pathListBranches, svc.ListBranches)
	route(mux, pathMergeFS, svc.MergeFS)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func route[Req, Resp any](mux *http.ServeMux, path string, fn func(context.Context, *Req) (*Resp, error)) {
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req Req
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, memory.Invalid("decode request: "+err.Error()))
			return
		}
		resp, err := fn(r.Context(), &req)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func writeErr(w http.ResponseWriter, err error) {
	k := memory.KindOf(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusForKind(k))
	_ = json.NewEncoder(w).Encode(errorEnvelope{Kind: string(k), Msg: err.Error()})
}

// ── Client (gateway side) ───────────────────────────────────────────────────

type httpClient struct {
	base string
	hc   *http.Client
}

// NewHTTPClient returns a RetrievalService that calls a worker's NewHTTPServer.
// Pass an mTLS-configured *http.Client for the in-mesh channel.
func NewHTTPClient(baseURL string, hc *http.Client) RetrievalService {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &httpClient{base: baseURL, hc: hc}
}

func (c *httpClient) Retrieve(ctx context.Context, req *RetrieveRequest) (*RetrieveResponse, error) {
	return call[RetrieveRequest, RetrieveResponse](ctx, c, pathRetrieve, req)
}
func (c *httpClient) Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error) {
	return call[WriteRequest, WriteResponse](ctx, c, pathWrite, req)
}
func (c *httpClient) Get(ctx context.Context, req *GetRequest) (*GetResponse, error) {
	return call[GetRequest, GetResponse](ctx, c, pathGet, req)
}
func (c *httpClient) Delete(ctx context.Context, req *DeleteRequest) (*DeleteResponse, error) {
	return call[DeleteRequest, DeleteResponse](ctx, c, pathDelete, req)
}
func (c *httpClient) ListNamespaces(ctx context.Context, req *ListNamespacesRequest) (*ListNamespacesResponse, error) {
	return call[ListNamespacesRequest, ListNamespacesResponse](ctx, c, pathListNamespaces, req)
}
func (c *httpClient) Summarize(ctx context.Context, req *SummarizeRequest) (*SummarizeResponse, error) {
	return call[SummarizeRequest, SummarizeResponse](ctx, c, pathSummarize, req)
}
func (c *httpClient) BranchFS(ctx context.Context, req *BranchFSRequest) (*BranchFSResponse, error) {
	return call[BranchFSRequest, BranchFSResponse](ctx, c, pathBranchFS, req)
}
func (c *httpClient) SnapshotFS(ctx context.Context, req *SnapshotFSRequest) (*SnapshotFSResponse, error) {
	return call[SnapshotFSRequest, SnapshotFSResponse](ctx, c, pathSnapshotFS, req)
}
func (c *httpClient) ListBranches(ctx context.Context, req *ListBranchesRequest) (*ListBranchesResponse, error) {
	return call[ListBranchesRequest, ListBranchesResponse](ctx, c, pathListBranches, req)
}
func (c *httpClient) MergeFS(ctx context.Context, req *MergeFSRequest) (*MergeFSResponse, error) {
	return call[MergeFSRequest, MergeFSResponse](ctx, c, pathMergeFS, req)
}

func call[Req, Resp any](ctx context.Context, c *httpClient, path string, req *Req) (*Resp, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, memory.Invalid("marshal request: " + err.Error())
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, memory.Internal(err.Error())
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, memory.BackendUnavailable(err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		var env errorEnvelope
		_ = json.Unmarshal(raw, &env)
		k := memory.Kind(env.Kind)
		if k == "" {
			k = kindForStatus(resp.StatusCode)
		}
		msg := env.Msg
		if msg == "" {
			msg = string(raw)
		}
		return nil, &memory.Error{Kind: k, Msg: msg}
	}
	var out Resp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, memory.Internal("decode response: " + err.Error())
	}
	return &out, nil
}

// compile-time assertion: the HTTP client satisfies the contract.
var _ RetrievalService = (*httpClient)(nil)
