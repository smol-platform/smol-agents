package mcp_test

// Tests for the memory://episodes/{agentId} resource (R-MEM-MCP-2).
//
// The episodes resource reads event-log namespaces for an agent via the worker.
// It enforces the same auth/tenant/policy/audit path as tools.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/memory/api"
	"github.com/stigen/smol-agents/pkg/memory/audit"
	"github.com/stigen/smol-agents/pkg/memory/mcp"
	"github.com/stigen/smol-agents/pkg/memory/quota"
	"github.com/stigen/smol-agents/pkg/memory/store"
)

// readResource sends a JSON-RPC resources/read request and returns the decoded response.
func readResource(t *testing.T, srv *httptest.Server, spiffeID, uri string) map[string]any {
	t.Helper()
	token := buildJWT(spiffeID, "memory-mcp")
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":%q}}`, uri)
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

// newEpisodesServer builds a gateway with the given worker and policy.
func newEpisodesServer(t *testing.T, worker api.RetrievalService, policy []v1.MemoryGrant, tenant string) *httptest.Server {
	t.Helper()
	rs := store.NewFakeStore()
	rs.Add("ns/ep-retriever", store.RetrieverInfo{
		Spec: v1.MemoryRetrieverSpec{
			Stores: []string{"s"},
			TopK:   10,
			Tenant: tenant,
			Policy: policy,
		},
		WorkerURL: "http://fake-worker",
	})
	gw := &mcp.Gateway{
		Auth:       mcp.AuthConfig{},
		Retrievers: rs,
		Quota:      quota.NewEnforcer(),
		AuditLog:   &audit.CollectorLogger{},
		WorkerFactory: func(_ string) api.RetrievalService {
			return worker
		},
	}
	srv := httptest.NewServer(mcp.NewDispatcher(gw))
	t.Cleanup(srv.Close)
	return srv
}

// TestEpisodes_HappyPath verifies that an authorised caller can read
// memory://episodes/{agentId} and receives the agent namespaces from the worker.
func TestEpisodes_HappyPath(t *testing.T) {
	const spiffeID = "spiffe://stigen.ai/ns/myteam/sa/agent"
	const agentID = "agent-abc123"

	worker := &fakeWorker{
		listNsResp: &api.ListNamespacesResponse{
			Namespaces: []string{"run-1", "run-2"},
		},
	}
	policy := []v1.MemoryGrant{{
		Identity:   spiffeID,
		Operations: []v1.MemoryOperation{v1.MemoryOpRead},
		Namespaces: []string{"*"},
	}}
	srv := newEpisodesServer(t, worker, policy, "myteam")

	uri := fmt.Sprintf("memory://episodes/%s?retrieverRef=ns/ep-retriever", agentID)
	resp := readResource(t, srv, spiffeID, uri)

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resp["result"])
	}
	contents, ok := result["contents"].([]any)
	if !ok || len(contents) == 0 {
		t.Fatalf("no contents in episodes result: %v", result)
	}
	item := contents[0].(map[string]any)
	text := item["text"].(string)

	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode episodes payload: %v", err)
	}
	if payload["agentId"] != agentID {
		t.Errorf("agentId = %q, want %q", payload["agentId"], agentID)
	}
	if payload["tenant"] != "myteam" {
		t.Errorf("tenant = %q, want myteam", payload["tenant"])
	}
	nsList, _ := payload["namespaces"].([]any)
	if len(nsList) != 2 {
		t.Errorf("namespaces count = %d, want 2", len(nsList))
	}
}

// TestEpisodes_MissingRetrieverRef verifies that omitting the retrieverRef
// query param returns InvalidParams.
func TestEpisodes_MissingRetrieverRef(t *testing.T) {
	const spiffeID = "spiffe://stigen.ai/ns/myteam/sa/agent"
	srv := newEpisodesServer(t, &fakeWorker{}, nil, "")

	// No retrieverRef query param.
	resp := readResource(t, srv, spiffeID, "memory://episodes/agent-abc")

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error for missing retrieverRef, got result: %v", resp["result"])
	}
	code := int(rpcErr["code"].(float64))
	if code != mcp.CodeInvalidParams {
		t.Errorf("want CodeInvalidParams, got %d", code)
	}
}

// TestEpisodes_Unauthenticated verifies that missing Authorization returns an error.
func TestEpisodes_Unauthenticated(t *testing.T) {
	srv := newEpisodesServer(t, &fakeWorker{}, nil, "")

	uri := "memory://episodes/agent-abc?retrieverRef=ns/ep-retriever"
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":%q}}`, uri)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)

	if out["error"] == nil {
		t.Fatal("expected error for unauthenticated episode read")
	}
}

// TestEpisodes_TenantIsolation verifies that a caller from "team-beta" cannot
// read episodes from a retriever scoped to "team-alpha".
func TestEpisodes_TenantIsolation(t *testing.T) {
	rs := store.NewFakeStore()
	rs.Add("ns/alpha-retriever", store.RetrieverInfo{
		Spec: v1.MemoryRetrieverSpec{
			Stores: []string{"s"},
			TopK:   10,
			Tenant: "team-alpha", // scoped to team-alpha
			Policy: []v1.MemoryGrant{{
				Identity:   "spiffe://stigen.ai/ns/team-alpha/",
				Operations: []v1.MemoryOperation{v1.MemoryOpRead},
				Namespaces: []string{"*"},
			}},
		},
		WorkerURL: "http://fake-worker",
	})
	gw := &mcp.Gateway{
		Auth:       mcp.AuthConfig{},
		Retrievers: rs,
		Quota:      quota.NewEnforcer(),
		WorkerFactory: func(_ string) api.RetrievalService {
			return &fakeWorker{}
		},
	}
	srv := httptest.NewServer(mcp.NewDispatcher(gw))
	t.Cleanup(srv.Close)

	// Caller from team-beta tries to read team-alpha's episodes.
	uri := "memory://episodes/agent-abc?retrieverRef=ns/alpha-retriever"
	resp := readResource(t, srv, "spiffe://stigen.ai/ns/team-beta/sa/intruder", uri)

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("cross-tenant episodes read must be denied, got result: %v", resp["result"])
	}
	code := int(rpcErr["code"].(float64))
	if code != mcp.CodePermissionDenied {
		t.Errorf("want CodePermissionDenied, got %d", code)
	}
}

// TestEpisodes_PolicyDenied verifies that a caller without read grant is denied.
func TestEpisodes_PolicyDenied(t *testing.T) {
	const spiffeID = "spiffe://stigen.ai/ns/myteam/sa/agent"
	// Empty policy → deny-by-default.
	srv := newEpisodesServer(t, &fakeWorker{}, nil, "myteam")

	uri := "memory://episodes/agent-abc?retrieverRef=ns/ep-retriever"
	resp := readResource(t, srv, spiffeID, uri)

	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("no-policy episodes read must be denied, got: %v", resp["result"])
	}
	code := int(rpcErr["code"].(float64))
	if code != mcp.CodePermissionDenied {
		t.Errorf("want CodePermissionDenied, got %d", code)
	}
}
