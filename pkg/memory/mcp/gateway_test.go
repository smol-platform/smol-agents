package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/memory"
	"github.com/smol-platform/smol-agents/pkg/memory/api"
	"github.com/smol-platform/smol-agents/pkg/memory/audit"
	"github.com/smol-platform/smol-agents/pkg/memory/mcp"
	"github.com/smol-platform/smol-agents/pkg/memory/quota"
	"github.com/smol-platform/smol-agents/pkg/memory/store"
)

// ── Fake RetrievalService ─────────────────────────────────────────────────

type fakeWorker struct {
	retrieveResp  *api.RetrieveResponse
	writeResp     *api.WriteResponse
	getResp       *api.GetResponse
	deleteResp    *api.DeleteResponse
	listNsResp    *api.ListNamespacesResponse
	summarizeResp *api.SummarizeResponse
	err           error

	// captured requests for assertion
	lastRetrieve *api.RetrieveRequest
	lastGet      *api.GetRequest
	lastWrite    *api.WriteRequest
	lastDelete   *api.DeleteRequest
}

func (f *fakeWorker) Retrieve(_ context.Context, req *api.RetrieveRequest) (*api.RetrieveResponse, error) {
	f.lastRetrieve = req
	if f.err != nil {
		return nil, f.err
	}
	if f.retrieveResp != nil {
		return f.retrieveResp, nil
	}
	return &api.RetrieveResponse{}, nil
}

func (f *fakeWorker) Write(_ context.Context, req *api.WriteRequest) (*api.WriteResponse, error) {
	f.lastWrite = req
	if f.err != nil {
		return nil, f.err
	}
	if f.writeResp != nil {
		return f.writeResp, nil
	}
	return &api.WriteResponse{Result: memory.WriteResult{ID: "doc-1"}}, nil
}

func (f *fakeWorker) Get(_ context.Context, req *api.GetRequest) (*api.GetResponse, error) {
	f.lastGet = req
	if f.err != nil {
		return nil, f.err
	}
	if f.getResp != nil {
		return f.getResp, nil
	}
	return &api.GetResponse{}, nil
}

func (f *fakeWorker) Delete(_ context.Context, req *api.DeleteRequest) (*api.DeleteResponse, error) {
	f.lastDelete = req
	if f.err != nil {
		return nil, f.err
	}
	return &api.DeleteResponse{}, nil
}

func (f *fakeWorker) ListNamespaces(_ context.Context, _ *api.ListNamespacesRequest) (*api.ListNamespacesResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.listNsResp != nil {
		return f.listNsResp, nil
	}
	return &api.ListNamespacesResponse{Namespaces: []string{"docs"}}, nil
}

func (f *fakeWorker) Summarize(_ context.Context, _ *api.SummarizeRequest) (*api.SummarizeResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &api.SummarizeResponse{Summary: "a summary"}, nil
}

func (f *fakeWorker) BranchFS(_ context.Context, _ *api.BranchFSRequest) (*api.BranchFSResponse, error) {
	return nil, &memory.Error{Kind: memory.KindNotSupported, Msg: "not a filesystem backend"}
}

func (f *fakeWorker) SnapshotFS(_ context.Context, _ *api.SnapshotFSRequest) (*api.SnapshotFSResponse, error) {
	return nil, &memory.Error{Kind: memory.KindNotSupported, Msg: "not a filesystem backend"}
}

func (f *fakeWorker) ListBranches(_ context.Context, _ *api.ListBranchesRequest) (*api.ListBranchesResponse, error) {
	return nil, &memory.Error{Kind: memory.KindNotSupported, Msg: "not a filesystem backend"}
}

func (f *fakeWorker) MergeFS(_ context.Context, _ *api.MergeFSRequest) (*api.MergeFSResponse, error) {
	return nil, &memory.Error{Kind: memory.KindNotSupported, Msg: "not a filesystem backend"}
}

var _ api.RetrievalService = (*fakeWorker)(nil)

// ── Test harness ──────────────────────────────────────────────────────────

// testGateway builds a Gateway with the given retriever policy/quota,
// a fake worker, and an audit collector.
func testGateway(t *testing.T, retrieverSpec v1.MemoryRetrieverSpec, worker *fakeWorker) (*mcp.Gateway, *audit.CollectorLogger) {
	t.Helper()
	rs := store.NewFakeStore()
	rs.Add("ns/test-retriever", store.RetrieverInfo{
		Spec:      retrieverSpec,
		WorkerURL: "http://fake-worker",
	})

	col := &audit.CollectorLogger{}
	gw := &mcp.Gateway{
		Auth:       mcp.AuthConfig{}, // insecure: no BundleSource → ParseInsecure
		Retrievers: rs,
		Quota:      quota.NewEnforcer(),
		AuditLog:   col,
		WorkerFactory: func(_ string) api.RetrievalService {
			return worker
		},
	}
	return gw, col
}

