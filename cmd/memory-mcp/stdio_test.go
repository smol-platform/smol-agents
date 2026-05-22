package main

// Tests for the stdio JSON-RPC transport.
// These are in package main so they can access stdioRunner and helpers directly.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/memory"
	"github.com/smol-platform/smol-agents/pkg/memory/api"
	"github.com/smol-platform/smol-agents/pkg/memory/audit"
	"github.com/smol-platform/smol-agents/pkg/memory/mcp"
	"github.com/smol-platform/smol-agents/pkg/memory/quota"
	"github.com/smol-platform/smol-agents/pkg/memory/store"
)

// buildTestDispatcher returns a Dispatcher backed by a Gateway configured
// for insecure mode (no SPIRE) with the given retriever and fake worker.
func buildTestDispatcher(ref string, spec v1.MemoryRetrieverSpec, worker api.RetrievalService) *mcp.Dispatcher {
	rs := store.NewFakeStore()
	rs.Add(ref, store.RetrieverInfo{Spec: spec, WorkerURL: "http://fake-worker"})
	gw := &mcp.Gateway{
		Auth:       mcp.AuthConfig{}, // insecure: ParseInsecure
		Retrievers: rs,
		Quota:      quota.NewEnforcer(),
		AuditLog:   &audit.CollectorLogger{},
		WorkerFactory: func(_ string) api.RetrievalService {
			return worker
		},
	}
	return mcp.NewDispatcher(gw)
}

// runStdio invokes stdioRunner with the given input lines, waits for it to
// complete (EOF), and returns the response lines.
func runStdio(t *testing.T, disp *mcp.Dispatcher, spiffeID string, lines ...string) []map[string]any {
	t.Helper()
	input := strings.Join(lines, "\n") + "\n"
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))

	ctx := context.Background()
	stdioRunner(ctx, disp, strings.NewReader(input), &out, spiffeID, log)

	// Parse response lines (skip log lines that are not valid JSON objects with "jsonrpc").
	var results []map[string]any
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if _, ok := m["jsonrpc"]; ok {
			results = append(results, m)
		}
	}
	return results
}

// TestStdio_Initialize verifies that the initialize handshake works over stdio.
func TestStdio_Initialize(t *testing.T) {
	disp := buildTestDispatcher("ns/r", v1.MemoryRetrieverSpec{TopK: 10}, &fakeStdioWorker{})
	const spiffeID = "spiffe://local/ns/team/sa/ide"

	resps := runStdio(t, disp, spiffeID,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"test","version":"1"}}}`,
	)

	if len(resps) == 0 {
		t.Fatal("no responses from stdio")
	}
	resp := resps[0]
	if resp["error"] != nil {
		t.Fatalf("initialize error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result not an object: %v", resp["result"])
	}
	if result["protocolVersion"] == "" {
		t.Error("protocolVersion missing")
	}
}

// TestStdio_ToolsList verifies that tools/list works over stdio.
func TestStdio_ToolsList(t *testing.T) {
	disp := buildTestDispatcher("ns/r", v1.MemoryRetrieverSpec{TopK: 10}, &fakeStdioWorker{})
	const spiffeID = "spiffe://local/ns/team/sa/ide"

	resps := runStdio(t, disp, spiffeID,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)

	if len(resps) == 0 {
		t.Fatal("no responses")
	}
	resp := resps[0]
	if resp["error"] != nil {
		t.Fatalf("tools/list error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) == 0 {
		t.Error("tools/list returned no tools")
	}
}

// TestStdio_ToolCall_Dispatch verifies that a retrieve_memory call is dispatched
// through the gateway and returns a result (not an auth error) when the
// syntheticSPIFFEID grants the required policy.
func TestStdio_ToolCall_Dispatch(t *testing.T) {
	const spiffeID = "spiffe://local/ns/team/sa/ide"
	spec := v1.MemoryRetrieverSpec{
		Stores: []string{"s"},
		TopK:   10,
		Policy: []v1.MemoryGrant{{
			Identity:   spiffeID,
			Operations: []v1.MemoryOperation{v1.MemoryOpRead},
			Namespaces: []string{"*"},
		}},
	}
	worker := &fakeStdioWorker{
		retrieveResp: &api.RetrieveResponse{
			Result: memory.RetrieveResult{
				Chunks: []memory.ScoredChunk{{Score: 0.9}},
				Total:  1,
			},
		},
	}
	disp := buildTestDispatcher("ns/r", spec, worker)

	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"retrieve_memory","arguments":{"query":"hello","retrieverRef":"ns/r"}}}`
	resps := runStdio(t, disp, spiffeID, call)

	if len(resps) == 0 {
		t.Fatal("no responses")
	}
	resp := resps[0]
	if resp["error"] != nil {
		t.Fatalf("retrieve_memory error: %v", resp["error"])
	}
}

