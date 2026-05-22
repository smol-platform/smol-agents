package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"

	"github.com/stigen/smol-agents/pkg/identity"
)

// runMemory exercises the memory-mcp gateway end-to-end against real SPIRE:
// the probe authenticates with its JWT-SVID, writes a document, retrieves it
// (proving the gateway injects the caller's tenant and the worker returns it),
// and confirms a cross-tenant filter is rejected (tenant isolation).
// R-E2E-SCN-MEMORY.
func runMemory(ctx context.Context, socket, mcpURL, retrieverRef, foreignRef, audience string) bool {
	const id = "memory"
	if mcpURL == "" || retrieverRef == "" {
		fail(id, "--memory-mcp-url and --retriever-ref are required")
		return false
	}

	src, err := identity.Open(ctx, identity.SourceConfig{WorkloadAPIAddr: socket, Mode: identity.ModeStrict})
	if err != nil {
		fail(id, "identity.Open: %v", err)
		return false
	}
	defer src.Close()
	tok, err := src.JWTSource().FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience})
	if err != nil {
		fail(id, "FetchJWTSVID: %v", err)
		return false
	}
	bearer := "Bearer " + tok.Marshal()

	// 1. write_memory
	const content = "GPU scheduling decisions: prefer bare-metal nodes for kata-fc"
	if _, rpcErr, err := mcpCall(ctx, mcpURL, bearer, "write_memory", map[string]any{
		"content": content, "namespace": "kb", "retrieverRef": retrieverRef,
	}); err != nil || rpcErr != nil {
		fail(id, "write_memory: err=%v rpc=%v", err, rpcErr)
		return false
	}

	// 2. retrieve_memory — must return what we wrote (tenant injected from SVID).
	text, rpcErr, err := mcpCall(ctx, mcpURL, bearer, "retrieve_memory", map[string]any{
		"query": "GPU scheduling", "topK": 5, "namespace": "kb", "retrieverRef": retrieverRef,
	})
	if err != nil || rpcErr != nil {
		fail(id, "retrieve_memory: err=%v rpc=%v", err, rpcErr)
		return false
	}
	if !strings.Contains(text, "GPU scheduling") {
		fail(id, "retrieve_memory did not return the written doc: %q", truncate(text, 200))
		return false
	}

	// 3. cross-tenant: using a retriever scoped to ANOTHER tenant must be
	//    rejected — the gateway derives the caller's tenant from the SVID and
	//    refuses a retriever whose tenant scope doesn't match (R-MEM-AUTH-1).
	if foreignRef != "" {
		_, rpcErr, err = mcpCall(ctx, mcpURL, bearer, "retrieve_memory", map[string]any{
			"query": "GPU", "topK": 5, "namespace": "kb", "retrieverRef": foreignRef,
		})
		if err != nil {
			fail(id, "cross-tenant call transport error: %v", err)
			return false
		}
		if rpcErr == nil {
			fail(id, "cross-tenant retriever %q was NOT denied (tenant isolation broken)", foreignRef)
			return false
		}
		pass(id, "write+retrieve OK; foreign-tenant retriever denied (%s); agent confined to its tenant", rpcErr.Message)
		return true
	}

	pass(id, "write+retrieve OK (no foreign retriever configured to test cross-tenant)")
	return true
}

// --- MCP JSON-RPC helpers ---

type mcpReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResp struct {
	Result *struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *mcpRPCError `json:"error"`
}

// mcpCall issues a tools/call and returns the concatenated text content, an
// RPC error (JSON-RPC error OR an isError tool result), and a transport error.
func mcpCall(ctx context.Context, base, bearer, tool string, args map[string]any) (string, *mcpRPCError, error) {
	argsJSON, _ := json.Marshal(args)
	body, _ := json.Marshal(mcpReq{
		JSONRPC: "2.0", ID: 1, Method: "tools/call",
		Params: map[string]any{"name": tool, "arguments": json.RawMessage(argsJSON)},
	})
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, strings.TrimRight(base, "/")+"/mcp", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out mcpResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("decode MCP response (status %d): %v: %s", resp.StatusCode, err, truncate(string(raw), 200))
	}
	if out.Error != nil {
		return "", out.Error, nil
	}
	if out.Result != nil && out.Result.IsError {
		var b strings.Builder
		for _, c := range out.Result.Content {
			b.WriteString(c.Text)
		}
		return "", &mcpRPCError{Code: -1, Message: b.String()}, nil
	}
	var b strings.Builder
	if out.Result != nil {
		for _, c := range out.Result.Content {
			b.WriteString(c.Text)
		}
	}
	return b.String(), nil, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