// buildJWT builds a minimal (unsigned) JWT-SVID for a given SPIFFE ID.
// Only used with ParseInsecure (no signature required in tests).
func buildJWT(spiffeID, audience string) string {
	header := base64URLJSON(map[string]any{"alg": "RS256", "typ": "JWT"})
	exp := time.Now().Add(time.Hour).Unix()
	claims := map[string]any{
		"sub": spiffeID,
		"aud": []string{audience},
		"exp": exp,
		"iat": time.Now().Unix(),
	}
	payload := base64URLJSON(claims)
	// Unsigned — valid only for ParseInsecure.
	return header + "." + payload + ".fakesig"
}

func base64URLJSON(v any) string {
	b, _ := json.Marshal(v)
	// Standard base64url without padding.
	enc := encodeBase64URL(b)
	return enc
}

func encodeBase64URL(b []byte) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var buf strings.Builder
	for i := 0; i < len(b); i += 3 {
		remaining := len(b) - i
		var b0, b1, b2 byte
		b0 = b[i]
		if remaining > 1 {
			b1 = b[i+1]
		}
		if remaining > 2 {
			b2 = b[i+2]
		}
		buf.WriteByte(chars[b0>>2])
		buf.WriteByte(chars[((b0&3)<<4)|(b1>>4)])
		if remaining > 1 {
			buf.WriteByte(chars[((b1&15)<<2)|(b2>>6)])
		}
		if remaining > 2 {
			buf.WriteByte(chars[b2&63])
		}
	}
	return buf.String()
}

// callTool sends a JSON-RPC tools/call request to the dispatcher and
// returns the decoded response.
func callTool(t *testing.T, srv *httptest.Server, spiffeID, toolName string, args any) map[string]any {
	t.Helper()
	token := buildJWT(spiffeID, "memory-mcp")
	argsJSON, _ := json.Marshal(args)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
		toolName, argsJSON)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// ── Tests ─────────────────────────────────────────────────────────────────

func TestGateway_Unauthenticated(t *testing.T) {
	spec := v1.MemoryRetrieverSpec{TopK: 10}
	gw, col := testGateway(t, spec, &fakeWorker{})
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"retrieve_memory","arguments":{"query":"hello","retrieverRef":"ns/test-retriever"}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)

	rpcErr := out["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != mcp.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated, got %v", rpcErr["code"])
	}

	// Audit must have fired even for unauthenticated.
	if len(col.Records) == 0 {
		t.Fatal("no audit record for unauthenticated call")
	}
	if col.Records[0].Decision != audit.DecisionDeny {
		t.Fatalf("audit decision = %q, want deny", col.Records[0].Decision)
	}
}

// TestGateway_TenantInjection verifies that the caller's tenant is derived
// from the SPIFFE identity and injected into the worker request — not from
// caller-supplied fields. R-MEM-AUTH-1.
func TestGateway_TenantInjection(t *testing.T) {
	worker := &fakeWorker{}
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   "spiffe://smol-agents.ai/ns/team-alpha/sa/coder",
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"*"},
		}},
	}
	gw, _ := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	spiffeID := "spiffe://smol-agents.ai/ns/team-alpha/sa/coder"
	resp := callTool(t, srv, spiffeID, "retrieve_memory", map[string]any{
		"query":        "test query",
		"retrieverRef": "ns/test-retriever",
		"namespace":    "docs",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}

	// The worker must receive the gateway-derived tenant, not any caller value.
	if worker.lastRetrieve == nil {
		t.Fatal("worker was not called")
	}
	if worker.lastRetrieve.Identity.Tenant != "team-alpha" {
		t.Fatalf("worker tenant = %q, want team-alpha", worker.lastRetrieve.Identity.Tenant)
	}
	if worker.lastRetrieve.Filters.Tenant != "team-alpha" {
		t.Fatalf("filter tenant = %q, want team-alpha", worker.lastRetrieve.Filters.Tenant)
	}
	if worker.lastRetrieve.Identity.CallerSPIFFEID != spiffeID {
		t.Fatalf("CallerSPIFFEID = %q, want %q", worker.lastRetrieve.Identity.CallerSPIFFEID, spiffeID)
	}
}

// TestGateway_CrossTenantDenial verifies that a retriever scoped to
// "team-alpha" rejects a caller from "team-beta". R-MEM-AUTH-1, R-MEM-SEC-1.
func TestGateway_CrossTenantDenial(t *testing.T) {
	worker := &fakeWorker{}
	spec := v1.MemoryRetrieverSpec{
		TopK:   10,
		Tenant: "team-alpha", // retriever scoped to team-alpha
		Policy: []v1.MemoryGrant{{
			Identity:   "spiffe://smol-agents.ai/ns/team-alpha/",
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"*"},
		}},
	}
	gw, col := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	// Caller is from team-beta — should be denied.
	resp := callTool(t, srv, "spiffe://smol-agents.ai/ns/team-beta/sa/intruder",
		"retrieve_memory", map[string]any{
			"query":        "secret docs",
			"retrieverRef": "ns/test-retriever",
		})

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error, got result: %v", resp["result"])
	}
	code := int(rpcErr["code"].(float64))
	if code != mcp.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied, got %d", code)
	}

	// Worker must never have been called.
	if worker.lastRetrieve != nil {
		t.Fatal("worker was called despite cross-tenant denial")
	}

	// Audit record must exist and be a deny.
	if len(col.Records) == 0 {
		t.Fatal("no audit record for cross-tenant denial")
	}
	if col.Records[0].Decision != audit.DecisionDeny {
		t.Fatalf("audit decision = %q, want deny", col.Records[0].Decision)
	}
}