// TestStdio_MultipleRequests verifies that multiple requests on stdin are each
// dispatched independently and produce one response per request.
func TestStdio_MultipleRequests(t *testing.T) {
	disp := buildTestDispatcher("ns/r", v1.MemoryRetrieverSpec{TopK: 10}, &fakeStdioWorker{})
	const spiffeID = "spiffe://local/ns/team/sa/ide"

	resps := runStdio(t, disp, spiffeID,
		`{"jsonrpc":"2.0","id":10,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":11,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":12,"method":"resources/list"}`,
	)

	if len(resps) != 3 {
		t.Fatalf("want 3 responses, got %d: %v", len(resps), resps)
	}
	for _, r := range resps {
		if r["error"] != nil {
			t.Errorf("unexpected error in response: %v", r["error"])
		}
	}
}

// TestStdio_BlankLinesSkipped verifies that blank lines in the input do not
// produce extra responses.
func TestStdio_BlankLinesSkipped(t *testing.T) {
	disp := buildTestDispatcher("ns/r", v1.MemoryRetrieverSpec{TopK: 10}, &fakeStdioWorker{})
	const spiffeID = "spiffe://local/ns/team/sa/ide"

	resps := runStdio(t, disp, spiffeID,
		"",
		"",
		`{"jsonrpc":"2.0","id":20,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"t","version":"1"}}}`,
		"",
	)

	if len(resps) != 1 {
		t.Fatalf("want exactly 1 response, got %d: %v", len(resps), resps)
	}
}

// TestStdio_ParseError verifies that malformed JSON produces a parse-error
// response rather than crashing.
func TestStdio_ParseError(t *testing.T) {
	disp := buildTestDispatcher("ns/r", v1.MemoryRetrieverSpec{TopK: 10}, &fakeStdioWorker{})
	const spiffeID = "spiffe://local/ns/team/sa/ide"

	resps := runStdio(t, disp, spiffeID, `{not valid json`)

	if len(resps) == 0 {
		t.Fatal("expected parse-error response for malformed JSON")
	}
	resp := resps[0]
	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error field, got: %v", resp)
	}
	code := int(rpcErr["code"].(float64))
	if code != -32700 && code != -32600 {
		t.Errorf("want parse-error code (-32700 or -32600), got %d", code)
	}
}

// TestBuildSyntheticBearerToken verifies the token builder produces a valid
// three-part JWT-ish string that ParseInsecure can handle.
func TestBuildSyntheticBearerToken(t *testing.T) {
	token := buildSyntheticBearerToken("spiffe://td/ns/team/sa/ide")
	if !strings.HasPrefix(token, "Bearer ") {
		t.Fatalf("token must start with 'Bearer ', got: %q", token)
	}
	parts := strings.SplitN(strings.TrimPrefix(token, "Bearer "), ".", 3)
	if len(parts) != 3 {
		t.Fatalf("JWT must have 3 parts, got %d", len(parts))
	}
	// Payload should decode to JSON with a "sub" claim.
	payload := decodeBase64URL(t, parts[1])
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if claims["sub"] != "spiffe://td/ns/team/sa/ide" {
		t.Errorf("sub = %q, want spiffe://td/ns/team/sa/ide", claims["sub"])
	}
}