// TestGateway_CrossTenantDenialDirectGetMemory verifies that even a direct
// get_memory by document ID cannot cross tenant boundaries. R-MEM-SEC-1.
func TestGateway_CrossTenantDenialDirectGetMemory(t *testing.T) {
	// The worker returns a document owned by team-alpha.
	worker := &fakeWorker{
		getResp: &api.GetResponse{
			Document: memory.Document{
				ID:     "doc-alpha-secret",
				Tenant: "team-alpha",
			},
		},
	}
	// Grant team-beta read on the retriever (in a hypothetical misconfiguration).
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   "spiffe://smol-agents.ai/ns/team-beta/sa/x",
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"*"},
		}},
	}
	gw, col := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	resp := callTool(t, srv, "spiffe://smol-agents.ai/ns/team-beta/sa/x",
		"get_memory", map[string]any{
			"id":           "doc-alpha-secret",
			"retrieverRef": "ns/test-retriever",
		})

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error (cross-tenant get), got result: %v", resp["result"])
	}
	code := int(rpcErr["code"].(float64))
	if code != mcp.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied for cross-tenant document, got %d", code)
	}

	// Must be audited as deny.
	if len(col.Records) == 0 || col.Records[0].Decision != audit.DecisionDeny {
		t.Fatal("cross-tenant get_memory must be audited as deny")
	}
}

// TestGateway_DenyByDefault verifies that an empty policy denies everything.
// R-MEM-AUTH-2.
func TestGateway_DenyByDefault(t *testing.T) {
	worker := &fakeWorker{}
	spec := v1.MemoryRetrieverSpec{
		TopK:   10,
		Policy: nil, // no grants
	}
	gw, col := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	for _, tool := range []string{"retrieve_memory", "write_memory", "delete_memory"} {
		var args map[string]any
		switch tool {
		case "retrieve_memory":
			args = map[string]any{"query": "q", "retrieverRef": "ns/test-retriever"}
		case "write_memory":
			args = map[string]any{"content": "x", "retrieverRef": "ns/test-retriever"}
		case "delete_memory":
			args = map[string]any{"id": "doc-1", "retrieverRef": "ns/test-retriever"}
		}

		resp := callTool(t, srv, "spiffe://smol-agents.ai/ns/any/sa/caller", tool, args)
		rpcErr, ok := resp["error"].(map[string]any)
		if !ok {
			t.Errorf("tool %q: expected deny, got result: %v", tool, resp["result"])
			continue
		}
		code := int(rpcErr["code"].(float64))
		if code != mcp.CodePermissionDenied {
			t.Errorf("tool %q: want CodePermissionDenied, got %d", tool, code)
		}
	}

	// All calls should be audited.
	if len(col.Records) == 0 {
		t.Fatal("no audit records for denied calls")
	}
	for _, rec := range col.Records {
		if rec.Decision != audit.DecisionDeny {
			t.Errorf("audit record decision = %q, want deny", rec.Decision)
		}
	}
}

// TestGateway_QuotaClamp verifies that topK above the ceiling is rejected
// with QuotaExceeded (not silently truncated). R-MEM-QUOTA-1.
func TestGateway_QuotaClamp(t *testing.T) {
	worker := &fakeWorker{}
	spec := v1.MemoryRetrieverSpec{
		TopK:  10,
		Quota: v1.QuotaSpec{MaxTopK: 20},
		Policy: []v1.MemoryGrant{{
			Identity:   "spiffe://smol-agents.ai/ns/team/sa/agent",
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"*"},
		}},
	}
	gw, _ := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	// Request topK=100 > MaxTopK=20 → must error, not truncate.
	resp := callTool(t, srv, "spiffe://smol-agents.ai/ns/team/sa/agent",
		"retrieve_memory", map[string]any{
			"query":        "find stuff",
			"retrieverRef": "ns/test-retriever",
			"topK":         100,
		})

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		// If the call succeeded, confirm the worker was NOT called with topK=100.
		t.Fatalf("expected QuotaExceeded error for topK=100 > MaxTopK=20, got result: %v", resp["result"])
	}
	code := int(rpcErr["code"].(float64))
	if code != mcp.CodeQuotaExceeded {
		t.Fatalf("want CodeQuotaExceeded, got %d", code)
	}
	if worker.lastRetrieve != nil {
		t.Fatal("worker must not be called when quota is exceeded")
	}
}

// TestGateway_QuotaClamp_WithinCeiling verifies a topK within quota succeeds.
func TestGateway_QuotaClamp_WithinCeiling(t *testing.T) {
	worker := &fakeWorker{
		retrieveResp: &api.RetrieveResponse{
			Result: memory.RetrieveResult{
				Chunks: []memory.ScoredChunk{{Score: 0.9}},
				Total:  1,
			},
		},
	}
	spec := v1.MemoryRetrieverSpec{
		TopK:  10,
		Quota: v1.QuotaSpec{MaxTopK: 20},
		Policy: []v1.MemoryGrant{{
			Identity:   "spiffe://smol-agents.ai/ns/team/sa/agent",
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"*"},
		}},
	}
	gw, _ := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	resp := callTool(t, srv, "spiffe://smol-agents.ai/ns/team/sa/agent",
		"retrieve_memory", map[string]any{
			"query":        "find stuff",
			"retrieverRef": "ns/test-retriever",
			"topK":         15, // within ceiling
		})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if worker.lastRetrieve == nil {
		t.Fatal("worker not called")
	}
	if worker.lastRetrieve.TopK != 15 {
		t.Fatalf("worker TopK = %d, want 15", worker.lastRetrieve.TopK)
	}
}

// TestGateway_AuditNoLeak verifies that audit records never contain document
// content, embeddings, or backend credentials. R-MEM-AUDIT-1, R-MEM-SEC-1.
func TestGateway_AuditNoLeak(t *testing.T) {
	secretContent := "top secret content that must not leak into audit"
	worker := &fakeWorker{
		retrieveResp: &api.RetrieveResponse{
			Result: memory.RetrieveResult{
				Chunks: []memory.ScoredChunk{{
					Chunk: memory.Chunk{Text: secretContent},
					Score: 0.99,
				}},
			},
		},
	}
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   "spiffe://smol-agents.ai/ns/audit-test/sa/agent",
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"*"},
		}},
	}
	gw, col := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	_ = callTool(t, srv, "spiffe://smol-agents.ai/ns/audit-test/sa/agent",
		"retrieve_memory", map[string]any{
			"query":        "search query",
			"retrieverRef": "ns/test-retriever",
		})

	if len(col.Records) == 0 {
		t.Fatal("no audit records")
	}

	rec := col.Records[0]

	// Verify the audit record fields. Serialize the record and check that
	// secret content does not appear anywhere in the serialized form.
	recJSON, _ := json.Marshal(rec)
	if strings.Contains(string(recJSON), secretContent) {
		t.Fatalf("audit record contains document content — must not leak: %s", recJSON)
	}

	// The record must have the required fields populated.
	if rec.CallerSPIFFEID == "" {
		t.Error("audit record missing CallerSPIFFEID")
	}
	if rec.Tenant == "" {
		t.Error("audit record missing Tenant")
	}
	if rec.Op == "" {
		t.Error("audit record missing Op")
	}
	if rec.Decision != audit.DecisionAllow {
		t.Errorf("audit decision = %q, want allow", rec.Decision)
	}
	if rec.LatencyMs < 0 {
		t.Error("audit LatencyMs is negative")
	}
}

// TestGateway_MCPToolList verifies the tools/list response covers all required tools.
func TestGateway_MCPToolList(t *testing.T) {
	gw, _ := testGateway(t, v1.MemoryRetrieverSpec{}, &fakeWorker{})
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)

	wantTools := []string{
		"retrieve_memory",
		"write_memory",
		"list_memory_namespaces",
		"get_memory",
		"delete_memory",
		"summarize_memory",
	}
	toolSet := make(map[string]bool)
	for _, t := range out.Result.Tools {
		toolSet[t.Name] = true
	}
	for _, name := range wantTools {
		if !toolSet[name] {
			t.Errorf("tool %q missing from tools/list", name)
		}
	}
}

// TestGateway_MCPInitialize verifies the initialize handshake.
func TestGateway_MCPInitialize(t *testing.T) {
	gw, _ := testGateway(t, v1.MemoryRetrieverSpec{}, &fakeWorker{})
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"test","version":"1"}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)

	if out["error"] != nil {
		t.Fatalf("initialize failed: %v", out["error"])
	}
	result := out["result"].(map[string]any)
	if result["protocolVersion"] == "" {
		t.Error("initialize response missing protocolVersion")
	}
	if result["capabilities"] == nil {
		t.Error("initialize response missing capabilities")
	}
}

// TestGateway_WriteMemory_PolicyEnforced verifies write requires write grant.
func TestGateway_WriteMemory_PolicyEnforced(t *testing.T) {
	worker := &fakeWorker{}
	// Grant only read, not write.
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   "spiffe://smol-agents.ai/ns/team/sa/reader",
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"*"},
		}},
	}
	gw, _ := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	resp := callTool(t, srv, "spiffe://smol-agents.ai/ns/team/sa/reader",
		"write_memory", map[string]any{
			"content":      "some content",
			"retrieverRef": "ns/test-retriever",
		})

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("write should be denied (no write grant): %v", resp)
	}
	if int(rpcErr["code"].(float64)) != mcp.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied, got %v", rpcErr["code"])
	}
}