// ── Fake worker ───────────────────────────────────────────────────────────

type fakeStdioWorker struct {
	retrieveResp *api.RetrieveResponse
}

func (f *fakeStdioWorker) Retrieve(_ context.Context, _ *api.RetrieveRequest) (*api.RetrieveResponse, error) {
	if f.retrieveResp != nil {
		return f.retrieveResp, nil
	}
	return &api.RetrieveResponse{}, nil
}
func (f *fakeStdioWorker) Write(_ context.Context, _ *api.WriteRequest) (*api.WriteResponse, error) {
	return &api.WriteResponse{Result: memory.WriteResult{ID: "doc-1"}}, nil
}
func (f *fakeStdioWorker) Get(_ context.Context, _ *api.GetRequest) (*api.GetResponse, error) {
	return &api.GetResponse{}, nil
}
func (f *fakeStdioWorker) Delete(_ context.Context, _ *api.DeleteRequest) (*api.DeleteResponse, error) {
	return &api.DeleteResponse{}, nil
}
func (f *fakeStdioWorker) ListNamespaces(_ context.Context, _ *api.ListNamespacesRequest) (*api.ListNamespacesResponse, error) {
	return &api.ListNamespacesResponse{Namespaces: []string{"ns1"}}, nil
}
func (f *fakeStdioWorker) Summarize(_ context.Context, _ *api.SummarizeRequest) (*api.SummarizeResponse, error) {
	return &api.SummarizeResponse{Summary: "summary"}, nil
}
func (f *fakeStdioWorker) BranchFS(_ context.Context, _ *api.BranchFSRequest) (*api.BranchFSResponse, error) {
	return nil, &memory.Error{Kind: memory.KindNotSupported}
}
func (f *fakeStdioWorker) SnapshotFS(_ context.Context, _ *api.SnapshotFSRequest) (*api.SnapshotFSResponse, error) {
	return nil, &memory.Error{Kind: memory.KindNotSupported}
}
func (f *fakeStdioWorker) ListBranches(_ context.Context, _ *api.ListBranchesRequest) (*api.ListBranchesResponse, error) {
	return nil, &memory.Error{Kind: memory.KindNotSupported}
}
func (f *fakeStdioWorker) MergeFS(_ context.Context, _ *api.MergeFSRequest) (*api.MergeFSResponse, error) {
	return nil, &memory.Error{Kind: memory.KindNotSupported}
}

var _ api.RetrievalService = (*fakeStdioWorker)(nil)

// ── Base64URL helper ───────────────────────────────────────────────────────

// decodeBase64URL decodes a base64url (no-padding) string to bytes.
func decodeBase64URL(t *testing.T, s string) []byte {
	t.Helper()
	// Add padding.
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	// Replace URL-safe chars.
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	import64 := func(c byte) byte {
		switch {
		case c >= 'A' && c <= 'Z':
			return c - 'A'
		case c >= 'a' && c <= 'z':
			return c - 'a' + 26
		case c >= '0' && c <= '9':
			return c - '0' + 52
		case c == '+':
			return 62
		case c == '/':
			return 63
		}
		return 0
	}
	// Minimal base64 decode.
	var out []byte
	for i := 0; i+3 < len(s); i += 4 {
		b0 := import64(s[i])
		b1 := import64(s[i+1])
		b2 := import64(s[i+2])
		b3 := import64(s[i+3])
		out = append(out, b0<<2|b1>>4)
		if s[i+2] != '=' {
			out = append(out, b1<<4|b2>>2)
		}
		if s[i+3] != '=' {
			out = append(out, b2<<6|b3)
		}
	}
	return out
}