// TestGateway_WriteSizeQuota verifies write payload size is enforced.
func TestGateway_WriteSizeQuota(t *testing.T) {
	worker := &fakeWorker{}
	spec := v1.MemoryRetrieverSpec{
		TopK:  10,
		Quota: v1.QuotaSpec{MaxWriteBytes: 10},
		Policy: []v1.MemoryGrant{{
			Identity:   "spiffe://smol-agents.ai/ns/team/sa/writer",
			Operations: []v1.MemoryOperation{v1.MemoryOpWrite},
			Namespaces: []string{"*"},
		}},
	}
	gw, _ := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	bigContent := strings.Repeat("x", 100) // 100 bytes > MaxWriteBytes=10
	resp := callTool(t, srv, "spiffe://smol-agents.ai/ns/team/sa/writer",
		"write_memory", map[string]any{
			"content":      bigContent,
			"retrieverRef": "ns/test-retriever",
		})

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("oversized write should be denied: %v", resp)
	}
	if int(rpcErr["code"].(float64)) != mcp.CodeQuotaExceeded {
		t.Fatalf("want CodeQuotaExceeded, got %v", rpcErr["code"])
	}
}

// TestGateway_ToolCallRPCMapping verifies that each MCP tool routes to the
// correct internal RetrievalService method. R-MEM-MCP-1.
func TestGateway_ToolCallRPCMapping(t *testing.T) {
	cases := []struct {
		tool    string
		args    map[string]any
		op      v1.MemoryOperation
		checkFn func(t *testing.T, w *fakeWorker)
	}{
		{
			tool: "retrieve_memory",
			args: map[string]any{"query": "q", "retrieverRef": "ns/test-retriever"},
			op:   v1.MemoryOpRead,
			checkFn: func(t *testing.T, w *fakeWorker) {
				if w.lastRetrieve == nil {
					t.Error("Retrieve not called")
				}
			},
		},
		{
			tool: "write_memory",
			args: map[string]any{"content": "data", "retrieverRef": "ns/test-retriever"},
			op:   v1.MemoryOpWrite,
			checkFn: func(t *testing.T, w *fakeWorker) {
				if w.lastWrite == nil {
					t.Error("Write not called")
				}
			},
		},
		{
			tool: "get_memory",
			args: map[string]any{"id": "doc-1", "retrieverRef": "ns/test-retriever"},
			op:   v1.MemoryOpRead,
			checkFn: func(t *testing.T, w *fakeWorker) {
				if w.lastGet == nil {
					t.Error("Get not called")
				}
			},
		},
		{
			tool: "delete_memory",
			args: map[string]any{"id": "doc-1", "retrieverRef": "ns/test-retriever"},
			op:   v1.MemoryOpDelete,
			checkFn: func(t *testing.T, w *fakeWorker) {
				if w.lastDelete == nil {
					t.Error("Delete not called")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			worker := &fakeWorker{}
			spiffeID := "spiffe://smol-agents.ai/ns/team/sa/agent"
			spec := v1.MemoryRetrieverSpec{
				TopK: 10,
				Policy: []v1.MemoryGrant{{
					Identity:   spiffeID,
					Operations: []v1.MemoryOperation{tc.op},
					Namespaces: []string{"*"},
				}},
			}
			gw, _ := testGateway(t, spec, worker)
			disp := mcp.NewDispatcher(gw)
			srv := httptest.NewServer(disp)
			defer srv.Close()

			resp := callTool(t, srv, spiffeID, tc.tool, tc.args)
			if resp["error"] != nil {
				t.Fatalf("unexpected error: %v", resp["error"])
			}
			tc.checkFn(t, worker)
		})
	}
}

// TestGateway_ResourcesList verifies the resources/list response.
func TestGateway_ResourcesList(t *testing.T) {
	gw, _ := testGateway(t, v1.MemoryRetrieverSpec{}, &fakeWorker{})
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)

	if out["error"] != nil {
		t.Fatalf("resources/list failed: %v", out["error"])
	}
	result := out["result"].(map[string]any)
	resources := result["resources"].([]any)
	if len(resources) == 0 {
		t.Fatal("resources/list returned no resources")
	}

	// Verify required resource URIs are present.
	uris := make(map[string]bool)
	for _, r := range resources {
		rm := r.(map[string]any)
		uris[rm["uri"].(string)] = true
	}
	for _, want := range []string{
		"memory://namespaces/{namespace}",
		"memory://retrievers/{retrieverRef}",
		"memory://documents/{id}",
		"memory://episodes/{agentId}",
	} {
		if !uris[want] {
			t.Errorf("resource URI %q missing from resources/list", want)
		}
	}
}

// TestGateway_WorkerErrorMappedToRPC verifies that typed worker errors are
// mapped to the appropriate MCP error codes (not swallowed or promoted).
func TestGateway_WorkerErrorMappedToRPC(t *testing.T) {
	cases := []struct {
		name      string
		workerErr error
		wantCode  int
	}{
		{"not_found", memory.NotFound("doc missing"), mcp.CodeNotFound},
		{"backend_unavailable", memory.BackendUnavailable("pg down"), mcp.CodeBackendError},
		{"internal", memory.Internal("oops"), mcp.CodeBackendError},
	}

	spiffeID := "spiffe://smol-agents.ai/ns/team/sa/agent"
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   spiffeID,
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"*"},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			worker := &fakeWorker{err: tc.workerErr}
			gw, _ := testGateway(t, spec, worker)
			disp := mcp.NewDispatcher(gw)
			srv := httptest.NewServer(disp)
			defer srv.Close()

			resp := callTool(t, srv, spiffeID, "retrieve_memory", map[string]any{
				"query":        "q",
				"retrieverRef": "ns/test-retriever",
			})

			rpcErr, ok := resp["error"].(map[string]any)
			if !ok {
				t.Fatalf("expected error for %s, got result: %v", tc.name, resp["result"])
			}
			code := int(rpcErr["code"].(float64))
			if code != tc.wantCode {
				t.Errorf("want code %d, got %d", tc.wantCode, code)
			}
		})
	}
}

// Verify that audit.CollectorLogger does not itself log to stderr (package-level).
func TestCollectorLogger_ImplementsInterface(_ *testing.T) {
	var _ audit.Logger = (*audit.CollectorLogger)(nil)
}

// ensure compile-time interface satisfaction.
var _ mcp.Handler = (*mcp.Gateway)(nil)

// TestGateway_AllowedCallAudited verifies the happy-path audit record.
func TestGateway_AllowedCallAudited(t *testing.T) {
	worker := &fakeWorker{
		retrieveResp: &api.RetrieveResponse{
			Result: memory.RetrieveResult{
				Chunks: []memory.ScoredChunk{{Score: 0.8}, {Score: 0.7}},
				Total:  2,
			},
		},
	}
	spiffeID := "spiffe://smol-agents.ai/ns/myteam/sa/reader"
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   spiffeID,
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"docs"},
		}},
	}
	gw, col := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	_ = callTool(t, srv, spiffeID, "retrieve_memory", map[string]any{
		"query":        "search",
		"retrieverRef": "ns/test-retriever",
		"namespace":    "docs",
	})

	if len(col.Records) == 0 {
		t.Fatal("no audit record for allowed call")
	}
	rec := col.Records[0]
	if rec.Decision != audit.DecisionAllow {
		t.Fatalf("audit decision = %q, want allow", rec.Decision)
	}
	if rec.CallerSPIFFEID != spiffeID {
		t.Fatalf("audit CallerSPIFFEID = %q, want %q", rec.CallerSPIFFEID, spiffeID)
	}
	if rec.Tenant != "myteam" {
		t.Fatalf("audit Tenant = %q, want myteam", rec.Tenant)
	}
	if rec.RetrieverRef != "ns/test-retriever" {
		t.Fatalf("audit RetrieverRef = %q", rec.RetrieverRef)
	}
	if rec.Op != "retrieve_memory" {
		t.Fatalf("audit Op = %q", rec.Op)
	}
	if rec.ResultCount != 2 {
		t.Fatalf("audit ResultCount = %d, want 2", rec.ResultCount)
	}
	if rec.LatencyMs < 0 {
		t.Error("audit LatencyMs is negative")
	}
	if rec.Timestamp.IsZero() {
		t.Error("audit Timestamp is zero")
	}
}

// TestGateway_MergeFSTool_Denied_NoWriteGrant verifies that merge_memory_fs
// requires a write grant on the target namespace — a read-only caller is denied.
func TestGateway_MergeFSTool_Denied_NoWriteGrant(t *testing.T) {
	worker := &fakeWorker{}
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   "spiffe://smol-agents.ai/ns/team/sa/reader",
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"*"},
		}},
	}
	gw, _ := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	resp := callTool(t, srv, "spiffe://smol-agents.ai/ns/team/sa/reader",
		"merge_memory_fs", map[string]any{
			"srcBranch":    "run-001",
			"dstBranch":    "main",
			"retrieverRef": "ns/test-retriever",
			"namespace":    "docs",
		})

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected permission denied, got result: %v", resp["result"])
	}
	if int(rpcErr["code"].(float64)) != mcp.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied, got %v", rpcErr["code"])
	}
}

// TestGateway_MergeFSTool_MissingArgs verifies that omitting srcBranch/dstBranch
// returns an invalid-params error, not a crash.
func TestGateway_MergeFSTool_MissingArgs(t *testing.T) {
	worker := &fakeWorker{}
	spiffeID := "spiffe://smol-agents.ai/ns/team/sa/writer"
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   spiffeID,
			Operations: []v1.MemoryOperation{v1.MemoryOpWrite},
			Namespaces: []string{"*"},
		}},
	}
	gw, _ := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	// srcBranch present but dstBranch absent.
	resp := callTool(t, srv, spiffeID,
		"merge_memory_fs", map[string]any{
			"srcBranch":    "run-001",
			"retrieverRef": "ns/test-retriever",
		})

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected invalid-params error, got result: %v", resp["result"])
	}
	if int(rpcErr["code"].(float64)) != mcp.CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams, got %v", rpcErr["code"])
	}
}

// TestGateway_MergeFSTool_Allowed verifies that a writer can invoke merge_memory_fs
// and that the worker's MergeFS method is called. The fakeWorker returns
// KindNotSupported (non-FS backend), which maps to CodeNotSupported.
func TestGateway_MergeFSTool_Allowed(t *testing.T) {
	worker := &fakeWorker{}
	spiffeID := "spiffe://smol-agents.ai/ns/team/sa/writer"
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   spiffeID,
			Operations: []v1.MemoryOperation{v1.MemoryOpWrite},
			Namespaces: []string{"*"},
		}},
	}
	gw, _ := testGateway(t, spec, worker)
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	resp := callTool(t, srv, spiffeID,
		"merge_memory_fs", map[string]any{
			"srcBranch":    "run-001",
			"dstBranch":    "main",
			"retrieverRef": "ns/test-retriever",
			"namespace":    "docs",
		})

	// fakeWorker.MergeFS returns KindNotSupported — confirm it reaches the wire
	// as CodeNotSupported (not CodePermissionDenied, which would mean authz failed).
	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		// If it succeeded, that's also fine (some fakes return success).
		return
	}
	code := int(rpcErr["code"].(float64))
	if code == mcp.CodePermissionDenied {
		t.Fatalf("authz should have passed for writer; got CodePermissionDenied")
	}
	// KindNotSupported → CodeNotSupported is the expected fakeWorker behaviour.
	if code != mcp.CodeNotSupported {
		t.Fatalf("want CodeNotSupported from fakeWorker, got %d", code)
	}
}

// TestGateway_MergeFSTool_OnConflictRestriction verifies that the gateway
// rejects an onConflict value not in AllowedMergePolicies.
func TestGateway_MergeFSTool_OnConflictRestriction(t *testing.T) {
	spiffeID := "spiffe://smol-agents.ai/ns/team/sa/writer"
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   spiffeID,
			Operations: []v1.MemoryOperation{v1.MemoryOpWrite},
			Namespaces: []string{"*"},
		}},
		AllowedMergePolicies: []string{"fail", "ours"},
	}
	gw, _ := testGateway(t, spec, &fakeWorker{})
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	// "theirs" is not in AllowedMergePolicies → PermissionDenied.
	resp := callTool(t, srv, spiffeID, "merge_memory_fs", map[string]any{
		"srcBranch":    "run-001",
		"dstBranch":    "main",
		"retrieverRef": "ns/test-retriever",
		"namespace":    "docs",
		"onConflict":   "theirs",
	})

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected permission denied, got result: %v", resp["result"])
	}
	if int(rpcErr["code"].(float64)) != mcp.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied, got %v", rpcErr["code"])
	}
}

// TestGateway_MergeFSTool_DefaultMergePolicy verifies that the DefaultMergePolicy
// is applied when the request omits onConflict.
func TestGateway_MergeFSTool_DefaultMergePolicy(t *testing.T) {
	spiffeID := "spiffe://smol-agents.ai/ns/team/sa/writer"

	var capturedReq *api.MergeFSRequest
	worker := &fakeWorkerCapture{
		mergeFn: func(req *api.MergeFSRequest) (*api.MergeFSResponse, error) {
			capturedReq = req
			return &api.MergeFSResponse{Committed: true}, nil
		},
	}

	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   spiffeID,
			Operations: []v1.MemoryOperation{v1.MemoryOpWrite},
			Namespaces: []string{"*"},
		}},
		DefaultMergePolicy: "ours",
	}

	rs := store.NewFakeStore()
	rs.Add("ns/test-retriever", store.RetrieverInfo{Spec: spec, WorkerURL: "http://fake"})
	gw := &mcp.Gateway{
		Auth:       mcp.AuthConfig{},
		Retrievers: rs,
		Quota:      quota.NewEnforcer(),
		WorkerFactory: func(_ string) api.RetrievalService {
			return worker
		},
	}
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	// Request without onConflict — should default to "ours".
	resp := callTool(t, srv, spiffeID, "merge_memory_fs", map[string]any{
		"srcBranch":    "run-001",
		"dstBranch":    "main",
		"retrieverRef": "ns/test-retriever",
		"namespace":    "docs",
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if capturedReq == nil {
		t.Fatal("worker MergeFS was not called")
	}
	if capturedReq.OnConflict != "ours" {
		t.Errorf("OnConflict=%q, want ours (from DefaultMergePolicy)", capturedReq.OnConflict)
	}
}

// fakeWorkerCapture is a fakeWorker that captures MergeFS calls via a function.
type fakeWorkerCapture struct {
	fakeWorker
	mergeFn func(*api.MergeFSRequest) (*api.MergeFSResponse, error)
}

func (f *fakeWorkerCapture) MergeFS(_ context.Context, req *api.MergeFSRequest) (*api.MergeFSResponse, error) {
	if f.mergeFn != nil {
		return f.mergeFn(req)
	}
	return &api.MergeFSResponse{}, nil
}

// TestGateway_MergeFSTool_DefaultPolicyRejectedByAllowed verifies that the
// effective policy — after DefaultMergePolicy fill-in — is checked against
// AllowedMergePolicies. If the default itself isn't allowed, reject.
func TestGateway_MergeFSTool_DefaultPolicyRejectedByAllowed(t *testing.T) {
	spiffeID := "spiffe://smol-agents.ai/ns/team/sa/writer"
	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   spiffeID,
			Operations: []v1.MemoryOperation{v1.MemoryOpWrite},
			Namespaces: []string{"*"},
		}},
		// Default is "markers" but only "fail" and "ours" are allowed.
		DefaultMergePolicy:   "markers",
		AllowedMergePolicies: []string{"fail", "ours"},
	}
	gw, _ := testGateway(t, spec, &fakeWorker{})
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	// Request omits onConflict → effective = DefaultMergePolicy = "markers"
	// which is not in AllowedMergePolicies → PermissionDenied.
	resp := callTool(t, srv, spiffeID, "merge_memory_fs", map[string]any{
		"srcBranch":    "run-001",
		"dstBranch":    "main",
		"retrieverRef": "ns/test-retriever",
		"namespace":    "docs",
		// onConflict intentionally omitted
	})

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected permission denied (default markers not in allowed), got result: %v", resp["result"])
	}
	if int(rpcErr["code"].(float64)) != mcp.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied, got %v", rpcErr["code"])
	}
}

// TestGateway_MergeFSTool_AllowedPolicies_ExplicitMarkers verifies that
// explicitly requesting "markers" passes when it is in AllowedMergePolicies.
func TestGateway_MergeFSTool_AllowedPolicies_ExplicitMarkers(t *testing.T) {
	spiffeID := "spiffe://smol-agents.ai/ns/team/sa/writer"

	var capturedReq *api.MergeFSRequest
	worker := &fakeWorkerCapture{
		mergeFn: func(req *api.MergeFSRequest) (*api.MergeFSResponse, error) {
			capturedReq = req
			return &api.MergeFSResponse{Committed: true}, nil
		},
	}

	spec := v1.MemoryRetrieverSpec{
		TopK: 10,
		Policy: []v1.MemoryGrant{{
			Identity:   spiffeID,
			Operations: []v1.MemoryOperation{v1.MemoryOpWrite},
			Namespaces: []string{"*"},
		}},
		AllowedMergePolicies: []string{"fail", "ours", "markers"},
	}

	rs := store.NewFakeStore()
	rs.Add("ns/test-retriever", store.RetrieverInfo{Spec: spec, WorkerURL: "http://fake"})
	gw := &mcp.Gateway{
		Auth:       mcp.AuthConfig{},
		Retrievers: rs,
		Quota:      quota.NewEnforcer(),
		WorkerFactory: func(_ string) api.RetrievalService {
			return worker
		},
	}
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	resp := callTool(t, srv, spiffeID, "merge_memory_fs", map[string]any{
		"srcBranch":    "run-001",
		"dstBranch":    "main",
		"retrieverRef": "ns/test-retriever",
		"namespace":    "docs",
		"onConflict":   "markers",
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if capturedReq == nil {
		t.Fatal("worker MergeFS was not called")
	}
	if capturedReq.OnConflict != "markers" {
		t.Errorf("OnConflict=%q, want markers", capturedReq.OnConflict)
	}
}

// TestGateway_FakeStoreNotFound tests the retriever-not-found path.
func TestGateway_FakeStoreNotFound(t *testing.T) {
	// Store is empty — any retrieverRef will return NotFound.
	col := &audit.CollectorLogger{}
	gw := &mcp.Gateway{
		Auth:       mcp.AuthConfig{},
		Retrievers: store.NewFakeStore(),
		Quota:      quota.NewEnforcer(),
		AuditLog:   col,
	}
	disp := mcp.NewDispatcher(gw)
	srv := httptest.NewServer(disp)
	defer srv.Close()

	resp := callTool(t, srv, "spiffe://smol-agents.ai/ns/x/sa/y",
		"retrieve_memory", map[string]any{
			"query":        "q",
			"retrieverRef": "ns/does-not-exist",
		})

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected NotFound error, got: %v", resp)
	}
	code := int(rpcErr["code"].(float64))
	if code != mcp.CodeNotFound {
		t.Fatalf("want CodeNotFound, got %d", code)
	}
}
